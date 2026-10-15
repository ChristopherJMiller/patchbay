package devices_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/devices"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

const annReissue = v1alpha1.AnnotationReissueRequested

// laptopToken is laptop's provisioning token in its Secret.
func (f *fixture) laptopToken() string {
	f.t.Helper()
	return string(f.secretOf(laptopID).Data[compile.SecretKeyProvisioningToken])
}

func (f *fixture) url(token string) string {
	return "http://" + pbxAddr + ":8088/p/" + token
}

// issuedLaptop creates Softphone laptop (150), compiles it and decides it
// once: Issued.
func issuedLaptop(t *testing.T, f *fixture) {
	t.Helper()
	f.newSoftphone("laptop", "150", "Laptop")
	f.compile()
	f.see()
	f.reconcileSoftphone("laptop")
	if st := f.softphone("laptop").Status.State; st != string(drivers.StateIssued) {
		t.Fatalf("laptop is %s; want Issued", st)
	}
	noLeak(t, f, f.rec.take(), f.laptopToken())
}

// noLeak fails if a token or a provisioning URL is in the logs or Events.
func noLeak(t *testing.T, f *fixture, events []recorded, tokens ...string) {
	t.Helper()
	text := f.logs.text()
	for _, e := range events {
		text += "\n" + e.String()
	}
	for _, tok := range tokens {
		if tok != "" && strings.Contains(text, tok) {
			t.Errorf("a provisioning token is in the logs or Events:\n%s", text)
		}
	}
	if strings.Contains(text, agentapi.ProvisioningPathPrefix) {
		t.Errorf("a provisioning URL is in the logs or Events:\n%s", text)
	}
}

// TestSoftphoneIssuedURL: status.provisioningURL carries the token the
// compiled bundle grants. A pbx-secrets written ahead of its pbx-config
// (core writes it first) keeps the old URL; the ConfigMap following it
// shows the new one.
func TestSoftphoneIssuedURL(t *testing.T) {
	f := newFixture(t)
	issuedLaptop(t, f)
	old := f.laptopToken()
	st := f.softphone("laptop").Status
	if st.ProvisioningURL != f.url(old) {
		t.Errorf("provisioningURL is not the compiled token's")
	}
	if st.HumanAction != "scan `patchbay softphone qr laptop` in Linphone" {
		t.Errorf("human action %q", st.HumanAction)
	}

	s := f.secretOf(laptopID)
	s.Data[compile.SecretKeyProvisioningToken] = []byte("notasecrettokenrotatedbyhand000")
	if err := env.Client.Update(f.ctx, s); err != nil {
		t.Fatal(err)
	}
	b := f.compileOnly()
	f.writeSecrets(b)
	f.reconcileSoftphone("laptop")
	if got := f.softphone("laptop").Status.ProvisioningURL; got != f.url(old) {
		t.Error("pbx-secrets ahead of pbx-config: the URL changed; want the compiled one kept")
	}
	f.writeConfigMap(b)
	f.reconcileSoftphone("laptop")
	if got := f.softphone("laptop").Status.ProvisioningURL; got != f.url("notasecrettokenrotatedbyhand000") {
		t.Error("pbx-config written: the URL is not the new token's")
	}
	noLeak(t, f, f.rec.take(), old, "notasecrettokenrotatedbyhand000")
}

