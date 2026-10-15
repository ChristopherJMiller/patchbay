package chart_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/cli"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/controller/settings"
)

// renderFile renders the chart with one values file (instead of the
// minimal one) plus sets.
func renderFile(t *testing.T, valuesFile string, sets ...string) manifest {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Fatal("helm is not on PATH: run the tests inside `nix develop`")
	}
	args := []string{"template", release, chartDir, "--namespace", "telephony", "--values", valuesFile}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(context.Background(), helm, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return parseManifest(t, stdout.String())
}

// parseManifest splits a rendered chart into its documents.
func parseManifest(t *testing.T, raw string) manifest {
	t.Helper()
	m := manifest{raw: raw, docs: map[string][]byte{}}
	for doc := range strings.SplitSeq(raw, "\n---\n") {
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatalf("rendered YAML: %v\n%s", err, doc)
		}
		if meta.Kind != "" {
			m.docs[meta.Kind+"/"+meta.Metadata.Name] = []byte(doc)
		}
	}
	return m
}

// settingsJSON is the settings.json the chart renders.
func settingsJSON(t *testing.T, m manifest) string {
	t.Helper()
	cm := get[corev1.ConfigMap](t, m, "ConfigMap", install.NewNames(release).Settings)
	js, ok := cm.Data[install.SettingsKey]
	if !ok || len(cm.Data) != 1 {
		t.Fatalf("%s holds %v, want exactly %s", cm.Name, slices.Collect(mapKeys(cm.Data)), install.SettingsKey)
	}
	return js
}

func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// TestChartSettings: with every ci values file, <fullname>-settings holds
// settings the controller accepts (settings.Parse: strict JSON, Validate;
// install.CheckSettings: this install's AMI Secret); the defaults are
// exactly core's fixture, the document TestParseChartDefaults parses.
func TestChartSettings(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(chartDir, "ci", "*-values.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("ci values files: %v %v", files, err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			s, err := settings.Parse([]byte(settingsJSON(t, renderFile(t, f))))
			if err != nil {
				t.Fatalf("the controller refuses the chart's settings: %v", err)
			}
			if err := install.CheckSettings(s, install.NewNames(release)); err != nil {
				t.Fatal(err)
			}
		})
	}

	got, err := settings.Parse([]byte(settingsJSON(t, render(t))))
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := settings.Load(filepath.Join("..", "..", "internal", "controller", "settings", "testdata", "chart-defaults.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, fixture) {
		t.Errorf("the default settings are\n  %+v\nwant core's fixture\n  %+v", got, fixture)
	}

	// The values reach the document; YAML numbers in the number plan
	// become strings.
	all, err := settings.Parse([]byte(settingsJSON(t, renderFile(t, filepath.Join(chartDir, "ci", "all-features-values.yaml")))))
	if err != nil {
		t.Fatal(err)
	}
	if all.Timezone != "America/Los_Angeles" || all.NTPServer != "192.0.2.1" || all.NumberPlan.EchoTest != "600" ||
		!slices.Equal(all.NumberPlan.Reserved, []string{"2XX", "0", "9.", "*8"}) ||
		!slices.Equal(all.AllowedNetworks, []string{"192.0.2.0/24"}) {
		t.Errorf("all-features settings: %+v", all)
	}
	s, err := settings.Parse([]byte(settingsJSON(t, render(t, "pbx.address=192.0.2.44", "pbx.ports.sip=5070",
		"pbx.ports.rtpStart=12000", "pbx.ports.rtpEnd=12049", "pbx.ports.deviceHTTP=8089"))))
	if err != nil {
		t.Fatal(err)
	}
	if s.PBXAddress != "192.0.2.44" || s.SIPPort != 5070 || s.RTPPorts != (compile.PortRange{First: 12000, Last: 12049}) || s.DeviceHTTPPort != 8089 {
		t.Errorf("pbx values did not reach the settings: %+v", s)
	}
}

