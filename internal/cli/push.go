package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// DefaultPushText is what test-push shows when given no --text.
const DefaultPushText = "patchbay test push"

// pushTimeout bounds the request: the agent waits on the phone, which can
// take a few seconds.
const pushTimeout = 30 * time.Second

// TestPush is what `patchbay phone test-push` sends.
type TestPush struct {
	// Text is the one message on the page (DefaultPushText when empty).
	Text string
	// Beep chimes ahead of the page.
	Beep bool
	// Clear drops the page and returns the phone to idle; no page is sent.
	Clear bool
	// Execute is raw URIs to push, and nothing else.
	Execute []string
}

// PhoneTestPush is a diagnostic: it asks the agent directly (the second
// CLI to agent call, after FirmwareLoad) to push to one Phone, outside the
// controller's decisions. By default it renders a page of one message, sent
// now, in the install's timezone, and pushes that. The phone must exist and
// its driver must render message pages; both are checked before the agent
// is opened. It prints "pushed to <phone>: <the phone's answer>", or fails
// with the agent's reason and the phone's answer.
func PhoneTestPush(ctx context.Context, env *Env, name string, opts TestPush) error {
	var phone v1alpha1.Phone
	if err := getNamed(ctx, env, "phone", name, &phone); err != nil {
		return err
	}
	if env.Registry == nil {
		return errors.New("phone test-push: no driver registry")
	}
	model := string(phone.Spec.Model)
	drv, err := env.Registry.ForModel(model)
	if err != nil {
		return fmt.Errorf("phone %s: %w", name, err)
	}
	renderer, ok := drv.(drivers.MessageRenderer)
	if !ok {
		return fmt.Errorf("phone %s: model %s cannot be pushed to: its driver does not render message pages", name, model)
	}
	req := agentapi.PushRequest{Device: drivers.PhoneDevice(&phone).ID}
	switch {
	case len(opts.Execute) > 0:
		req.Execute = opts.Execute
	case opts.Clear:
		req.Clear = true
		req.Beep = opts.Beep
	default:
		text := opts.Text
		if text == "" {
			text = DefaultPushText
		}
		// A test page is no Message and has no expiry (Expires zero): its
		// time shows as a short-lived message's.
		line := drivers.MessageLine{Sent: env.Clock.Now(), Text: text}
		page, err := renderer.RenderMessages(model, []drivers.MessageLine{line}, installZone(ctx, env))
		if err != nil {
			return fmt.Errorf("phone %s: rendering the page: %w", name, err)
		}
		sum := sha256.Sum256(page)
		req.Page = string(page)
		req.PageVersion = hex.EncodeToString(sum[:])[:16]
		req.Beep = opts.Beep
	}
	res, err := sendPush(ctx, env, req)
	if err != nil {
		return err
	}
	if !res.Sent {
		reason := res.Error
		if reason == "" {
			reason = "no reason given"
		}
		if res.Phone != "" {
			reason += " (the phone answered: " + printable(res.Phone) + ")"
		}
		return fmt.Errorf("phone %s: not pushed: %s", name, printable(reason))
	}
	p := &printer{w: env.Out}
	p.printf("pushed to %s: %s\n", name, printable(res.Phone))
	return p.err
}

func sendPush(ctx context.Context, env *Env, req agentapi.PushRequest) (agentapi.PushResponse, error) {
	var res agentapi.PushResponse
	if env.Agent == nil {
		return res, errors.New("phone test-push: no way to reach the agent")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return res, err
	}
	conn, err := env.Agent.OpenAgent(ctx)
	if err != nil {
		return res, fmt.Errorf("reaching the agent: %w", err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	hres, err := agentRequest(ctx, conn, http.MethodPost, agentapi.PathActionPush, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return res, fmt.Errorf("POST %s: %w", agentapi.PathActionPush, err)
	}
	defer func() { _ = hres.Body.Close() }()
	if hres.StatusCode != http.StatusOK {
		return res, fmt.Errorf("POST %s: %d %s", agentapi.PathActionPush, hres.StatusCode, printable(answerError(hres)))
	}
	if err := json.NewDecoder(io.LimitReader(hres.Body, maxAnswerBody)).Decode(&res); err != nil {
		return res, fmt.Errorf("POST %s: %w", agentapi.PathActionPush, err)
	}
	return res, nil
}

// installZone is the install's timezone, from <fullname>-settings. When it
// cannot be read it is UTC, with one note on env.Err.
func installZone(ctx context.Context, env *Env) *time.Location {
	loc, why := readZone(ctx, env)
	if why != "" {
		p := &printer{w: env.Err}
		p.printf("note: %s; the page shows times in UTC\n", why)
		return time.UTC
	}
	return loc
}

// componentController is the component label of the chart's controller
// objects, the settings ConfigMap among them.
const componentController = "controller"

func readZone(ctx context.Context, env *Env) (*time.Location, string) {
	sel := client.MatchingLabels{LabelPartOf: PartOf, LabelComponent: componentController}
	if env.Instance != "" {
		sel[LabelInstance] = env.Instance
	}
	var cms corev1.ConfigMapList
	if err := env.Kube.List(ctx, &cms, client.InNamespace(env.Namespace), sel); err != nil {
		return nil, "could not list the settings ConfigMaps: " + err.Error()
	}
	var found []corev1.ConfigMap
	for _, cm := range cms.Items {
		if _, ok := cm.Data[install.SettingsKey]; ok {
			found = append(found, cm)
		}
	}
	switch {
	case len(found) == 0:
		return nil, "no settings ConfigMap in namespace " + env.Namespace + " to read the timezone from"
	case len(found) > 1:
		return nil, fmt.Sprintf("%d installs in namespace %s; --instance names the one to read the timezone from", len(found), env.Namespace)
	}
	cm := found[0]
	var s struct {
		Timezone string `json:"timezone"`
	}
	if err := json.Unmarshal([]byte(cm.Data[install.SettingsKey]), &s); err != nil {
		return nil, cm.Name + " " + install.SettingsKey + " does not parse: " + err.Error()
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil || s.Timezone == "" {
		return nil, fmt.Sprintf("%s has no usable timezone (%q)", cm.Name, s.Timezone)
	}
	return loc, ""
}
