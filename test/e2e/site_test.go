//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	agentapi "github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/test/e2e/fakephone/script"
)

// The site: the install's resources (docs/e2e.md, "Site resources"). The
// harness creates them after the chart, the controller compiles them, and
// the PBX pod starts on that bundle. TestDeskLoop owns desk and laptop
// (their phones are its own); the W2 tests share kitchen.
const (
	deskName      = "desk"
	deskMAC       = "0200E2E00001" // locally administered: no real phone has it
	deskNumber    = "100"
	deskLabel     = "Desk"
	kitchenName   = "kitchen"
	kitchenMAC    = "0200E2E00002"
	kitchenNumber = "101"
	kitchenLabel  = "Kitchen"
	laptopName    = "laptop"
	laptopNumber  = "150"
	laptopLabel   = "Laptop"
	personName    = "chris"
	personNumber  = "301"
	echoNumber    = "600"

	// unknownMAC asks for a SEP file no Phone has.
	unknownMAC = "0200E2E0FFFF"
)

var (
	// names are the install's object names (the release's fullname is the
	// release name: it contains the chart's).
	names = install.NewNames(release)

	deskID     = drivers.DeviceID(drivers.KindPhone, deskName)
	kitchenID  = drivers.DeviceID(drivers.KindPhone, kitchenName)
	laptopID   = drivers.DeviceID(drivers.KindSoftphone, laptopName)
	kitchenSEP = "SEP" + kitchenMAC + ".cnf.xml"
	amiRef     = names.AMIRef()
)

// siteResources are the install's Phones desk and kitchen, Softphone
// laptop, Person chris (301: desk, then laptop) and the site Streams
// (e2eStreams: tuner-a, tuner-b, radio-a, radio-b), as the harness writes
// them. The Streams are written at install, with the rest, so the kubelet
// has synced the bridge's <fullname>-streams long before the stream tests.
func siteResources() []client.Object {
	objs := []client.Object{
		&v1alpha1.Phone{
			ObjectMeta: metav1.ObjectMeta{Name: deskName, Namespace: namespace},
			Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: deskMAC, Number: deskNumber, Label: deskLabel},
		},
		&v1alpha1.Phone{
			ObjectMeta: metav1.ObjectMeta{Name: kitchenName, Namespace: namespace},
			Spec:       v1alpha1.PhoneSpec{Model: v1alpha1.PhoneModelCisco7945, MAC: kitchenMAC, Number: kitchenNumber, Label: kitchenLabel},
		},
		&v1alpha1.Softphone{
			ObjectMeta: metav1.ObjectMeta{Name: laptopName, Namespace: namespace},
			Spec:       v1alpha1.SoftphoneSpec{Number: laptopNumber, Label: laptopLabel},
		},
		&v1alpha1.Person{
			ObjectMeta: metav1.ObjectMeta{Name: personName, Namespace: namespace},
			Spec: v1alpha1.PersonSpec{Number: personNumber, Name: "Chris", Rings: []v1alpha1.RingTarget{
				{Phone: deskName}, {Softphone: laptopName},
			}},
		},
	}
	for _, s := range e2eStreams() {
		objs = append(objs, &s)
	}
	return objs
}

// applyResource creates obj, or, if it exists, puts its spec back to obj's
// (a reused cluster keeps a previous run's edits otherwise).
func (h *harness) applyResource(ctx context.Context, obj client.Object) error {
	cur := obj.DeepCopyObject().(client.Object)
	err := h.kube.Get(ctx, client.ObjectKeyFromObject(obj), cur)
	switch {
	case apierrors.IsNotFound(err):
		return h.kube.Create(ctx, obj)
	case err != nil:
		return err
	}
	same := false
	switch want := obj.(type) {
	case *v1alpha1.Phone:
		c := cur.(*v1alpha1.Phone)
		same = equality.Semantic.DeepEqual(c.Spec, want.Spec)
		c.Spec = want.Spec
	case *v1alpha1.Softphone:
		c := cur.(*v1alpha1.Softphone)
		same = equality.Semantic.DeepEqual(c.Spec, want.Spec)
		c.Spec = want.Spec
	case *v1alpha1.Person:
		c := cur.(*v1alpha1.Person)
		same = equality.Semantic.DeepEqual(c.Spec, want.Spec)
		c.Spec = want.Spec
	case *v1alpha1.Stream:
		c := cur.(*v1alpha1.Stream)
		same = equality.Semantic.DeepEqual(c.Spec, want.Spec)
		c.Spec = want.Spec
	default:
		return fmt.Errorf("applyResource: %T", obj)
	}
	if same {
		return nil
	}
	return h.kube.Update(ctx, cur)
}

