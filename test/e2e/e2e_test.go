//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
)

type ctxKey string

const crdNamespaceKey ctxKey = "crd-namespace"

// TestCRDs installs nothing itself: the CRDs come from the chart. It applies
// config/samples, reads them back typed, and checks that the API server
// enforces a CEL rule.
func TestCRDs(t *testing.T) {
	f := features.New("CRDs from the chart").
		Setup(func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			suffix, err := randomHex(3)
			if err != nil {
				t.Fatal(err)
			}
			ns := "patchbay-e2e-crds-" + suffix
			if _, err := h.cs.CoreV1().Namespaces().Create(ctx,
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			return context.WithValue(ctx, crdNamespaceKey, ns)
		}).
		Assess("the samples are accepted and read back", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value(crdNamespaceKey).(string)
			r := typedResources(t, cfg)
			samples := os.DirFS(filepath.Join(h.root, "config", "samples"))
			if err := decoder.DecodeEachFile(ctx, samples, "patchbay_v1alpha1_*.yaml",
				decoder.CreateHandler(r), decoder.MutateNamespace(ns)); err != nil {
				t.Fatalf("apply config/samples: %v", err)
			}

			var phone v1alpha1.Phone
			if err := r.Get(ctx, "desk", ns, &phone); err != nil {
				t.Fatal(err)
			}
			if phone.Spec.Number != "100" || phone.Spec.MAC.Normalized() != "D0C78915E8C8" || phone.Spec.Model != v1alpha1.PhoneModelCisco7945 {
				t.Errorf("Phone desk read back as %+v", phone.Spec)
			}
			var soft v1alpha1.Softphone
			if err := r.Get(ctx, "chris-laptop", ns, &soft); err != nil {
				t.Fatal(err)
			}
			if soft.Spec.Number != "150" {
				t.Errorf("Softphone chris-laptop read back as %+v", soft.Spec)
			}
			var person v1alpha1.Person
			if err := r.Get(ctx, "chris", ns, &person); err != nil {
				t.Fatal(err)
			}
			if person.Spec.Number != "301" || len(person.Spec.Rings) != 2 {
				t.Errorf("Person chris read back as %+v", person.Spec)
			}
			var stream v1alpha1.Stream
			if err := r.Get(ctx, "king5", ns, &stream); err != nil {
				t.Fatal(err)
			}
			if stream.Spec.Number != "705" {
				t.Errorf("Stream king5 read back as %+v", stream.Spec)
			}
			var radio v1alpha1.Stream
			if err := r.Get(ctx, "groovesalad", ns, &radio); err != nil {
				t.Fatal(err)
			}
			if radio.Spec.Number != "750" || !strings.HasPrefix(radio.Spec.Source, "https://") {
				t.Errorf("Stream groovesalad read back as %+v", radio.Spec)
			}
			return ctx
		}).
		Assess("a ring target naming both a phone and a softphone is rejected (CEL)", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			ns := ctx.Value(crdNamespaceKey).(string)
			bad := &v1alpha1.Person{
				ObjectMeta: metav1.ObjectMeta{Name: "both", Namespace: ns},
				Spec: v1alpha1.PersonSpec{
					Number: "302",
					Name:   "Both",
					Rings:  []v1alpha1.RingTarget{{Phone: "desk", Softphone: "chris-laptop"}},
				},
			}
			err := typedResources(t, cfg).Create(ctx, bad)
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "exactly one of phone or softphone") {
				t.Fatalf("create a Person breaking the CEL rule: err = %v, want Invalid with the rule's message", err)
			}
			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			if ns, ok := ctx.Value(crdNamespaceKey).(string); ok {
				if err := h.cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}); err != nil {
					t.Logf("delete namespace %s: %v", ns, err)
				}
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// TestPBXUp checks the PBX pod's shape; that every process in it runs with
// the identity and privilege the design gives it, as the kernel reports it
// in /proc (not just the pod spec); that the file server's identity cannot
// read the resolved config; that Asterisk runs the compiled bundle; and
// that the agent answers through its Service from the pod network (the
// path the controller will use).
func TestPBXUp(t *testing.T) {
	f := features.New("PBX pod").
		Assess("runs on the host network of the pinned node: modules, agent sidecar, Asterisk, file server", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			pod := pbxPod(ctx, t)
			if !pod.Spec.HostNetwork || pod.Spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
				t.Errorf("hostNetwork=%v dnsPolicy=%s", pod.Spec.HostNetwork, pod.Spec.DNSPolicy)
			}
			if pod.Spec.NodeName != h.nodeName || pod.Status.PodIP != h.nodeIP {
				t.Errorf("pod on %s with IP %s, want %s / %s", pod.Spec.NodeName, pod.Status.PodIP, h.nodeName, h.nodeIP)
			}
			ready := false
			for _, c := range pod.Status.Conditions {
				ready = ready || (c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue)
			}
			if !ready {
				t.Errorf("PBX pod %s is not Ready", pod.Name)
			}
			var inits, mains []string
			for _, c := range pod.Spec.InitContainers {
				inits = append(inits, c.Name)
			}
			for _, c := range pod.Spec.Containers {
				mains = append(mains, c.Name)
			}
			if !slices.Equal(inits, []string{"asterisk-modules", "agent"}) || !slices.Equal(mains, []string{"asterisk", "fileserver"}) {
				t.Errorf("containers: init %v, main %v", inits, mains)
			}
			if rp := pod.Spec.InitContainers[1].RestartPolicy; rp == nil || *rp != corev1.ContainerRestartPolicyAlways {
				t.Errorf("the agent is not a native sidecar")
			}
			for _, s := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
				if s.Name == "asterisk-modules" {
					if s.State.Terminated == nil || s.State.Terminated.ExitCode != 0 {
						t.Errorf("asterisk-modules did not complete: %+v", s.State)
					}
					continue
				}
				if !s.Ready || s.State.Running == nil {
					t.Errorf("container %s: ready=%v state=%+v", s.Name, s.Ready, s.State)
				}
				// A reused cluster's pod may carry a previous run's restarts.
				var before int32
				if pod.UID == h.priorPBX {
					before = h.priorRestarts[s.Name]
				}
				if s.RestartCount > before {
					t.Errorf("container %s restarted %d times (%d before this run)", s.Name, s.RestartCount, before)
				}
			}
			return ctx
		}).
		Assess("Asterisk runs as uid 1000 with no capabilities on a read-only root", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			// PID 1 of the asterisk container is Asterisk itself (the chart
			// bypasses the image's root entrypoint), so its own /proc entry
			// is what the kernel enforces, not just what the spec asks for.
			st := procStatus(execInAsterisk(ctx, t, cfg, "cat", "/proc/1/status"))
			if st["Name"] != "asterisk" {
				t.Errorf("PID 1 is %q, not asterisk", st["Name"])
			}
			wantStatus(t, "asterisk", st, map[string]string{
				"Uid": "1000 1000 1000 1000", "Gid": "1000 1000 1000 1000",
				"CapInh": capsNone, "CapPrm": capsNone, "CapEff": capsNone, "CapBnd": capsNone, "CapAmb": capsNone,
				"NoNewPrivs": "1",
			})
			opts := rootMountOptions(execInAsterisk(ctx, t, cfg, "cat", "/proc/1/mounts"))
			if !slices.Contains(opts, "ro") {
				t.Errorf("asterisk's root filesystem is mounted %v, want ro", opts)
			}
			return ctx
		}).
		Assess("the agent runs as 65532 and the file server as 65533, both with no capabilities", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// The agent and the file server are distroless: no shell, no
			// cat. A hostPID tools pod reads their /proc entries instead.
			agent := startTool(ctx, t, toolPod{name: "proc-agent", hostPID: true, args: []string{"proc", "patchbay", "agent"}})
			fs := startTool(ctx, t, toolPod{name: "proc-fileserver", hostPID: true, args: []string{"proc", "patchbay", "fileserver"}})
			for _, tc := range []struct {
				name, pod string
				want      map[string]string
			}{
				{"agent", agent, map[string]string{
					"Uid": "65532 65532 65532 65532", "Gid": "65532 65532 65532 65532",
					// The pod's fsGroup (it writes the live config with it) and
					// its own gid, which the runtime lists too.
					"Groups": strconv.Itoa(layoutFSGroup) + " 65532",
					"CapInh": capsNone, "CapPrm": capsNone, "CapEff": capsNone, "CapBnd": capsNone, "CapAmb": capsNone,
					"NoNewPrivs": "1",
				}},
				{"file server", fs, map[string]string{
					"Uid": "65533 65533 65533 65533", "Gid": "65533 65533 65533 65533",
					"Groups": "", // dropped, the pod's fsGroup with them
					"CapInh": capsNone, "CapPrm": capsNone, "CapEff": capsNone, "CapAmb": capsNone,
					// The bounding set keeps the three capabilities the
					// container started with: clearing it needs CAP_SETPCAP.
					// With no_new_privs and a static image (no setuid or
					// file-capability binary) they can never come back.
					"CapBnd":     capsFileserverBounding,
					"NoNewPrivs": "1",
				}},
			} {
				r := waitTool(ctx, t, tc.pod, true)
				procs := splitProcs(r.Logs)
				if len(procs) != 1 {
					t.Errorf("%s: want exactly one process, found %d:\n%s", tc.name, len(procs), r.Logs)
					continue
				}
				for _, st := range procs {
					wantStatus(t, tc.name, st, tc.want)
				}
			}
			return ctx
		}).
		Assess("the file server's identity reads tftp/ and nothing resolved", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// The live config volume, from the node (an emptyDir of the PBX
			// pod), mounted where the PBX pod mounts it, and read as
			// 65533:65533 with no other group: the file server after its
			// drop (the previous assess shows it has exactly that identity).
			pod := pbxPod(ctx, t)
			hostDir := fmt.Sprintf("/var/lib/kubelet/pods/%s/volumes/kubernetes.io~empty-dir/config", pod.UID)
			cur := compile.LiveDir + "/" + compile.CurrentLink
			laptop := h.compiled(ctx, t).Devices[laptopID].ConfigFile
			readable := []string{cur + "/tftp/" + kitchenSEP}
			unreadable := []string{
				cur + "/asterisk/manager.conf", cur + "/asterisk/pjsip.conf",
				cur + "/" + laptop, cur + "/bundle.json",
			}
			r := runTool(ctx, t, toolPod{
				name: "read-as-fileserver", runAsUser: ptr.To[int64](65533), runAsGroup: ptr.To[int64](65533),
				args: append(append([]string{"read"}, readable...), unreadable...),
				volumes: []corev1.Volume{{Name: "live", VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: hostDir, Type: ptr.To(corev1.HostPathDirectory)},
				}}},
				mounts: []corev1.VolumeMount{{Name: "live", MountPath: compile.LiveDir, ReadOnly: true}},
			}, true)
			for _, p := range readable {
				if !strings.Contains(r.Logs, "read "+p+": ok") {
					t.Errorf("65533 cannot read %s", p)
				}
			}
			for _, p := range unreadable {
				if !strings.Contains(r.Logs, "read "+p+": error: ") || !strings.Contains(r.Logs, "read "+p+": error: open "+p+": permission denied") {
					t.Errorf("65533 is not denied %s", p)
				}
			}
			return ctx
		}).
		Assess("Asterisk 22 runs the compiled config", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if v := asterisk(ctx, t, cfg, "core show version"); !strings.HasPrefix(v, "Asterisk 22.") {
				t.Errorf("core show version: %q", v)
			}
			transports := asterisk(ctx, t, cfg, "pjsip show transports")
			for _, want := range []string{
				fmt.Sprintf("transport-udp             udp      0      0  %s:%d", h.nodeIP, sipPort),
				fmt.Sprintf("transport-tcp             tcp      0      0  %s:%d", h.nodeIP, sipPort),
			} {
				if !strings.Contains(transports, want) {
					t.Errorf("pjsip show transports lacks %q:\n%s", want, transports)
				}
			}
			endpoints := asterisk(ctx, t, cfg, "pjsip show endpoints")
			for _, id := range []string{deskID, kitchenID, laptopID} {
				if !strings.Contains(endpoints, " "+id+" ") && !strings.Contains(endpoints, " "+id+"/") {
					t.Errorf("no endpoint %s:\n%s", id, endpoints)
				}
			}
			modules := asterisk(ctx, t, cfg, "module show")
			for _, m := range compile.Modules() {
				if !moduleRunning(modules, m) {
					t.Errorf("module %s is not Running", m)
				}
			}
			if ami := asterisk(ctx, t, cfg, "manager show settings"); !strings.Contains(ami, "127.0.0.1:5038") {
				t.Errorf("AMI is not bound to loopback only:\n%s", ami)
			}
			if httpStatus := asterisk(ctx, t, cfg, "http show status"); !strings.Contains(httpStatus, "Server Disabled") {
				t.Errorf("Asterisk's HTTP server is not disabled:\n%s", httpStatus)
			}
			if plan := asterisk(ctx, t, cfg, "dialplan show "+echoNumber+"@"+compile.DevicesContext); !strings.Contains(plan, "Echo()") {
				t.Errorf("no echo extension:\n%s", plan)
			}
			return ctx
		}).
		Assess("the agent answers through its Service from the pod network", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			base := fmt.Sprintf("http://%s.%s.svc:%d", agentService, namespace, controlPort)
			if r := runTool(ctx, t, toolPod{name: "readyz", args: []string{"http", "get", base + agentapi.PathReadyz}}, true); !r.Succeeded {
				t.Errorf("GET %s from a pod failed", agentapi.PathReadyz)
			}
			if r := runTool(ctx, t, toolPod{name: "noauth", args: []string{"http", "get", "--expect-status", "401", base + agentapi.PathObservations}}, true); !r.Succeeded {
				t.Errorf("GET %s without the token was not refused with 401", agentapi.PathObservations)
			}
			r := runTool(ctx, t, toolPod{
				name: "observations",
				env:  []corev1.EnvVar{secretEnv("AGENT_TOKEN", agentTokenSecret, "token")},
				args: []string{"http", "get", "--header", "Authorization: Bearer $(AGENT_TOKEN)", base + agentapi.PathObservations},
			}, true)
			if !r.Succeeded {
				t.Fatalf("GET %s with the token failed", agentapi.PathObservations)
			}
			// The agent answers one line of JSON; it must be the wire type.
			var body string
			for line := range strings.SplitSeq(r.Logs, "\n") {
				if strings.HasPrefix(line, "{") {
					body = line
				}
			}
			var obs agentapi.Observations
			if err := json.Unmarshal([]byte(body), &obs); err != nil {
				t.Fatalf("observations are not an Observations document: %v\n%s", err, r.Logs)
			}
			if obs.Agent.StartedAt.IsZero() || obs.Devices == nil || obs.Agent.AppliedVersion == "" {
				t.Errorf("observations: %+v, want a version applied", obs.Agent)
			}
			// And through the port-forward the CLI uses: Asterisk is up over
			// AMI, and the version the controller compiled last is applied and
			// verified (no apply error).
			h.eventually(ctx, t, "AMI up", time.Minute, func(o agentapi.Observations) (bool, string) {
				a := o.Agent
				b, err := h.readCompiled(ctx)
				if err != nil {
					return false, err.Error()
				}
				return a.AsteriskUp && strings.HasPrefix(a.AsteriskVersion, "22.") && a.LastApplyError == "" &&
					a.AppliedVersion == b.Version, fmt.Sprintf("compiled %.12s; %+v", b.Version, a)
			})
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// Capability sets as /proc/<pid>/status prints them.
const (
	capsNone = "0000000000000000"
	// capsFileserverBounding is CAP_NET_BIND_SERVICE (10), CAP_SETUID (7) and
	// CAP_SETGID (6).
	capsFileserverBounding = "00000000000004c0"
	// layoutFSGroup is the chart's pbx.fsGroup (layout.DefaultFSGroup).
	layoutFSGroup = 1000
)

// wantStatus checks /proc status fields.
func wantStatus(t *testing.T, who string, st, want map[string]string) {
	t.Helper()
	for field, v := range want {
		if got, ok := st[field]; !ok || got != v {
			t.Errorf("%s %s = %q (present: %v), want %q", who, field, got, ok, v)
		}
	}
}

// splitProcs parses `fakephone proc` output: one status per process.
func splitProcs(logs string) []map[string]string {
	var out []map[string]string
	for chunk := range strings.SplitSeq(logs, "=== pid ") {
		if strings.Contains(chunk, "Uid:") {
			out = append(out, procStatus(chunk))
		}
	}
	return out
}

// TestRegister registers fakephone on the compiled config as Phone kitchen
// over TCP (what the 79x5 uses), its number as SIP user and its device ID
// as digest username; Asterisk challenged first and holds the pod's
// contact, and the agent reports it with its Call-ID. (A Softphone over
// UDP, whose contact Asterisk rewrites, is TestDeskLoop's laptop.) Then
// the refusals: a wrong password and a contact outside the
// allowed networks end on 401 (Asterisk re-challenges an endpoint's own
// refusals), and only a source outside them meets the global ACL's 403.
func TestRegister(t *testing.T) {
	t.Parallel()
	kitchenContact.Lock()
	defer kitchenContact.Unlock()
	f := features.New("REGISTER")
	for _, tc := range []struct {
		transport, id, number string
		rewritten             bool
	}{
		{"tcp", kitchenID, kitchenNumber, false},
	} {
		f = f.Assess(tc.id+" over "+tc.transport, func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			r := runTool(ctx, t, toolPod{
				name: "register-" + tc.transport, env: deviceEnv(tc.id, tc.number),
				args: []string{"register", "--transport", tc.transport, "--expires", "300"},
			}, true)
			if !r.Succeeded {
				t.Fatalf("fakephone register as %s over %s failed", tc.id, tc.transport)
			}
			// An endpoint without auth would accept the first, unanswered
			// REGISTER; fakephone reports whether it was challenged.
			if !strings.Contains(r.Logs, "challenged=true") {
				t.Errorf("Asterisk accepted REGISTER without a digest challenge")
			}
			ip := r.Pod.Status.PodIP
			// The Phone keeps the Contact it sent (rewrite_contact = no, the
			// 79x5 profile); the Softphone's is rewritten to where the
			// REGISTER came from (rewrite_contact = yes, Linphone over UDP).
			want := fmt.Sprintf("sip:%s@%s:%d;transport=%s", tc.number, ip, sipPort, tc.transport)
			if tc.rewritten {
				m := regexp.MustCompile(`contact=<(sip:` + tc.number + `@` + regexp.QuoteMeta(ip) + `:\d+)>`).FindStringSubmatch(r.Logs)
				if m == nil {
					t.Fatalf("the registrar did not echo a contact at %s", ip)
				}
				want = m[1]
			}
			if ip == "" || !strings.Contains(r.Logs, "contact=<"+want+">") {
				t.Fatalf("the registrar did not echo the contact %s", want)
			}
			// pjsip show contacts truncates long URIs; astdb holds the
			// contact in full.
			db := asterisk(ctx, t, cfg, "database show registrar")
			if !strings.Contains(db, `"uri":"`+want+`"`) || !strings.Contains(db, `"user_agent":"patchbay-fakephone"`) {
				t.Errorf("Asterisk does not hold contact %s from fakephone:\n%s", want, db)
			}
			// The agent sees it over AMI, with the REGISTER's Call-ID.
			h.eventually(ctx, t, tc.id+"'s registration in the observations", 30*time.Second, func(o agentapi.Observations) (bool, string) {
				d := device(o, tc.id)
				if d == nil || d.Registration == nil {
					return false, "no registration"
				}
				reg := d.Registration
				return reg.Address == ip && reg.CallID != "" && reg.UserAgent == "patchbay-fakephone" &&
					strings.HasPrefix(reg.Contact, "sip:"+tc.number+"@"+ip), fmt.Sprintf("%+v", *reg)
			})
			return ctx
		})
	}
	f = f.Assess("a wrong password ends on 401 and registers nothing", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		r := runFakephone(ctx, t, "register-wrong-password", false,
			"register", "--password", "not-the-password", "--expires", "300")
		if r.Succeeded || !strings.Contains(r.Logs, "REGISTER: 401") || !strings.Contains(r.Logs, "challenged=true") {
			t.Errorf("want a 401 after a digest challenge, got:\n%s", r.Logs)
		}
		if db := asterisk(ctx, t, cfg, "database show registrar"); strings.Contains(db, "@"+r.Pod.Status.PodIP+":") {
			t.Errorf("Asterisk holds a contact for %s after a wrong password:\n%s", r.Pod.Status.PodIP, db)
		}
		return ctx
	})
	f = f.Assess("a contact outside the allowed networks ends on 401 (the endpoint's contact ACL)", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		r := runFakephone(ctx, t, "register-contact-acl", false,
			"register", "--contact-host", "192.0.2.7", "--expires", "300")
		if r.Succeeded || !strings.Contains(r.Logs, "REGISTER: 401") || !strings.Contains(r.Logs, "challenged=true") {
			t.Errorf("want a 401 after a digest challenge, got:\n%s", r.Logs)
		}
		if db := asterisk(ctx, t, cfg, "database show registrar"); strings.Contains(db, "192.0.2.7") {
			t.Errorf("Asterisk holds the refused contact:\n%s", db)
		}
		return ctx
	})
	f = f.Assess("a source outside the allowed networks gets 403 from the global ACL, unchallenged", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
		// On the host network, fakephone's REGISTER comes from the node's
		// own address, which is not in the allowed pod CIDR.
		r := runTool(ctx, t, toolPod{
			name: "register-global-acl", hostNetwork: true, env: kitchenEnv(),
			args: []string{"register", "--sip-port", "5099", "--expires", "300"},
		}, false)
		if r.Succeeded || !strings.Contains(r.Logs, "REGISTER: 403") || !strings.Contains(r.Logs, "challenged=false") {
			t.Errorf("want an unchallenged 403, got:\n%s", r.Logs)
		}
		return ctx
	})
	testenv.Test(t, f.Feature())
}