// TestChartControllerReadsTheSettings: the controller gets its fullname,
// the settings file where the ConfigMap is mounted read-only, and the
// agent Service's control port; a checksum of the settings rolls it when
// one changes, and only then.
func TestChartControllerReadsTheSettings(t *testing.T) {
	m := render(t)
	d := get[appsv1.Deployment](t, m, "Deployment", release+"-controller")
	pod := &d.Spec.Template.Spec
	c := &pod.Containers[0]
	names := install.NewNames(release)
	wantFlags(t, c, map[string]string{
		"fullname":      release,
		"settings-file": "/etc/patchbay/settings/" + install.SettingsKey,
		"agent-url":     install.AgentURL(names, "telephony", agentapi.DefaultPort),
	})
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].Name != "settings" || c.VolumeMounts[0].MountPath != "/etc/patchbay/settings" || !c.VolumeMounts[0].ReadOnly {
		t.Errorf("the controller mounts %+v", c.VolumeMounts)
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0].ConfigMap == nil || pod.Volumes[0].ConfigMap.Name != names.Settings {
		t.Errorf("the controller's volumes are %+v, want the settings ConfigMap", pod.Volumes)
	}

	checksum := func(m manifest) string {
		return get[appsv1.Deployment](t, m, "Deployment", release+"-controller").Spec.Template.Annotations["checksum/settings"]
	}
	// Helm hashes the JSON as the helper renders it; the ConfigMap's block
	// scalar adds one trailing newline.
	base := checksum(m)
	if base != sha256Hex(strings.TrimSuffix(settingsJSON(t, m), "\n")) {
		t.Errorf("checksum/settings = %q, want the SHA-256 of settings.json", base)
	}
	for _, set := range []string{"timezone=Europe/Berlin", "pbx.address=192.0.2.11", "allowedNetworks[0]=10.0.0.0/8"} {
		if checksum(render(t, set)) == base {
			t.Errorf("--set %s left checksum/settings unchanged", set)
		}
	}
	if checksum(render(t, "controller.logLevel=debug")) != base {
		t.Error("the controller's log level changed checksum/settings")
	}

	// A control port of its own reaches the agent URL.
	other := renderFile(t, filepath.Join(chartDir, "ci", "all-features-values.yaml"))
	oc := &get[appsv1.Deployment](t, other, "Deployment", release+"-controller").Spec.Template.Spec.Containers[0]
	wantFlags(t, oc, map[string]string{"agent-url": install.AgentURL(names, "telephony", 9450)})
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestChartCLIFindsTheAgent: what `patchbay status` and `firmware load`
// look for (internal/cli's constants) is what the PBX pod has: its labels,
// the agent init container with its control port, and the agent-token
// volume, a Secret volume of the controller-minted token that is not
// optional (the pod waits for it), mounted where --token-file reads.
func TestChartCLIFindsTheAgent(t *testing.T) {
	m := render(t)
	d := get[appsv1.Deployment](t, m, "Deployment", release+"-pbx")
	labels := d.Spec.Template.Labels
	for k, v := range map[string]string{cli.LabelPartOf: cli.PartOf, cli.LabelComponent: cli.ComponentPBX, cli.LabelInstance: release} {
		if labels[k] != v {
			t.Errorf("the PBX pod has %s=%q, want %q (the CLI selects it so)", k, labels[k], v)
		}
	}
	pod := &d.Spec.Template.Spec
	var agent *corev1.Container
	for i := range pod.InitContainers {
		if pod.InitContainers[i].Name == cli.AgentContainer {
			agent = &pod.InitContainers[i]
		}
	}
	if agent == nil {
		t.Fatalf("no init container %s", cli.AgentContainer)
	}
	if !slices.ContainsFunc(agent.Ports, func(p corev1.ContainerPort) bool {
		return p.Name == cli.ControlPortName && p.ContainerPort == agentapi.DefaultPort
	}) {
		t.Errorf("the agent has no port %s (%d): %+v", cli.ControlPortName, agentapi.DefaultPort, agent.Ports)
	}
	if v, ok := flag(agent.Args, "listen"); !ok || v != ":"+strconv.Itoa(agentapi.DefaultPort) {
		t.Errorf("the agent listens on %q", v)
	}

	i := slices.IndexFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == cli.AgentTokenVolume })
	if i < 0 {
		t.Fatalf("no volume %s", cli.AgentTokenVolume)
	}
	sec := pod.Volumes[i].Secret
	if sec == nil || sec.SecretName != install.NewNames(release).AgentTokenSecret {
		t.Fatalf("volume %s is %+v, want the Secret %s", cli.AgentTokenVolume, pod.Volumes[i].VolumeSource, install.NewNames(release).AgentTokenSecret)
	}
	if sec.Optional != nil && *sec.Optional {
		t.Error("the agent token volume is optional: the agent would start without its token")
	}
	var mount string
	for _, vm := range agent.VolumeMounts {
		if vm.Name == cli.AgentTokenVolume {
			mount = vm.MountPath
		}
	}
	if v, ok := flag(agent.Args, "token-file"); !ok || v != mount+"/"+install.AgentTokenKey {
		t.Errorf("--token-file=%q, want %s/%s", v, mount, install.AgentTokenKey)
	}
	if strings.Contains(m.raw, "tokenSecretName") {
		t.Error("the chart still mentions tokenSecretName")
	}
}

// TestChartControllerRole: least privilege. The controller reads its
// resources and writes only their status (and their finalizers, for
// blockOwnerDeletion); creates ConfigMaps and Secrets and updates them,
// pbx-config by name; emits Events; holds the install's own Lease by
// name; never deletes or patches a resource it manages.
func TestChartControllerRole(t *testing.T) {
	role := get[rbacv1.Role](t, render(t), "Role", release+"-controller")
	group := []string{"patchbay.chrismiller.xyz"}
	core := []string{""}
	want := []rbacv1.PolicyRule{
		{APIGroups: group, Resources: []string{"phones", "softphones", "people", "streams"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: group, Resources: []string{"phones/status", "softphones/status", "people/status", "streams/status"}, Verbs: []string{"update"}},
		{APIGroups: group, Resources: []string{"phones/finalizers", "softphones/finalizers"}, Verbs: []string{"update"}},
		{APIGroups: core, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "watch", "create"}},
		{APIGroups: core, Resources: []string{"configmaps"}, ResourceNames: []string{install.NewNames(release).ConfigMap}, Verbs: []string{"update"}},
		{APIGroups: core, Resources: []string{"secrets"}, Verbs: []string{"get", "list", "watch", "create", "update"}},
		{APIGroups: []string{"events.k8s.io"}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
		{APIGroups: core, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"create"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, ResourceNames: []string{controller.LeaderElectionID(release)}, Verbs: []string{"get", "update"}},
	}
	if fmt.Sprint(role.Rules) != fmt.Sprint(want) {
		t.Errorf("the controller's Role grants\n  %+v\nwant\n  %+v", role.Rules, want)
	}
	for _, r := range role.Rules {
		if slices.Contains(r.Verbs, "delete") || slices.Contains(r.Verbs, "deletecollection") || slices.Contains(r.Verbs, "*") {
			t.Errorf("the controller may delete: %+v", r)
		}
	}
}