// TestSoftphoneRotatesToken: a served fetch of the provisioning document
// spends the token: the controller writes a new one (32 alphanumerics) into
// the Secret and remembers the rotation; a Provisioned softphone shows no
// URL, before or after the new token is compiled. Neither token reaches a
// log or an Event.
func TestSoftphoneRotatesToken(t *testing.T) {
	f := newFixture(t)
	issuedLaptop(t, f)
	old := f.laptopToken()
	ring := agentapi.DeviceObservations{Device: laptopID,
		Fetches: []agentapi.Fetch{provisioningFetch(f.version(laptopID), t0.Add(20*time.Second))}}
	f.at(30*time.Second + 400*time.Millisecond)
	f.see(ring)
	f.reconcileSoftphone("laptop")

	fresh := f.laptopToken()
	if fresh == old || !regexp.MustCompile(`^[A-Za-z0-9]{32}$`).MatchString(fresh) {
		t.Errorf("the token was not replaced by 32 alphanumerics (len %d)", len(fresh))
	}
	st := f.softphone("laptop").Status
	at := metav1.NewTime(t0.Add(31 * time.Second))
	if la := st.LastAction; la == nil || la.Kind != v1alpha1.ActionRotateToken || !la.At.Equal(&at) || la.Attempts != 1 {
		t.Errorf("lastAction %+v; want rotateToken at %v", la, at)
	}
	if st.State != string(drivers.StateProvisioned) || st.ProvisioningURL != "" {
		t.Errorf("state %s, URL set %v; want Provisioned, no URL (design §8.4: only Issued shows one)",
			st.State, st.ProvisioningURL != "")
	}
	events := f.rec.take()
	wantEvents(t, events,
		laptopEvent(corev1.EventTypeNormal, "ActionSent", "RotateToken", "provisioning token rotated"),
		laptopEvent(corev1.EventTypeNormal, "StateChanged", "Decide",
			"Issued -> Provisioned: Linphone fetched its settings, which spends its one-time URL; minting a new one"))

	f.compile()
	f.at(35 * time.Second)
	f.see(ring)
	f.reconcileSoftphone("laptop")
	if got := f.softphone("laptop").Status.ProvisioningURL; got != "" {
		t.Error("compiled, Provisioned: the new token's URL is published; it stays unpublished until a reissue")
	}
	if f.laptopToken() != fresh {
		t.Error("the token was rotated again for the same fetch")
	}
	noLeak(t, f, append(events, f.rec.take()...), old, fresh)
}

// TestSoftphoneURLOnlyWhileIssued: status.provisioningURL is a live,
// credential-bearing grant, so it is published only while the softphone
// is Issued (design §8.4) and only for the token the device Secret still
// holds: never in Provisioned, Registered or Offline (a reader of
// Softphones who cannot read Secrets must not get the SIP password), and
// never for a token a reissue is replacing, even before the bundle
// reconciler has compiled the new one (`qr --reissue` would print it).
func TestSoftphoneURLOnlyWhileIssued(t *testing.T) {
	f := newFixture(t)
	issuedLaptop(t, f)
	spend := agentapi.DeviceObservations{Device: laptopID,
		Fetches:      []agentapi.Fetch{provisioningFetch(f.version(laptopID), t0.Add(20*time.Second))},
		Registration: contact("150", laptopIP, t0.Add(time.Hour))}
	f.at(30 * time.Second)
	f.see(spend)
	f.reconcileSoftphone("laptop") // spent: rotated, Provisioned
	f.compile()
	f.at(40 * time.Second)
	f.see(spend)
	f.reconcileSoftphone("laptop")
	st := f.softphone("laptop").Status
	if st.State != string(drivers.StateRegistered) || st.ProvisioningURL != "" {
		t.Fatalf("state %s, URL set %v; want Registered with no URL", st.State, st.ProvisioningURL != "")
	}
	asleep := spend
	asleep.Registration = nil
	f.at(10 * time.Minute)
	f.see(asleep)
	f.reconcileSoftphone("laptop")
	if st := f.softphone("laptop").Status; st.State != string(drivers.StateOffline) || st.ProvisioningURL != "" {
		t.Errorf("state %s, URL set %v; want Offline with no URL", st.State, st.ProvisioningURL != "")
	}
	compiled := f.laptopToken()

	// A reissue: the reconcile that rotates shows no URL (the compiled
	// token is being replaced), nor does one before the bundle reconciler
	// has compiled the new token: the device Secret no longer holds the
	// compiled one.
	edit(f, f.softphone("laptop"), func(s *v1alpha1.Softphone) {
		s.Annotations = map[string]string{annReissue: "2026-10-15T12:15:00Z"}
	})
	f.at(15 * time.Minute)
	f.see(asleep)
	f.reconcileSoftphone("laptop")
	st = f.softphone("laptop").Status
	if st.State != string(drivers.StateIssued) || st.HandledRequest != "2026-10-15T12:15:00Z" || st.ProvisioningURL != "" {
		t.Errorf("rotating: state %s, handled %q, URL set %v; want Issued, handled, no URL",
			st.State, st.HandledRequest, st.ProvisioningURL != "")
	}
	fresh := f.laptopToken()
	if fresh == compiled {
		t.Fatal("the reissue did not rotate the token")
	}
	f.at(15*time.Minute + 5*time.Second)
	f.see(asleep)
	f.reconcileSoftphone("laptop")
	if got := f.softphone("laptop").Status.ProvisioningURL; got != "" {
		t.Errorf("Issued, the reissued token not compiled yet: a URL is published (the replaced token's: %v)", got == f.url(compiled))
	}
	f.compile()
	f.at(15*time.Minute + 10*time.Second)
	f.see(asleep)
	f.reconcileSoftphone("laptop")
	if st := f.softphone("laptop").Status; st.State != string(drivers.StateIssued) || st.ProvisioningURL != f.url(fresh) {
		t.Errorf("compiled: state %s, URL is the new token's %v; want Issued with it", st.State, st.ProvisioningURL == f.url(fresh))
	}
	noLeak(t, f, f.rec.take(), compiled, fresh)
}

