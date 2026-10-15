//go:build e2e

package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
)

// TestPBXDown: with Asterisk down the agent still answers the controller,
// so every device reads Registered=False PBXDown (design §7), never
// Ready=Unknown AgentUnreachable; and once Asterisk is back, PBXDown
// clears. The PBX pod is NotReady while Asterisk is down (its readiness
// probe), and the controller reaches the agent only through the agent
// Service, so this holds only because that Service publishes not-ready
// addresses (the final review's finding).
//
// Asterisk is stopped (crictl, like killAgent) every time it comes back,
// until the controller has shown PBXDown; the kubelet's restart backoff
// then brings it back within a minute or so. Not parallel: every other
// test needs Asterisk. Its astdb lives on an emptyDir, so registrations
// survive the container restarts.
func TestPBXDown(t *testing.T) {
	f := features.New("Asterisk down").
		Assess("devices show PBXDown, not AgentUnreachable, then recover", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			tr := h.track(ctx, t, "Phone", kitchenName)
			healthy := func(d deviceView) (bool, string) {
				return len(d.Conditions) > 0 && !d.is(v1alpha1.ConditionRegistered, metav1.ConditionFalse, v1alpha1.ReasonPBXDown) &&
					!d.is(v1alpha1.ConditionReady, metav1.ConditionUnknown, ""), "not decided yet, or Unknown"
			}
			tr.wait(ctx, t, "decided, not Unknown", 2*time.Minute, healthy)

			stopCtx, stop := context.WithCancel(ctx)
			errc := make(chan error, 1)
			go func() { errc <- keepAsteriskDown(stopCtx) }()
			var unreachable string
			tr.wait(ctx, t, "Registered=False PBXDown", 3*time.Minute, func(d deviceView) (bool, string) {
				if d.is(v1alpha1.ConditionReady, metav1.ConditionUnknown, v1alpha1.ReasonAgentUnreachable) && unreachable == "" {
					unreachable = d.String()
				}
				return d.is(v1alpha1.ConditionRegistered, metav1.ConditionFalse, v1alpha1.ReasonPBXDown), "not PBXDown"
			})
			stop()
			if err := <-errc; err != nil {
				t.Errorf("stopping Asterisk: %v", err)
			}
			if unreachable != "" {
				t.Errorf("the agent read as unreachable while only Asterisk was down: %s", unreachable)
			}

			// The kubelet brings Asterisk back after its restart backoff.
			err := wait.PollUntilContextTimeout(ctx, time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
				for _, s := range pbxPod(ctx, t).Status.ContainerStatuses {
					if s.Name == "asterisk" {
						return s.Ready, nil
					}
				}
				return false, nil
			})
			if err != nil {
				t.Fatalf("Asterisk did not come back ready: %v", err)
			}
			h.eventually(ctx, t, "the agent's AMI session up again", 2*time.Minute, func(o agentapi.Observations) (bool, string) {
				return o.Agent.AsteriskUp, "Asterisk down"
			})
			tr.wait(ctx, t, "PBXDown cleared", 2*time.Minute, healthy)
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// keepAsteriskDown stops the asterisk container whenever it is running,
// until ctx is done.
func keepAsteriskDown(ctx context.Context) error {
	stopped := 0
	for {
		id, err := h.run(ctx, "docker", "exec", h.nodeName, "crictl", "ps", "--name", "^asterisk$", "--state", "Running", "-q")
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			return err
		}
		if id = strings.TrimSpace(id); id != "" {
			if _, err := h.run(ctx, "docker", "exec", h.nodeName, "crictl", "stop", "--timeout", "0", id); err != nil && ctx.Err() == nil {
				return err
			}
			stopped++
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
		if ctx.Err() != nil {
			break
		}
	}
	if stopped == 0 {
		return errors.New("Asterisk was never found running")
	}
	return nil
}
