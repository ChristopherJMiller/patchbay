//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	messagesapi "github.com/ChristopherJMiller/patchbay/internal/controller/messages/api"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/cisco79x5"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/softphone"
	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// The tones of TestDeskLoop's calls: each phone sends its own, so the far
// end can tell whose audio it hears.
const (
	deskTone    = 600
	laptopTone  = 800
	kitchenTone = 1000
	// minToneFrames is how many 20 ms packets of a tone prove two-way
	// audio (a second of it).
	minToneFrames = 50
)

// detectAll is what every TestDeskLoop phone counts in the calls it
// answers.
var detectAll = []float64{deskTone, laptopTone, kitchenTone}

// TestDeskLoop's phones (Server, and laptop's ProvisioningURL, are set when
// they start). Each does its later steps when signalled.
var (
	deskScript = script.Script{
		Mode: script.ModeCisco, MAC: deskMAC, ToneHz: deskTone,
		Answer: &script.Answer{After: script.Duration(time.Second), DetectHz: detectAll},
		Steps: []script.Step{
			{Label: "config", FetchConfig: &script.FetchConfig{}},
			{Label: "register", Register: &script.Register{}},
			{Label: "wait-call-150", WaitSignal: &script.WaitSignal{Name: "call-150"}},
			{Label: "call-150", Call: &script.Call{Number: laptopNumber, Hold: script.Duration(4 * time.Second),
				ExpectToneHz: laptopTone, MinToneFrames: minToneFrames}},
			{Label: "wait-transfer", WaitSignal: &script.WaitSignal{Name: "transfer"}},
			{Label: "answer-kitchen", WaitCall: &script.WaitCall{Within: script.Duration(time.Minute)}},
			// A word first: Asterisk bridges the call a moment after the 200, and
			// a REFER before the bridge is refused (a 400 in its NOTIFY).
			{Label: "talk", Sleep: &script.Sleep{For: script.Duration(2 * time.Second)}},
			{Label: "transfer", Transfer: &script.Transfer{To: laptopNumber}},
			{Label: "wait-call-600", WaitSignal: &script.WaitSignal{Name: "call-600"}},
			{Label: "call-600", Call: &script.Call{Number: echoNumber, Open: true}},
			{Label: "wait-hangup", WaitSignal: &script.WaitSignal{Name: "hangup"}},
			{Label: "hangup", Hangup: &script.Hangup{}},
		},
	}
	laptopScript = script.Script{
		Mode: script.ModeLinphone, ToneHz: laptopTone,
		Answer: &script.Answer{After: script.Duration(3 * time.Second), DetectHz: detectAll},
		Steps: []script.Step{
			{Label: "provision", FetchConfig: &script.FetchConfig{}},
			{Label: "register", Register: &script.Register{}},
			{Label: "wait-call-100", WaitSignal: &script.WaitSignal{Name: "call-100"}},
			{Label: "call-100", Call: &script.Call{Number: deskNumber, Hold: script.Duration(4 * time.Second),
				ExpectToneHz: deskTone, MinToneFrames: minToneFrames}},
		},
	}
)

