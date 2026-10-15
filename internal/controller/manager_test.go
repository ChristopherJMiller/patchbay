package controller_test

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/internal/controller"
)

func TestNewManagerRequiresNamespace(t *testing.T) {
	if _, err := controller.NewManager(nil, controller.Options{}); err == nil ||
		!strings.Contains(err.Error(), "namespace") {
		t.Fatalf("want a namespace error, got %v", err)
	}
	if _, err := controller.NewManager(nil, controller.Options{Namespace: "ns", LeaderElection: true}); err == nil ||
		!strings.Contains(err.Error(), "fullname") {
		t.Fatalf("leader election without a fullname: want an error naming it, got %v", err)
	}
}

func TestLeaderElectionIDPerInstall(t *testing.T) {
	if got := controller.LeaderElectionID("patchbay"); got != "patchbay-controller.patchbay.chrismiller.xyz" {
		t.Errorf("LeaderElectionID(patchbay) = %s", got)
	}
	if controller.LeaderElectionID("a") == controller.LeaderElectionID("b") {
		t.Error("two installs share a Lease")
	}
}

// TestManagerStartsAndElects runs the real manager against envtest: it must
// win the leader-election Lease in its namespace and serve /readyz.
func TestManagerStartsAndElects(t *testing.T) {
	needEnvtest(t)
	ns := newNamespace(t)
	probeAddr := freeAddr(t)

	mgr, err := controller.NewManager(testCfg, controller.Options{
		Namespace:      ns,
		MetricsAddr:    "0",
		ProbeAddr:      probeAddr,
		LeaderElection: true,
		Fullname:       "patchbay",
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager stopped with error: %v", err)
		}
	}()

	select {
	case <-mgr.Elected():
	case <-time.After(30 * time.Second):
		t.Fatal("manager was not elected leader within 30s")
	}
	var lease coordinationv1.Lease
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: controller.LeaderElectionID("patchbay")}, &lease); err != nil {
		t.Fatalf("the install's Lease: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get("http://" + probeAddr + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz never returned 200 (last err %v)", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// freeAddr returns a loopback address with a port that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}
