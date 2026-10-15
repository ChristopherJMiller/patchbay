// Package chart_test renders the Helm chart (helm template, from the flake's
// shell) and checks the PBX pod against the Go side of its contracts: the
// live directory and Asterisk's config (internal/compile/layout.go), the
// agent's mounts, uids and file server identity (internal/agent/layout),
// the narrowed module directory (compile.Modules) and the sound prompts.
// The chart hard-codes these paths; these tests keep the two in step.
package chart_test

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sounds"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

const release = "patchbay" // contains the chart name, so the fullname is "patchbay"

var chartDir = filepath.Join("..", "..", "charts", "patchbay")

// manifest is one rendered chart.
type manifest struct {
	raw  string
	docs map[string][]byte // "<Kind>/<name>" -> YAML
}

// render runs helm template with the chart's minimal ci values plus sets.
func render(t *testing.T, sets ...string) manifest {
	t.Helper()
	return renderFile(t, filepath.Join(chartDir, "ci", "minimal-values.yaml"), sets...)
}

func get[T any](t *testing.T, m manifest, kind, name string) *T {
	t.Helper()
	doc, ok := m.docs[kind+"/"+name]
	if !ok {
		t.Fatalf("the chart renders no %s %s", kind, name)
	}
	var v T
	if err := yaml.UnmarshalStrict(doc, &v); err != nil {
		t.Fatalf("%s %s: %v", kind, name, err)
	}
	return &v
}

// pbx returns the PBX pod spec of a rendered chart.
func pbx(t *testing.T, m manifest) *corev1.PodSpec {
	t.Helper()
	return &get[appsv1.Deployment](t, m, "Deployment", release+"-pbx").Spec.Template.Spec
}

// container finds a container or init container by name.
func container(t *testing.T, pod *corev1.PodSpec, name string) *corev1.Container {
	t.Helper()
	for _, cs := range [][]corev1.Container{pod.InitContainers, pod.Containers} {
		for i := range cs {
			if cs[i].Name == name {
				return &cs[i]
			}
		}
	}
	t.Fatalf("the PBX pod has no container %s", name)
	return nil
}

// mounts lists, per container, where volume is mounted and whether read-only.
func mounts(pod *corev1.PodSpec, volume string) map[string]string {
	out := map[string]string{}
	for _, cs := range [][]corev1.Container{pod.InitContainers, pod.Containers} {
		for _, c := range cs {
			for _, vm := range c.VolumeMounts {
				if vm.Name == volume {
					mode := "rw"
					if vm.ReadOnly {
						mode = "ro"
					}
					out[c.Name] = vm.MountPath + " " + mode
				}
			}
		}
	}
	return out
}

func wantMounts(t *testing.T, pod *corev1.PodSpec, volume string, want map[string]string) {
	t.Helper()
	got := mounts(pod, volume)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("volume %s is mounted %v, want %v", volume, got, want)
	}
}

// flag returns the value of --name=value in args, and whether it is there
// exactly once.
func flag(args []string, name string) (string, bool) {
	var vals []string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
			vals = append(vals, v)
		}
	}
	if len(vals) != 1 {
		return "", false
	}
	return vals[0], true
}

func wantFlags(t *testing.T, c *corev1.Container, want map[string]string) {
	t.Helper()
	for name, v := range want {
		if got, ok := flag(c.Args, name); !ok || got != v {
			t.Errorf("%s: --%s = %q (once: %v), want %q", c.Name, name, got, ok, v)
		}
	}
}

// TestChartUsesTheLiveLayout: the live config volume is mounted whole at
// compile.LiveDir in exactly three containers (the agent writing, Asterisk
// and the file server read-only), Asterisk runs on the compiled
// asterisk.conf through the current symlink, and the prompts reach
// Asterisk from where the agent installs them.
func TestChartUsesTheLiveLayout(t *testing.T) {
	pod := pbx(t, render(t))
	wantMounts(t, pod, "config", map[string]string{
		"agent":      compile.LiveDir + " rw",
		"asterisk":   compile.LiveDir + " ro",
		"fileserver": compile.LiveDir + " ro",
	})
	for _, c := range []string{"agent", "fileserver"} {
		wantFlags(t, container(t, pod, c), map[string]string{"live-dir": compile.LiveDir})
	}
	asterisk := container(t, pod, "asterisk")
	conf := compile.AsteriskConfigDir + "/asterisk.conf"
	if !slices.Equal(asterisk.Command, []string{"/usr/sbin/asterisk"}) || !containsSeq(asterisk.Args, "-C", conf) {
		t.Errorf("asterisk runs %v %v, want -C %s", asterisk.Command, asterisk.Args, conf)
	}

	wantMounts(t, pod, "sounds", map[string]string{
		"agent":    compile.SoundsDir + " rw",
		"asterisk": compile.SoundsDir + " ro",
	})
	wantFlags(t, container(t, pod, "agent"), map[string]string{
		"sounds-dir": compile.SoundsDir,
		"sounds-src": sounds.DefaultSource,
	})
}