// TestDeskLoop is the home loop of §9 on the site's desk (a 79x5 over TCP)
// and laptop (Linphone over UDP), each started once with its whole script:
// every later action is a step behind a named signal, and both answer
// calls throughout (desk after 1 s with 600 Hz, laptop after 3 s with
// 800 Hz). In order (plan 6.2.9):
//
//   - softphone: `patchbay softphone qr laptop --reissue` gives a URL;
//     Linphone fetches it once, downloads the contacts it names (desk and
//     laptop among them) and registers, the controller replaces the spent
//     token, and the same URL then answers 404 while laptop stays
//     Registered (Review Focus 4);
//   - provision: desk fetches its config over TFTP and registers;
//   - push: `patchbay phone test-push desk` is taken by desk after its
//     authenticationURL says AUTHORIZED, and desk fetches the page;
//   - calls: 100 calls 150 and 150 calls 100, tones both ways, each
//     callee seeing the caller's name;
//   - person: 301 rings both; desk answers first, laptop's INVITE is
//     cancelled;
//   - transfer: kitchen calls desk, desk blind-transfers it to 150, and
//     kitchen's audio reaches laptop;
//   - label: a new label reaches the phone's screen through one resync;
//   - messages: `patchbay phone message desk dinner` reaches desk's screen
//     with the chime, and the Message is Shown;
//   - messages from Home Assistant: a tools pod POSTs a message with a key
//     to the messages endpoint with the bearer token; it is created as
//     posted and Shown, with the chime; POST /v1/withdraw with its key
//     withdraws it, and the page goes again without it, silently;
//   - change-during-call: no resync while desk is on a call, one after it
//     hangs up (Review Focus 5); a message sent during the call is Held
//     InCall, nothing is pushed while it lasts, and after the BYE the
//     message goes out with the chime; then `patchbay phone resync desk`
//     restarts desk (it registers under a new Call-ID: its screen is
//     idle), and the controller pushes the same page once more, without
//     the chime;
//   - messages in quiet hours: with the house quiet (a tools pod PUTs it to
//     the messages endpoint with the bearer token) a message stays Held
//     QuietHours while an urgent one is pushed; withdrawing the urgent one
//     by its key pushes the page without it;
//   - secret-heals: deleting desk's Secret gives a new password, which desk
//     fetches after one resync and registers with; the old one gets 401.
func TestDeskLoop(t *testing.T) {
	t.Parallel()
	outer := t // the phones and trackers outlive each assessment
	var (
		desk, laptop       *tracker
		deskPod, laptopPod string
		deskIP             string
		dinner             string // the first Message's name, current through the run
	)
	f := features.New("desk loop").
		Setup(func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// A reused cluster may hold an earlier run's label.
			for _, obj := range siteResources() {
				if obj.GetName() == deskName || obj.GetName() == laptopName {
					if err := h.applyResource(ctx, obj); err != nil {
						t.Fatal(err)
					}
				}
			}
			// ... and an earlier run's Messages and house state: this run's
			// messages start from none, in a house nobody told anything.
			h.forgetMessages(ctx, t)
			desk = h.track(ctx, outer, "Phone", deskName)
			laptop = h.track(ctx, outer, "Softphone", laptopName)
			return ctx
		}).
		Assess("softphone: the QR code's URL provisions Linphone once", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			out, err := h.patchbay(ctx, "softphone", "qr", laptopName, "--reissue")
			if err != nil {
				t.Fatalf("patchbay softphone qr --reissue: %v", err)
			}
			url := ""
			for line := range strings.SplitSeq(out, "\n") {
				if strings.HasPrefix(line, "http://") && url == "" {
					url = strings.TrimSpace(line)
				}
			}
			if url == "" || !strings.Contains(out, "This code works once") {
				t.Fatal("softphone qr printed no URL and QR code")
			}
			issued := laptop.wait(ctx, t, "Issued with the printed URL (S1)", stateTimeout, func(d deviceView) (bool, string) {
				return d.State == string(drivers.StateIssued) && d.ProvisioningURL == url &&
					d.HumanAction == "scan `patchbay softphone qr "+laptopName+"` in Linphone", ""
			})
			if c := issued.cond(v1alpha1.ConditionReady); c.Reason != softphone.ReasonIssued {
				t.Errorf("laptop Ready %+v", c)
			}

			sc := laptopScript
			sc.Server, sc.ProvisioningURL = h.nodeIP, url
			laptopPod = h.startPhone(ctx, t, outer, laptopName, sc)
			// Spent: the token is rotated (a lastAction newer than the
			// reissue's), and no URL is published until the next reissue
			// (design §8.4: only Issued shows one).
			laptop.wait(ctx, t, "its spent token replaced (rotateToken), no URL published", stateTimeout, func(d deviceView) (bool, string) {
				return d.LastAction != nil && d.LastAction.Kind == v1alpha1.ActionRotateToken &&
					(issued.LastAction == nil || !d.LastAction.At.Equal(&issued.LastAction.At)) && d.ProvisioningURL == "", ""
			})
			laptop.wait(ctx, t, "Registered, no URL published", stateTimeout, func(d deviceView) (bool, string) {
				return d.State == string(drivers.StateRegistered) && d.is(v1alpha1.ConditionReady, metav1.ConditionTrue, softphone.ReasonRegistered) &&
					d.ProvisioningURL == "", ""
			})
			if seen := laptop.seen(); !containsInOrder(seen, string(drivers.StateIssued), string(drivers.StateProvisioned), string(drivers.StateRegistered)) {
				t.Errorf("laptop went through %v, want Issued, Provisioned, Registered", seen)
			}
			st := h.waitPhone(ctx, t, laptopPod, "Linphone registered", func(s script.State) (bool, string) {
				return s.Registered && s.Config != nil, "registered " + fmt.Sprint(s.Registered)
			})
			if !st.Config.ConfigURICleared {
				t.Error("the provisioning document does not clear misc/config-uri")
			}
			if st.Config.Transport != "udp" || st.Config.User != laptopNumber || st.Config.AuthUser != laptopID {
				t.Errorf("Linphone read %+v", *st.Config)
			}
			// Its contacts: the PBX's directory, downloaded from device
			// HTTP as Linphone does once configured, each card dialing its
			// number on the PBX.
			if want := fmt.Sprintf("http://%s:%d/directory/contacts.vcf", h.nodeIP, deviceHTTPPort); st.Config.ContactsURL != want || !st.Config.ContactsAtStartup {
				t.Errorf("contacts URL %q (at startup %t), want %q", st.Config.ContactsURL, st.Config.ContactsAtStartup, want)
			}
			for _, c := range []script.Contact{
				{Name: deskLabel, URI: "sip:" + deskNumber + "@" + h.nodeIP},
				{Name: laptopLabel, URI: "sip:" + laptopNumber + "@" + h.nodeIP},
			} {
				if !slices.Contains(st.Contacts, c) {
					t.Errorf("Linphone's contacts %+v lack %+v (fetches %+v)", st.Contacts, c, st.Fetches)
				}
			}

			// The same URL again, as Linphone fetches it at its next start:
			// 404, and the softphone stays registered.
			env := secretEnv("URL", h.holdSecret(ctx, t, "laptop-url", []byte(url)), "value")
			r := runTool(ctx, t, toolPod{name: "provision-again", env: append(kitchenEnv(), env),
				args: []string{"http", "get", "--print-body=false", "--expect-status", "404", "$(URL)"}}, true)
			if !r.Succeeded {
				t.Error("the spent URL did not answer 404")
			}
			laptop.wait(ctx, t, "still Registered", stateTimeout, state(drivers.StateRegistered))
			ip := h.podIP(ctx, t, laptopPod)
			h.eventually(ctx, t, "laptop's registration from its pod", 30*time.Second, func(o agentapi.Observations) (bool, string) {
				d := device(o, laptopID)
				return d != nil && d.Registration != nil && d.Registration.Address == ip, fmt.Sprintf("%+v", d)
			})
			return ctx
		}).
		Assess("provision: desk fetches its config and registers", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			sc := deskScript
			sc.Server = h.nodeIP
			deskPod = h.startPhone(ctx, t, outer, deskName, sc)
			deskIP = h.podIP(ctx, t, deskPod)
			desk.wait(ctx, t, "Registered from its pod, its config current", stateTimeout, func(d deviceView) (bool, string) {
				return d.State == string(drivers.StateRegistered) && d.Address == deskIP &&
					d.is(v1alpha1.ConditionConfigCurrent, metav1.ConditionTrue, cisco79x5.ReasonCurrent), ""
			})
			st := h.waitPhone(ctx, t, deskPod, "desk registered", func(s script.State) (bool, string) {
				return s.Registered && s.Config != nil, ""
			})
			if c := st.Config; c.PhoneLabel != deskLabel || c.User != deskNumber || c.AuthUser != deskID || c.Transport != "tcp" {
				t.Errorf("desk read %+v", *c)
			}
			return ctx
		}).
		Assess("push: test-push reaches desk's screen", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// The CLI talks to the agent, which pushes to desk's pod on port 80;
			// the fake asks the SEP file's authenticationURL (the agent's
			// /authenticate) before it takes the push, then fetches the page.
			out, err := h.patchbay(ctx, "phone", "test-push", deskName, "--text", "e2e", "--beep")
			if err != nil {
				t.Fatalf("test-push: %v\n%s", err, out)
			}
			if !strings.Contains(out, "status 0") {
				t.Errorf("test-push printed %q, want status 0", out)
			}
			pagePrefix := "/messages/" + deskID + ".xml"
			find := func(s script.State, match func(script.PushRecord) bool) bool {
				return slices.ContainsFunc(s.Pushes, match)
			}
			shown := func(p script.PushRecord) bool {
				return p.Authorized && p.Status == 0 && len(p.Items) == 2 && p.Items[0] == "Play:patchbay-chime.raw" &&
					strings.Contains(p.Items[1], pagePrefix) && strings.Contains(p.Fetched, "e2e")
			}
			h.waitPhone(ctx, t, deskPod, "desk took the push and fetched its page", func(s script.State) (bool, string) {
				return find(s, shown), fmt.Sprintf("%d pushes: %+v", len(s.Pushes), s.Pushes)
			})

			out, err = h.patchbay(ctx, "phone", "test-push", deskName, "--clear")
			if err != nil {
				t.Fatalf("test-push --clear: %v\n%s", err, out)
			}
			h.waitPhone(ctx, t, deskPod, "desk took the clear", func(s script.State) (bool, string) {
				return find(s, func(p script.PushRecord) bool {
					return p.Authorized && len(p.Items) == 1 && p.Items[0] == "Init:Services"
				}), fmt.Sprintf("%d pushes: %+v", len(s.Pushes), s.Pushes)
			})
			return ctx
		}).
		Assess("calls: 100 and 150 call each other, tones both ways, names shown", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			h.signal(ctx, t, deskPod, "call-150")
			deskSt := h.waitPhone(ctx, t, deskPod, "desk's call to 150 done", after(deskScript, "call-150"))
			out := lastCall(deskSt, "out", laptopNumber)
			if out == nil || !out.Answered || out.ToneFrames[script.HzKey(laptopTone)] < minToneFrames {
				t.Errorf("desk's call to 150: %+v, want answered with %d packets of %d Hz", out, minToneFrames, laptopTone)
			}
			lapSt, err := h.phoneState(ctx, laptopPod)
			if err != nil {
				t.Fatal(err)
			}
			in := lastCall(lapSt, "in", deskNumber)
			if in == nil || !in.Answered || in.ToneFrames[script.HzKey(deskTone)] < minToneFrames {
				t.Errorf("laptop's call from 100: %+v, want answered with %d packets of %d Hz", in, minToneFrames, deskTone)
			} else if !strings.Contains(in.FromDisplay, deskLabel) && !strings.Contains(in.PAssertedIdentity, deskLabel) {
				t.Errorf("laptop saw the caller as From %q, P-Asserted-Identity %q; want %q", in.FromDisplay, in.PAssertedIdentity, deskLabel)
			}

			h.signal(ctx, t, laptopPod, "call-100")
			lapSt = h.waitPhone(ctx, t, laptopPod, "laptop's call to 100 done", after(laptopScript, "call-100"))
			out = lastCall(lapSt, "out", deskNumber)
			if out == nil || !out.Answered || out.ToneFrames[script.HzKey(deskTone)] < minToneFrames {
				t.Errorf("laptop's call to 100: %+v, want answered with %d packets of %d Hz", out, minToneFrames, deskTone)
			}
			deskSt, err = h.phoneState(ctx, deskPod)
			if err != nil {
				t.Fatal(err)
			}
			in = lastCall(deskSt, "in", laptopNumber)
			if in == nil || !in.Answered || in.ToneFrames[script.HzKey(laptopTone)] < minToneFrames {
				t.Errorf("desk's call from 150: %+v, want answered with %d packets of %d Hz", in, minToneFrames, laptopTone)
			} else if !strings.Contains(in.RemotePartyID, laptopLabel) {
				t.Errorf("desk saw the caller as Remote-Party-ID %q; want %q", in.RemotePartyID, laptopLabel)
			}
			return ctx
		}).
		Assess("person: 301 rings desk and laptop, desk answers", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			deskBefore, lapBefore := h.callCount(ctx, t, deskPod), h.callCount(ctx, t, laptopPod)
			r := runTool(ctx, t, toolPod{name: "call-301", env: kitchenEnv(), args: []string{
				"call", personNumber, "--duration", "3s", "--expect-hz", fmt.Sprint(deskTone),
			}}, true)
			if !r.Succeeded {
				t.Errorf("kitchen's call to 301 did not hear desk's %d Hz", deskTone)
			}
			deskSt := h.waitPhone(ctx, t, deskPod, "desk's call from 101 ended", newEndedCall(deskBefore, kitchenNumber))
			lapSt := h.waitPhone(ctx, t, laptopPod, "laptop's call from 101 ended", newEndedCall(lapBefore, kitchenNumber))
			if c := lastCall(deskSt, "in", kitchenNumber); c == nil || !c.Answered {
				t.Errorf("desk's call from 101: %+v, want answered", c)
			}
			if c := lastCall(lapSt, "in", kitchenNumber); c == nil || c.Answered || c.Status != 487 {
				t.Errorf("laptop's call from 101: %+v, want cancelled (487)", c)
			}
			return ctx
		}).
		Assess("transfer: desk sends kitchen's call to 150", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			lapBefore := h.callCount(ctx, t, laptopPod)
			h.signal(ctx, t, deskPod, "transfer")
			h.waitPhone(ctx, t, deskPod, "desk waiting for a call", func(s script.State) (bool, string) {
				return s.Label == "answer-kitchen", "at step " + s.Label
			})
			kitchen := startTool(ctx, t, toolPod{name: "call-transfer", env: kitchenEnv(), args: []string{
				"call", deskNumber, "--duration", "12s", "--tone-hz", fmt.Sprint(kitchenTone), "--expect-tone=false",
			}})
			deskSt := h.waitPhone(ctx, t, deskPod, "desk transferred the call", after(deskScript, "transfer"))
			if c := lastCall(deskSt, "in", kitchenNumber); c == nil || c.TransferredTo != laptopNumber || c.TransferStatus != 202 {
				t.Errorf("desk's call from 101: %+v, want transferred to %s with 202", c, laptopNumber)
			}
			// The REFER's last NOTIFY carries the transfer's outcome (RFC 3515).
			var outcome string
			for _, n := range deskSt.Notifies {
				if n.Event == "refer" {
					outcome = n.Body
				}
			}
			if !strings.HasPrefix(outcome, "SIP/2.0 200") {
				t.Errorf("the transfer ended with %q, want SIP/2.0 200", outcome)
			}
			if r := waitTool(ctx, t, kitchen, true); !r.Succeeded {
				t.Error("kitchen's transferred call failed")
			}
			lapSt := h.waitPhone(ctx, t, laptopPod, "laptop's transferred call ended", func(s script.State) (bool, string) {
				calls := s.Calls[min(lapBefore, len(s.Calls)):]
				for _, c := range calls {
					if c.Direction == "in" && c.Answered && !c.End.IsZero() {
						return true, ""
					}
				}
				return false, fmt.Sprintf("calls since: %+v", calls)
			})
			var got *script.CallRecord
			for i := range lapSt.Calls[lapBefore:] {
				if c := &lapSt.Calls[lapBefore+i]; c.Direction == "in" && c.Answered {
					got = c
				}
			}
			if got == nil || got.ToneFrames[script.HzKey(kitchenTone)] < minToneFrames {
				t.Errorf("laptop's transferred call: %+v, want %d packets of kitchen's %d Hz", got, minToneFrames, kitchenTone)
			} else if got.Number != kitchenNumber {
				t.Logf("laptop saw the transferred call from %q", got.Number)
			}
			return ctx
		}).
		Assess("label: a new label reaches the screen through one resync", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			before := h.notifyCount(ctx, t, deskPod)
			v := h.relabel(ctx, t, "Desk Two")
			h.waitPhone(ctx, t, deskPod, "desk refetched its config", func(s script.State) (bool, string) {
				return s.Config != nil && s.Config.PhoneLabel == "DeskTwo", fmt.Sprintf("notifies %d, label %q", len(s.Notifies), labelOf(s))
			})
			desk.wait(ctx, t, "Registered on the new version", stateTimeout, registeredOn(v))
			st, err := h.phoneState(ctx, deskPod)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(st.Notifies) - before; n != 1 {
				t.Errorf("desk got %d NOTIFYs for the label change, want 1: %+v", n, st.Notifies[before:])
			} else if nt := st.Notifies[before]; nt.Event != "service-control" || !nt.Refetched || !strings.Contains(nt.Body, "action=restart") {
				t.Errorf("the NOTIFY: %+v", nt)
			}
			return ctx
		}).
		Assess("messages: a message reaches desk's screen, with the chime", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// desk is free (the label's resync went out, so the controller
			// saw no call) and the house was never told anything: the
			// controller pushes desk a page holding the message, the chime
			// first, which desk takes after its authenticationURL says
			// AUTHORIZED, and fetches.
			before := h.pushCount(ctx, t, deskPod)
			dinner = h.message(ctx, t, "dinner")
			h.waitPhone(ctx, t, deskPod, "desk took a push of a page holding dinner, the chime first",
				pushedSince(before, messagePage(true, []string{"dinner"}, nil)))
			h.waitMessage(ctx, t, dinner, "Shown", isShown)
			return ctx
		}).
		Assess("messages from Home Assistant: POSTed to the endpoint, withdrawn by key", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// Home Assistant's main path, through the API server, not the
			// CLI: a tools pod POSTs to the messages Service with the bearer
			// token. The house was never told anything, so it goes out.
			before := h.pushCount(ctx, t, deskPod)
			posted := h.postMessage(ctx, t, "ha-message", messagesapi.MessageRequest{
				To: []string{deskName}, Title: "HA", Text: "from home assistant", Key: "ha-test",
			})
			h.waitPhone(ctx, t, deskPod, "desk took a push of a page holding the posted message, the chime first",
				pushedSince(before, messagePage(true, []string{"from home assistant", "dinner"}, nil)))
			h.waitMessage(ctx, t, posted, "Shown", isShown)

			before = h.pushCount(ctx, t, deskPod)
			var withdrew messagesapi.WithdrawResponse
			sendEndpoint(ctx, t, "ha-withdraw", "POST", messagesapi.PathWithdraw,
				messagesapi.WithdrawRequest{Key: "ha-test", To: []string{deskName}}, 200, &withdrew)
			if withdrew.Withdrawn != 1 {
				t.Errorf("POST %s withdrew %d, want 1", messagesapi.PathWithdraw, withdrew.Withdrawn)
			}
			h.waitPhone(ctx, t, deskPod, "a page without the withdrawn message, no chime",
				pushedSince(before, messagePage(false, []string{"dinner"}, []string{"from home assistant"})))
			h.waitMessage(ctx, t, posted, "Withdrawn", func(st v1alpha1.MessageStatus) bool {
				return st.State == v1alpha1.MessageStateWithdrawn && st.FinishedAt != nil
			})
			return ctx
		}).
		Assess("change-during-call: no resync during a call, one after it", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			h.signal(ctx, t, deskPod, "call-600")
			h.waitPhone(ctx, t, deskPod, "desk on a call to 600", after(deskScript, "call-600"))
			h.eventually(ctx, t, "the agent counts desk's call", 30*time.Second, func(o agentapi.Observations) (bool, string) {
				d := device(o, deskID)
				return d != nil && d.Calls > 0, fmt.Sprintf("%+v", d)
			})
			before := h.notifyCount(ctx, t, deskPod)
			v := h.relabel(ctx, t, "Desk Three")
			desk.wait(ctx, t, "ConfigStale, waiting for the call to end", stateTimeout, func(d deviceView) (bool, string) {
				c := d.cond(v1alpha1.ConditionConfigCurrent)
				return d.State == string(drivers.StateConfigStale) && c.Status == metav1.ConditionFalse &&
					c.Reason == cisco79x5.ReasonStale && strings.Contains(c.Message, "waiting for the call to end"), ""
			})
			// messages: a message sent during the call waits for its end.
			// ConfigStale "waiting for the call to end" is the controller's
			// own snapshot counting the call (the Message reconciler reads
			// the same one), so the Message is decided in the call, not in
			// the poll before it.
			pushesBefore := h.pushCount(ctx, t, deskPod)
			callBack := h.message(ctx, t, "call back")
			h.waitMessage(ctx, t, callBack, "Held InCall", isHeld(v1alpha1.ReasonInCall))
			// The rule under test is "nothing during the call": watch for a
			// while, as a person would stay on the line.
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(20 * time.Second):
			}
			if n := h.notifyCount(ctx, t, deskPod) - before; n != 0 {
				t.Fatalf("desk got %d NOTIFYs during its call", n)
			}
			if d := desk.wait(ctx, t, "still waiting", stateTimeout, state(drivers.StateConfigStale)); d.LastAction != nil &&
				d.LastAction.Kind == v1alpha1.ActionResync && d.LastAction.Version == v {
				t.Errorf("a resync of %.12s was recorded during the call: %+v", v, d.LastAction)
			}
			if n := h.pushCount(ctx, t, deskPod) - pushesBefore; n != 0 {
				t.Errorf("desk got %d pushes during its call", n)
			}
			if st := h.messageStatus(ctx, t, callBack); !isHeld(v1alpha1.ReasonInCall)(st) {
				t.Errorf("Message %s is %s %s during the call, want Held InCall", callBack, st.State, st.Reason)
			}

			h.signal(ctx, t, deskPod, "hangup")
			h.waitPhone(ctx, t, deskPod, "one NOTIFY after the BYE, the new label", func(s script.State) (bool, string) {
				return len(s.Notifies)-before == 1 && labelOf(s) == "DeskThree", fmt.Sprintf("notifies %d, label %q", len(s.Notifies)-before, labelOf(s))
			})
			desk.wait(ctx, t, "Registered on the new version", stateTimeout, registeredOn(v))
			if n := h.notifyCount(ctx, t, deskPod) - before; n != 1 {
				t.Errorf("desk got %d NOTIFYs after its call, want 1", n)
			}
			// After the BYE the held message goes out, newest first above
			// dinner (still current), with the chime: it is new.
			h.waitPhone(ctx, t, deskPod, "the held message pushed with the chime after the BYE",
				pushedSince(pushesBefore, messagePage(true, []string{"call back", "dinner"}, nil)))
			h.waitMessage(ctx, t, callBack, "Shown", isShown)

			// That resync restarted desk (action=restart reboots a 79x5,
			// which comes back on its idle screen, under a new Call-ID). It
			// raced the chime push: whichever came first, desk ends on the
			// page. One more resync, once desk holds the page, pins the rule:
			// the same page again, once, without the chime.
			h.resyncResendsPage(ctx, t, deskPod, []string{"call back", "dinner"}, callBack, dinner)
			return ctx
		}).
		Assess("messages in quiet hours: held unless urgent; withdrawn by key", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// Home Assistant's side: the house is quiet, PUT to the
			// messages Service with the bearer token.
			putHouse(ctx, t, "house-quiet", messagesapi.HouseUpdate{Quiet: ptr.To(true)})
			laundry := h.message(ctx, t, "laundry done")
			h.waitMessage(ctx, t, laundry, "Held QuietHours", isHeld(v1alpha1.ReasonQuietHours))
			before := h.pushCount(ctx, t, deskPod)
			leak := h.message(ctx, t, "leak", "--urgent", "--key", "k")
			h.waitPhone(ctx, t, deskPod, "the urgent message pushed in quiet hours with the chime, the held one not on its page",
				pushedSince(before, messagePage(true, []string{"leak", "call back", "dinner"}, []string{"laundry done"})))
			h.waitMessage(ctx, t, leak, "Shown", isShown)
			// Decided with the urgent one, and still waiting.
			if st := h.messageStatus(ctx, t, laundry); !isHeld(v1alpha1.ReasonQuietHours)(st) {
				t.Errorf("Message %s is %s %s once the urgent one is shown, want Held QuietHours", laundry, st.State, st.Reason)
			}

			// Withdrawn by its key: the page again without it, and no chime
			// (nothing on it is new). call back and dinner are still
			// current, so it is a page, not a clear.
			before = h.pushCount(ctx, t, deskPod)
			out, err := h.patchbay(ctx, "phone", "withdraw", deskName, "--key", "k")
			if err != nil || !strings.Contains(out, "withdrew 1 message with key k") {
				t.Fatalf("phone withdraw --key k: %v\n%s", err, out)
			}
			h.waitPhone(ctx, t, deskPod, "a page without the withdrawn message, no chime",
				pushedSince(before, messagePage(false, []string{"call back", "dinner"}, []string{"leak", "laundry done"})))
			h.waitMessage(ctx, t, leak, "Withdrawn", func(st v1alpha1.MessageStatus) bool {
				return st.State == v1alpha1.MessageStateWithdrawn && st.FinishedAt != nil
			})
			return ctx
		}).
		Assess("secret-heals: a deleted Secret becomes a new password, fetched and registered with", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			name := compile.DeviceSecretName(deskID)
			old, err := h.cs.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			oldPassword := old.Data[compile.SecretKeySIPPassword]
			oldVersion := h.compiled(ctx, t).Devices[deskID].Version
			before := h.notifyCount(ctx, t, deskPod)
			if err := h.cs.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			var newPassword []byte
			err = wait.PollUntilContextTimeout(ctx, time.Second, stateTimeout, true, func(ctx context.Context) (bool, error) {
				s, err := h.cs.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return false, nil //nolint:nilerr // not recreated yet
				}
				newPassword = s.Data[compile.SecretKeySIPPassword]
				return s.UID != old.UID && len(newPassword) > 0, nil
			})
			if err != nil {
				t.Fatalf("Secret %s was not recreated: %v", name, err)
			}
			if string(newPassword) == string(oldPassword) {
				t.Fatal("the recreated Secret has the old password")
			}
			var v string
			err = wait.PollUntilContextTimeout(ctx, time.Second, stateTimeout, true, func(ctx context.Context) (bool, error) {
				b, err := h.readCompiled(ctx)
				if err != nil {
					return false, nil //nolint:nilerr // between writes
				}
				v = b.Devices[deskID].Version
				return v != oldVersion, nil
			})
			if err != nil {
				t.Fatalf("no new version for desk: %v", err)
			}
			sum := sha256.Sum256(newPassword)
			st := h.waitPhone(ctx, t, deskPod, "desk registered with the new password", func(s script.State) (bool, string) {
				n := len(s.Registrations)
				return s.Config != nil && s.Config.PasswordSHA256 == hex.EncodeToString(sum[:]) && n > 0 &&
					s.Registrations[n-1].Status == 200 && s.Registrations[n-1].PasswordSHA256 == hex.EncodeToString(sum[:]), ""
			})
			if n := len(st.Notifies) - before; n != 1 {
				t.Errorf("desk got %d NOTIFYs for its new password, want 1", n)
			}
			desk.wait(ctx, t, "Registered on the new version", stateTimeout, registeredOn(v))

			env := append(deviceEnvWithout(deskID, deskNumber), secretEnv("FAKEPHONE_PASSWORD", h.holdSecret(ctx, t, "old-password", oldPassword), "value"))
			r := runTool(ctx, t, toolPod{name: "register-old-password", env: env, args: []string{"register", "--expires", "300"}}, false)
			if r.Succeeded || !strings.Contains(r.Logs, "REGISTER: 401") {
				t.Errorf("the old password was not refused with 401")
			}
			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			ctx = context.WithoutCancel(ctx)
			for _, obj := range siteResources() {
				if obj.GetName() == deskName {
					if err := h.applyResource(ctx, obj); err != nil {
						t.Errorf("putting desk's label back: %v", err)
					}
				}
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// relabel sets desk's label and returns desk's next compiled version.
func (h *harness) relabel(ctx context.Context, t *testing.T, label string) string {
	t.Helper()
	old := h.compiled(ctx, t).Devices[deskID].Version
	var p v1alpha1.Phone
	if err := h.kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deskName}, &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Label = label
	if err := h.kube.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	var v string
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, stateTimeout, true, func(ctx context.Context) (bool, error) {
		b, err := h.readCompiled(ctx)
		if err != nil {
			return false, nil //nolint:nilerr // between writes
		}
		v = b.Devices[deskID].Version
		return v != "" && v != old, nil
	})
	if err != nil {
		t.Fatalf("desk's label %q compiled into no new version: %v", label, err)
	}
	t.Logf("desk labelled %q: version %.12s", label, v)
	return v
}