// recreate deletes obj if it exists, waits until it is gone, and creates
// it: a test's own resources start fresh on a reused cluster.
func (h *harness) recreate(ctx context.Context, t *testing.T, obj client.Object) {
	t.Helper()
	h.deleteAndWait(ctx, t, obj)
	if err := h.kube.Create(ctx, obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

// deleteAndWait deletes obj and waits until it is gone.
func (h *harness) deleteAndWait(ctx context.Context, t *testing.T, obj client.Object) {
	t.Helper()
	cur := obj.DeepCopyObject().(client.Object)
	if err := h.kube.Delete(ctx, cur); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete %T %s: %v", obj, obj.GetName(), err)
	}
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		err := h.kube.Get(ctx, client.ObjectKeyFromObject(obj), cur)
		return apierrors.IsNotFound(err), nil
	})
	if err != nil {
		t.Fatalf("%T %s was not deleted: %v", obj, obj.GetName(), err)
	}
}

// deleteLeftovers deletes every Phone whose name starts with one of the
// prefixes, or is one without its trailing "-" (an older run's fixed
// name), and waits until each is gone: the Phones an interrupted run of a
// test with random names left on a reused cluster.
func (h *harness) deleteLeftovers(ctx context.Context, t *testing.T, prefixes ...string) {
	t.Helper()
	var l v1alpha1.PhoneList
	if err := h.kube.List(ctx, &l, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	for i := range l.Items {
		name := l.Items[i].Name
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) || name == strings.TrimSuffix(p, "-") {
				t.Logf("deleting Phone %s, left by an earlier run", name)
				h.deleteAndWait(ctx, t, &l.Items[i])
				break
			}
		}
	}
}

// waitForCRDs waits until the API server serves the chart's CRDs, so the
// site resources can be created right after helm returns.
func (h *harness) waitForCRDs(ctx context.Context) error {
	for _, l := range []client.ObjectList{
		&v1alpha1.PhoneList{}, &v1alpha1.SoftphoneList{}, &v1alpha1.PersonList{}, &v1alpha1.StreamList{},
	} {
		var last error
		err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
			last = h.kube.List(ctx, l, client.InNamespace(namespace))
			return last == nil, nil
		})
		if err != nil {
			return fmt.Errorf("the CRD of %T is not served: %w", l, errors.Join(err, last))
		}
	}
	return nil
}

// compiledBundle is the bundle the controller wrote, and the secret
// values behind it: pbx-config with the pbx-secrets of the same version.
type compiledBundle struct {
	compile.Bundle
	values map[compile.SecretRef][]byte
	// projected is pbx-secrets' data, what the agent resolves with.
	projected map[string][]byte
}

// compiled reads the controller's bundle, waiting up to a minute until
// pbx-secrets carries pbx-config's version (the controller writes the
// Secret first, so the two differ only for a moment).
func (h *harness) compiled(ctx context.Context, t *testing.T) compiledBundle {
	t.Helper()
	var out compiledBundle
	var why string
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		c, err := h.readCompiled(ctx)
		if err != nil {
			why = err.Error()
			return false, nil //nolint:nilerr // between the two writes: keep polling
		}
		out = c
		return true, nil
	})
	if err != nil {
		t.Fatalf("the compiled bundle: %v (%s)", err, why)
	}
	return out
}

