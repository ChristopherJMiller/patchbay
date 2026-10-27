package cli_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/cli"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// pushRenderer is a driver that renders message pages: the line it is
// given and the zone it is given, so a test can read both back.
type pushRenderer struct{ fakeDriver }

func (pushRenderer) RenderMessages(model string, lines []drivers.MessageLine, loc *time.Location) ([]byte, error) {
	var b strings.Builder
	b.WriteString("page " + model + " " + loc.String())
	for _, l := range lines {
		b.WriteString(" | " + l.Sent.In(loc).Format("15:04") + " " + l.Text)
	}
	return []byte(b.String()), nil
}

// pushEnv is a test env whose registry has a rendering driver for
// cisco-7945, with a Phone "desk" on it and "plain" on test-a (no
// renderer).
func pushEnv(t *testing.T, objs ...client.Object) *testEnv {
	t.Helper()
	desk := &v1alpha1.Phone{
		ObjectMeta: objectMeta("desk", testNow),
		Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: "0200E2E00001", Number: "100"},
	}
	plain := &v1alpha1.Phone{
		ObjectMeta: objectMeta("plain", testNow),
		Spec:       v1alpha1.PhoneSpec{Model: "test-a", MAC: "0200E2E00002", Number: "101"},
	}
	te := newTestEnv(t, append([]client.Object{desk, plain}, objs...)...)
	reg, err := drivers.NewRegistry(
		pushRenderer{fakeDriver{name: "pushcisco", models: []string{"cisco-7945"}}},
		fakeDriver{name: "plain", models: []string{"test-a"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	te.Registry = reg
	return te
}

func settingsMap(instance, json string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: instance + "-patchbay-settings", Namespace: testNamespace, Labels: map[string]string{
			cli.LabelPartOf: cli.PartOf, cli.LabelComponent: "controller", cli.LabelInstance: instance,
		}},
		Data: map[string]string{install.SettingsKey: json},
	}
}

func sum16(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}

func TestPhoneTestPush(t *testing.T) {
	te := pushEnv(t, settingsMap("patchbay", `{"timezone": "America/Los_Angeles"}`))
	te.agent.pushAnswer.Sent = true
	te.agent.pushAnswer.Phone = "status 0"
	err := cli.PhoneTestPush(context.Background(), te.Env, "desk", cli.TestPush{Text: "hi", Beep: true})
	if err != nil {
		t.Fatal(err)
	}
	pushes := te.agent.pushesSoFar()
	if len(pushes) != 1 {
		t.Fatalf("pushes = %+v", pushes)
	}
	// 09:30 UTC is 02:30 in Los Angeles in October (PDT).
	page := "page cisco-7945 America/Los_Angeles | 02:30 hi"
	p := pushes[0]
	if p.Device != "phone-desk" || p.Page != page || p.PageVersion != sum16(page) || !p.Beep || p.Clear || len(p.Execute) != 0 {
		t.Fatalf("push = %+v", p)
	}
	if got := te.out.String(); got != "pushed to desk: status 0\n" {
		t.Fatalf("out = %q", got)
	}
	if te.errOut.Len() != 0 {
		t.Fatalf("err = %q", te.errOut.String())
	}
}

func TestPhoneTestPushDefaultText(t *testing.T) {
	te := pushEnv(t, settingsMap("patchbay", `{"timezone": "UTC"}`))
	te.agent.pushAnswer = apiPushOK()
	if err := cli.PhoneTestPush(context.Background(), te.Env, "desk", cli.TestPush{}); err != nil {
		t.Fatal(err)
	}
	if p := te.agent.pushesSoFar()[0]; !strings.HasSuffix(p.Page, "09:30 patchbay test push") {
		t.Fatalf("page = %q", p.Page)
	}
}

func TestPhoneTestPushTimezoneUnreadable(t *testing.T) {
	for name, objs := range map[string][]client.Object{
		"no configmap": nil,
		"bad zone":     {settingsMap("patchbay", `{"timezone": "Mars/Base"}`)},
		"bad json":     {settingsMap("patchbay", `{`)},
	} {
		t.Run(name, func(t *testing.T) {
			te := pushEnv(t, objs...)
			te.agent.pushAnswer = apiPushOK()
			if err := cli.PhoneTestPush(context.Background(), te.Env, "desk", cli.TestPush{Text: "x"}); err != nil {
				t.Fatal(err)
			}
			if p := te.agent.pushesSoFar()[0]; p.Page != "page cisco-7945 UTC | 09:30 x" {
				t.Fatalf("page = %q", p.Page)
			}
			if n := strings.Count(te.errOut.String(), "\n"); n != 1 || !strings.Contains(te.errOut.String(), "UTC") {
				t.Fatalf("err = %q, want one note naming UTC", te.errOut.String())
			}
		})
	}
}

