package chart_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/settings"
	"github.com/ChristopherJMiller/patchbay/internal/streambridge"
	bridgeapi "github.com/ChristopherJMiller/patchbay/internal/streambridge/api"
	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// The stream bridge's image repository (the Dockerfile's stream-bridge
// target, as CI publishes it).
const bridgeRepository = "ghcr.io/christopherjmiller/patchbay-stream-bridge"

var names = install.NewNames(release)

// appVersion is Chart.yaml's appVersion, the images' default tag.
func appVersion(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		AppVersion string `json:"appVersion"`
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil || chart.AppVersion == "" {
		t.Fatalf("Chart.yaml's appVersion: %q, %v", chart.AppVersion, err)
	}
	return chart.AppVersion
}

// bridge returns the stream bridge's Deployment of a rendered chart.
func bridge(t *testing.T, m manifest) *appsv1.Deployment {
	t.Helper()
	return get[appsv1.Deployment](t, m, "Deployment", names.StreamBridge)
}

// bridgeContainer is the bridge pod's one container.
func bridgeContainer(t *testing.T, pod *corev1.PodSpec) *corev1.Container {
	t.Helper()
	if len(pod.Containers) != 1 || len(pod.InitContainers) != 0 || pod.Containers[0].Name != "stream-bridge" {
		t.Fatalf("the bridge pod runs %d containers and %d init containers, want exactly stream-bridge", len(pod.Containers), len(pod.InitContainers))
	}
	return &pod.Containers[0]
}

func controllerContainer(t *testing.T, m manifest) *corev1.Container {
	t.Helper()
	return &get[appsv1.Deployment](t, m, "Deployment", release+"-controller").Spec.Template.Spec.Containers[0]
}