// TestSoftphoneRotateFails: a missing Secret, or one that cannot be
// written, is a failed action (6.2.1): nothing remembered, ActionFailed,
// 30 s.
func TestSoftphoneRotateFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(f *fixture)
		reason string
	}{
		{"Secret missing", func(f *fixture) {
			if err := env.Client.Delete(f.ctx, f.secretOf(laptopID)); err != nil {
				f.t.Fatal(err)
			}
		}, "Secret patchbay-softphone-laptop does not exist"},
		{"Secret write conflicts", func(f *fixture) { f.failSecrets.Store(true) },
			"writing Secret patchbay-softphone-laptop: Operation cannot be fulfilled on secrets \"patchbay-softphone-laptop\": injected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			issuedLaptop(t, f)
			old := f.laptopToken()
			f.rec.take()
			tc.break_(f)
			ring := agentapi.DeviceObservations{Device: laptopID,
				Fetches: []agentapi.Fetch{provisioningFetch(f.version(laptopID), t0.Add(20*time.Second))}}
			f.at(30 * time.Second)
			f.see(ring)
			res := f.reconcileSoftphone("laptop")
			st := f.softphone("laptop").Status
			if st.LastAction != nil || res.RequeueAfter != devices.ActionRetryAfter {
				t.Errorf("lastAction %+v, requeue %v; want none, %v", st.LastAction, res.RequeueAfter, devices.ActionRetryAfter)
			}
			events := f.rec.take()
			wantEvents(t, events,
				laptopEvent(corev1.EventTypeWarning, "ActionFailed", "RotateToken", "rotateToken not sent: "+tc.reason),
				laptopEvent(corev1.EventTypeNormal, "StateChanged", "Decide",
					"Issued -> Provisioned: Linphone fetched its settings, which spends its one-time URL; minting a new one"))
			noLeak(t, f, events, old)
		})
	}
}

// TestSoftphoneReissue: a reissue request rotates the token and sends the
// softphone back to Issued, answering the request (no RequestIgnored).
func TestSoftphoneReissue(t *testing.T) {
	f := newFixture(t)
	issuedLaptop(t, f)
	ring := agentapi.DeviceObservations{Device: laptopID,
		Fetches:      []agentapi.Fetch{provisioningFetch(f.version(laptopID), t0.Add(20*time.Second))},
		Registration: contact("150", laptopIP, t0.Add(time.Hour))}
	f.at(30 * time.Second)
	f.see(ring)
	f.reconcileSoftphone("laptop") // spent: rotated
	f.compile()
	f.at(40 * time.Second)
	f.see(ring)
	f.reconcileSoftphone("laptop")
	if st := f.softphone("laptop").Status.State; st != string(drivers.StateRegistered) {
		t.Fatalf("laptop is %s; want Registered", st)
	}
	before := f.laptopToken()
	f.rec.take()

	edit(f, f.softphone("laptop"), func(s *v1alpha1.Softphone) {
		s.Annotations = map[string]string{annReissue: "2026-10-15T12:05:00Z"}
	})
	f.at(5 * time.Minute)
	f.see(ring)
	f.reconcileSoftphone("laptop")
	st := f.softphone("laptop").Status
	if st.State != string(drivers.StateIssued) || st.HandledRequest != "2026-10-15T12:05:00Z" ||
		st.LastAction == nil || st.LastAction.Kind != v1alpha1.ActionRotateToken {
		t.Errorf("state %s, handledRequest %q, lastAction %+v; want Issued, the request, rotateToken", st.State, st.HandledRequest, st.LastAction)
	}
	if st.Observed == nil || st.Observed.LastConfigFetch != nil {
		t.Errorf("observed %+v; want the config fetch forgotten", st.Observed)
	}
	if f.laptopToken() == before {
		t.Error("the token was not rotated")
	}
	wantEvents(t, f.rec.take(),
		laptopEvent(corev1.EventTypeNormal, "ActionSent", "RotateToken", "provisioning token rotated"),
		laptopEvent(corev1.EventTypeNormal, "StateChanged", "Decide",
			"Registered -> Issued: a new provisioning token was requested; waiting for Linphone to fetch its settings"))
}

