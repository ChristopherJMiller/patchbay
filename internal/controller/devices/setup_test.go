package devices_test

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/controller/agentview"
	"github.com/ChristopherJMiller/patchbay/internal/controller/ctrltest"
	"github.com/ChristopherJMiller/patchbay/internal/controller/devices"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// countingClient counts the reconcilers' Gets of a Phone or a Softphone:
// each reconcile starts with exactly one.
type countingClient struct {
	client.Client
	mu   sync.Mutex
	gets map[string]int
}

func (c *countingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.mu.Lock()
	switch obj.(type) {
	case *v1alpha1.Phone:
		c.gets["phone"]++
	case *v1alpha1.Softphone:
		c.gets["softphone"]++
	}
	c.mu.Unlock()
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *countingClient) counts() (phone, softphone int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets["phone"], c.gets["softphone"]
}

// settle waits until the counts have not moved for half a second.
func (c *countingClient) settle(t *testing.T) (phone, softphone int) {
	t.Helper()
	p, s := c.counts()
	stable := time.Now()
	for time.Since(stable) < 500*time.Millisecond {
		time.Sleep(50 * time.Millisecond)
		if np, ns := c.counts(); np != p || ns != s {
			p, s, stable = np, ns, time.Now()
		}
	}
	return p, s
}

// TestSetupTriggers: what enqueues a device (6.2.6 "Watches"): the poller's
// Changes event of its kind, a pbx-config change (every device of both
// kinds), a pbx-secrets change (Softphones), a new generation or
// annotation; not a status-only write, a label, or another ConfigMap.
func TestSetupTriggers(t *testing.T) {
	f := newFixture(t)
	f.newPhone("desk", deskMAC, "100", "Desk")
	f.newSoftphone("laptop", "150", "Laptop")
	f.compile()

	mgr := ctrltest.NewManager(t, env, f.ns)
	counter := &countingClient{Client: mgr.GetClient(), gets: map[string]int{}}
	cfg := f.config()
	cfg.Client = counter
	cfg.Observer = &agentview.Holder{} // never started: every reconcile stops at the gate
	changes := agentview.NewChanges(16)
	cfg.Changes = changes
	if err := devices.Setup(mgr, cfg); err != nil {
		t.Fatal(err)
	}
	ctrltest.StartManager(t, mgr)

	if err := wait.PollUntilContextTimeout(f.ctx, 50*time.Millisecond, 20*time.Second, true, func(context.Context) (bool, error) {
		p, s := counter.counts()
		return p > 0 && s > 0, nil
	}); err != nil {
		t.Fatal("neither device was reconciled at start")
	}
	phones, softphones := counter.settle(t)

	expect := func(what string, dPhone, dSoftphone int, do func()) {
		t.Helper()
		do()
		if dPhone+dSoftphone > 0 {
			_ = wait.PollUntilContextTimeout(f.ctx, 20*time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) {
				p, s := counter.counts()
				return p >= phones+dPhone && s >= softphones+dSoftphone, nil
			})
		}
		p, s := counter.settle(t)
		if p-phones != dPhone || s-softphones != dSoftphone {
			t.Errorf("%s: %d Phone and %d Softphone reconciles; want %d and %d", what, p-phones, s-softphones, dPhone, dSoftphone)
		}
		phones, softphones = p, s
	}
	annotate := func(obj client.Object) {
		t.Helper()
		if err := env.Client.Get(f.ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatal(err)
		}
		a := obj.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		a["test/touched"] = time.Now().String()
		obj.SetAnnotations(a)
		if err := env.Client.Update(f.ctx, obj); err != nil {
			t.Fatal(err)
		}
	}

	expect("a Phone Changes event", 1, 0, func() {
		if err := changes.Notify(f.ctx, drivers.KindPhone, f.ns, "desk"); err != nil {
			t.Fatal(err)
		}
	})
	expect("a Softphone Changes event", 0, 1, func() {
		if err := changes.Notify(f.ctx, drivers.KindSoftphone, f.ns, "laptop"); err != nil {
			t.Fatal(err)
		}
	})
	expect("a pbx-config change", 1, 1, func() {
		annotate(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: f.ns, Name: f.names.ConfigMap}})
	})
	expect("a pbx-secrets change", 0, 1, func() {
		annotate(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: f.ns, Name: f.names.ProjectedSecret}})
	})
	expect("another ConfigMap", 0, 0, func() {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: f.ns, Name: "unrelated"}}
		if err := env.Client.Create(f.ctx, cm); err != nil {
			t.Fatal(err)
		}
	})
	expect("a status-only write", 0, 0, func() {
		p := f.phone("desk")
		p.Status.State = "Unseen"
		if err := env.Client.Status().Update(f.ctx, p); err != nil {
			t.Fatal(err)
		}
		s := f.softphone("laptop")
		s.Status.State = "Issued"
		if err := env.Client.Status().Update(f.ctx, s); err != nil {
			t.Fatal(err)
		}
	})
	expect("a label", 0, 0, func() {
		edit(f, f.phone("desk"), func(p *v1alpha1.Phone) { p.Labels = map[string]string{"room": "study"} })
	})
	expect("a request annotation", 1, 0, func() {
		edit(f, f.phone("desk"), func(p *v1alpha1.Phone) { p.Annotations = map[string]string{annResync: "now"} })
	})
	expect("a spec change", 0, 1, func() {
		edit(f, f.softphone("laptop"), func(s *v1alpha1.Softphone) { s.Spec.Label = "Laptop Two" })
	})
}
