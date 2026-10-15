//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
)

// TestModulesNarrowed: Asterisk's module directory holds exactly the
// modules the bundle loads, so even the agent's AMI account, which may
// ModuleLoad, cannot load one that runs commands, not even by a relative
// path into the image's own directory (docs/plans/release-a.md §5.2.1).
func TestModulesNarrowed(t *testing.T) {
	t.Parallel()
	f := features.New("narrowed modules").
		Assess("the module directory and the loaded modules are exactly compile.Modules()", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			want := slices.Sorted(slices.Values(compile.Modules()))
			ls := strings.Fields(execInAsterisk(ctx, t, cfg, "ls", "-1", compile.AsteriskModuleDir))
			if slices.Sort(ls); !slices.Equal(ls, want) {
				t.Errorf("%s holds\n  %v\nwant\n  %v", compile.AsteriskModuleDir, ls, want)
			}
			var loaded []string
			for line := range strings.SplitSeq(asterisk(ctx, t, cfg, "module show"), "\n") {
				if f := strings.Fields(line); len(f) > 0 && strings.HasSuffix(f[0], ".so") {
					loaded = append(loaded, f[0])
				}
			}
			if slices.Sort(loaded); !slices.Equal(loaded, want) {
				t.Errorf("module show lists\n  %v\nwant\n  %v", loaded, want)
			}
			return ctx
		}).
		Assess("ModuleLoad of app_system, func_shell, res_agi and res_ari fails with the agent's account", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			actions := []string{
				// The control: this account may ModuleLoad.
				"ModuleLoad Module=res_pjsip_notify.so LoadType=reload",
				"ModuleLoad Module=app_system.so LoadType=load",
				"ModuleLoad Module=func_shell.so LoadType=load",
				"ModuleLoad Module=res_agi.so LoadType=load",
				"ModuleLoad Module=res_ari.so LoadType=load",
				"ModuleLoad Module=../usr/lib/asterisk/modules/app_system.so LoadType=load",
			}
			// AMI listens on the node's loopback only.
			r := runTool(ctx, t, toolPod{
				name: "ami", hostNetwork: true,
				env: []corev1.EnvVar{
					{Name: "FAKEPHONE_AMI_USER", Value: compile.AMIUsername},
					secretEnv("FAKEPHONE_AMI_SECRET", amiRef.Name, amiRef.Key),
				},
				args: append([]string{"ami"}, actions...),
			}, true)
			if !r.Succeeded {
				t.Fatal("the AMI session failed")
			}
			for i, a := range actions {
				want := "response=Error"
				if i == 0 {
					want = "response=Success"
				}
				if line := fmt.Sprintf("action=%d %s %s", i, a, want); !strings.Contains(r.Logs, line) {
					t.Errorf("want %q", line)
				}
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// resolvedFile is a file of the compiled bundle with its placeholders
// resolved from pbx-secrets, as the agent writes it.
func resolvedFile(t *testing.T, b compiledBundle, path string) []byte {
	t.Helper()
	res, err := intake.Resolve(b.Bundle, b.projected)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := res.Files[path]
	if !ok {
		t.Fatalf("the bundle has no %s", path)
	}
	return f
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fetched reports whether the device's observations hold a fetch matching
// all of the given fields.
func fetched(d *agentapi.DeviceObservations, transport agentapi.Transport, file string, result agentapi.FetchResult, version, source string) bool {
	if d == nil {
		return false
	}
	return slices.ContainsFunc(d.Fetches, func(f agentapi.Fetch) bool {
		return f.Transport == transport && f.File == file && f.Result == result && f.Version == version && f.Source == source
	})
}

// TestTFTPAndHTTP6970: the file server serves the Phone's SEP file, with
// its password resolved, over TFTP and HTTP :6970 alike; a .sgn and an
// unknown phone's SEP file are not found; and every request reaches the
// observations, attributed to the device (with its config version for what
// was served) or listed as an unknown phone.
func TestTFTPAndHTTP6970(t *testing.T) {
	t.Parallel()
	f := features.New("TFTP and HTTP 6970").
		Assess("the SEP file, resolved, over both; the misses; all in the observations", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			b := h.waitApplied(ctx, t, time.Minute)
			want := resolvedFile(t, b, "tftp/"+kitchenSEP)
			password := b.values[compile.PasswordRef(kitchenID)]
			if !bytes.Contains(want, password) || bytes.Contains(want, []byte("@@secret:")) {
				t.Fatal("the resolved SEP file does not carry the phone's password")
			}
			index := b.Devices[kitchenID]

			tftp := startTool(ctx, t, toolPod{name: "tftp-sep", env: kitchenEnv(),
				args: []string{"tftp", "get", "-o", "/dev/null", kitchenSEP}})
			web := startTool(ctx, t, toolPod{name: "http6970-sep",
				args: []string{"http", "get", "--print-body=false", fmt.Sprintf("http://%s:%d/%s", h.nodeIP, ciscoHTTPPort, kitchenSEP)}})
			sgn := startTool(ctx, t, toolPod{name: "tftp-sgn", env: kitchenEnv(),
				args: []string{"tftp", "get", "-o", "/dev/null", kitchenSEP + ".sgn"}})
			unknown := startTool(ctx, t, toolPod{name: "tftp-unknown", env: kitchenEnv(),
				args: []string{"tftp", "get", "-o", "/dev/null", "SEP" + unknownMAC + ".cnf.xml"}})

			rt := waitTool(ctx, t, tftp, true)
			rw := waitTool(ctx, t, web, true)
			rs := waitTool(ctx, t, sgn, false)
			ru := waitTool(ctx, t, unknown, false)
			for name, r := range map[string]fakephoneResult{"TFTP": rt, "HTTP 6970": rw} {
				if !r.Succeeded || !strings.Contains(r.Logs, "sha256="+sha(want)) {
					t.Errorf("%s did not serve the resolved SEP file (sha256 %s)", name, sha(want))
				}
			}
			if rs.Succeeded || ru.Succeeded {
				t.Errorf("served what does not exist: .sgn %v, unknown phone %v", rs.Succeeded, ru.Succeeded)
			}

			h.eventually(ctx, t, "the fetches in the observations", 30*time.Second, func(o agentapi.Observations) (bool, string) {
				d := device(o, kitchenID)
				ok := fetched(d, agentapi.TransportTFTP, kitchenSEP, agentapi.FetchServed, index.Version, rt.Pod.Status.PodIP) &&
					fetched(d, agentapi.TransportHTTP6970, kitchenSEP, agentapi.FetchServed, index.Version, rw.Pod.Status.PodIP) &&
					fetched(d, agentapi.TransportTFTP, kitchenSEP+".sgn", agentapi.FetchNotFound, "", rs.Pod.Status.PodIP) &&
					slices.ContainsFunc(o.UnknownDevices, func(u agentapi.UnknownDevice) bool {
						return u.MAC == unknownMAC && u.Source == ru.Pod.Status.PodIP
					})
				var fetches []agentapi.Fetch
				if d != nil {
					fetches = d.Fetches
				}
				return ok, fmt.Sprintf("kitchen fetches %+v; unknown %+v", fetches, o.UnknownDevices)
			})
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// e2eLoad is the e2e test firmware load (only in -tags e2e builds).
func e2eLoad(t *testing.T) drivers.FirmwareLoad {
	t.Helper()
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	d, err := reg.ForModel(string(v1alpha1.PhoneModelCisco7945))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range d.Firmware() {
		if strings.HasPrefix(l.Name, "E2E") {
			return l
		}
	}
	t.Fatal("the e2e build has no e2e firmware load")
	return drivers.FirmwareLoad{}
}

func (h *harness) putFirmware(ctx context.Context, t *testing.T, load, name string, body []byte) (int, agentapi.FirmwareUploadResponse) {
	t.Helper()
	code, raw := h.agentDo(ctx, t, http.MethodPut, agentapi.FirmwarePath(load, name), body)
	var resp agentapi.FirmwareUploadResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("PUT %s/%s: %d, not an upload response: %s", load, name, code, raw)
	}
	return code, resp
}

// TestFirmwareUploadAndServe uploads the e2e load through the agent (over a
// port-forward with the token, the path `patchbay firmware load` takes): a
// corrupted file is refused with 422 and nothing stored, a good one is
// stored (201) and then known (200); the store sensor reports it present,
// and the file server serves it, firmware first, over TFTP and HTTP 6970.
func TestFirmwareUploadAndServe(t *testing.T) {
	f := features.New("firmware upload").
		Assess("422, 201, 200, then present and served", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			load := e2eLoad(t)
			var sbn string
			for _, f := range load.Files {
				if strings.HasSuffix(f.Name, ".sbn") {
					sbn = f.Name
				}
			}
			good := cisco79x5.E2ELoadFile(sbn)
			bad := bytes.Clone(good)
			bad[len(bad)-1] ^= 0xFF
			// A reused cluster keeps the store (the persistent volume): what
			// is there already answers 200, not 201.
			before := h.observations(ctx, t)
			stored := func(name string) bool {
				return slices.Contains(before.Firmware.Files, agentapi.FirmwareFileStatus{Load: load.Name, Name: name, Present: true})
			}
			created := func(name string) int {
				if stored(name) {
					return http.StatusOK
				}
				return http.StatusCreated
			}
			if code, resp := h.putFirmware(ctx, t, load.Name, sbn, bad); code != http.StatusUnprocessableEntity || resp.Stored != stored(sbn) {
				t.Errorf("a corrupted %s: %d %+v, want 422, nothing stored", sbn, code, resp)
			}
			if code, resp := h.putFirmware(ctx, t, load.Name, sbn, good); code != created(sbn) || !resp.Stored || resp.SHA256 != sha(good) {
				t.Errorf("%s: %d %+v, want %d, stored", sbn, code, resp, created(sbn))
			}
			if code, resp := h.putFirmware(ctx, t, load.Name, sbn, good); code != http.StatusOK || !resp.Stored {
				t.Errorf("%s again: %d %+v, want 200, stored", sbn, code, resp)
			}
			for _, f := range load.Files {
				if f.Name == sbn {
					continue
				}
				if code, resp := h.putFirmware(ctx, t, load.Name, f.Name, cisco79x5.E2ELoadFile(f.Name)); code != created(f.Name) {
					t.Errorf("%s: %d %+v, want %d", f.Name, code, resp, created(f.Name))
				}
			}

			obs := h.observations(ctx, t)
			for _, f := range load.Files {
				if !slices.Contains(obs.Firmware.Files, agentapi.FirmwareFileStatus{Load: load.Name, Name: f.Name, Present: true}) {
					t.Errorf("the store sensor does not report %s/%s present: %+v", load.Name, f.Name, obs.Firmware.Files)
				}
			}

			tftp := startTool(ctx, t, toolPod{name: "tftp-firmware", env: kitchenEnv(),
				args: []string{"tftp", "get", "-o", "/dev/null", sbn}})
			web := startTool(ctx, t, toolPod{name: "http6970-firmware",
				args: []string{"http", "get", "--print-body=false", fmt.Sprintf("http://%s:%d/%s", h.nodeIP, ciscoHTTPPort, sbn)}})
			for _, name := range []string{tftp, web} {
				if r := waitTool(ctx, t, name, true); !r.Succeeded || !strings.Contains(r.Logs, "sha256="+sha(good)) {
					t.Errorf("%s did not serve %s (sha256 %s)", name, sbn, sha(good))
				}
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// TestPrompts calls 911, the reserved numbers 0, 999 (9.) and 205 (2XX), and
// an unassigned one (199), as kitchen: each is answered and plays its announcement, at least 2 s of non-silent
// audio, of which at least 1 s is not the SIT tone (the prompt itself, which
// the agent installed from the image).
func TestPrompts(t *testing.T) {
	t.Parallel()
	f := features.New("prompts").
		Assess("911, 0, 999, 205 and 199 are answered with their announcements", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			pods := map[string]string{}
			for _, number := range []string{"911", "0", "999", "205", "199"} {
				pods[number] = startTool(ctx, t, toolPod{name: "call-" + number, env: kitchenEnv(), args: []string{
					"call", number, "--expect-tone=false", "--expect-audio", "2s", "--min-voice", "1s", "--duration", "4s",
				}})
			}
			for number, name := range pods {
				r := waitTool(ctx, t, name, true)
				stats, ok := parseMedia(r.Logs)
				if !r.Succeeded || !ok {
					t.Errorf("%s: no announcement", number)
					continue
				}
				t.Logf("%s: %d packets back, %d not silent, %d of them not the SIT tone", number, stats.Received, stats.Loud, stats.Voice)
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// TestResyncNotify: POST /v1/actions/resync makes the agent send the
// registered phone a service-control NOTIFY with the plan's body (quoting
// the Call-ID of its REGISTER); the phone refetches its SEP file. As kitchen,
// whose contact TestRegister must not replace meanwhile (kitchenContact).
func TestResyncNotify(t *testing.T) {
	t.Parallel()
	kitchenContact.Lock()
	defer kitchenContact.Unlock()
	f := features.New("resync").
		Assess("the phone gets the service-control NOTIFY and refetches", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			version := h.waitApplied(ctx, t, time.Minute).Devices[kitchenID].Version
			name := startTool(ctx, t, toolPod{name: "notify", env: kitchenEnv(), args: []string{
				"register", "--expires", "300", "--wait-notify", "90s", "--refetch", kitchenSEP,
			}})
			waitLog(ctx, t, name, "registered ", time.Minute)
			pod, err := h.cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			ip := pod.Status.PodIP
			var callID string
			h.eventually(ctx, t, "the waiting phone's registration", 30*time.Second, func(o agentapi.Observations) (bool, string) {
				d := device(o, kitchenID)
				if d == nil || d.Registration == nil {
					return false, "no registration"
				}
				callID = d.Registration.CallID
				return d.Registration.Address == ip && callID != "", fmt.Sprintf("%+v", *d.Registration)
			})

			req, _ := json.Marshal(agentapi.ActionRequest{Device: kitchenID, Version: version})
			code, body := h.agentDo(ctx, t, http.MethodPost, agentapi.PathActionResync, req)
			var resp agentapi.ActionResponse
			if err := json.Unmarshal(body, &resp); code != http.StatusOK || err != nil || !resp.Sent {
				t.Fatalf("resync: %d %s", code, body)
			}

			r := waitTool(ctx, t, name, true)
			wantBody := strings.Join([]string{
				"action=restart",
				"RegisterCallId={" + callID + "}",
				"ConfigVersionStamp={00000000-0000-0000-0000-000000000000}",
				"DialplanVersionStamp={00000000-0000-0000-0000-000000000000}",
				"SoftkeyVersionStamp={00000000-0000-0000-0000-000000000000}",
				"FeatureControlVersionStamp={00000000-0000-0000-0000-000000000000}",
				"HeadsetVersionStamp={0-0000000000}",
			}, "\r\n")
			if !strings.Contains(r.Logs, "notify event=service-control ") || !strings.Contains(r.Logs, "body="+strconv.Quote(wantBody)) {
				t.Errorf("the NOTIFY did not carry the body %q", wantBody)
			}
			if !strings.Contains(r.Logs, `"active"`) {
				t.Error("the NOTIFY has no Subscription-State: active")
			}
			if !strings.Contains(r.Logs, "refetched "+kitchenSEP+" ") {
				t.Error("the phone did not refetch its SEP file")
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// killAgent stops the agent container (the kubelet restarts it, a native
// sidecar) and waits until it is ready again.
func killAgent(ctx context.Context, t *testing.T) {
	t.Helper()
	before := agentRestarts(ctx, t)
	id, err := h.run(ctx, "docker", "exec", h.nodeName, "crictl", "ps", "--name", "^agent$", "--state", "Running", "-q")
	if err != nil || strings.TrimSpace(id) == "" {
		t.Fatalf("finding the agent container: %v %q", err, id)
	}
	if _, err := h.run(ctx, "docker", "exec", h.nodeName, "crictl", "stop", "--timeout", "0", strings.TrimSpace(id)); err != nil {
		t.Fatal(err)
	}
	err = wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		p := pbxPod(ctx, t)
		for _, s := range p.Status.InitContainerStatuses {
			if s.Name == "agent" {
				return s.RestartCount > before && s.Ready, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("the agent did not come back ready: %v", err)
	}
}

func agentRestarts(ctx context.Context, t *testing.T) int32 {
	t.Helper()
	for _, s := range pbxPod(ctx, t).Status.InitContainerStatuses {
		if s.Name == "agent" {
			return s.RestartCount
		}
	}
	t.Fatal("no agent container status")
	return 0
}

// TestProvisioningOneTime: Softphone laptop's provisioning URL, as the
// controller shows it in laptop's status, serves its document once over
// the pod network, then 404, and still 404 after the agent restarts (the
// spend is on the persistent volume); the served fetch, with the
// softphone's version, is back in the restarted agent's observations; and
// the controller, which was scaled to 0 meanwhile, sees it and replaces
// the spent token (Provisioned, lastAction rotateToken) and publishes no
// URL (design §8.4: only an Issued softphone shows one). A laptop with no
// URL (a reused cluster's, already provisioned) is reissued first.
//
// The controller is down so the agent restarts while the token is still
// granted: once a rotated version has settled, the agent rightly forgets
// the spend (it guards a token no bundle grants any more), and the restart
// would have nothing to remember. The case that matters is the agent
// restarting before the controller has acted on the fetch.
func TestProvisioningOneTime(t *testing.T) {
	f := features.New("one-time provisioning").
		Assess("200, 404, agent restart, 404, served fetch remembered, token rotated", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			var cur v1alpha1.Softphone
			if err := h.kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: laptopName}, &cur); err != nil {
				t.Fatal(err)
			}
			if cur.Status.ProvisioningURL == "" {
				if _, err := h.patchbay(ctx, "softphone", "qr", laptopName, "--reissue"); err != nil {
					t.Fatalf("patchbay softphone qr --reissue: %v", err)
				}
			}
			laptop := h.track(ctx, t, "Softphone", laptopName)
			before := laptop.wait(ctx, t, "a provisioning URL the agent serves", 2*time.Minute, func(d deviceView) (bool, string) {
				return d.ProvisioningURL != "" && d.cond(v1alpha1.ConditionConfigCurrent).Reason != v1alpha1.ReasonApplyPending, ""
			})
			b := h.waitApplied(ctx, t, time.Minute)
			index := b.Devices[laptopID]
			if index.ProvisioningToken == nil {
				t.Fatalf("%s has no provisioning token in the index", laptopID)
			}
			token := b.values[*index.ProvisioningToken]
			if install.ProvisioningURL(h.settings, string(token)) != before.ProvisioningURL {
				t.Fatal("laptop's status shows a URL other than its compiled token's")
			}
			h.scaleController(ctx, t, 0)
			// The URL's token, fixed in a Secret of its own: the controller
			// rotates the device Secret's as soon as the first GET spends it.
			env := []corev1.EnvVar{secretEnv("TOKEN", h.holdSecret(ctx, t, "laptop-token", token), "value")}
			url := fmt.Sprintf("http://%s:%d%s$(TOKEN)", h.nodeIP, deviceHTTPPort, agentapi.ProvisioningPathPrefix)
			get := func(name, status string, want bool) fakephoneResult {
				return runTool(ctx, t, toolPod{name: name, env: env,
					args: []string{"http", "get", "--print-body=false", "--expect-status", status, url}}, want)
			}
			want := resolvedFile(t, b, index.ConfigFile)
			if r := get("provision", "200", true); !r.Succeeded || !strings.Contains(r.Logs, "sha256="+sha(want)) {
				t.Fatalf("the first fetch did not serve the softphone's document (sha256 %s)", sha(want))
			}
			if r := get("provision-again", "404", true); !r.Succeeded {
				t.Error("the spent URL did not answer 404")
			}
			killAgent(ctx, t)
			if r := get("provision-after-restart", "404", true); !r.Succeeded {
				t.Error("the spent URL did not answer 404 after the agent restarted")
			}
			obs := h.eventually(ctx, t, "the served fetch after the restart", 30*time.Second, func(o agentapi.Observations) (bool, string) {
				d := device(o, laptopID)
				ok := d != nil && slices.ContainsFunc(d.Fetches, func(f agentapi.Fetch) bool {
					return f.Transport == agentapi.TransportHTTPDevice && f.File == index.ConfigFile &&
						f.Result == agentapi.FetchServed && f.Version == index.Version
				})
				return ok, fmt.Sprintf("%+v", d)
			})
			if strings.Contains(fmt.Sprint(obs), string(token)) {
				t.Error("the observations carry the token")
			}
			h.scaleController(ctx, t, 1)
			laptop.wait(ctx, t, "Provisioned, its spent token rotated, no URL", time.Minute, func(d deviceView) (bool, string) {
				return d.State == string(drivers.StateProvisioned) && d.LastAction != nil && d.LastAction.Kind == v1alpha1.ActionRotateToken &&
					(before.LastAction == nil || !d.LastAction.At.Equal(&before.LastAction.At)) && d.ProvisioningURL == "", ""
			})
			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// Never leave the controller scaled down for the tests after.
			h.scaleController(context.WithoutCancel(ctx), t, 1)
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// holdSecret keeps value in a Secret of its own (key "value") for the
// test's tools pods, deleted when the test ends, and returns its name.
func (h *harness) holdSecret(ctx context.Context, t *testing.T, label string, value []byte) string {
	t.Helper()
	suffix, err := randomHex(3)
	if err != nil {
		t.Fatal(err)
	}
	name := "e2e-" + label + "-" + suffix
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{phonePodLabel: label}},
		Data:       map[string][]byte{"value": value},
	}
	if _, err := h.cs.CoreV1().Secrets(namespace).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = h.cs.CoreV1().Secrets(namespace).Delete(context.WithoutCancel(ctx), name, metav1.DeleteOptions{})
	})
	return name
}

// TestRollback is §9's rollback, the agent half (the controller half is
// controller-devices' TestApplyFailedControllerHalf). With the controller
// scaled to 0, a bundle Asterisk does not take as given (kitchen's endpoint
// renamed) is written as the controller writes, pbx-secrets first: the
// agent applies it, fails verification, swaps the previous version back
// and reloads, and reports the refused version and why; the old config
// still registers and echoes. Scaled back up, the controller writes its own
// version again, and a reissued laptop token (a new version) leaves the
// agent's last apply a success. No device reports ApplyFailed for a version
// the controller compiled: a restarted controller reads pbx-config as it
// finds it (install.ReadCompiled), so until its own compile replaces the
// test's, a device may report the refusal of the broken version, and only
// that one (plan 6.2.11).
func TestRollback(t *testing.T) {
	start := metav1.Now()
	f := features.New("rollback").
		Assess("a broken bundle is rolled back and reported", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			good := h.waitApplied(ctx, t, time.Minute)
			h.scaleController(ctx, t, 0)

			broken := good.Bundle
			broken.Files = maps.Clone(good.Files)
			from := []byte("\n[" + kitchenID + "]\ntype = endpoint\n")
			pjsip := good.Files["asterisk/pjsip.conf"]
			if bytes.Count(pjsip, from) != 1 {
				t.Fatalf("pjsip.conf does not hold %q once", from)
			}
			broken.Files["asterisk/pjsip.conf"] = bytes.Replace(pjsip, from, []byte("\n["+kitchenID+"-broken]\ntype = endpoint\n"), 1)
			// Its own version, as Compile would give it: the agent applies a
			// version only with the secret values it was compiled with.
			v, err := broken.VersionWith(good.values)
			if err != nil {
				t.Fatal(err)
			}
			broken.Version = v
			h.writeBundle(ctx, t, broken, good.values)

			// Verification retries for 30 s before it gives up.
			obs := h.eventually(ctx, t, "the broken version refused", 2*time.Minute, func(o agentapi.Observations) (bool, string) {
				a := o.Agent
				return a.LastApplyVersion == broken.Version && a.LastApplyError != "", fmt.Sprintf("%+v", a)
			})
			t.Logf("after the broken version: applied %.12s, last apply %.12s: %s", obs.Agent.AppliedVersion, obs.Agent.LastApplyVersion, obs.Agent.LastApplyError)
			if a := obs.Agent; a.AppliedVersion != good.Version || !strings.Contains(a.LastApplyError, "verify") ||
				!strings.Contains(a.LastApplyError, kitchenID) {
				t.Errorf("after the rollback: %+v, want version %s still applied and a verify error naming %s", a, good.Version, kitchenID)
			}
			if endpoints := asterisk(ctx, t, cfg, "pjsip show endpoints"); !strings.Contains(endpoints, " "+kitchenID+"/") ||
				strings.Contains(endpoints, kitchenID+"-broken") {
				t.Errorf("Asterisk was not reloaded with the old config:\n%s", endpoints)
			}

			if r := runFakephone(ctx, t, "rollback-register", true, "register", "--expires", "300"); !r.Succeeded {
				t.Error("the old config does not register")
			}
			r := runFakephone(ctx, t, "rollback-echo", true, "call", echoNumber, "--duration", "2s", "--min-tone-frames", "30")
			if !r.Succeeded {
				t.Error("the old config does not echo")
			}
			return context.WithValue(context.WithValue(ctx, rollbackGoodKey, good.Version), rollbackBrokenKey, broken.Version)
		}).
		Assess("the controller, back, writes its own version; a reissued token applies cleanly", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			goodVersion, _ := ctx.Value(rollbackGoodKey).(string)
			h.scaleController(ctx, t, 1)
			err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
				b, err := h.readCompiled(ctx)
				return err == nil && b.Version == goodVersion, nil
			})
			if err != nil {
				t.Fatalf("the controller did not write its own version %.12s back: %v", goodVersion, err)
			}
			// The controller wrote pbx-secrets (its version's annotation)
			// before pbx-config: if the agent's intake saw the new Secret
			// while pbx-config still held the refused version, it applies
			// that version again and spends its 30 s verifying it (plan
			// §11), and the reissue waits behind it, past the CLI's 30 s.
			// Then a second reissue finds the agent idle.
			if _, err := h.patchbay(ctx, "softphone", "qr", laptopName, "--reissue"); err != nil {
				t.Logf("the first reissue timed out behind the agent's re-verification: %v", err)
				if _, err := h.patchbay(ctx, "softphone", "qr", laptopName, "--reissue"); err != nil {
					t.Fatalf("patchbay softphone qr --reissue: %v", err)
				}
			}
			b := h.waitApplied(ctx, t, 2*time.Minute)
			if b.Version == goodVersion {
				t.Errorf("the reissue made no new version")
			}
			return ctx
		}).
		Assess("no device reported ApplyFailed for a compiled version, and none shows it", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			broken, _ := ctx.Value(rollbackBrokenKey).(string)
			for _, e := range h.eventsSince(ctx, t, start, install.EventReasonApplyFailed) {
				if broken == "" || !strings.Contains(e, "config version "+broken[:12]+" was not applied") {
					t.Errorf("ApplyFailed Event for a version other than the test's broken one: %s", e)
				}
			}
			var phones v1alpha1.PhoneList
			var softphones v1alpha1.SoftphoneList
			if err := h.kube.List(ctx, &phones, client.InNamespace(namespace)); err != nil {
				t.Fatal(err)
			}
			if err := h.kube.List(ctx, &softphones, client.InNamespace(namespace)); err != nil {
				t.Fatal(err)
			}
			var views []deviceView
			for i := range phones.Items {
				views = append(views, phoneView(&phones.Items[i]))
			}
			for i := range softphones.Items {
				views = append(views, softphoneView(&softphones.Items[i]))
			}
			for _, d := range views {
				if d.cond(v1alpha1.ConditionConfigCurrent).Reason == v1alpha1.ReasonApplyFailed {
					t.Errorf("%v", d)
				}
			}
			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// Never leave the controller scaled down for the tests after.
			h.scaleController(context.WithoutCancel(ctx), t, 1)
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

const rollbackGoodKey ctxKey = "rollback-good-version"

const rollbackBrokenKey ctxKey = "rollback-broken-version"

// scaleController scales the controller Deployment and waits: for 0, until
// its pods are gone; otherwise until it has rolled out.
func (h *harness) scaleController(ctx context.Context, t *testing.T, replicas int) {
	t.Helper()
	if _, err := h.kubectl(ctx, "scale", "deployment/"+controllerDeployment, "--namespace", namespace,
		fmt.Sprintf("--replicas=%d", replicas)); err != nil {
		t.Fatal(err)
	}
	err := wait.PollUntilContextTimeout(ctx, time.Second, rolloutTimeout, true, func(ctx context.Context) (bool, error) {
		if replicas == 0 {
			pods, err := h.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/instance=" + release + ",app.kubernetes.io/component=controller",
			})
			return err == nil && len(pods.Items) == 0, nil
		}
		d, err := h.cs.AppsV1().Deployments(namespace).Get(ctx, controllerDeployment, metav1.GetOptions{})
		if err != nil {
			return false, nil //nolint:nilerr // keep polling
		}
		done, _ := rolledOut(d)
		return done && d.Status.ReadyReplicas == int32(replicas), nil
	})
	if err != nil {
		t.Fatalf("scaling the controller to %d: %v", replicas, err)
	}
	t.Logf("controller scaled to %d", replicas)
}

// writeBundle writes b as the controller does (install.ProjectedSecret,
// then install.ConfigMap), over pbx-secrets and pbx-config.
func (h *harness) writeBundle(ctx context.Context, t *testing.T, b compile.Bundle, values map[compile.SecretRef][]byte) {
	t.Helper()
	sec, err := install.ProjectedSecret(names, namespace, b, values)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := h.cs.CoreV1().Secrets(namespace).Get(ctx, sec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cur.Data, cur.StringData, cur.Labels, cur.Annotations = sec.Data, nil, sec.Labels, sec.Annotations
	if _, err := h.cs.CoreV1().Secrets(namespace).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	cm, err := install.ConfigMap(names, namespace, b)
	if err != nil {
		t.Fatal(err)
	}
	curCM, err := h.cs.CoreV1().ConfigMaps(namespace).Get(ctx, cm.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	curCM.Data, curCM.BinaryData, curCM.Labels, curCM.Annotations = cm.Data, cm.BinaryData, cm.Labels, cm.Annotations
	if _, err := h.cs.CoreV1().ConfigMaps(namespace).Update(ctx, curCM, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// eventsSince lists the namespace's events.k8s.io Events with reason
// created at or after since, as "<regarding>: <note>".
func (h *harness) eventsSince(ctx context.Context, t *testing.T, since metav1.Time, reason string) []string {
	t.Helper()
	l, err := h.cs.EventsV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range l.Items {
		if e.Reason == reason && !e.CreationTimestamp.Before(&since) {
			out = append(out, e.Regarding.Kind+"/"+e.Regarding.Name+": "+e.Note)
		}
	}
	return out
}