// TestSoftphoneInvalidURL: a softphone that is not rendered (its number
// left the plan) shows no URL, and a request is ignored with an Event.
func TestSoftphoneInvalidURL(t *testing.T) {
	f := newFixture(t)
	issuedLaptop(t, f)
	edit(f, f.softphone("laptop"), func(s *v1alpha1.Softphone) {
		s.Spec.Number = "250"
		s.Annotations = map[string]string{annReissue: "again"}
	})
	f.compile()
	f.rec.take()
	f.reconcileSoftphone("laptop")
	st := f.softphone("laptop").Status
	if st.State != string(drivers.StateInvalid) || st.ProvisioningURL != "" || st.HandledRequest != "again" {
		t.Errorf("state %s, url set %v, handledRequest %q; want Invalid, no URL, again", st.State, st.ProvisioningURL != "", st.HandledRequest)
	}
	wantCondition(t, st.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonNumberNotInPlan,
		"number 250 is not in the Softphone range 1XX")
	wantEvents(t, f.rec.take(),
		laptopEvent(corev1.EventTypeNormal, "StateChanged", "Decide",
			"Issued -> Invalid: number 250 is not in the Softphone range 1XX"),
		laptopEvent(corev1.EventTypeNormal, "RequestIgnored", "Decide",
			annReissue+" request again ignored: the device is Invalid"))
	if calls := f.actor.take(); len(calls) != 0 {
		t.Errorf("actions %v; want none", calls)
	}
}

// TestStatusWriteError: a failed status write is returned (and requeued
// by controller-runtime), still recognisable as the API error, without
// anything an Invalid error quotes from the object: here the URL.
func TestStatusWriteError(t *testing.T) {
	f := newFixture(t)
	f.newSoftphone("laptop", "150", "Laptop")
	f.compile()
	f.see()
	url := f.url(f.laptopToken())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.ns, Name: "laptop"}}

	invalid := error(apierrors.NewInvalid(schema.GroupKind{Group: v1alpha1.GroupVersion.Group, Kind: "Softphone"}, "laptop",
		field.ErrorList{field.Invalid(field.NewPath("status", "provisioningURL"), url, "too long")}))
	f.statusErr.Store(&invalid)
	_, err := f.softphones.Reconcile(f.ctx, req)
	if !apierrors.IsInvalid(err) || strings.Contains(err.Error(), url) || strings.Contains(err.Error(), agentapi.ProvisioningPathPrefix) {
		t.Errorf("error %q: want an Invalid error that does not quote the URL", err)
	}

	conflict := error(apierrors.NewConflict(schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "softphones"},
		"laptop", nil))
	f.statusErr.Store(&conflict)
	if _, err := f.softphones.Reconcile(f.ctx, req); !apierrors.IsConflict(err) {
		t.Errorf("error %v: want a Conflict", err)
	}
	f.newPhone("desk", deskMAC, "100", "Desk")
	f.compile()
	if _, err := f.phones.Reconcile(f.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.ns, Name: "desk"}}); !apierrors.IsConflict(err) {
		t.Errorf("Phone: error %v: want a Conflict", err)
	}
	if e := f.rec.take(); len(e) != 0 {
		t.Errorf("events %v after failed writes; want none", e)
	}
}