// TestChartAsteriskProbesUseTheConfig: every Asterisk probe runs the remote
// console with -C, like Asterisk itself.
func TestChartAsteriskProbesUseTheConfig(t *testing.T) {
	asterisk := container(t, pbx(t, render(t)), "asterisk")
	conf := compile.AsteriskConfigDir + "/asterisk.conf"
	probes := map[string]*corev1.Probe{
		"startup": asterisk.StartupProbe, "readiness": asterisk.ReadinessProbe, "liveness": asterisk.LivenessProbe,
	}
	n := 0
	for name, p := range probes {
		if p == nil {
			continue
		}
		n++
		if p.Exec == nil || !containsSeq(p.Exec.Command, "-C", conf) || !slices.Contains(p.Exec.Command, "-rx") {
			t.Errorf("asterisk %s probe %+v does not run asterisk -C %s -rx", name, p.ProbeHandler, conf)
		}
	}
	if n == 0 {
		t.Error("asterisk has no probes")
	}
}

// TestChartModulesMatchCompile: the init container copies exactly
// compile.Modules() into the volume Asterisk mounts read-only over
// compile.AsteriskModuleDir, before anything else in the pod starts.
func TestChartModulesMatchCompile(t *testing.T) {
	pod := pbx(t, render(t))
	if len(pod.InitContainers) == 0 || pod.InitContainers[0].Name != "asterisk-modules" {
		t.Fatalf("the first init container is not asterisk-modules: %v", pod.InitContainers)
	}
	init := pod.InitContainers[0]
	if !slices.Equal(init.Args, compile.Modules()) {
		t.Errorf("asterisk-modules copies\n  %v\nwant compile.Modules()\n  %v", init.Args, compile.Modules())
	}
	script := strings.Join(init.Command, " ")
	if !strings.Contains(script, compile.AsteriskModuleDir+`/"$m" `+layout.ModulesStagingDir+"/") {
		t.Errorf("asterisk-modules runs %q, want a copy from %s to %s", script, compile.AsteriskModuleDir, layout.ModulesStagingDir)
	}
	if init.RestartPolicy != nil {
		t.Errorf("asterisk-modules is a sidecar (restartPolicy %s); it must run to completion first", *init.RestartPolicy)
	}
	wantMounts(t, pod, "asterisk-modules", map[string]string{
		"asterisk-modules": layout.ModulesStagingDir + " rw",
		"asterisk":         compile.AsteriskModuleDir + " ro",
	})
	if init.Image != container(t, pod, "asterisk").Image {
		t.Errorf("asterisk-modules runs %s, not Asterisk's image", init.Image)
	}
	if u := init.SecurityContext.RunAsUser; u == nil || *u != layout.AsteriskUID {
		t.Errorf("asterisk-modules runs as %v, want %d", u, layout.AsteriskUID)
	}
}