func (h *harness) readCompiled(ctx context.Context) (compiledBundle, error) {
	cm, err := h.cs.CoreV1().ConfigMaps(namespace).Get(ctx, names.ConfigMap, metav1.GetOptions{})
	if err != nil {
		return compiledBundle{}, err
	}
	b, err := compile.BundleFromConfigMap(cm.Data, cm.BinaryData)
	if err != nil {
		return compiledBundle{}, err
	}
	sec, err := h.cs.CoreV1().Secrets(namespace).Get(ctx, names.ProjectedSecret, metav1.GetOptions{})
	if err != nil {
		return compiledBundle{}, err
	}
	if v := sec.Annotations[install.AnnotationBundleVersion]; v != b.Version {
		return compiledBundle{}, fmt.Errorf("pbx-secrets is at %.12s, pbx-config at %.12s", v, b.Version)
	}
	values, err := install.SecretValues(sec)
	if err != nil {
		return compiledBundle{}, err
	}
	return compiledBundle{Bundle: b, values: values, projected: sec.Data}, nil
}

// waitCompiled waits until the bundle the controller compiled last holds
// device id. Call it before waitApplied for a resource just created:
// until the bundle reconciler has written it, pbx-config is the previous
// bundle, applied long ago, and waitApplied would return at once.
func (h *harness) waitCompiled(ctx context.Context, t *testing.T, id string, timeout time.Duration) {
	t.Helper()
	why := ""
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		c, err := h.readCompiled(ctx)
		if err != nil {
			why = err.Error()
			return false, nil //nolint:nilerr // between the two writes: keep polling
		}
		_, ok := c.Devices[id]
		why = fmt.Sprintf("bundle %.12s does not hold %s", c.Version, id)
		return ok, nil
	})
	if err != nil {
		t.Fatalf("%s was not compiled: %v (%s)", id, err, why)
	}
}

// waitApplied waits until the agent has applied and verified the bundle
// the controller compiled last, and returns it.
func (h *harness) waitApplied(ctx context.Context, t *testing.T, timeout time.Duration) compiledBundle {
	t.Helper()
	var b compiledBundle
	h.eventually(ctx, t, "the compiled bundle applied", timeout, func(o agentapi.Observations) (bool, string) {
		c, err := h.readCompiled(ctx)
		if err != nil {
			return false, err.Error()
		}
		b = c
		a := o.Agent
		return a.AsteriskUp && a.AppliedVersion == c.Version && a.LastApplyVersion == c.Version && a.LastApplyError == "",
			fmt.Sprintf("compiled %.12s; agent applied %.12s, last apply %.12s %q", c.Version, a.AppliedVersion, a.LastApplyVersion, a.LastApplyError)
	})
	return b
}

// patchbay runs the e2e build of the CLI (.e2e/bin/patchbay, built with
// -tags e2e by `make e2e-images`) against the e2e cluster only:
// --kubeconfig .e2e/kubeconfig and -n patchbay-e2e on every run. It
// returns stdout and stderr together.
func (h *harness) patchbay(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	return h.run(ctx, h.cli, append(append([]string{"--kubeconfig", h.kubeconfig}, args...), "-n", namespace)...)
}

// cliTimeout bounds one CLI run.
const cliTimeout = 60 * time.Second

// phonePodLabel marks a scripted phone's pod and its script Secret.
const phonePodLabel = "e2e.patchbay.chrismiller.xyz/phone"

