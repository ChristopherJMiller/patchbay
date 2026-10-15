package intake_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ChristopherJMiller/patchbay/internal/agent/intake"
	"github.com/ChristopherJMiller/patchbay/internal/agent/intake/intaketest"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

const (
	ns         = "telephony"
	cmName     = "patchbay-pbx-config"
	secretName = "patchbay-pbx-secrets"
)

// fakeApplier records what the intake hands it.
type fakeApplier struct {
	mu      sync.Mutex
	calls   []string
	errs    []error // Apply's answers, in turn; nil when they run out
	refused []error
}

func (a *fakeApplier) Apply(_ context.Context, r intake.Resolved) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, "apply "+r.Bundle.Version[:4])
	if len(a.errs) == 0 {
		return nil
	}
	err := a.errs[0]
	a.errs = a.errs[1:]
	return err
}

func (a *fakeApplier) Refuse(version string, reason error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := version
	if len(v) > 4 {
		v = v[:4]
	}
	a.calls = append(a.calls, "refuse "+v)
	a.refused = append(a.refused, reason)
}

func (a *fakeApplier) got() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

func (a *fakeApplier) failNext(errs ...error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.errs = append(a.errs, errs...)
}

// harness runs an Intake with a fake clientset, a fake clock and a retry
// timer the test fires.
type harness struct {
	t       *testing.T
	client  *fake.Clientset
	applier *fakeApplier
	syncs   chan struct{}

	mu     sync.Mutex
	now    time.Time
	timers []timer
}

type timer struct {
	d  time.Duration
	ch chan time.Time
}

func newHarness(t *testing.T, log logr.Logger, objs ...*corev1.ConfigMap) *harness {
	t.Helper()
	h := &harness{
		t: t, client: fake.NewClientset(), applier: &fakeApplier{},
		syncs: make(chan struct{}, 100), now: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC),
	}
	for _, cm := range objs {
		h.createCM(cm)
	}
	in, err := intake.New(intake.Config{
		Client: h.client, Namespace: ns, ConfigMap: cmName, Secret: secretName, Applier: h.applier,
		Now: h.clock, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	in.SetTestHooks(h.after, func() { h.syncs <- struct{}{} })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- in.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return")
		}
	})
	return h
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

func (h *harness) after(d time.Duration) <-chan time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan time.Time, 1)
	h.timers = append(h.timers, timer{d, ch})
	return ch
}

// fireLast fires the newest retry timer and returns its duration.
func (h *harness) fireLast() time.Duration {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.timers) == 0 {
		h.t.Fatal("no retry timer was set")
	}
	tm := h.timers[len(h.timers)-1]
	tm.ch <- h.now
	return tm.d
}

// lastTimer is the newest retry timer's duration.
func (h *harness) lastTimer() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.timers) == 0 {
		return 0
	}
	return h.timers[len(h.timers)-1].d
}

// waitSync waits for the next evaluation.
func (h *harness) waitSync() {
	h.t.Helper()
	select {
	case <-h.syncs:
	case <-time.After(5 * time.Second):
		h.t.Fatal("no evaluation")
	}
}

// settle waits until no evaluation has happened for a moment.
func (h *harness) settle() {
	for {
		select {
		case <-h.syncs:
		case <-time.After(150 * time.Millisecond):
			return
		}
	}
}

