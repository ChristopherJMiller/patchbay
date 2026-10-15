package cli_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/cli"
)

const (
	oldURL = "http://192.168.0.240:8088/p/not-a-secret-provisioning-token3"
	newURL = "http://192.168.0.240:8088/p/not-a-secret-provisioning-token4"
)

// requestValue is what the commands write: the fake clock's now, UTC, RFC
// 3339 with nanoseconds.
var requestValue = testNow.UTC().Format(time.RFC3339Nano)

func softphone(state, url string, conds ...metav1.Condition) *v1alpha1.Softphone {
	return &v1alpha1.Softphone{
		ObjectMeta: objectMeta("laptop", testNow.Add(-time.Hour)),
		Spec:       v1alpha1.SoftphoneSpec{Number: "150", Label: "Laptop"},
		Status:     v1alpha1.SoftphoneStatus{State: state, ProvisioningURL: url, Conditions: conds},
	}
}

func phone(name, state string) *v1alpha1.Phone {
	p := &v1alpha1.Phone{
		ObjectMeta: objectMeta(name, testNow.Add(-time.Hour)),
		Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "0200E2E00001", Number: "100"},
		Status:     v1alpha1.PhoneStatus{State: state},
	}
	p.Annotations = map[string]string{"example.com/keep": "yes"}
	return p
}

// qrOutput is the whole output of softphone qr for url.
func qrOutput(t *testing.T, name, url string) string {
	t.Helper()
	var code bytes.Buffer
	if err := cli.RenderQR(&code, url); err != nil {
		t.Fatal(err)
	}
	return url + "\n\n" + code.String() + "\nThis code works once; patchbay softphone qr " + name + " --reissue makes a new one.\n"
}

func TestSoftphoneQR(t *testing.T) {
	te := newTestEnv(t, softphone("Issued", oldURL))
	if err := cli.SoftphoneQR(context.Background(), te.Env, "laptop", false); err != nil {
		t.Fatal(err)
	}
	if want := qrOutput(t, "laptop", oldURL); te.out.String() != want {
		t.Errorf("output:\n%q\nwant:\n%q", te.out, want)
	}
	var sp v1alpha1.Softphone
	if err := te.Kube.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "laptop"}, &sp); err != nil {
		t.Fatal(err)
	}
	if _, set := sp.Annotations[v1alpha1.AnnotationReissueRequested]; set {
		t.Error("qr without --reissue requested a reissue")
	}
}

func TestSoftphoneQRNoURL(t *testing.T) {
	te := newTestEnv(t, softphone("", ""))
	err := cli.SoftphoneQR(context.Background(), te.Env, "laptop", false)
	if err == nil || !strings.Contains(err.Error(), "no provisioning URL") || !strings.Contains(err.Error(), "Pending") {
		t.Errorf("SoftphoneQR without a URL = %v, want an error naming the state", err)
	}
	if te.out.Len() != 0 {
		t.Errorf("printed %q", te.out)
	}
}

func TestSoftphoneQRNotFound(t *testing.T) {
	te := newTestEnv(t)
	for _, reissue := range []bool{false, true} {
		err := cli.SoftphoneQR(context.Background(), te.Env, "laptop", reissue)
		if err == nil || !strings.Contains(err.Error(), "softphone laptop not found in namespace "+testNamespace) {
			t.Errorf("reissue=%v: %v, want not found naming the namespace", reissue, err)
		}
	}
}

// playController runs fn each time the command is waiting on the fake
// clock, then advances the clock by one poll, until the command returns.
func playController(t *testing.T, te *testEnv, run func() error, fn func(polls int)) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- run() }()
	deadline := time.After(20 * time.Second)
	for polls := 1; ; {
		select {
		case err := <-done:
			return err
		case <-deadline:
			t.Fatal("the command did not finish")
		default:
		}
		if !te.clock.HasWaiters() {
			time.Sleep(time.Millisecond)
			continue
		}
		fn(polls)
		polls++
		te.clock.Step(cli.ReissuePoll)
	}
}

func setSoftphoneStatus(t *testing.T, te *testEnv, edit func(*v1alpha1.SoftphoneStatus)) {
	t.Helper()
	ctx := context.Background()
	var sp v1alpha1.Softphone
	if err := te.Kube.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "laptop"}, &sp); err != nil {
		t.Error(err)
		return
	}
	edit(&sp.Status)
	if err := te.Kube.Status().Update(ctx, &sp); err != nil {
		t.Error(err)
	}
}