// startPhone starts a long-lived scripted phone (`fakephone run`): a pod on
// the pod network that may bind port 80 as non-root (the safe sysctl
// net.ipv4.ip_unprivileged_port_start=0), where the agent scrapes its web
// pages and the harness reads its State and sends it signals (pods/proxy).
// The script travels in a Secret (it can hold a provisioning URL), so no
// pod description shows it. The pod and its Secret are deleted when owner
// ends (the top-level test, so the phone outlives the assessment that
// started it; t is the assessment, for failures). It returns once the
// phone answers GET /fakephone/state.
func (h *harness) startPhone(ctx context.Context, t *testing.T, owner testing.TB, label string, sc script.Script) string {
	t.Helper()
	raw, err := sc.Encode()
	if err != nil {
		t.Fatalf("phone %s: %v", label, err)
	}
	suffix, err := randomHex(3)
	if err != nil {
		t.Fatal(err)
	}
	name := "fakephone-run-" + label + "-" + suffix
	labels := map[string]string{"app.kubernetes.io/name": "fakephone", phonePodLabel: label}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Data:       map[string][]byte{"script": []byte(raw)},
	}
	if _, err := h.cs.CoreV1().Secrets(namespace).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		t.Fatalf("phone %s: script Secret: %v", label, err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:         ptr.To[int64](1800),
			TerminationGracePeriodSeconds: ptr.To[int64](1),
			AutomountServiceAccountToken:  ptr.To(false),
			EnableServiceLinks:            ptr.To(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				Sysctls:        []corev1.Sysctl{{Name: "net.ipv4.ip_unprivileged_port_start", Value: "0"}},
			},
			Containers: []corev1.Container{{
				Name:            "fakephone",
				Image:           toolsImage,
				ImagePullPolicy: corev1.PullNever,
				Args:            []string{"run"},
				Env: []corev1.EnvVar{
					secretEnv(script.EnvScript, name, "script"),
					{Name: "FAKEPHONE_LOCAL_IP", ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
					}},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					ReadOnlyRootFilesystem:   ptr.To(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
	if _, err := h.cs.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("phone %s: %v", label, err)
	}
	owner.Cleanup(func() {
		ctx := context.WithoutCancel(ctx)
		_ = h.cs.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})
		_ = h.cs.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	})
	t.Logf("phone %s: pod %s (%s mode)", label, name, sc.Mode)
	var last error
	err = wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		p, err := h.cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
			return false, fmt.Errorf("pod %s ended (%s) before it ran", name, p.Status.Phase)
		}
		_, last = h.phoneState(ctx, name)
		return last == nil, nil
	})
	if err != nil {
		t.Fatalf("phone %s never answered: %v (last: %v)", label, err, last)
	}
	return name
}

// phoneState reads a phone's State (GET /fakephone/state).
func (h *harness) phoneState(ctx context.Context, pod string) (script.State, error) {
	r := h.cs.CoreV1().RESTClient().Get().Namespace(namespace).Resource("pods").
		Name(pod + ":80").SubResource("proxy").Suffix(script.PathState)
	raw, err := r.DoRaw(ctx)
	if err != nil {
		return script.State{}, err
	}
	var s script.State
	if err := json.Unmarshal(raw, &s); err != nil {
		return script.State{}, fmt.Errorf("state of %s: %w", pod, err)
	}
	return s, nil
}

// signal releases a phone's WaitSignal step (POST /fakephone/signal/<name>).
func (h *harness) signal(ctx context.Context, t *testing.T, pod, name string) {
	t.Helper()
	r := h.cs.CoreV1().RESTClient().Post().Namespace(namespace).Resource("pods").
		Name(pod + ":80").SubResource("proxy").Suffix(script.SignalPath(name))
	if _, err := r.DoRaw(ctx); err != nil {
		t.Fatalf("signal %s to %s: %v", name, pod, err)
	}
	t.Logf("signal %s -> %s", name, pod)
}

// waitPhone polls a phone's State every second until cond holds, for up to
// stateTimeout, and returns the State that satisfied it. A step that failed
// fails the test at once (the pod has ended).
func (h *harness) waitPhone(ctx context.Context, t *testing.T, pod, what string, cond func(script.State) (bool, string)) script.State {
	const timeout = stateTimeout
	t.Helper()
	var (
		st  script.State
		why string
	)
	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		s, err := h.phoneState(ctx, pod)
		if err != nil {
			p, perr := h.cs.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
			if perr == nil && (p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded) {
				return false, fmt.Errorf("the phone's pod ended (%s): %s", p.Status.Phase, h.podLogTail(ctx, pod))
			}
			why = err.Error()
			return false, nil
		}
		st = s
		ok, w := cond(s)
		why = w
		return ok, nil
	})
	if err != nil {
		t.Fatalf("%s: %v (%s)", what, err, why)
	}
	return st
}

// podLogTail is the last lines of a pod's log, for a failure message.
func (h *harness) podLogTail(ctx context.Context, pod string) string {
	raw, err := h.cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: "fakephone", TailLines: ptr.To[int64](20)}).DoRaw(ctx)
	if err != nil {
		return "(no log: " + err.Error() + ")"
	}
	return "\n" + indent(string(raw))
}