// TestChartStreamBridge: the bridge's Deployment, against the contract
// constants: one replica, Recreate; no Kubernetes credentials; 65532, a
// read-only root, no capabilities; `stream-bridge` with exactly
// streambridge's Flag* flags at their Default* values (the chart's ports
// are bridgeapi's), and the zap level; ports audiosocket and control; its
// probes on the control API's paths; the <fullname>-streams ConfigMap's
// streams.json, not optional, read-only at DefaultStreamsDir, so the file
// is DefaultStreamsFile; and the image at appVersion, pinned when a digest
// is set.
func TestChartStreamBridge(t *testing.T) {
	m := render(t)
	d := bridge(t, m)
	pod := &d.Spec.Template.Spec
	c := bridgeContainer(t, pod)

	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("replicas %v, strategy %q: want exactly 1, Recreate (two bridges would hold two tuners)", d.Spec.Replicas, d.Spec.Strategy.Type)
	}
	if got := d.Spec.Template.Labels["app.kubernetes.io/component"]; got != "stream-bridge" {
		t.Errorf("the bridge pod's component is %q", got)
	}
	for k, v := range d.Spec.Selector.MatchLabels {
		if d.Spec.Template.Labels[k] != v {
			t.Errorf("the selector's %s=%s is not on the pod", k, v)
		}
	}

	// No credentials of any kind.
	if pod.ServiceAccountName != names.StreamBridge || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Errorf("ServiceAccount %q, automount %v: want %s without a token", pod.ServiceAccountName, pod.AutomountServiceAccountToken, names.StreamBridge)
	}
	sa := get[corev1.ServiceAccount](t, m, "ServiceAccount", names.StreamBridge)
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Error("the bridge's ServiceAccount automounts its token")
	}
	for kind := range m.docs {
		if strings.HasPrefix(kind, "Role/"+names.StreamBridge) || strings.HasPrefix(kind, "RoleBinding/"+names.StreamBridge) {
			t.Errorf("the bridge has RBAC: %s", kind)
		}
	}
	if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks || pod.HostNetwork {
		t.Errorf("enableServiceLinks %v, hostNetwork %v: want neither", pod.EnableServiceLinks, pod.HostNetwork)
	}
	if len(c.Env) != 0 || len(c.EnvFrom) != 0 {
		t.Errorf("the bridge has environment %v %v", c.Env, c.EnvFrom)
	}

	// Who it runs as.
	psc := pod.SecurityContext
	if psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot || psc.RunAsUser == nil || *psc.RunAsUser != 65532 ||
		psc.RunAsGroup == nil || *psc.RunAsGroup != 65532 || psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod securityContext %+v: want non-root 65532:65532, seccomp RuntimeDefault", psc)
	}
	sc := c.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || len(sc.Capabilities.Add) != 0 {
		t.Errorf("container securityContext %+v: want no privilege escalation, a read-only root, every capability dropped", sc)
	}

	// What it runs: the command and exactly these flags.
	if len(c.Args) == 0 || c.Args[0] != "stream-bridge" {
		t.Fatalf("the bridge runs %v, want the stream-bridge command", c.Args)
	}
	want := map[string]string{
		streambridge.FlagListen:        streambridge.DefaultListen,
		streambridge.FlagControlListen: streambridge.DefaultControlListen,
		streambridge.FlagStreamsFile:   streambridge.DefaultStreamsFile,
		streambridge.FlagFFmpeg:        streambridge.DefaultFFmpeg,
		"zap-log-level":                "info",
	}
	wantFlags(t, c, want)
	if len(c.Args) != 1+len(want) {
		t.Errorf("the bridge gets %v: want the command and exactly %d flags", c.Args, len(want))
	}
	if c.Command != nil {
		t.Errorf("the bridge overrides the image's entrypoint: %v", c.Command)
	}

	// Ports and probes.
	ports := map[string]int32{}
	for _, p := range c.Ports {
		if p.Protocol != corev1.ProtocolTCP {
			t.Errorf("port %s is %s", p.Name, p.Protocol)
		}
		ports[p.Name] = p.ContainerPort
	}
	if !reflect.DeepEqual(ports, map[string]int32{"audiosocket": bridgeapi.DefaultAudioSocketPort, "control": bridgeapi.DefaultControlPort}) {
		t.Errorf("container ports %v", ports)
	}
	for name, tc := range map[string]struct {
		p    *corev1.Probe
		path string
	}{
		"startup":   {c.StartupProbe, bridgeapi.PathReadyz},
		"readiness": {c.ReadinessProbe, bridgeapi.PathReadyz},
		"liveness":  {c.LivenessProbe, bridgeapi.PathHealthz},
	} {
		if tc.p == nil || tc.p.HTTPGet == nil || tc.p.HTTPGet.Path != tc.path || tc.p.HTTPGet.Port != intstr.FromString("control") {
			t.Errorf("%s probe %+v: want GET %s on port control", name, tc.p, tc.path)
		}
	}
	if p := c.StartupProbe; p != nil && (p.PeriodSeconds != 2 || p.FailureThreshold != 30) {
		t.Errorf("startup probe every %d s, %d failures: want 2 and 30", p.PeriodSeconds, p.FailureThreshold)
	}

	// streams.json: the controller's ConfigMap, mounted whole (the
	// kubelet's symlink swap), read-only, not optional.
	wantMounts(t, pod, "streams", map[string]string{"stream-bridge": streambridge.DefaultStreamsDir + " ro"})
	wantMounts(t, pod, "tmp", map[string]string{"stream-bridge": "/tmp rw"})
	if len(pod.Volumes) != 2 {
		t.Errorf("the bridge has volumes %+v, want streams and tmp only", pod.Volumes)
	}
	for _, vm := range c.VolumeMounts {
		if vm.SubPath != "" {
			t.Errorf("mount %s uses subPath %q: a ConfigMap update would never reach it", vm.Name, vm.SubPath)
		}
	}
	i := slices.IndexFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == "streams" })
	if i < 0 || pod.Volumes[i].ConfigMap == nil {
		t.Fatalf("the streams volume is not a ConfigMap: %+v", pod.Volumes)
	}
	cm := pod.Volumes[i].ConfigMap
	if cm.Name != names.StreamsConfigMap || len(cm.Items) != 1 || cm.Items[0].Key != install.StreamsKey {
		t.Errorf("the streams volume is %+v, want %s's key %s", cm, names.StreamsConfigMap, install.StreamsKey)
	} else if got := path.Join(streambridge.DefaultStreamsDir, cm.Items[0].Path); got != streambridge.DefaultStreamsFile {
		t.Errorf("streams.json is mounted at %s, but the bridge reads %s", got, streambridge.DefaultStreamsFile)
	}
	if cm.Optional != nil && *cm.Optional {
		t.Error("the streams ConfigMap is optional: the bridge would start without streams.json")
	}
	if j := slices.IndexFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == "tmp" }); j < 0 || pod.Volumes[j].EmptyDir == nil {
		t.Errorf("/tmp is not an emptyDir: %+v", pod.Volumes)
	}

	// Resources and shutdown.
	if r := c.Resources; !r.Requests.Cpu().Equal(resource.MustParse("50m")) || !r.Requests.Memory().Equal(resource.MustParse("96Mi")) ||
		!r.Limits.Memory().Equal(resource.MustParse("512Mi")) {
		t.Errorf("resources %+v: want requests 50m/96Mi, a 512Mi memory limit", r)
	}
	if g := pod.TerminationGracePeriodSeconds; g == nil || *g != 10 {
		t.Errorf("terminationGracePeriodSeconds %v, want 10", g)
	}

	// The image: appVersion by default; tag and digest when pinned, quoted
	// as the release job looks for it.
	if want := bridgeRepository + ":" + appVersion(t); c.Image != want || c.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("the bridge runs %s (%s), want %s (IfNotPresent)", c.Image, c.ImagePullPolicy, want)
	}
	const digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	pinned := render(t, "streamBridge.image.tag=9.9.9", "streamBridge.image.digest="+digest, "streamBridge.image.pullPolicy=Always")
	pc := bridgeContainer(t, &bridge(t, pinned).Spec.Template.Spec)
	if want := bridgeRepository + ":9.9.9@" + digest; pc.Image != want || pc.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("pinned, the bridge runs %s (%s), want %s (Always)", pc.Image, pc.ImagePullPolicy, want)
	}
	if want := `image: "` + bridgeRepository + ":9.9.9@" + digest + `"`; strings.Count(pinned.raw, want) != 1 {
		t.Errorf("the rendered chart does not hold %s once (the release job checks for it)", want)
	}
	// The patchbay image's digest is not the bridge's.
	other := render(t, "image.digest="+digest)
	if img := bridgeContainer(t, &bridge(t, other).Spec.Template.Spec).Image; strings.Contains(img, "@") {
		t.Errorf("image.digest pinned the bridge too: %s", img)
	}

	// Values that move it.
	moved := render(t, "streamBridge.logLevel=debug", "streamBridge.ports.audioSocket=9192", "streamBridge.ports.control=9193")
	mc := bridgeContainer(t, &bridge(t, moved).Spec.Template.Spec)
	wantFlags(t, mc, map[string]string{
		streambridge.FlagListen: ":9192", streambridge.FlagControlListen: ":9193", "zap-log-level": "debug",
	})
}