// TestChartUsesTheAgentLayout: every internal/agent/layout path and uid,
// as the chart spells it.
func TestChartUsesTheAgentLayout(t *testing.T) {
	m := render(t)
	pod := pbx(t, m)
	agent, fileserver, asterisk := container(t, pod, "agent"), container(t, pod, "fileserver"), container(t, pod, "asterisk")

	wantMounts(t, pod, "persistent", map[string]string{
		"agent":      layout.PersistentDir + " rw",
		"fileserver": layout.PersistentDir + " ro",
	})
	wantMounts(t, pod, "fetchlog", map[string]string{
		"agent":      layout.FetchSocketDir + " rw",
		"fileserver": layout.FetchSocketDir + " ro",
	})
	wantFlags(t, agent, map[string]string{
		"firmware-dir": layout.FirmwareDir,
		"state-dir":    layout.StateDir,
		"fetch-socket": layout.FetchSocket,
		"fs-group":     strconv.Itoa(layout.DefaultFSGroup),
	})
	wantFlags(t, fileserver, map[string]string{
		"firmware-dir": layout.FirmwareDir,
		"fetch-socket": layout.FetchSocket,
		"uid":          strconv.Itoa(layout.FileserverUID),
		"gid":          strconv.Itoa(layout.FileserverGID),
	})
	if v := pod.Volumes[slices.IndexFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == "persistent" })]; v.PersistentVolumeClaim == nil ||
		v.PersistentVolumeClaim.ClaimName != release+"-firmware" {
		t.Errorf("the persistent volume is %+v, want the firmware PVC", v.VolumeSource)
	}
	if fs := pod.SecurityContext.FSGroup; fs == nil || *fs != layout.DefaultFSGroup {
		t.Errorf("pod fsGroup %v, want %d", fs, layout.DefaultFSGroup)
	}

	for _, tc := range []struct {
		c        *corev1.Container
		uid, gid int64
	}{
		{agent, layout.AgentUID, layout.AgentGID},
		{asterisk, layout.AsteriskUID, layout.AsteriskUID},
		{fileserver, 0, 0},
	} {
		sc := tc.c.SecurityContext
		switch {
		case sc == nil || sc.RunAsUser == nil || sc.RunAsGroup == nil:
			t.Errorf("%s has no runAsUser/runAsGroup", tc.c.Name)
		case *sc.RunAsUser != tc.uid || *sc.RunAsGroup != tc.gid:
			t.Errorf("%s runs as %d:%d, want %d:%d", tc.c.Name, *sc.RunAsUser, *sc.RunAsGroup, tc.uid, tc.gid)
		}
		if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
			sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
			sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
			t.Errorf("%s: securityContext %+v, want no privilege escalation, a read-only root and every capability dropped", tc.c.Name, sc)
		}
	}
	if add := fileserver.SecurityContext.Capabilities.Add; !slices.Equal(add, []corev1.Capability{"NET_BIND_SERVICE", "SETUID", "SETGID"}) {
		t.Errorf("the file server adds %v, want exactly NET_BIND_SERVICE, SETUID and SETGID", add)
	}
	if nr := fileserver.SecurityContext.RunAsNonRoot; nr == nil || *nr {
		t.Errorf("the file server's runAsNonRoot is %v; it starts as root and must say so", nr)
	}
	for _, c := range []*corev1.Container{agent, asterisk} {
		if len(c.SecurityContext.Capabilities.Add) > 0 {
			t.Errorf("%s adds capabilities %v", c.Name, c.SecurityContext.Capabilities.Add)
		}
	}

	// The file server binds the PBX address, never 0.0.0.0, and owns the
	// TFTP and Cisco HTTP ports; device HTTP binds it too.
	wantFlags(t, fileserver, map[string]string{"tftp-listen": "192.0.2.10:69", "http-listen": "192.0.2.10:6970"})
	wantFlags(t, agent, map[string]string{"device-http-listen": "192.0.2.10:8088"})
	ports := func(c *corev1.Container) (out []string) {
		for _, p := range c.Ports {
			out = append(out, fmt.Sprintf("%s/%d/%s", p.Name, p.ContainerPort, p.Protocol))
		}
		return out
	}
	if got := ports(fileserver); !slices.Equal(got, []string{"tftp/69/UDP", "cisco-http/6970/TCP"}) {
		t.Errorf("file server ports %v", got)
	}
	if got := ports(agent); !slices.Equal(got, []string{"control/9440/TCP", "device-http/8088/TCP"}) {
		t.Errorf("agent ports %v", got)
	}
}