// deviceView is what the tests read of a Phone's or Softphone's status.
// String never shows the provisioning URL (it holds a token).
type deviceView struct {
	Kind, Name       string
	UID              types.UID
	Generation       int64
	State            string
	Conditions       []metav1.Condition
	HumanAction      string
	Address          string
	Firmware         string
	LastConfigFetch  *v1alpha1.ConfigFetch
	UpgradeStartedAt *metav1.Time
	LastAction       *v1alpha1.LastAction
	HandledRequest   string
	ProvisioningURL  string
}

func phoneView(p *v1alpha1.Phone) deviceView {
	d := deviceView{Kind: "Phone", Name: p.Name, UID: p.UID, Generation: p.Generation, State: p.Status.State,
		Conditions: p.Status.Conditions, HumanAction: p.Status.HumanAction, LastAction: p.Status.LastAction,
		HandledRequest: p.Status.HandledRequest}
	if o := p.Status.Observed; o != nil {
		d.Address, d.Firmware, d.LastConfigFetch, d.UpgradeStartedAt = o.Address, o.Firmware, o.LastConfigFetch, o.UpgradeStartedAt
	}
	return d
}

func softphoneView(s *v1alpha1.Softphone) deviceView {
	d := deviceView{Kind: "Softphone", Name: s.Name, UID: s.UID, Generation: s.Generation, State: s.Status.State,
		Conditions: s.Status.Conditions, HumanAction: s.Status.HumanAction, LastAction: s.Status.LastAction,
		HandledRequest: s.Status.HandledRequest, ProvisioningURL: s.Status.ProvisioningURL}
	if o := s.Status.Observed; o != nil {
		d.Address, d.LastConfigFetch = o.Address, o.LastConfigFetch
	}
	return d
}

// cond returns the condition of type ct, or a zero one.
func (d deviceView) cond(ct string) metav1.Condition {
	if c := meta.FindStatusCondition(d.Conditions, ct); c != nil {
		return *c
	}
	return metav1.Condition{Type: ct}
}

// is reports whether condition ct has status and reason ("" any reason).
func (d deviceView) is(ct string, status metav1.ConditionStatus, reason string) bool {
	c := d.cond(ct)
	return c.Status == status && (reason == "" || c.Reason == reason)
}

func (d deviceView) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: state %q, human action %q", d.Kind, d.Name, d.State, d.HumanAction)
	for _, c := range d.Conditions {
		fmt.Fprintf(&b, "; %s=%s %s %q", c.Type, c.Status, c.Reason, c.Message)
	}
	if d.LastAction != nil {
		fmt.Fprintf(&b, "; lastAction %s %.12s x%d", d.LastAction.Kind, d.LastAction.Version, d.LastAction.Attempts)
	}
	if d.LastConfigFetch != nil {
		fmt.Fprintf(&b, "; lastConfigFetch %.12s", d.LastConfigFetch.Version)
	}
	if d.ProvisioningURL != "" {
		b.WriteString("; provisioningURL set")
	}
	return b.String()
}

// tracker follows one Phone or Softphone through a watch, keeping its
// latest status and every state it went through.
type tracker struct {
	h          *harness
	kind, name string

	mu      sync.Mutex
	cur     deviceView
	have    bool
	states  []string // each state seen, consecutive repeats dropped
	changed chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
}

// track starts following the device kind ("Phone" or "Softphone") name;
// it stops when owner ends (a top-level test, to follow a device across
// assessments).
func (h *harness) track(ctx context.Context, owner testing.TB, kind, name string) *tracker {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	tr := &tracker{h: h, kind: kind, name: name, changed: make(chan struct{}), cancel: cancel, done: make(chan struct{})}
	go tr.run(ctx)
	owner.Cleanup(tr.stop)
	return tr
}

func (tr *tracker) stop() {
	tr.cancel()
	<-tr.done
}

