//go:build e2e

package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// The two phones of TestPhoneLifecycle, each on the e2e load. Their names
// are these prefixes and a random suffix per run: the agent keeps a
// device's fetch log by its ID (kind-name) after the device leaves the
// bundle, so a Phone recreated under a run's name on a reused cluster would
// start with that run's fetches, never Unseen (plan §11).
const (
	lifecyclePrefix = "lifecycle-"
	lifecycleMAC    = "0200E2E00011"
	lifecycleNumber = "111"
	resetPrefix     = "reset-"
	resetMAC        = "0200E2E00012"
	resetNumber     = "112"

	// What a 79x5 on SCCP firmware shows on its web page (the first load
	// the Desk phone is expected to run, design §2).
	sccpLoad  = "SCCP45.9-4-2SR4-3S"
	sccpImage = "jar45sccp.9-4-2SR4-3.sbn"

	// stateTimeout bounds a state the controller reaches within a poll or
	// two; slowStateTimeout one behind the driver's 3 min download window.
	stateTimeout     = 60 * time.Second
	slowStateTimeout = 5 * time.Minute
)

// TestPhoneLifecycle is §9's "Desk walks to Registered", through the real
// controller, agent and file server, with the e2e firmware load, in both
// of the design's paths (plan 6.2.9):
//
//   - lifecycle: a phone on SCCP firmware fetches its config and asks for
//     the target load, which is not staged: NeedsFirmware (P3). `patchbay
//     firmware load` refuses a zip whose only file is corrupted, then stages
//     the good one; the phone downloads it: Upgrading (P7), until the
//     download window has passed and it has fetched its config again on the
//     new load: Registering; then it registers: Registered, every condition
//     True.
//   - reset: the Desk phone's own path. It runs the target load but holds
//     its old CUCM's trust list and asks only for its signed config, which
//     does not exist. The agent scrapes only a phone it has served or that
//     registered, so its web page (whose log shows the trust error) is
//     never read: after 3 min of nothing but .sgn requests it is
//     NeedsTrustReset with P6 (the default load is Cisco's, whose files e2e
//     never has, so P5's "factory reset" can never be offered in e2e; the
//     driver's TestLifecycle pins P5). After the factory reset it takes its
//     config and downloads the staged load with its web page down:
//     Upgrading, and never NeedsTrustReset again; then it fetches its config
//     on the new load and registers: Registered with FirmwareCurrent Unknown
//     (no page has been read), no resync, no NOTIFY.
func TestPhoneLifecycle(t *testing.T) {
	t.Parallel()
	outer := t // phones and trackers outlive each assessment
	load := e2eLoad(t)
	var loadsFile, sbn string
	for _, f := range load.Files {
		switch {
		case strings.HasSuffix(f.Name, ".loads"):
			loadsFile = f.Name
		case strings.HasSuffix(f.Name, ".sbn"):
			sbn = f.Name
		}
	}
	type phones struct {
		lifecycle, reset       *tracker
		lifecyclePod, resetPod string
	}
	var p phones
	suffix, err := randomHex(2)
	if err != nil {
		t.Fatal(err)
	}
	lifecycleName, resetName := lifecyclePrefix+suffix, resetPrefix+suffix

	f := features.New("phone lifecycle").
		Setup(func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			h.deleteLeftovers(ctx, t, lifecyclePrefix, resetPrefix)
			h.wipeE2ELoad(ctx, t, load)
			return ctx
		}).
		Assess("reset: asks only for its signed config", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// First, because the controller waits 3 min on a phone that asks
			// only for its .sgn: the rest of the test runs meanwhile.
			h.recreate(ctx, t, &v1alpha1.Phone{
				ObjectMeta: metav1.ObjectMeta{Name: resetName, Namespace: namespace},
				Spec: v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: resetMAC, Number: resetNumber,
					Label: "Reset", Firmware: load.Name},
			})
			p.reset = h.track(ctx, outer, "Phone", resetName)
			// It asks for its .sgn once, and only a fetch the agent can
			// attribute to an applied device counts: wait for the bundle that
			// holds it, not just the one applied now.
			h.waitCompiled(ctx, t, drivers.DeviceID(drivers.KindPhone, resetName), time.Minute)
			h.waitApplied(ctx, t, time.Minute)
			p.resetPod = h.startPhone(ctx, t, outer, resetName, script.Script{
				Mode: script.ModeCisco, Server: h.nodeIP, MAC: resetMAC,
				// The agent never reads this page: it scrapes only a phone it
				// has served or that registered (the Desk phone, too).
				Web: &script.Web{VersionID: load.Name, ModelNumber: "CP-7945G",
					Log: []string{"10:02:18a 7: Trust List update failed"}},
				Steps: []script.Step{
					{Label: "signed-config", Fetch: &script.Fetch{File: "SEP{mac}.cnf.xml.sgn", Expect: script.NotFound}},
					{Label: "wait-reset", WaitSignal: &script.WaitSignal{Name: "reset"}},
					// The factory reset: its web page is down while it boots
					// and flashes; with no trust list it takes the unsigned
					// config, then the load the config names.
					{Label: "flashing", SetWeb: &script.Web{Down: true}},
					{Label: "default-loads", Fetch: &script.Fetch{File: "term45.default.loads", Expect: script.Any}},
					{Label: "config", FetchConfig: &script.FetchConfig{}},
					{Label: "loads", Fetch: &script.Fetch{File: "{load}.loads", Retry: retry(time.Second, time.Minute)}},
					{Label: "image", Fetch: &script.Fetch{File: sbn, Retry: retry(time.Second, time.Minute)}},
					// Flashing takes the real phone minutes; a config fetch in
					// the same second as the download would be the same moment
					// to the controller (whole seconds).
					{Label: "flash", Sleep: &script.Sleep{For: script.Duration(2 * time.Second)}},
					{Label: "config-on-new-load", FetchConfig: &script.FetchConfig{}},
					{Label: "register", Register: &script.Register{}},
				},
			})
			p.reset.wait(ctx, t, "seen (its .sgn request)", stateTimeout, func(d deviceView) (bool, string) {
				return d.State != "" && d.State != string(drivers.StateUnseen), ""
			})
			return ctx
		}).
		Assess("lifecycle: Unseen, then NeedsFirmware on SCCP firmware (P3, both files missing)", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			h.recreate(ctx, t, &v1alpha1.Phone{
				ObjectMeta: metav1.ObjectMeta{Name: lifecycleName, Namespace: namespace},
				Spec: v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: lifecycleMAC, Number: lifecycleNumber,
					Label: "Lifecycle", Firmware: load.Name},
			})
			p.lifecycle = h.track(ctx, outer, "Phone", lifecycleName)
			p.lifecycle.wait(ctx, t, "Unseen (P2)", stateTimeout, func(d deviceView) (bool, string) {
				return d.State == string(drivers.StateUnseen) &&
					d.is(v1alpha1.ConditionReady, metav1.ConditionFalse, cisco79x5.ReasonUnseen) &&
					strings.HasPrefix(d.HumanAction, "connect it to a VLAN 20 switch port"), ""
			})
			// Its SEP file must be served before the phone asks for it.
			h.waitApplied(ctx, t, time.Minute)
			p.lifecyclePod = h.startPhone(ctx, t, outer, lifecycleName, script.Script{
				Mode: script.ModeCisco, Server: h.nodeIP, MAC: lifecycleMAC,
				Web: &script.Web{VersionID: sccpLoad, AppLoadID: sccpImage, ModelNumber: "CP-7945G"},
				Steps: []script.Step{
					{Label: "config", FetchConfig: &script.FetchConfig{}},
					{Label: "loads", Fetch: &script.Fetch{File: "{load}.loads", Retry: retry(3*time.Second, 3*time.Minute)}},
					// `firmware load` stores the .loads a moment before the .sbn.
					{Label: "image", Fetch: &script.Fetch{File: sbn, Retry: retry(time.Second, 30*time.Second)}},
					{Label: "flashing", SetWeb: &script.Web{Down: true}},
					{Label: "boot", WaitSignal: &script.WaitSignal{Name: "boot"}},
					{Label: "booted", SetWeb: &script.Web{VersionID: load.Name, ModelNumber: "CP-7945G"}},
					{Label: "config-on-new-load", FetchConfig: &script.FetchConfig{}},
					{Label: "wait-register", WaitSignal: &script.WaitSignal{Name: "register"}},
					{Label: "register", Register: &script.Register{}},
				},
			})
			p.lifecycle.wait(ctx, t, "NeedsFirmware", stateTimeout, func(d deviceView) (bool, string) {
				return d.State == string(drivers.StateNeedsFirmware) &&
					d.is(v1alpha1.ConditionFirmwareAvailable, metav1.ConditionFalse, cisco79x5.ReasonNotStaged) &&
					d.is(v1alpha1.ConditionFirmwareCurrent, metav1.ConditionFalse, cisco79x5.ReasonOtherLoad) &&
					strings.HasPrefix(d.HumanAction, "run `patchbay firmware load <zip>`") &&
					strings.Contains(d.HumanAction, loadsFile) && strings.Contains(d.HumanAction, sbn), ""
			})
			return ctx
		}).
		Assess("firmware-load: a corrupted file is refused locally, the good zip is stored", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			dir := t.TempDir()
			bad := bytes.Clone(cisco79x5.E2ELoadFile(sbn))
			bad[len(bad)-1] ^= 0xFF
			// As Cisco ships them: a top-level folder, a readme beside the
			// firmware. The corrupted zip holds no good .loads: one would be
			// stored and start the download.
			badZip := writeZip(t, filepath.Join(dir, "cmterm-e2e-corrupted.zip"), map[string][]byte{
				"cmterm-e2e/readme.txt": []byte("patchbay e2e test load\n"),
				"cmterm-e2e/" + sbn:     bad,
			})
			out, err := h.patchbay(ctx, "firmware", "load", badZip)
			if err == nil {
				t.Fatalf("firmware load of a corrupted file exited 0:\n%s", out)
			}
			if !strings.Contains(err.Error(), sbn) || !strings.Contains(err.Error(), "not uploaded") {
				t.Errorf("the refusal does not name %s: %v", sbn, err)
			}
			obs := h.observations(ctx, t)
			if slices.Contains(obs.Firmware.Files, agentapi.FirmwareFileStatus{Load: load.Name, Name: sbn, Present: true}) {
				t.Errorf("the corrupted %s was stored", sbn)
			}
			p.lifecycle.wait(ctx, t, "still NeedsFirmware", stateTimeout, state(drivers.StateNeedsFirmware))

			goodZip := writeZip(t, filepath.Join(dir, "cmterm-e2e.zip"), map[string][]byte{
				"cmterm-e2e/readme.txt":   []byte("patchbay e2e test load\n"),
				"cmterm-e2e/" + loadsFile: cisco79x5.E2ELoadFile(loadsFile),
				"cmterm-e2e/" + sbn:       cisco79x5.E2ELoadFile(sbn),
			})
			out, err = h.patchbay(ctx, "firmware", "load", goodZip)
			if err != nil {
				t.Fatalf("firmware load of the e2e load: %v", err)
			}
			for _, f := range load.Files {
				line := load.Name + "/" + f.Name + ": "
				if !strings.Contains(out, line+"stored (201)") && !strings.Contains(out, line+"already stored (200)") {
					t.Errorf("firmware load did not store %s:\n%s", f.Name, out)
				}
			}
			p.lifecycle.wait(ctx, t, "FirmwareAvailable", stateTimeout, func(d deviceView) (bool, string) {
				return d.is(v1alpha1.ConditionFirmwareAvailable, metav1.ConditionTrue, cisco79x5.ReasonStaged), ""
			})
			return ctx
		}).
		Assess("lifecycle: downloads the load: Upgrading (P7)", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			d := p.lifecycle.wait(ctx, t, "Upgrading (P7)", stateTimeout, func(d deviceView) (bool, string) {
				return d.State == string(drivers.StateUpgrading) && d.HumanAction == "upgrading: do not unplug" &&
					d.UpgradeStartedAt != nil, ""
			})
			t.Logf("lifecycle upgrading since %s", d.UpgradeStartedAt.UTC().Format(time.RFC3339))
			// The phone is flashing until told it booted.
			h.waitPhone(ctx, t, p.lifecyclePod, "lifecycle downloaded its image", func(s script.State) (bool, string) {
				return s.Label == "boot", "at step " + s.Label
			})
			h.signal(ctx, t, p.lifecyclePod, "boot")
			return ctx
		}).
		Assess("reset: NeedsTrustReset after 3 min of .sgn requests, asking to stage firmware first (P6)", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			p.reset.wait(ctx, t, "NeedsTrustReset (P6)", slowStateTimeout, func(d deviceView) (bool, string) {
				return d.State == string(drivers.StateNeedsTrustReset) &&
					d.is(v1alpha1.ConditionReady, metav1.ConditionFalse, cisco79x5.ReasonNeedsTrustReset) &&
					strings.HasPrefix(d.HumanAction, "stage firmware first"), ""
			})
			h.signal(ctx, t, p.resetPod, "reset")
			p.reset.wait(ctx, t, "Upgrading", stateTimeout, state(drivers.StateUpgrading))
			return ctx
		}).
		Assess("lifecycle: Registering after the download window, then Registered", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			d := p.lifecycle.wait(ctx, t, "Registering", slowStateTimeout, func(d deviceView) (bool, string) {
				return d.State == string(drivers.StateRegistering) &&
					d.is(v1alpha1.ConditionProvisioned, metav1.ConditionTrue, cisco79x5.ReasonFetched), ""
			})
			if d.HumanAction != "" {
				t.Errorf("lifecycle Registering asks %q", d.HumanAction)
			}
			h.signal(ctx, t, p.lifecyclePod, "register")
			d = p.lifecycle.wait(ctx, t, "Registered, every condition True", slowStateTimeout, func(d deviceView) (bool, string) {
				ok := d.State == string(drivers.StateRegistered) && d.HumanAction == ""
				for _, c := range []string{
					v1alpha1.ConditionReady, v1alpha1.ConditionFirmwareAvailable, v1alpha1.ConditionFirmwareCurrent,
					v1alpha1.ConditionProvisioned, v1alpha1.ConditionRegistered, v1alpha1.ConditionConfigCurrent,
				} {
					ok = ok && d.is(c, metav1.ConditionTrue, "")
				}
				return ok, ""
			})
			if c := d.cond(v1alpha1.ConditionFirmwareCurrent); c.Reason != cisco79x5.ReasonRunning {
				t.Errorf("FirmwareCurrent %+v, want Running", c)
			}
			t.Logf("lifecycle went through %v", p.lifecycle.seen())
			return ctx
		}).
		Assess("reset: Registered without a resync, never NeedsTrustReset again", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			d := p.reset.wait(ctx, t, "Registered", slowStateTimeout, func(d deviceView) (bool, string) {
				return d.State == string(drivers.StateRegistered) &&
					d.is(v1alpha1.ConditionReady, metav1.ConditionTrue, "") &&
					d.is(v1alpha1.ConditionFirmwareCurrent, metav1.ConditionUnknown, cisco79x5.ReasonNotReported) &&
					d.is(v1alpha1.ConditionConfigCurrent, metav1.ConditionTrue, ""), ""
			})
			if d.LastAction != nil || d.HumanAction != "" {
				t.Errorf("reset: lastAction %+v, human action %q; want neither", d.LastAction, d.HumanAction)
			}
			seen := p.reset.seen()
			i := slices.Index(seen, string(drivers.StateNeedsTrustReset))
			if i < 0 || slices.Contains(seen[i+1:], string(drivers.StateNeedsTrustReset)) ||
				!slices.Contains(seen[i+1:], string(drivers.StateUpgrading)) {
				t.Errorf("reset went through %v: want NeedsTrustReset once, then Upgrading", seen)
			}
			t.Logf("reset went through %v", seen)
			st, err := h.phoneState(ctx, p.resetPod)
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Notifies) != 0 {
				t.Errorf("reset got NOTIFYs: %+v", st.Notifies)
			}
			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			ctx = context.WithoutCancel(ctx)
			for _, name := range []string{lifecycleName, resetName} {
				h.deleteAndWait(ctx, t, &v1alpha1.Phone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}})
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// retry is a Fetch's retry: every every, for at most within.
func retry(every, within time.Duration) script.Retry {
	return script.Retry{Every: script.Duration(every), Within: script.Duration(within)}
}

// writeZip writes a zip of files (name -> content, written in name order)
// to path and returns path.
func writeZip(t *testing.T, path string, files map[string][]byte) string {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		f, err := w.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(files[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