// TestChartAgentAloneHasCredentials: the pod automounts no token; the agent
// alone gets a projected ServiceAccount token and the control API token;
// its Role reads exactly pbx-config and pbx-secrets.
func TestChartAgentAloneHasCredentials(t *testing.T) {
	m := render(t)
	pod := pbx(t, m)
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the PBX pod automounts its ServiceAccount token")
	}
	wantMounts(t, pod, "kube-api-access", map[string]string{"agent": "/var/run/secrets/kubernetes.io/serviceaccount ro"})
	wantMounts(t, pod, "agent-token", map[string]string{"agent": "/var/run/patchbay/agent-token ro"})
	for _, v := range pod.Volumes {
		if v.Name == "kube-api-access" && (v.Projected == nil || len(v.Projected.Sources) == 0 || v.Projected.Sources[0].ServiceAccountToken == nil) {
			t.Errorf("kube-api-access is %+v, want a projected ServiceAccount token", v.VolumeSource)
		}
	}
	if sa := pod.ServiceAccountName; sa != release+"-agent" {
		t.Errorf("the PBX pod runs as ServiceAccount %s", sa)
	}

	role := get[rbacv1.Role](t, m, "Role", release+"-agent")
	want := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{release + "-pbx-config"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{release + "-pbx-secrets"}, Verbs: []string{"get", "list", "watch"}},
	}
	if fmt.Sprint(role.Rules) != fmt.Sprint(want) {
		t.Errorf("the agent's Role grants %+v, want %+v", role.Rules, want)
	}
	rb := get[rbacv1.RoleBinding](t, m, "RoleBinding", release+"-agent")
	if rb.RoleRef.Name != role.Name || len(rb.Subjects) != 1 || rb.Subjects[0].Name != release+"-agent" {
		t.Errorf("the agent's RoleBinding is %+v", rb)
	}
	agent := container(t, pod, "agent")
	wantFlags(t, agent, map[string]string{
		"bundle-configmap": release + "-pbx-config",
		"secrets":          release + "-pbx-secrets",
	})
}

// TestChartRestartsOnNonReloadableSettings: a change to what Asterisk
// cannot reload changes the pod's checksum annotation (so the pod rolls);
// one it can, does not.
func TestChartRestartsOnNonReloadableSettings(t *testing.T) {
	sum := func(sets ...string) string {
		t.Helper()
		d := get[appsv1.Deployment](t, render(t, sets...), "Deployment", release+"-pbx")
		v := d.Spec.Template.Annotations["checksum/asterisk-restart"]
		if len(v) != 64 {
			t.Fatalf("checksum/asterisk-restart = %q", v)
		}
		return v
	}
	base := sum()
	for _, set := range []string{"pbx.address=192.0.2.11", "pbx.ports.sip=5070"} {
		if sum(set) == base {
			t.Errorf("--set %s left the restart checksum unchanged", set)
		}
	}
	if sum("pbx.agent.logLevel=debug") != base {
		t.Error("the agent's log level changed the restart checksum")
	}
}

func TestChartWithoutPersistence(t *testing.T) {
	m := render(t, "firmware.persistence.enabled=false")
	pod := pbx(t, m)
	v := pod.Volumes[slices.IndexFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == "persistent" })]
	if v.EmptyDir == nil {
		t.Errorf("without persistence the persistent volume is %+v, want an emptyDir", v.VolumeSource)
	}
	if _, ok := m.docs["PersistentVolumeClaim/"+release+"-firmware"]; ok {
		t.Error("a PVC is rendered without persistence")
	}
}