func (tr *tracker) run(ctx context.Context) {
	defer close(tr.done)
	for ctx.Err() == nil {
		if err := tr.watchOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("tracker %s %s: %v (watching again)", tr.kind, tr.name, err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (tr *tracker) watchOnce(ctx context.Context) error {
	var list client.ObjectList = &v1alpha1.PhoneList{}
	if tr.kind == "Softphone" {
		list = &v1alpha1.SoftphoneList{}
	}
	w, err := tr.h.kubeWatch.Watch(ctx, list, client.InNamespace(namespace),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("metadata.name", tr.name)})
	if err != nil {
		return err
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.ResultChan():
			if !ok {
				return errors.New("watch closed")
			}
			switch ev.Type {
			case watch.Added, watch.Modified:
				switch o := ev.Object.(type) {
				case *v1alpha1.Phone:
					tr.update(phoneView(o), true)
				case *v1alpha1.Softphone:
					tr.update(softphoneView(o), true)
				}
			case watch.Deleted:
				tr.update(deviceView{}, false)
			case watch.Error:
				return fmt.Errorf("watch error: %w", apierrors.FromObject(ev.Object))
			case watch.Bookmark:
			}
		}
	}
}

func (tr *tracker) update(d deviceView, present bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.cur, tr.have = d, present
	if present && d.State != "" && (len(tr.states) == 0 || tr.states[len(tr.states)-1] != d.State) {
		tr.states = append(tr.states, d.State)
	}
	close(tr.changed)
	tr.changed = make(chan struct{})
}

// seen returns every state seen so far.
func (tr *tracker) seen() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return slices.Clone(tr.states)
}

// wait blocks until cond holds for the device's latest status (checked on
// each change, and every second for conditions that move with time), for
// up to timeout, and returns that status.
func (tr *tracker) wait(ctx context.Context, t *testing.T, what string, timeout time.Duration,
	cond func(deviceView) (bool, string),
) deviceView {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	why := "never seen"
	start := time.Now()
	for {
		tr.mu.Lock()
		cur, have, changed := tr.cur, tr.have, tr.changed
		tr.mu.Unlock()
		if have {
			ok, w := cond(cur)
			if ok {
				t.Logf("%s %s: %s (%s)", tr.kind, tr.name, what, time.Since(start).Round(100*time.Millisecond))
				return cur
			}
			why = w + "\n    " + cur.String()
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s %s: %s: %v: %s", tr.kind, tr.name, what, context.Cause(ctx), why)
		case <-deadline.C:
			t.Fatalf("%s %s: %s: not within %s: %s\n    states seen: %v", tr.kind, tr.name, what, timeout, why, tr.seen())
		case <-changed:
		case <-tick.C:
		}
	}
}

// state is a wait condition: the device is in state s.
func state(s drivers.State) func(deviceView) (bool, string) {
	return func(d deviceView) (bool, string) { return d.State == string(s), "state " + d.State }
}

// wipeE2ELoad deletes the e2e load's files from the firmware volume (on the
// kind node, where the local-path provisioner keeps it), so a test can
// stage it again; it waits until the agent's store sensor no longer
// reports any of them.
func (h *harness) wipeE2ELoad(ctx context.Context, t *testing.T, load drivers.FirmwareLoad) {
	t.Helper()
	pvc, err := h.cs.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, release+"-firmware", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pv, err := h.cs.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var dir string
	switch {
	case pv.Spec.HostPath != nil:
		dir = pv.Spec.HostPath.Path
	case pv.Spec.Local != nil:
		dir = pv.Spec.Local.Path
	default:
		t.Fatalf("PV %s is neither hostPath nor local", pv.Name)
	}
	if _, err := h.run(ctx, "docker", "exec", h.nodeName, "rm", "-rf", "--", dir+"/firmware/"+load.Name); err != nil {
		t.Fatal(err)
	}
	h.eventually(ctx, t, "the e2e load gone from the store", 30*time.Second, func(o agentapi.Observations) (bool, string) {
		var present []string
		for _, f := range o.Firmware.Files {
			if f.Load == load.Name && f.Present {
				present = append(present, f.Name)
			}
		}
		return len(present) == 0, fmt.Sprintf("still present: %v", present)
	})
}

// kitchenContact serializes the W2 tests that register as kitchen
// (TestRegister, TestResyncNotify): its AOR keeps one contact
// (remove_existing), so one test's REGISTER would take the other's NOTIFY.
// The tests that only call as kitchen need no turn.
var kitchenContact sync.Mutex