// Each thing --reissue waits for, alone: the command must still be waiting
// after poll 1 and finish right after poll 2 brings the whole answer.
func TestSoftphoneQRReissue(t *testing.T) {
	live := condition(v1alpha1.ConditionConfigCurrent, metav1.ConditionTrue, "Current", "")
	pending := condition(v1alpha1.ConditionConfigCurrent, metav1.ConditionFalse, v1alpha1.ReasonApplyPending, "")
	const midURL = "http://192.168.0.240:8088/p/not-a-secret-provisioning-token5"
	for _, tc := range []struct {
		name  string
		poll1 func(s *v1alpha1.SoftphoneStatus)
	}{
		{"not handled yet", func(s *v1alpha1.SoftphoneStatus) { s.ProvisioningURL = midURL }},
		{"old URL", func(s *v1alpha1.SoftphoneStatus) { s.HandledRequest = requestValue }},
		{"no URL", func(s *v1alpha1.SoftphoneStatus) { s.HandledRequest, s.ProvisioningURL = requestValue, "" }},
		{"not live", func(s *v1alpha1.SoftphoneStatus) {
			s.HandledRequest, s.ProvisioningURL = requestValue, midURL
			meta.SetStatusCondition(&s.Conditions, pending)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t, softphone("Registered", oldURL, live))
			var sawRequest string
			lastPoll := 0
			err := playController(t, te, func() error {
				return cli.SoftphoneQR(context.Background(), te.Env, "laptop", true)
			}, func(polls int) {
				lastPoll = polls
				switch polls {
				case 1:
					var sp v1alpha1.Softphone
					if err := te.Kube.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "laptop"}, &sp); err != nil {
						t.Error(err)
					}
					sawRequest = sp.Annotations[v1alpha1.AnnotationReissueRequested]
					setSoftphoneStatus(t, te, tc.poll1)
				case 2:
					setSoftphoneStatus(t, te, func(s *v1alpha1.SoftphoneStatus) {
						s.State, s.HandledRequest, s.ProvisioningURL = "Issued", requestValue, newURL
						meta.SetStatusCondition(&s.Conditions, live)
					})
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if lastPoll != 2 {
				t.Errorf("finished after poll %d, want 2", lastPoll)
			}
			if sawRequest != requestValue {
				t.Errorf("annotation %q, want the clock's time %q", sawRequest, requestValue)
			}
			if want := qrOutput(t, "laptop", newURL); te.out.String() != want {
				t.Errorf("output:\n%q\nwant the new URL's:\n%q", te.out, want)
			}
			if !strings.Contains(te.errOut.String(), "reissue requested ("+requestValue+")") {
				t.Errorf("stderr %q does not report the request", te.errOut)
			}
			for _, url := range []string{oldURL, midURL, newURL} {
				if strings.Contains(te.errOut.String(), url) {
					t.Errorf("stderr carries a provisioning URL: %q", te.errOut)
				}
			}
		})
	}
}

// A controller that handled an older request, or answers with the old URL,
// is not an answer.
func TestSoftphoneQRReissueTimesOut(t *testing.T) {
	sp := softphone("Issued", oldURL,
		condition(v1alpha1.ConditionConfigCurrent, metav1.ConditionTrue, "Current", ""))
	sp.Status.HandledRequest = "2026-10-14T00:00:00Z"
	te := newTestEnv(t, sp)
	start := te.clock.Now()
	err := playController(t, te, func() error {
		return cli.SoftphoneQR(context.Background(), te.Env, "laptop", true)
	}, func(polls int) {
		if polls == 5 { // handled, but no new URL ever comes
			setSoftphoneStatus(t, te, func(s *v1alpha1.SoftphoneStatus) { s.HandledRequest = requestValue })
		}
	})
	if err == nil {
		t.Fatal("no error after the wait")
	}
	for _, want := range []string{"softphone laptop", "within 30s", "state Issued", "no new provisioning URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), oldURL) {
		t.Errorf("the error carries the URL: %v", err)
	}
	if waited := te.clock.Since(start); waited != cli.ReissueWait {
		t.Errorf("gave up after %s, want %s", waited, cli.ReissueWait)
	}
	if te.out.Len() != 0 {
		t.Errorf("printed %q", te.out)
	}
}

