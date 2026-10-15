package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// How softphone qr --reissue waits for the controller's answer.
const (
	// ReissueWait is how long --reissue waits for a new provisioning URL.
	ReissueWait = 30 * time.Second
	// ReissuePoll is how often it reads the Softphone meanwhile.
	ReissuePoll = time.Second
)

// SoftphoneQR prints a Softphone's provisioning URL, its QR code and a note
// that it works once. This is the one command that prints a provisioning
// URL: it is what the user asked for.
//
// With reissue it first asks the controller for a fresh token (the
// reissue-requested annotation, set to the clock's time) and waits, every
// ReissuePoll for at most ReissueWait, until the controller has handled
// that request, status.provisioningURL is a new URL, and the config that
// serves it is live (ConfigCurrent's reason is not ApplyPending). Without
// it, a Softphone with no URL is an error naming its state.
func SoftphoneQR(ctx context.Context, env *Env, name string, reissue bool) error {
	var sp v1alpha1.Softphone
	if err := getNamed(ctx, env, "softphone", name, &sp); err != nil {
		return err
	}
	url := sp.Status.ProvisioningURL
	if reissue {
		before := url
		value := requestTime(env)
		if err := requestAnnotation(ctx, env.Kube, &sp, v1alpha1.AnnotationReissueRequested, value); err != nil {
			return fmt.Errorf("softphone %s: requesting a reissue: %w", name, err)
		}
		p := &printer{w: env.Err}
		p.printf("reissue requested (%s); waiting up to %s for a new provisioning URL\n", value, ReissueWait)
		var err error
		if url, err = waitReissued(ctx, env, name, value, before); err != nil {
			return err
		}
	} else if url == "" {
		return fmt.Errorf("softphone %s has no provisioning URL (state %s): patchbay softphone qr %s --reissue makes one",
			name, displayState(sp.Status.State), name)
	}
	var code bytes.Buffer
	if err := RenderQR(&code, url); err != nil {
		return fmt.Errorf("softphone %s: %w", name, err)
	}
	p := &printer{w: env.Out}
	p.printf("%s\n\n%s\nThis code works once; patchbay softphone qr %s --reissue makes a new one.\n", url, code.String(), name)
	return p.err
}

// waitReissued reads the Softphone every ReissuePoll until the controller
// has answered the request value with a new, live URL, or ReissueWait has
// passed. Errors describe the state, never a URL.
func waitReissued(ctx context.Context, env *Env, name, value, before string) (string, error) {
	deadline := env.Clock.Now().Add(ReissueWait)
	for {
		var sp v1alpha1.Softphone
		if err := getNamed(ctx, env, "softphone", name, &sp); err != nil {
			return "", err
		}
		waiting := reissueWaitingOn(&sp.Status, value, before)
		if waiting == "" {
			return sp.Status.ProvisioningURL, nil
		}
		if !env.Clock.Now().Before(deadline) {
			return "", fmt.Errorf("softphone %s: no new provisioning URL within %s: state %s; %s",
				name, ReissueWait, displayState(sp.Status.State), waiting)
		}
		t := env.Clock.NewTimer(ReissuePoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", ctx.Err()
		case <-t.C():
		}
	}
}

// reissueWaitingOn says what a reissue still waits for, or "" when the
// status answers it.
func reissueWaitingOn(s *v1alpha1.SoftphoneStatus, value, before string) string {
	switch {
	case s.HandledRequest != value:
		return "the controller has not handled the request yet"
	case s.ProvisioningURL == "" || s.ProvisioningURL == before:
		return "the new token is not compiled yet"
	}
	if c := meta.FindStatusCondition(s.Conditions, v1alpha1.ConditionConfigCurrent); c != nil && c.Reason == v1alpha1.ReasonApplyPending {
		return "the agent does not serve the new token yet (ConfigCurrent " + v1alpha1.ReasonApplyPending + ")"
	}
	return ""
}

// PhoneResync asks the controller to make a phone re-read its config: the
// resync-requested annotation, set to the clock's time. The controller
// resyncs only a Registered or ConfigStale phone (it marks any other
// request handled without one), so for any other state it warns on Err.
func PhoneResync(ctx context.Context, env *Env, name string) error {
	var ph v1alpha1.Phone
	if err := getNamed(ctx, env, "phone", name, &ph); err != nil {
		return err
	}
	value := requestTime(env)
	if err := requestAnnotation(ctx, env.Kube, &ph, v1alpha1.AnnotationResyncRequested, value); err != nil {
		return fmt.Errorf("phone %s: requesting a resync: %w", name, err)
	}
	if s := drivers.State(ph.Status.State); s != drivers.StateRegistered && s != drivers.StateConfigStale {
		p := &printer{w: env.Err}
		p.printf("warning: phone %s is %s; the controller resyncs only a Registered or ConfigStale phone, so it will mark this request handled without a resync\n",
			name, displayState(ph.Status.State))
		if p.err != nil {
			return p.err
		}
	}
	p := &printer{w: env.Out}
	p.printf("resync requested for phone %s (%s)\n", name, value)
	return p.err
}

// requestTime is a request annotation's value: the clock's time, UTC, in
// RFC 3339 with nanoseconds, so every request is a new value.
func requestTime(env *Env) string {
	return env.Clock.Now().UTC().Format(time.RFC3339Nano)
}

// requestAnnotation sets one annotation with a JSON merge patch, leaving
// the others alone.
func requestAnnotation(ctx context.Context, kube client.Client, obj client.Object, key, value string) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{key: value}},
	})
	if err != nil {
		return err
	}
	return kube.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch))
}

// getNamed reads one resource of the namespace; NotFound names the
// namespace, so a missing -n is obvious.
func getNamed(ctx context.Context, env *Env, kind, name string, obj client.Object) error {
	err := env.Kube.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: name}, obj)
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%s %s not found in namespace %s", kind, name, env.Namespace)
	default:
		return fmt.Errorf("reading %s %s in namespace %s: %w", strings.ToLower(kind), name, env.Namespace, err)
	}
}
