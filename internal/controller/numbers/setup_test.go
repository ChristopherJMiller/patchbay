package numbers_test

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/controller/ctrltest"
	"github.com/ChristopherJMiller/patchbay/internal/controller/numbers"
)

// countingClient counts Gets of a Person: each reconcile starts with
// exactly one.
type countingClient struct {
	client.Client
	mu   sync.Mutex
	gets int
}

func (c *countingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*v1alpha1.Person); ok {
		c.mu.Lock()
		c.gets++
		c.mu.Unlock()
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *countingClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets
}

func (c *countingClient) settle() int {
	n := c.count()
	stable := time.Now()
	for time.Since(stable) < 500*time.Millisecond {
		time.Sleep(50 * time.Millisecond)
		if m := c.count(); m != n {
			n, stable = m, time.Now()
		}
	}
	return n
}

// TestSetup: the manager judges Persons, writes their status, and judges
// them again on a pbx-config change or a new generation, never on its own
// status write or an annotation. Streams are not this package's (their
// reconciler is streamstate's): none is judged here.
func TestSetup(t *testing.T) {
	f := newFixture(t)
	f.newPhone("desk", "0200E2E00001", "100")
	f.newPerson("chris", "301", "phone desk")
	f.create(&v1alpha1.Stream{ObjectMeta: meta_("radio"),
		Spec: v1alpha1.StreamSpec{Number: "705", Name: "Radio", Source: "http://192.168.0.58:5004/auto/v5.1"}})
	f.compile()

	mgr := ctrltest.NewManager(t, env, f.ns)
	counter := &countingClient{Client: mgr.GetClient()}
	cfg := f.config()
	cfg.Client = counter
	if err := numbers.Setup(mgr, cfg); err != nil {
		t.Fatal(err)
	}
	ctrltest.StartManager(t, mgr)

	err := wait.PollUntilContextTimeout(f.ctx, 50*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		var p v1alpha1.Person
		if err := env.Client.Get(ctx, client.ObjectKey{Namespace: f.ns, Name: "chris"}, &p); err != nil {
			return false, err
		}
		return p.Status.State == numbers.StateReady, nil
	})
	if err != nil {
		t.Fatalf("the manager never judged chris: %v", err)
	}
	people := counter.settle()

	expect := func(what string, want int, do func()) {
		t.Helper()
		do()
		if want > 0 {
			_ = wait.PollUntilContextTimeout(f.ctx, 20*time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) {
				return counter.count() >= people+want, nil
			})
		}
		n := counter.settle()
		if n-people != want {
			t.Errorf("%s: %d Person reconciles; want %d", what, n-people, want)
		}
		people = n
	}
	touch := func(obj client.Object, change func(client.Object)) {
		t.Helper()
		obj.SetNamespace(f.ns)
		if err := env.Client.Get(f.ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatal(err)
		}
		change(obj)
		if err := env.Client.Update(f.ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	annotate := func(o client.Object) { o.SetAnnotations(map[string]string{"test/touched": time.Now().String()}) }

	expect("a pbx-config change", 1, func() {
		touch(&corev1.ConfigMap{ObjectMeta: meta_(f.names.ConfigMap)}, annotate)
	})
	expect("an annotation", 0, func() {
		touch(&v1alpha1.Person{ObjectMeta: meta_("chris")}, annotate)
	})
	expect("a spec change", 1, func() {
		touch(&v1alpha1.Person{ObjectMeta: meta_("chris")}, func(o client.Object) { o.(*v1alpha1.Person).Spec.Name = "Chris M" })
	})

	var s v1alpha1.Stream
	if err := env.Client.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "radio"}, &s); err != nil {
		t.Fatal(err)
	}
	if s.Status.State != "" {
		t.Errorf("numbers judged a Stream: %+v", s.Status)
	}
}
