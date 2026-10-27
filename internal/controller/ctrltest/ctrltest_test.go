package ctrltest_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/controller/ctrltest"
)

var env *ctrltest.Env

func TestMain(m *testing.M) { os.Exit(ctrltest.Main(m, &env)) }

func TestCRDDir(t *testing.T) {
	dir, err := ctrltest.CRDDir()
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil || len(matches) != 5 {
		t.Errorf("%s holds %v (%v), want the five CRDs", dir, matches, err)
	}
}

// TestManagerSeesResources runs the harness end to end: a namespace, a
// manager scoped to it, and its cache serving a Phone written directly.
func TestManagerSeesResources(t *testing.T) {
	ctrltest.Need(t, env)
	ctx := context.Background()
	ns := ctrltest.Namespace(t, env)
	mgr := ctrltest.NewManager(t, env, ns)
	ctrltest.StartManager(t, mgr)

	phone := &v1alpha1.Phone{
		ObjectMeta: metav1.ObjectMeta{Name: "desk", Namespace: ns},
		Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "0200E2E00001", Number: "100"},
	}
	if err := env.Client.Create(ctx, phone); err != nil {
		t.Fatal(err)
	}
	err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		var got v1alpha1.Phone
		err := mgr.GetClient().Get(ctx, client.ObjectKeyFromObject(phone), &got)
		return err == nil, nil
	})
	if err != nil {
		t.Fatalf("the manager's cache never saw the Phone: %v", err)
	}
}