// TestChartPodMonitor: the PodMonitor scrapes the controller's metrics port
// and the agent's /metrics on its control port.
func TestChartPodMonitor(t *testing.T) {
	m := render(t, "podMonitor.enabled=true")
	var pm struct {
		Spec struct {
			Selector struct {
				MatchExpressions []struct {
					Key    string   `json:"key"`
					Values []string `json:"values"`
				} `json:"matchExpressions"`
			} `json:"selector"`
			Endpoints []struct {
				Port string `json:"port"`
				Path string `json:"path"`
			} `json:"podMetricsEndpoints"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(m.docs["PodMonitor/"+release], &pm); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(pm.Spec.Endpoints); got != "[{metrics /metrics} {control /metrics}]" {
		t.Errorf("PodMonitor endpoints %s", got)
	}
	if got := fmt.Sprint(pm.Spec.Selector.MatchExpressions); got != "[{app.kubernetes.io/component [controller pbx]}]" {
		t.Errorf("PodMonitor selects %s", got)
	}
}

// TestChartStaticConfigRetired: nothing of the hand-written e2e config is
// left; Asterisk's config comes only from the agent.
func TestChartStaticConfigRetired(t *testing.T) {
	m := render(t)
	for _, gone := range []string{"static", "/etc/asterisk"} {
		if strings.Contains(m.raw, gone) {
			t.Errorf("the rendered chart still mentions %q", gone)
		}
	}
}

// containsSeq reports whether args holds a, immediately followed by b.
func containsSeq(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}

// TestChartAgentProbesUseLoopback: the agent's probes reach the control
// API on the node's loopback (host 127.0.0.1; the pod is on the host
// network), which its connection caps keep outside the total, so LAN hosts
// holding every other connection cannot fail them (agent.MaxControlConns).
func TestChartAgentProbesUseLoopback(t *testing.T) {
	agent := container(t, pbx(t, render(t)), "agent")
	probes := map[string]*corev1.Probe{
		"startup": agent.StartupProbe, "readiness": agent.ReadinessProbe, "liveness": agent.LivenessProbe,
	}
	for name, p := range probes {
		if p == nil || p.HTTPGet == nil {
			t.Errorf("agent %s probe %+v is not an httpGet", name, p)
			continue
		}
		if p.HTTPGet.Host != "127.0.0.1" || p.HTTPGet.Port.String() != "control" {
			t.Errorf("agent %s probe goes to %q port %s, want 127.0.0.1 port control", name, p.HTTPGet.Host, p.HTTPGet.Port.String())
		}
	}
}

// TestChartAgentServicePublishesNotReady: the controller reaches the agent
// only through the agent Service, and the PBX pod is NotReady whenever
// Asterisk is down (its readiness probe), so the Service publishes the pod
// regardless: the agent then still answers, and devices show PBXDown, not
// AgentUnreachable (design §7).
func TestChartAgentServicePublishesNotReady(t *testing.T) {
	svc := get[corev1.Service](t, render(t), "Service", release+"-agent")
	if !svc.Spec.PublishNotReadyAddresses {
		t.Error("the agent Service does not publish not-ready addresses: an Asterisk outage would read as AgentUnreachable")
	}
}

// TestChartAgentKnowsItsSIPBind: the agent is told the SIP address and
// port this pod's Asterisk binds (pbx.address, pbx.ports.sip), which is
// what the settings compile into the transports, so it applies only
// versions compiled for them (compile.Boot); a change of either rolls the
// pod.
func TestChartAgentKnowsItsSIPBind(t *testing.T) {
	for port, sets := range map[string][]string{"5060": nil, "5070": {"pbx.ports.sip=5070"}} {
		agent := container(t, pbx(t, render(t, sets...)), "agent")
		wantFlags(t, agent, map[string]string{"sip-bind": "192.0.2.10:" + port})
	}
	a := get[appsv1.Deployment](t, render(t), "Deployment", release+"-pbx").Spec.Template.Annotations
	b := get[appsv1.Deployment](t, render(t, "pbx.ports.sip=5070"), "Deployment", release+"-pbx").Spec.Template.Annotations
	if a["checksum/asterisk-restart"] == b["checksum/asterisk-restart"] {
		t.Error("a new SIP port does not roll the PBX pod")
	}
}

// TestChartDeviceColumns: `kubectl get phones|softphones` shows Ready and
// its reason beside State. During an agent or PBX outage Decide keeps
// status.state at the last state (design §8.3's Unknown row), so State
// alone would read Registered all through it.
func TestChartDeviceColumns(t *testing.T) {
	m := render(t)
	for _, crd := range []string{"phones", "softphones"} {
		var c struct {
			Spec struct {
				Versions []struct {
					Columns []struct {
						Name     string `json:"name"`
						JSONPath string `json:"jsonPath"`
						Priority int    `json:"priority"`
					} `json:"additionalPrinterColumns"`
				} `json:"versions"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(m.docs["CustomResourceDefinition/"+crd+".patchbay.chrismiller.xyz"], &c); err != nil || len(c.Spec.Versions) == 0 {
			t.Fatalf("CRD %s: %v", crd, err)
		}
		got := map[string]string{}
		for _, col := range c.Spec.Versions[0].Columns {
			if col.Priority == 0 {
				got[col.Name] = col.JSONPath
			}
		}
		for name, path := range map[string]string{
			"State":  ".status.state",
			"Ready":  `.status.conditions[?(@.type=="Ready")].status`,
			"Reason": `.status.conditions[?(@.type=="Ready")].reason`,
		} {
			if got[name] != path {
				t.Errorf("%s: column %s is %q, want %q (priority 0)", crd, name, got[name], path)
			}
		}
	}
}