// TestChartStreamBridgePlacement: the bridge's placement values reach its
// pod (all-features-values.yaml sets each), and the PBX's do not.
func TestChartStreamBridgePlacement(t *testing.T) {
	m := renderFile(t, filepath.Join(chartDir, "ci", "all-features-values.yaml"))
	pod := &bridge(t, m).Spec.Template
	if pod.Annotations["example.com/note"] != "lint-only" {
		t.Errorf("podAnnotations: %v", pod.Annotations)
	}
	if pod.Spec.NodeSelector["kubernetes.io/arch"] != "amd64" || len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Key != "media" ||
		pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		t.Errorf("placement: nodeSelector %v, tolerations %v, affinity %+v", pod.Spec.NodeSelector, pod.Spec.Tolerations, pod.Spec.Affinity)
	}
	if ps := pod.Spec.ImagePullSecrets; len(ps) != 1 || ps[0].Name != "regcred" {
		t.Errorf("imagePullSecrets %v", ps)
	}
	if def := &bridge(t, render(t)).Spec.Template; len(def.Annotations) != 0 || def.Spec.NodeSelector != nil || def.Spec.Tolerations != nil || def.Spec.Affinity != nil {
		t.Errorf("the default bridge pod has placement: %v %v %v %v", def.Annotations, def.Spec.NodeSelector, def.Spec.Tolerations, def.Spec.Affinity)
	}
}