// registeredOn is a wait condition: Registered, having fetched version v.
func registeredOn(v string) func(deviceView) (bool, string) {
	return func(d deviceView) (bool, string) {
		return d.State == string(drivers.StateRegistered) && d.LastConfigFetch != nil && d.LastConfigFetch.Version == v &&
			d.is(v1alpha1.ConditionConfigCurrent, metav1.ConditionTrue, ""), ""
	}
}

// after is a phone wait condition: sc's step labelled label is done (the
// phone is at a later step, or finished).
func after(sc script.Script, label string) func(script.State) (bool, string) {
	i := slices.IndexFunc(sc.Steps, func(s script.Step) bool { return s.Label == label })
	if i < 0 {
		panic("no step " + label)
	}
	return func(s script.State) (bool, string) {
		return s.Step > i, fmt.Sprintf("at step %d (%s)", s.Step, s.Label)
	}
}

// newEndedCall is a phone wait condition: a call from number that started
// after the first n calls has ended.
func newEndedCall(n int, number string) func(script.State) (bool, string) {
	return func(s script.State) (bool, string) {
		for _, c := range s.Calls[min(n, len(s.Calls)):] {
			if c.Direction == "in" && c.Number == number && !c.End.IsZero() {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d calls", len(s.Calls))
	}
}

// lastCall is the phone's latest call in direction with number.
func lastCall(s script.State, direction, number string) *script.CallRecord {
	for i := len(s.Calls) - 1; i >= 0; i-- {
		if c := &s.Calls[i]; c.Direction == direction && c.Number == number {
			return c
		}
	}
	return nil
}

func labelOf(s script.State) string {
	if s.Config == nil {
		return ""
	}
	return s.Config.PhoneLabel
}

func (h *harness) callCount(ctx context.Context, t *testing.T, pod string) int {
	t.Helper()
	s, err := h.phoneState(ctx, pod)
	if err != nil {
		t.Fatal(err)
	}
	return len(s.Calls)
}

func (h *harness) notifyCount(ctx context.Context, t *testing.T, pod string) int {
	t.Helper()
	s, err := h.phoneState(ctx, pod)
	if err != nil {
		t.Fatal(err)
	}
	return len(s.Notifies)
}

// podIP is a running pod's IP.
func (h *harness) podIP(ctx context.Context, t *testing.T, pod string) string {
	t.Helper()
	p, err := h.cs.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil || p.Status.PodIP == "" {
		t.Fatalf("pod %s has no IP: %v", pod, err)
	}
	return p.Status.PodIP
}

// containsInOrder reports whether want appear in seen in that order (not
// necessarily next to each other).
func containsInOrder(seen []string, want ...string) bool {
	i := 0
	for _, s := range seen {
		if i < len(want) && s == want[i] {
			i++
		}
	}
	return i == len(want)
}