func (h *harness) createCM(cm *corev1.ConfigMap) {
	h.t.Helper()
	if _, err := h.client.CoreV1().ConfigMaps(ns).Create(context.Background(), cm, metav1.CreateOptions{}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) updateCM(cm *corev1.ConfigMap) {
	h.t.Helper()
	if _, err := h.client.CoreV1().ConfigMaps(ns).Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) putSecret(s *corev1.Secret) {
	h.t.Helper()
	_, err := h.client.CoreV1().Secrets(ns).Update(context.Background(), s, metav1.UpdateOptions{})
	if err != nil {
		if _, err = h.client.CoreV1().Secrets(ns).Create(context.Background(), s, metav1.CreateOptions{}); err != nil {
			h.t.Fatal(err)
		}
	}
}

// expectCalls waits for the applier's calls to become want, then checks
// they stay so.
func (h *harness) expectCalls(want ...string) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Join(h.applier.got(), "|") != strings.Join(want, "|") {
		if time.Now().After(deadline) {
			h.t.Fatalf("applier calls = %q, want %q", h.applier.got(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.settle()
	if got := h.applier.got(); strings.Join(got, "|") != strings.Join(want, "|") {
		h.t.Fatalf("applier calls = %q, want %q", got, want)
	}
}

func TestRetryPolicy(t *testing.T) {
	in := intaketest.House()
	b1 := in.Compile(t)
	v1 := b1.Version[:4]
	in.AddPhone("front", "D0C78915E8C9", "101", "Front")
	b2 := in.Compile(t)
	v2 := b2.Version[:4]
	data := in.SecretData(t, b2) // holds every key b1 needs too

	h := newHarness(t, logr.Discard(), intaketest.ConfigMap(t, ns, cmName, b1))
	h.applier.failNext(errors.New("verify: endpoints missing phone-desk"))
	h.putSecret(intaketest.Secret(ns, secretName, "10", data))
	h.expectCalls("apply " + v1)

	// The failed pair waits RetryAfter: a rewrite with the same version and
	// Secret does nothing, and so does an early timer.
	cm := intaketest.ConfigMap(t, ns, cmName, b1)
	cm.Annotations = map[string]string{"touched": "1"}
	h.updateCM(cm)
	h.expectCalls("apply " + v1)
	h.advance(4 * time.Minute)
	if d := h.fireLast(); d != intake.DefaultRetryAfter {
		t.Errorf("retry timer %s, want 5m", d)
	}
	h.waitSync()
	h.expectCalls("apply " + v1)
	if d := h.lastTimer(); d != time.Minute {
		t.Errorf("after 4 of 5 minutes the timer is %s, want the 1m left", d)
	}
	// ... and is tried again once RetryAfter has passed.
	h.advance(time.Minute)
	h.fireLast()
	h.expectCalls("apply "+v1, "apply "+v1)

	// A new Secret resourceVersion retries at once.
	h.failNext(errors.New("reload res_pjsip.so: boom"))
	h.putSecret(intaketest.Secret(ns, secretName, "11", data))
	h.expectCalls("apply "+v1, "apply "+v1, "apply "+v1)

	// So does a new bundle version; it applies, and is never applied again:
	// not on a rewrite, not on a timer left from the failure.
	h.updateCM(intaketest.ConfigMap(t, ns, cmName, b2))
	h.expectCalls("apply "+v1, "apply "+v1, "apply "+v1, "apply "+v2)
	cm = intaketest.ConfigMap(t, ns, cmName, b2)
	cm.Annotations = map[string]string{"touched": "2"}
	h.updateCM(cm)
	h.advance(time.Hour)
	h.fireLast()
	h.expectCalls("apply "+v1, "apply "+v1, "apply "+v1, "apply "+v2)

	// Going back to v1 is a new attempt (v2 is what is applied).
	h.updateCM(intaketest.ConfigMap(t, ns, cmName, b1))
	h.expectCalls("apply "+v1, "apply "+v1, "apply "+v1, "apply "+v2, "apply "+v1)
}

func (h *harness) failNext(errs ...error) { h.applier.failNext(errs...) }

// TestRefusals: a bundle that does not decode, has a bad version or does
// not resolve is refused, never applied; a refused pair waits like a failed
// one, and a Secret change retries it.
func TestRefusals(t *testing.T) {
	in := intaketest.House()
	b := in.Compile(t)
	v := b.Version[:4]
	data := in.SecretData(t, b)
	partial := map[string][]byte{}
	for k, val := range data {
		if !strings.HasPrefix(k, "patchbay-phone-desk_") {
			partial[k] = val
		}
	}

	h := newHarness(t, logr.Discard(), intaketest.ConfigMap(t, ns, cmName, b))
	h.putSecret(intaketest.Secret(ns, secretName, "1", partial))
	h.expectCalls("refuse " + v)
	if err := h.applier.refused[0]; err == nil || err.Error() != "unresolved placeholder patchbay-phone-desk/sip-password" {
		t.Errorf("refusal = %v", err)
	}
	h.putSecret(intaketest.Secret(ns, secretName, "2", data))
	h.expectCalls("refuse "+v, "apply "+v)
}

func TestRefusesBadVersion(t *testing.T) {
	in := intaketest.House()
	b := in.Compile(t)
	data := in.SecretData(t, b)
	for name, version := range map[string]string{
		"upper case": strings.ToUpper(b.Version),
		"short":      b.Version[:63],
		"a path":     "../../etc",
	} {
		t.Run(name, func(t *testing.T) {
			bad := b
			bad.Version = version
			cm := intaketest.ConfigMap(t, ns, cmName, bad)
			h := newHarness(t, logr.Discard(), cm)
			h.putSecret(intaketest.Secret(ns, secretName, "1", data))
			h.expectCalls("refuse ")
			if err := h.applier.refused[0]; err == nil || !strings.Contains(err.Error(), "not 64 lower-case hex digits") {
				t.Errorf("refusal = %v", err)
			}
		})
	}
}

func TestRefusesUndecodable(t *testing.T) {
	in := intaketest.House()
	b := in.Compile(t)
	data := in.SecretData(t, b)

	// A file key that is not a bundle path: the version is still named.
	cm := intaketest.ConfigMap(t, ns, cmName, b)
	cm.Data["not-a-path"] = "x"
	h := newHarness(t, logr.Discard(), cm)
	h.putSecret(intaketest.Secret(ns, secretName, "1", data))
	h.expectCalls("refuse " + b.Version[:4])

	// No index at all: refused without a version; a fixed ConfigMap is a
	// new attempt.
	cm = intaketest.ConfigMap(t, ns, cmName, b)
	delete(cm.Data, compile.IndexKey)
	cm.ResourceVersion = ""
	h2 := newHarness(t, logr.Discard(), cm)
	h2.putSecret(intaketest.Secret(ns, secretName, "1", data))
	h2.expectCalls("refuse ")
	h2.updateCM(intaketest.ConfigMap(t, ns, cmName, b))
	h2.expectCalls("refuse ", "apply "+b.Version[:4])
}

// TestWaitsForMissingObjects: nothing happens until both exist, and the
// wait is logged once.
func TestWaitsForMissingObjects(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	log := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, args)
	}, funcr.Options{})
	in := intaketest.House()
	b := in.Compile(t)

	h := newHarness(t, log)
	h.settle()
	h.putSecret(intaketest.Secret(ns, secretName, "1", in.SecretData(t, b)))
	h.settle()
	h.putSecret(intaketest.Secret(ns, secretName, "2", in.SecretData(t, b)))
	h.settle()
	h.expectCalls()
	mu.Lock()
	waits := 0
	for _, l := range lines {
		if strings.Contains(l, "waiting for ConfigMap "+cmName) {
			waits++
		}
	}
	mu.Unlock()
	if waits != 1 {
		t.Errorf("the wait was logged %d times, want once:\n%s", waits, strings.Join(lines, "\n"))
	}
	h.createCM(intaketest.ConfigMap(t, ns, cmName, b))
	h.expectCalls("apply " + b.Version[:4])
}

// TestNoSecretInLogs: whatever happens, no secret value is logged or
// handed to the applier in an error.
func TestNoSecretInLogs(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	log := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, prefix+" "+args)
	}, funcr.Options{Verbosity: 10})
	in := intaketest.House()
	b := in.Compile(t)
	data := in.SecretData(t, b)
	partial := map[string][]byte{}
	for k, v := range data {
		if !strings.Contains(k, "token") {
			partial[k] = v
		}
	}

	h := newHarness(t, log, intaketest.ConfigMap(t, ns, cmName, b))
	h.putSecret(intaketest.Secret(ns, secretName, "1", partial))
	h.expectCalls("refuse " + b.Version[:4])
	h.failNext(errors.New("verify: endpoints missing phone-desk"))
	h.putSecret(intaketest.Secret(ns, secretName, "2", data))
	h.expectCalls("refuse "+b.Version[:4], "apply "+b.Version[:4])
	h.putSecret(intaketest.Secret(ns, secretName, "3", data))
	h.expectCalls("refuse "+b.Version[:4], "apply "+b.Version[:4], "apply "+b.Version[:4])

	mu.Lock()
	text := strings.Join(lines, "\n")
	mu.Unlock()
	for _, err := range h.applier.refused {
		text += "\n" + err.Error()
	}
	if !strings.Contains(text, "applying bundle") || !strings.Contains(text, "unresolved placeholder") {
		t.Errorf("the log misses the attempts:\n%s", text)
	}
	for _, v := range in.Values() {
		if strings.Contains(text, v) {
			t.Errorf("a secret value reached the log: %s", v)
		}
	}
}

func TestNewChecksTheConfig(t *testing.T) {
	ok := intake.Config{Client: fake.NewClientset(), Namespace: ns, ConfigMap: cmName, Secret: secretName, Applier: &fakeApplier{}}
	if _, err := intake.New(ok); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*intake.Config){
		"no client":    func(c *intake.Config) { c.Client = nil },
		"no namespace": func(c *intake.Config) { c.Namespace = "" },
		"no secret":    func(c *intake.Config) { c.Secret = "" },
		"no applier":   func(c *intake.Config) { c.Applier = nil },
	} {
		c := ok
		mutate(&c)
		if _, err := intake.New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