// TestChartStreamBridgeService: the Service <fullname>-streams selects
// exactly the bridge pod and publishes its two ports by name; settings.json's
// streamBridge (what Asterisk dials) is that Service's AudioSocket address,
// install.StreamBridgeAddress, and moves with the port.
func TestChartStreamBridgeService(t *testing.T) {
	for _, port := range []int{bridgeapi.DefaultAudioSocketPort, 9192} {
		var sets []string
		if port != bridgeapi.DefaultAudioSocketPort {
			sets = []string{"streamBridge.ports.audioSocket=" + strconv.Itoa(port)}
		}
		m := render(t, sets...)
		svc := get[corev1.Service](t, m, "Service", names.StreamBridge)
		if svc.Spec.Type != corev1.ServiceTypeClusterIP && svc.Spec.Type != "" {
			t.Errorf("the bridge Service is %s", svc.Spec.Type)
		}
		got := map[string]string{}
		for _, p := range svc.Spec.Ports {
			got[p.Name] = fmt.Sprintf("%d->%s/%s", p.Port, p.TargetPort.String(), p.Protocol)
		}
		want := map[string]string{
			"audiosocket": fmt.Sprintf("%d->audiosocket/TCP", port),
			"control":     fmt.Sprintf("%d->control/TCP", bridgeapi.DefaultControlPort),
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Service ports %v, want %v", got, want)
		}
		pod := bridge(t, m).Spec.Template
		for k, v := range svc.Spec.Selector {
			if pod.Labels[k] != v {
				t.Errorf("the Service selects %s=%s, which the bridge pod lacks", k, v)
			}
		}
		if svc.Spec.Selector["app.kubernetes.io/component"] != "stream-bridge" {
			t.Errorf("the Service selects %v: not only the bridge", svc.Spec.Selector)
		}
		s, err := settings.Parse([]byte(settingsJSON(t, m)))
		if err != nil {
			t.Fatal(err)
		}
		if want := install.StreamBridgeAddress(names, "telephony", port); s.StreamBridge != want {
			t.Errorf("settings.json's streamBridge is %q, want %q (the Service's audiosocket)", s.StreamBridge, want)
		}
	}
	// No other Service selects the bridge (the agent's selects the PBX).
	agent := get[corev1.Service](t, render(t), "Service", names.AgentService)
	if agent.Spec.Selector["app.kubernetes.io/component"] != "pbx" {
		t.Errorf("the agent Service selects %v", agent.Spec.Selector)
	}
}