func TestSoftphoneQRReissueNotHandled(t *testing.T) {
	te := newTestEnv(t, softphone("Registered", oldURL))
	err := playController(t, te, func() error {
		return cli.SoftphoneQR(context.Background(), te.Env, "laptop", true)
	}, func(int) {})
	if err == nil || !strings.Contains(err.Error(), "has not handled the request") || !strings.Contains(err.Error(), "state Registered") {
		t.Errorf("error %v, want one saying the controller has not handled the request", err)
	}
}

func TestSoftphoneQRReissueCancelled(t *testing.T) {
	te := newTestEnv(t, softphone("Registered", oldURL))
	ctx, cancel := context.WithCancel(context.Background())
	err := playController(t, te, func() error {
		return cli.SoftphoneQR(ctx, te.Env, "laptop", true)
	}, func(int) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v, want context.Canceled", err)
	}
}

func TestPhoneResync(t *testing.T) {
	const warning = "warning: phone %s is %s; the controller resyncs only a Registered or ConfigStale phone, so it will mark this request handled without a resync\n"
	for _, tc := range []struct {
		state, shown string
		warn         bool
	}{
		{"Registered", "Registered", false},
		{"ConfigStale", "ConfigStale", false},
		{"Upgrading", "Upgrading", true},
		{"", "Pending", true},
	} {
		t.Run(tc.shown, func(t *testing.T) {
			te := newTestEnv(t, phone("desk", tc.state))
			if err := cli.PhoneResync(context.Background(), te.Env, "desk"); err != nil {
				t.Fatal(err)
			}
			if want := "resync requested for phone desk (" + requestValue + ")\n"; te.out.String() != want {
				t.Errorf("stdout %q, want %q", te.out, want)
			}
			wantErr := ""
			if tc.warn {
				wantErr = strings.Replace(strings.Replace(warning, "%s", "desk", 1), "%s", tc.shown, 1)
			}
			if te.errOut.String() != wantErr {
				t.Errorf("stderr %q, want %q", te.errOut, wantErr)
			}
			var p v1alpha1.Phone
			if err := te.Kube.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "desk"}, &p); err != nil {
				t.Fatal(err)
			}
			if got := p.Annotations[v1alpha1.AnnotationResyncRequested]; got != requestValue {
				t.Errorf("annotation %q, want %q", got, requestValue)
			}
			if p.Annotations["example.com/keep"] != "yes" {
				t.Errorf("other annotations lost: %v", p.Annotations)
			}
		})
	}
	t.Run("not found", func(t *testing.T) {
		te := newTestEnv(t)
		err := cli.PhoneResync(context.Background(), te.Env, "desk")
		if err == nil || !strings.Contains(err.Error(), "phone desk not found in namespace "+testNamespace) {
			t.Errorf("%v, want not found naming the namespace", err)
		}
	})
}

// API errors other than NotFound are returned, wrapped with what was being
// done.
func TestRequestsAPIErrors(t *testing.T) {
	scheme, err := cli.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	withErrors := func(te *testEnv, funcs interceptor.Funcs, objs ...client.Object) {
		te.Kube = interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), funcs)
	}
	getFails := interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return errors.New("etcdserver: request timed out")
	}}
	patchFails := interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
		return errors.New("admission webhook said no")
	}}

	te := newTestEnv(t)
	withErrors(te, getFails)
	if err := cli.SoftphoneQR(context.Background(), te.Env, "laptop", false); err == nil ||
		!strings.Contains(err.Error(), "reading softphone laptop in namespace "+testNamespace+": etcdserver") {
		t.Errorf("qr: %v", err)
	}
	if err := cli.PhoneResync(context.Background(), te.Env, "desk"); err == nil || !strings.Contains(err.Error(), "reading phone desk") {
		t.Errorf("resync: %v", err)
	}

	te = newTestEnv(t)
	withErrors(te, patchFails, softphone("Registered", oldURL), phone("desk", "Registered"))
	if err := cli.SoftphoneQR(context.Background(), te.Env, "laptop", true); err == nil ||
		!strings.Contains(err.Error(), "softphone laptop: requesting a reissue: admission webhook said no") {
		t.Errorf("qr --reissue: %v", err)
	}
	if err := cli.PhoneResync(context.Background(), te.Env, "desk"); err == nil ||
		!strings.Contains(err.Error(), "phone desk: requesting a resync: admission webhook said no") {
		t.Errorf("resync: %v", err)
	}
	if te.out.Len() != 0 {
		t.Errorf("printed %q", te.out)
	}
}
