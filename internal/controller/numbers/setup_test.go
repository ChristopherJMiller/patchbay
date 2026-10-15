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

// countingClient counts Gets of a Person or a Stream: each reconcile
// starts with exactly one.
type countingClient struct {
	client.Client
	mu   sync.Mutex
	gets map[string]int
}

func (c *countingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.mu.Lock()
	switch obj.(type) {
	case *v1alpha1.Person:
		c.gets["person"]++
	case *v1alpha1.Stream:
		c.gets["stream"]++
	}
	c.mu.Unlock()
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *countingClient) counts() (person, stream int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets["person"], c.gets["stream"]
}

func (c *countingClient) settle() (person, stream int) {
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

// TestSetup: the manager judges both kinds, writes their status, and judges
// them again on a pbx-config change or a new generation, never on its own
// status write or an annotation.
func TestSetup(t *testing.T) {
	f := newFixture(t)
	f.newPhone("desk", "0200E2E00001", "100")
	f.newPerson("chris", "301", "phone desk")
	f.newStream("radio", "705")
	f.compile()

	mgr := ctrltest.NewManager(t, env, f.ns)
	counter := &countingClient{Client: mgr.GetClient(), gets: map[string]int{}}
	cfg := f.config()
	cfg.Client = counter
	if err := numbers.Setup(mgr, cfg); err != nil {
		t.Fatal(err)
	}
	ctrltest.StartManager(t, mgr)

	err := wait.PollUntilContextTimeout(f.ctx, 50*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		var p v1alpha1.Person
		var s v1alpha1.Stream
		if err := env.Client.Get(ctx, client.ObjectKey{Namespace: f.ns, Name: "chris"}, &p); err != nil {
			return false, err
		}
		if err := env.Client.Get(ctx, client.ObjectKey{Namespace: f.ns, Name: "radio"}, &s); err != nil {
			return false, err
		}
		return p.Status.State == numbers.StateReady && s.Status.State == numbers.StateUnavailable, nil
	})
	if err != nil {
		t.Fatalf("the manager never judged chris and radio: %v", err)
	}
	people, streams := counter.settle()

	expect := func(what string, dPerson, dStream int, do func()) {
		t.Helper()
		do()
		if dPerson+dStream > 0 {
			_ = wait.PollUntilContextTimeout(f.ctx, 20*time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) {
				p, s := counter.counts()
				return p >= people+dPerson && s >= streams+dStream, nil
			})
		}
		p, s := counter.settle()
		if p-people != dPerson || s-streams != dStream {
			t.Errorf("%s: %d Person and %d Stream reconciles; want %d and %d", what, p-people, s-streams, dPerson, dStream)
		}
		people, streams = p, s
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

	expect("a pbx-config change", 1, 1, func() {
		touch(&corev1.ConfigMap{ObjectMeta: meta_(f.names.ConfigMap)}, annotate)
	})
	expect("an annotation", 0, 0, func() {
		touch(&v1alpha1.Person{ObjectMeta: meta_("chris")}, annotate)
	})
	expect("a spec change", 0, 1, func() {
		touch(&v1alpha1.Stream{ObjectMeta: meta_("radio")}, func(o client.Object) { o.(*v1alpha1.Stream).Spec.Name = "Radio Two" })
	})
}