// TestChartStreamBridgeDisabled: streamBridge.enabled=false renders none of
// the bridge's objects, no --stream-bridge-url, and settings.json without
// streamBridge (streams are disabled: every Stream plays "not available");
// streams' call limit is still there, and the controller may still write
// <fullname>-streams, which it compiles either way.
func TestChartStreamBridgeDisabled(t *testing.T) {
	for _, m := range []manifest{
		renderFile(t, filepath.Join(chartDir, "ci", "no-streams-values.yaml")),
		render(t, "streamBridge.enabled=false"),
	} {
		for _, kind := range []string{"Deployment", "Service", "ServiceAccount"} {
			if _, ok := m.docs[kind+"/"+names.StreamBridge]; ok {
				t.Errorf("a %s %s is rendered with the bridge off", kind, names.StreamBridge)
			}
		}
		if strings.Contains(m.raw, "stream-bridge") && strings.Contains(m.raw, "component: stream-bridge") {
			t.Error("something with component stream-bridge is rendered with the bridge off")
		}
		c := controllerContainer(t, m)
		for _, a := range c.Args {
			if strings.HasPrefix(a, "--stream-bridge-url") {
				t.Errorf("the controller gets %s with the bridge off", a)
			}
		}
		js := settingsJSON(t, m)
		var raw map[string]any
		if err := json.Unmarshal([]byte(js), &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["streamBridge"]; ok {
			t.Errorf("settings.json has streamBridge with the bridge off: %v", raw["streamBridge"])
		}
		s, err := settings.Parse([]byte(js))
		if err != nil {
			t.Fatal(err)
		}
		if s.StreamBridge != "" || s.Streams.CallLimit.Duration != 2*time.Hour {
			t.Errorf("settings: streamBridge %q, callLimit %s", s.StreamBridge, s.Streams.CallLimit.Duration)
		}
		role := get[rbacv1.Role](t, m, "Role", release+"-controller")
		if !slices.ContainsFunc(role.Rules, func(r rbacv1.PolicyRule) bool {
			return slices.Contains(r.ResourceNames, names.StreamsConfigMap) && slices.Equal(r.Verbs, []string{"update"})
		}) {
			t.Errorf("with the bridge off the controller may not update %s, which it still writes", names.StreamsConfigMap)
		}
	}
}

// TestChartControllerStreams: the controller polls the bridge at
// install.StreamBridgeURL on the control port, and may update
// <fullname>-streams beside pbx-config, and no other ConfigMap.
func TestChartControllerStreams(t *testing.T) {
	m := render(t)
	wantFlags(t, controllerContainer(t, m), map[string]string{
		"stream-bridge-url": install.StreamBridgeURL(names, "telephony", bridgeapi.DefaultControlPort),
	})
	all := renderFile(t, filepath.Join(chartDir, "ci", "all-features-values.yaml"))
	wantFlags(t, controllerContainer(t, all), map[string]string{
		"stream-bridge-url": install.StreamBridgeURL(names, "telephony", 9193),
	})
	if got := bridgeContainer(t, &bridge(t, all).Spec.Template.Spec); !slices.Contains(got.Args, "--"+streambridge.FlagControlListen+"=:9193") {
		t.Errorf("the controller polls 9193 but the bridge gets %v", got.Args)
	}

	role := get[rbacv1.Role](t, m, "Role", release+"-controller")
	var updates [][]string
	for _, r := range role.Rules {
		if slices.Contains(r.Resources, "configmaps") && slices.Contains(r.Verbs, "update") {
			updates = append(updates, r.ResourceNames)
		}
	}
	if want := [][]string{{names.ConfigMap, names.StreamsConfigMap}}; !reflect.DeepEqual(updates, want) {
		t.Errorf("the controller may update ConfigMaps %v, want exactly %v", updates, want)
	}
}

// TestChartSettingsStreams: streams.callLimit and streams.tunerPools reach
// settings.json as the controller reads them (compile.StreamSettings), the
// pools only when there are any; a pool missing a field, or not a map,
// fails `helm template` naming it; a change rolls the controller only.
func TestChartSettingsStreams(t *testing.T) {
	m := render(t)
	var raw struct {
		Streams map[string]any `json:"streams"`
	}
	if err := json.Unmarshal([]byte(settingsJSON(t, m)), &raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw.Streams, map[string]any{"callLimit": "2h"}) {
		t.Errorf("the default settings' streams are %v, want only callLimit 2h", raw.Streams)
	}

	all, err := settings.Parse([]byte(settingsJSON(t, renderFile(t, filepath.Join(chartDir, "ci", "all-features-values.yaml")))))
	if err != nil {
		t.Fatal(err)
	}
	want := []compile.TunerPool{
		{Name: "hdhomerun", Hosts: []string{"192.0.2.58", "hdhomerun.example.com"}, Max: 1},
		{Name: "second-tuner", Hosts: []string{"tuner2.example.com"}, Max: 2},
	}
	if all.Streams.CallLimit.Duration != 90*time.Minute || !reflect.DeepEqual(all.Streams.TunerPools, want) {
		t.Errorf("all-features streams: %+v, want callLimit 90m and pools %+v", all.Streams, want)
	}

	set, err := settings.Parse([]byte(settingsJSON(t, render(t, "streams.callLimit=45s",
		"streams.tunerPools[0].name=tuner", "streams.tunerPools[0].hosts[0]=192.0.2.7", "streams.tunerPools[0].max=1"))))
	if err != nil {
		t.Fatal(err)
	}
	if set.Streams.Limit() != 45*time.Second || !reflect.DeepEqual(set.Streams.TunerPools, []compile.TunerPool{{Name: "tuner", Hosts: []string{"192.0.2.7"}, Max: 1}}) {
		t.Errorf("--set streams: %+v", set.Streams)
	}

	minimal := filepath.Join(chartDir, "ci", "minimal-values.yaml")
	for want, sets := range map[string][]string{
		"streams.tunerPools[0].max is required":   {"streams.tunerPools[0].name=t", "streams.tunerPools[0].hosts[0]=h"},
		"streams.tunerPools[0].name is required":  {"streams.tunerPools[0].max=1", "streams.tunerPools[0].hosts[0]=h"},
		"streams.tunerPools[0].hosts is required": {"streams.tunerPools[0].name=t", "streams.tunerPools[0].max=1"},
		"streams.tunerPools[1].max is required": {"streams.tunerPools[0].name=t", "streams.tunerPools[0].max=1", "streams.tunerPools[0].hosts[0]=h",
			"streams.tunerPools[1].name=u", "streams.tunerPools[1].hosts[0]=i"},
		"streams.tunerPools[0] must be a map": {"streams.tunerPools[0]=hdhomerun"},
		"streams.callLimit is required":       {"streams.callLimit="},
	} {
		_, stderr, err := helmTemplate(t, minimal, sets...)
		if err == nil || !strings.Contains(stderr, want) {
			t.Errorf("--set %v: err %v, stderr %q; want a failure saying %q", sets, err, stderr, want)
		}
	}

	// The settings roll the controller (checksum/settings), never the PBX
	// pod nor the bridge, which reads only streams.json.
	changed := render(t, "streams.callLimit=1h")
	checksum := func(m manifest) string {
		return get[appsv1.Deployment](t, m, "Deployment", release+"-controller").Spec.Template.Annotations["checksum/settings"]
	}
	if checksum(m) == checksum(changed) {
		t.Error("a new call limit left checksum/settings unchanged")
	}
	for _, name := range []string{release + "-pbx", names.StreamBridge} {
		a := get[appsv1.Deployment](t, m, "Deployment", name).Spec.Template
		b := get[appsv1.Deployment](t, changed, "Deployment", name).Spec.Template
		if !reflect.DeepEqual(a, b) {
			t.Errorf("a new call limit changed %s's pod template", name)
		}
	}
}

// TestChartPodMonitorStreamBridge: the PodMonitor's control endpoint
// scrapes the bridge's metrics too (a port named control, /metrics:
// bridgeapi.PathMetrics), and selects the bridge only while it is on.
func TestChartPodMonitorStreamBridge(t *testing.T) {
	for on, wantSel := range map[bool]string{
		true:  "[{app.kubernetes.io/component [controller pbx stream-bridge]}]",
		false: "[{app.kubernetes.io/component [controller pbx]}]",
	} {
		pm := podMonitor(t, render(t, "podMonitor.enabled=true", "streamBridge.enabled="+strconv.FormatBool(on)))
		if got := fmt.Sprint(pm.Spec.Selector.MatchExpressions); got != wantSel {
			t.Errorf("bridge on=%t: the PodMonitor selects %s, want %s", on, got, wantSel)
		}
	}
	m := render(t, "podMonitor.enabled=true")
	pm := podMonitor(t, m)
	c := bridgeContainer(t, &bridge(t, m).Spec.Template.Spec)
	scraped := false
	for _, e := range pm.Spec.Endpoints {
		for _, p := range c.Ports {
			if p.Name == e.Port {
				scraped = e.Path == bridgeapi.PathMetrics && p.ContainerPort == bridgeapi.DefaultControlPort
			}
		}
	}
	if !scraped {
		t.Errorf("no PodMonitor endpoint scrapes the bridge's control port at %s: %+v", bridgeapi.PathMetrics, pm.Spec.Endpoints)
	}
}

// podMonitorSpec is the part of a PodMonitor the tests read.
type podMonitorSpec struct {
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

func podMonitor(t *testing.T, m manifest) podMonitorSpec {
	t.Helper()
	var pm podMonitorSpec
	doc, ok := m.docs["PodMonitor/"+release]
	if !ok {
		t.Fatal("no PodMonitor rendered")
	}
	if err := yaml.Unmarshal(doc, &pm); err != nil {
		t.Fatal(err)
	}
	return pm
}

// TestChartNotesStreams: NOTES names the bridge and <fullname>-streams
// among the objects the controller leaves on uninstall.
func TestChartNotesStreams(t *testing.T) {
	notes := func(sets ...string) string {
		t.Helper()
		args := append([]string{"--set", "pbx.nodeName=top", "--set", "pbx.address=192.0.2.10"}, flatSets(sets)...)
		stdout, stderr, err := helm(t, append([]string{"install", "--dry-run=client", release, chartDir, "--namespace", "telephony"}, args...)...)
		if err != nil {
			t.Fatalf("helm install --dry-run: %v\n%s", err, stderr)
		}
		_, n, ok := strings.Cut(stdout, "\nNOTES:\n")
		if !ok {
			t.Fatalf("no NOTES in\n%s", stdout)
		}
		return n
	}
	on := notes()
	for _, want := range []string{
		"delete configmap " + names.ConfigMap + " " + names.StreamsConfigMap,
		"deploy/" + names.StreamBridge,
		"port-forward svc/" + names.StreamBridge + " " + strconv.Itoa(bridgeapi.DefaultControlPort),
		bridgeapi.PathObservations,
	} {
		if !strings.Contains(on, want) {
			t.Errorf("NOTES lack %q:\n%s", want, on)
		}
	}
	off := notes("streamBridge.enabled=false")
	if !strings.Contains(off, `plays "not available"`) || strings.Contains(off, "deploy/"+names.StreamBridge) ||
		!strings.Contains(off, names.StreamsConfigMap) {
		t.Errorf("NOTES with the bridge off:\n%s", off)
	}
}

func flatSets(sets []string) []string {
	var out []string
	for _, s := range sets {
		out = append(out, "--set", s)
	}
	return out
}

// TestChartStreamsNameFits: <fullname>-streams is a DNS label (a Service
// name) for the longest fullname the chart makes, which design §9's
// <fullname>-stream-bridge is not (docs/plans/release-b.md §2(i)).
func TestChartStreamsNameFits(t *testing.T) {
	long := strings.Repeat("a", 60)
	m := render(t, "fullnameOverride="+long)
	full := long[:50]
	n := install.NewNames(full)
	if len(n.StreamBridge) > 63 {
		t.Fatalf("%s is %d characters", n.StreamBridge, len(n.StreamBridge))
	}
	if _, ok := m.docs["Service/"+n.StreamBridge]; !ok {
		t.Errorf("no Service %s for fullname %s", n.StreamBridge, full)
	}
	if len(full+"-stream-bridge") <= 63 {
		t.Errorf("the reason for -streams is gone: %s-stream-bridge fits", full)
	}
	if streams.FileName != install.StreamsKey {
		t.Errorf("streams.FileName %q is not install.StreamsKey %q", streams.FileName, install.StreamsKey)
	}
}

// helm runs the flake's helm with args.
func helm(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	bin, err := exec.LookPath("helm")
	if err != nil {
		t.Fatal("helm is not on PATH: run the tests inside `nix develop`")
	}
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(context.Background(), bin, args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// TestChartE2EValues: the e2e suite's values (test/e2e/values.yaml, with
// the node the harness adds at run time) render settings the controller
// accepts, the stream bridge from the e2e image, never pulled, and the
// fake tuner as the only pool.
func TestChartE2EValues(t *testing.T) {
	m := renderFile(t, filepath.Join("..", "e2e", "values.yaml"), "pbx.nodeName=patchbay-e2e-control-plane", "pbx.address=172.18.0.2")
	s, err := settings.Parse([]byte(settingsJSON(t, m)))
	if err != nil {
		t.Fatalf("the controller refuses the e2e settings: %v", err)
	}
	if s.Streams.Limit() != 20*time.Second ||
		!reflect.DeepEqual(s.Streams.TunerPools, []compile.TunerPool{{Name: "tuner", Hosts: []string{"fake-tuner.patchbay-e2e.svc"}, Max: 1}}) {
		t.Errorf("e2e streams settings: %+v", s.Streams)
	}
	c := bridgeContainer(t, &bridge(t, m).Spec.Template.Spec)
	if c.Image != bridgeRepository+":e2e" || c.ImagePullPolicy != corev1.PullNever {
		t.Errorf("the e2e bridge runs %s (%s), want %s:e2e, never pulled", c.Image, c.ImagePullPolicy, bridgeRepository)
	}
}
