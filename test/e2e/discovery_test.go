//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

const (
	discoveredName   = "discovered"
	discoveredNumber = "113"
)

// TestDiscovery is §9's second phone: a phone with no Phone resource that
// asks for its config becomes an UnknownPhone Event on pbx-config, naming
// its MAC and address, and a line of `patchbay status`; once a Phone names
// its MAC, it is provisioned and reaches Registered. The MAC is random, so
// a reused cluster's agent has never reported it.
func TestDiscovery(t *testing.T) {
	t.Parallel()
	outer := t
	suffix, err := randomHex(2)
	if err != nil {
		t.Fatal(err)
	}
	mac := "0200E2E1" + strings.ToUpper(suffix)
	start := metav1.Now()
	f := features.New("discovery").
		Assess("an unknown phone is an Event on pbx-config and a line of status", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// The Phone of an earlier run with this MAC (an unlikely draw)
			// would make it known.
			h.deleteAndWait(ctx, t, &v1alpha1.Phone{ObjectMeta: metav1.ObjectMeta{Name: discoveredName, Namespace: namespace}})
			r := runTool(ctx, t, toolPod{name: "tftp-discovery", env: kitchenEnv(),
				args: []string{"tftp", "get", "-o", "/dev/null", "SEP" + mac + ".cnf.xml"}}, false)
			if r.Succeeded {
				t.Fatal("an unknown phone was served a config")
			}
			ip := r.Pod.Status.PodIP
			want := names.ConfigMap + ": " + install.UnknownPhoneMessage(mac, ip)
			var evs []string
			deadline := time.Now().Add(30 * time.Second)
			for !slices.Contains(evs, "ConfigMap/"+want) {
				if time.Now().After(deadline) {
					t.Fatalf("no UnknownPhone Event %q within 30 s; got %v", want, evs)
				}
				time.Sleep(time.Second)
				evs = h.eventsSince(ctx, t, start, install.EventReasonUnknownPhone)
			}
			out, err := h.patchbay(ctx, "status")
			if err != nil {
				t.Fatalf("patchbay status: %v", err)
			}
			if !strings.Contains(out, "\n  "+mac+" from "+ip+", first seen ") {
				t.Errorf("patchbay status does not list %s from %s:\n%s", mac, ip, out)
			}
			for _, line := range []string{
				"Phone " + deskName + " (" + deskNumber + "): ",
				"Softphone " + laptopName + " (" + laptopNumber + "): ",
				"Person " + personName + " (" + personNumber + "): ",
			} {
				if !strings.Contains(out, line) {
					t.Errorf("patchbay status has no line %q", line)
				}
			}
			return ctx
		}).
		Assess("a Phone for it: provisioned and Registered", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			if err := h.kube.Create(ctx, &v1alpha1.Phone{
				ObjectMeta: metav1.ObjectMeta{Name: discoveredName, Namespace: namespace},
				Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: v1alpha1.MAC(mac), Number: discoveredNumber, Label: "Discovered"},
			}); err != nil {
				t.Fatal(err)
			}
			tr := h.track(ctx, outer, "Phone", discoveredName)
			// The Phone must be compiled, and that bundle applied, before the
			// phone asks again: waitApplied alone could see the previous
			// bundle, applied long ago.
			h.waitCompiled(ctx, t, drivers.DeviceID(drivers.KindPhone, discoveredName), time.Minute)
			h.waitApplied(ctx, t, time.Minute)
			if _, ok := h.compiled(ctx, t).Devices[drivers.DeviceID(drivers.KindPhone, discoveredName)]; !ok {
				t.Fatalf("Phone %s is not compiled", discoveredName)
			}
			h.startPhone(ctx, t, outer, discoveredName, script.Script{
				Mode: script.ModeCisco, Server: h.nodeIP, MAC: mac,
				Steps: []script.Step{
					{Label: "config", FetchConfig: &script.FetchConfig{}},
					{Label: "register", Register: &script.Register{}},
				},
			})
			tr.wait(ctx, t, "Registered (FirmwareCurrent Unknown)", stateTimeout, func(d deviceView) (bool, string) {
				return d.State == string(drivers.StateRegistered) &&
					d.is(v1alpha1.ConditionFirmwareCurrent, metav1.ConditionUnknown, cisco79x5.ReasonNotReported) &&
					d.is(v1alpha1.ConditionConfigCurrent, metav1.ConditionTrue, ""), fmt.Sprint(tr.seen())
			})
			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			h.deleteAndWait(context.WithoutCancel(ctx), t, &v1alpha1.Phone{ObjectMeta: metav1.ObjectMeta{Name: discoveredName, Namespace: namespace}})
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}