// TestEchoCall calls the echo test from a pod as Phone kitchen: SIP to the
// node's address, RTP between the pod network and the host network, and a
// 1 kHz tone that must come back from the node's own address.
func TestEchoCall(t *testing.T) {
	t.Parallel()
	f := features.New("echo call").
		Assess("600 echoes a 1 kHz tone", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			r := runFakephone(ctx, t, "call-echo", true, "call", echoNumber, "--duration", "3s")
			stats, ok := parseMedia(r.Logs)
			if !r.Succeeded || !ok {
				t.Fatalf("fakephone call %s failed", echoNumber)
			}
			// 3 s of 20 ms packets is 150; fakephone already insists on
			// 50 tone packets, so this restates the bar for the report.
			if stats.Sent != 150 || stats.ToneFrames < 50 {
				t.Errorf("media: %+v", stats)
			}
			if !strings.HasPrefix(stats.Source, h.nodeIP+":") {
				t.Errorf("RTP came back from %s, want the node %s", stats.Source, h.nodeIP)
			}
			t.Logf("echo media: sent %d, received %d, tone in %d, from %s", stats.Sent, stats.Received, stats.ToneFrames, stats.Source)
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// TestController checks the controller as the chart runs it, under its
// least-privilege Role: it holds its install's Lease; it minted the
// install's Secrets (the agent token the PBX pod waited for, the AMI
// password, one per device, owned by its resource) with its labels; it
// compiled the site into pbx-secrets and pbx-config at one version, which
// the agent applied; and it decided the Person routed.
func TestController(t *testing.T) {
	f := features.New("controller").
		Assess("holds its leader-election Lease", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			var lease *coordinationv1.Lease
			err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
				l, err := h.cs.CoordinationV1().Leases(namespace).Get(ctx, controller.LeaderElectionID(release), metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				if err != nil {
					return false, err
				}
				lease = l
				return l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity != "", nil
			})
			if err != nil {
				t.Fatalf("no holder of Lease %s/%s within a minute: %v (lease %+v)", namespace, controller.LeaderElectionID(release), err, lease)
			}
			t.Logf("Lease %s held by %s", controller.LeaderElectionID(release), *lease.Spec.HolderIdentity)
			return ctx
		}).
		Assess("minted the install's Secrets, with its labels", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			owners := map[string]string{
				names.AgentTokenSecret:              "",
				names.AMISecret:                     "",
				names.ProjectedSecret:               "",
				compile.DeviceSecretName(deskID):    "Phone/" + deskName,
				compile.DeviceSecretName(kitchenID): "Phone/" + kitchenName,
				compile.DeviceSecretName(laptopID):  "Softphone/" + laptopName,
			}
			for name, owner := range owners {
				sec, err := h.cs.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					t.Errorf("Secret %s: %v", name, err)
					continue
				}
				for k, v := range install.ManagedLabels() {
					if sec.Labels[k] != v {
						t.Errorf("Secret %s has %s=%q, want %q", name, k, sec.Labels[k], v)
					}
				}
				var got string
				for _, ref := range sec.OwnerReferences {
					if ptr.Deref(ref.Controller, false) && ptr.Deref(ref.BlockOwnerDeletion, false) {
						got = ref.Kind + "/" + ref.Name
					}
				}
				if got != owner || (owner == "" && len(sec.OwnerReferences) > 0) {
					t.Errorf("Secret %s is owned by %q (%+v), want %q", name, got, sec.OwnerReferences, owner)
				}
			}
			tok, err := h.secretValue(ctx, names.AgentTokenRef())
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(tok)) != h.agentToken || h.agentToken == "" {
				t.Error("the agent token the harness uses is not the minted one")
			}
			return ctx
		}).
		Assess("compiled the site into pbx-secrets, then pbx-config, which the agent applied", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			b := h.waitApplied(ctx, t, time.Minute)
			cm, err := h.cs.CoreV1().ConfigMaps(namespace).Get(ctx, names.ConfigMap, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if v := cm.Annotations[install.AnnotationBundleVersion]; v != b.Version {
				t.Errorf("pbx-config's %s is %q, its index %s", install.AnnotationBundleVersion, v, b.Version)
			}
			for k, v := range install.ManagedLabels() {
				if cm.Labels[k] != v {
					t.Errorf("pbx-config has %s=%q, want %q", k, cm.Labels[k], v)
				}
			}
			// pbx-secrets holds exactly what the bundle references, with the
			// values it was compiled with (the agent's own check).
			refs, err := b.SecretRefs()
			if err != nil {
				t.Fatal(err)
			}
			if len(b.projected) != len(refs) {
				t.Errorf("pbx-secrets holds %d keys, the bundle references %d", len(b.projected), len(refs))
			}
			if v, err := b.VersionWith(b.values); err != nil || v != b.Version {
				t.Errorf("pbx-secrets' values give version %s (%v), want %s", v, err, b.Version)
			}
			for _, id := range []string{deskID, kitchenID, laptopID} {
				if d, ok := b.Devices[id]; !ok || d.Version == "" || d.Number == "" {
					t.Errorf("%s is not compiled and routed: %+v", id, d)
				}
			}
			return ctx
		}).
		Assess("decided Person chris routed to desk and laptop", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
				var p v1alpha1.Person
				if err := h.kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: personName}, &p); err != nil {
					return false, err
				}
				c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionReady)
				return p.Status.State == "Ready" && c != nil && c.Status == metav1.ConditionTrue && c.Reason == v1alpha1.ReasonRouted, nil
			})
			if err != nil {
				t.Errorf("Person %s is not Ready (Routed): %v", personName, err)
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// typedResources is a klient Resources that knows the patchbay types.
func typedResources(t *testing.T, cfg *envconf.Config) *resources.Resources {
	t.Helper()
	r, err := resources.New(cfg.Client().RESTConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(r.GetScheme()); err != nil {
		t.Fatal(err)
	}
	return r
}

// moduleRunning reports whether `module show` lists name as Running.
func moduleRunning(moduleShow, name string) bool {
	for line := range strings.SplitSeq(moduleShow, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == name && strings.Contains(line, "Running") {
			return true
		}
	}
	return false
}