func TestPhoneTestPushTwoInstances(t *testing.T) {
	la := `{"timezone": "America/Los_Angeles"}`
	ny := `{"timezone": "America/New_York"}`
	unlabeled := &corev1.ConfigMap{
		ObjectMeta: objectMeta("stray-settings", testNow),
		Data:       map[string]string{install.SettingsKey: `{"timezone": "Asia/Tokyo"}`},
	}
	for _, tc := range []struct{ name, instance, want string }{
		{"no instance", "", "page cisco-7945 UTC | 09:30 x"},
		{"one", "one", "page cisco-7945 America/Los_Angeles | 02:30 x"},
		{"two", "two", "page cisco-7945 America/New_York | 05:30 x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := pushEnv(t, settingsMap("one", la), settingsMap("two", ny), unlabeled)
			te.Instance = tc.instance
			te.agent.pushAnswer = apiPushOK()
			if err := cli.PhoneTestPush(context.Background(), te.Env, "desk", cli.TestPush{Text: "x"}); err != nil {
				t.Fatal(err)
			}
			if p := te.agent.pushesSoFar()[0]; p.Page != tc.want {
				t.Fatalf("page = %q, want %q", p.Page, tc.want)
			}
			note := te.errOut.String()
			if (tc.instance == "") != strings.Contains(note, "--instance") {
				t.Fatalf("err = %q", note)
			}
		})
	}
}

func TestPhoneTestPushUnlabeledIgnored(t *testing.T) {
	te := pushEnv(t, &corev1.ConfigMap{
		ObjectMeta: objectMeta("stray-settings", testNow),
		Data:       map[string]string{install.SettingsKey: `{"timezone": "Asia/Tokyo"}`},
	})
	te.agent.pushAnswer = apiPushOK()
	if err := cli.PhoneTestPush(context.Background(), te.Env, "desk", cli.TestPush{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if p := te.agent.pushesSoFar()[0]; p.Page != "page cisco-7945 UTC | 09:30 x" || te.errOut.Len() == 0 {
		t.Fatalf("page = %q, err = %q", p.Page, te.errOut.String())
	}
}

func TestPhoneTestPushClearAndExecute(t *testing.T) {
	te := pushEnv(t)
	te.agent.pushAnswer = apiPushOK()
	ctx := context.Background()
	if err := cli.PhoneTestPush(ctx, te.Env, "desk", cli.TestPush{Clear: true}); err != nil {
		t.Fatal(err)
	}
	if err := cli.PhoneTestPush(ctx, te.Env, "desk", cli.TestPush{Execute: []string{"Play:Chime.raw", "Key:Messages"}}); err != nil {
		t.Fatal(err)
	}
	ps := te.agent.pushesSoFar()
	if len(ps) != 2 || !ps[0].Clear || ps[0].Page != "" || ps[0].PageVersion != "" || ps[0].Device != "phone-desk" {
		t.Fatalf("pushes = %+v", ps)
	}
	if ps[1].Page != "" || strings.Join(ps[1].Execute, ",") != "Play:Chime.raw,Key:Messages" {
		t.Fatalf("pushes = %+v", ps)
	}
}

func TestPhoneTestPushRefused(t *testing.T) {
	te := pushEnv(t)
	te.agent.pushAnswer.Error = "no contact for phone-desk"
	te.agent.pushAnswer.Phone = "error 4"
	err := cli.PhoneTestPush(context.Background(), te.Env, "desk", cli.TestPush{})
	if err == nil || !strings.Contains(err.Error(), "no contact for phone-desk") || !strings.Contains(err.Error(), "error 4") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(te.out.String(), "pushed to") {
		t.Fatalf("out = %q", te.out.String())
	}
}

func TestPhoneTestPushNotAPhone(t *testing.T) {
	te := pushEnv(t)
	ctx := context.Background()
	for name, want := range map[string]string{"nope": "not found", "plain": "test-a"} {
		err := cli.PhoneTestPush(ctx, te.Env, name, cli.TestPush{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: error = %v, want %q", name, err, want)
		}
	}
	if opens, _ := te.opener.counts(); opens != 0 || len(te.agent.pushesSoFar()) != 0 {
		t.Fatalf("opened the agent %d times", opens)
	}
}

func apiPushOK() (r agentapi.PushResponse) { r.Sent, r.Phone = true, "status 0"; return }
