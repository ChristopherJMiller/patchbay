// Package secrets creates and completes every Secret one patchbay install
// needs (docs/plans/release-a.md W3.2, §6.2.1 "Secret writers" and "Agent
// token"):
//
//   - per Phone and Softphone, patchbay-<device ID>
//     (compile.DeviceSecretName), controlled by the resource (controller
//     and blockOwnerDeletion, so the garbage collector removes it with the
//     resource): sip-password under the model's driver PasswordPolicy, and
//     for a Softphone provisioning-token (install.GenerateProvisioningToken);
//   - <fullname>-ami (password, install.GenerateAMIPassword) and
//     <fullname>-agent-token (token, install.GenerateAgentToken), unowned.
//
// The Manager is the only creator of these Secrets (the bundle reconciler
// calls Ensure on every reconcile). It creates a missing Secret, fills a
// missing or empty key, and never changes a non-empty value, so a
// controller restart rotates nothing and a recreated Phone keeps its
// password; a deleted Secret is generated anew. Every value comes from the
// install package's generators on crypto/rand (Config.Rand in tests).
//
// A value the controller would never have generated (a character outside
// the key's alphabet, which could reach a device's config or manager.conf
// unescaped), a Secret that is not Opaque, an immutable Secret that needs a
// key, or a device Secret controlled by another object is refused with an
// error wrapping ErrInvalid: a human must fix or delete it. Nothing this
// package logs or returns ever holds a value.
package secrets

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/controller/install"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// ErrInvalid marks a Secret the install needs that the controller cannot
// use or complete as it is, and will not overwrite: a human must fix or
// delete it. The error names the Secret and key, never a value.
var ErrInvalid = errors.New("secrets: a Secret the install needs is unusable as it is")

// Config configures a Manager.
type Config struct {
	// Client is the manager's client: cached reads, writes to the API
	// server. Its scheme must hold the patchbay API (owner references).
	Client client.Client
	// Namespace is the PBX's namespace, where every Secret lives.
	Namespace string
	// Names are the install's object names.
	Names install.Names
	// Registry gives each model's PasswordPolicy.
	Registry *drivers.Registry
	// Rand is the randomness every value is drawn from. Nil:
	// crypto/rand.Reader.
	Rand io.Reader
	// Log is the Manager's logger. Values are never logged.
	Log logr.Logger
}

// Manager creates, completes and reads the install's Secrets.
type Manager struct {
	c         client.Client
	namespace string
	names     install.Names
	reg       *drivers.Registry
	rand      io.Reader
	log       logr.Logger

	mu       sync.Mutex
	current  string // the agent token Tokens returned last
	previous string // the one it replaced
}

// New checks the configuration and returns a Manager.
func New(cfg Config) (*Manager, error) {
	var errs []error
	if cfg.Client == nil {
		errs = append(errs, errors.New("secrets: no client"))
	}
	if cfg.Namespace == "" {
		errs = append(errs, errors.New("secrets: no namespace"))
	}
	if err := cfg.Names.Validate(); err != nil {
		errs = append(errs, err)
	}
	if cfg.Registry == nil {
		errs = append(errs, errors.New("secrets: no driver registry"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	return &Manager{
		c: cfg.Client, namespace: cfg.Namespace, names: cfg.Names, reg: cfg.Registry,
		rand: cfg.Rand, log: cfg.Log,
	}, nil
}

// field is one generated key of a Secret.
type field struct {
	key string
	// valid reports whether a stored value is one the controller could
	// have generated (its alphabet; lengths are not checked, so a policy
	// change keeps working passwords).
	valid func(v []byte) bool
	// empty reports whether a stored value counts as missing.
	empty    func(v []byte) bool
	generate func() (string, error)
}

// spec is one Secret Ensure keeps complete.
type spec struct {
	name   string
	owner  client.Object // nil: unowned
	fields []field
}

// Ensure creates or completes every Secret the install needs and returns
// the values Compile reads (compile.Inputs.Secrets): the PasswordRef of
// every Phone and Softphone whose model a driver serves, the
// ProvisioningTokenRef of every Softphone, and the AMI password. It also
// ensures the agent token. A device whose model no driver serves gets no
// password (and, for a Phone, no Secret): it is never rendered.
//
// A Create that finds the Secret already there (the cache lags) is an
// error, so the next reconcile reads it; so is any other API error. A
// Secret it cannot use is an error wrapping ErrInvalid.
func (m *Manager) Ensure(ctx context.Context, phones []v1alpha1.Phone, softphones []v1alpha1.Softphone) (map[compile.SecretRef][]byte, error) {
	out := map[compile.SecretRef][]byte{}

	ami, err := m.ensure(ctx, spec{name: m.names.AMISecret, fields: []field{
		alphanumeric(install.AMIPasswordKey, func() (string, error) { return install.GenerateAMIPassword(m.rand) }),
	}})
	if err != nil {
		return nil, err
	}
	out[m.names.AMIRef()] = ami[install.AMIPasswordKey]

	if _, err := m.ensure(ctx, spec{name: m.names.AgentTokenSecret, fields: []field{{
		key:      install.AgentTokenKey,
		valid:    func(v []byte) bool { return validToken(strings.TrimSpace(string(v))) },
		empty:    func(v []byte) bool { return strings.TrimSpace(string(v)) == "" },
		generate: func() (string, error) { return install.GenerateAgentToken(m.rand) },
	}}}); err != nil {
		return nil, err
	}

	type device struct {
		id     string
		owner  client.Object
		model  string
		tokens bool
	}
	var devices []device
	for i := range phones {
		p := &phones[i]
		devices = append(devices, device{id: drivers.DeviceID(drivers.KindPhone, p.Name), owner: p, model: string(p.Spec.Model)})
	}
	for i := range softphones {
		s := &softphones[i]
		devices = append(devices, device{id: drivers.DeviceID(drivers.KindSoftphone, s.Name), owner: s, model: drivers.ModelSoftphone, tokens: true})
	}
	slices.SortFunc(devices, func(a, b device) int { return strings.Compare(a.id, b.id) })

	for _, d := range devices {
		var fields []field
		policy, served := m.policy(d.model)
		if served {
			fields = append(fields, field{
				key:      compile.SecretKeySIPPassword,
				valid:    within(policy.Alphabet),
				empty:    func(v []byte) bool { return len(v) == 0 },
				generate: func() (string, error) { return install.GeneratePassword(m.rand, policy) },
			})
		}
		if d.tokens {
			fields = append(fields, alphanumeric(compile.SecretKeyProvisioningToken,
				func() (string, error) { return install.GenerateProvisioningToken(m.rand) }))
		}
		if len(fields) == 0 {
			continue
		}
		data, err := m.ensure(ctx, spec{name: compile.DeviceSecretName(d.id), owner: d.owner, fields: fields})
		if err != nil {
			return nil, err
		}
		if served {
			out[compile.PasswordRef(d.id)] = data[compile.SecretKeySIPPassword]
		}
		if d.tokens {
			out[compile.ProvisioningTokenRef(d.id)] = data[compile.SecretKeyProvisioningToken]
		}
	}
	return out, nil
}

// policy is a model's password policy, if a driver serves the model.
func (m *Manager) policy(model string) (drivers.PasswordPolicy, bool) {
	drv, err := m.reg.ForModel(model)
	if err != nil {
		return drivers.PasswordPolicy{}, false
	}
	p, ok := drv.Profile(model)
	if !ok {
		return drivers.PasswordPolicy{}, false
	}
	return p.Provisioning.Password, true
}

// ensure creates the Secret, or completes and adopts it, and returns its
// data.
func (m *Manager) ensure(ctx context.Context, s spec) (map[string][]byte, error) {
	log := m.log.WithValues("secret", s.name)
	var cur corev1.Secret
	err := m.c.Get(ctx, client.ObjectKey{Namespace: m.namespace, Name: s.name}, &cur)
	switch {
	case apierrors.IsNotFound(err):
		return m.create(ctx, log, s)
	case err != nil:
		return nil, fmt.Errorf("secrets: reading Secret %s: %w", s.name, err)
	}

	if cur.Type != "" && cur.Type != corev1.SecretTypeOpaque {
		return nil, fmt.Errorf("%w: Secret %s has type %s, not %s: delete it and the controller creates a new one",
			ErrInvalid, s.name, cur.Type, corev1.SecretTypeOpaque)
	}
	want := cur.DeepCopy()
	var filled []string
	for _, f := range s.fields {
		v := want.Data[f.key]
		switch {
		case f.empty(v):
			gen, err := f.generate()
			if err != nil {
				return nil, fmt.Errorf("secrets: generating Secret %s key %s: %w", s.name, f.key, err)
			}
			if want.Data == nil {
				want.Data = map[string][]byte{}
			}
			want.Data[f.key] = []byte(gen)
			filled = append(filled, f.key)
		case !f.valid(v):
			return nil, fmt.Errorf("%w: Secret %s key %s holds characters the controller never generates "+
				"(it could reach a config file unescaped): delete the key, or the Secret, and the controller generates a new value",
				ErrInvalid, s.name, f.key)
		}
	}
	if len(filled) > 0 && cur.Immutable != nil && *cur.Immutable {
		return nil, fmt.Errorf("%w: Secret %s is immutable and lacks key(s) %s: delete it and the controller creates a new one",
			ErrInvalid, s.name, strings.Join(filled, ", "))
	}
	if want.Labels == nil {
		want.Labels = map[string]string{}
	}
	for k, v := range install.ManagedLabels() {
		// Add, never overwrite: a Secret someone else manages keeps its
		// managed-by.
		if _, ok := want.Labels[k]; !ok {
			want.Labels[k] = v
		}
	}
	adopted := false
	if s.owner != nil {
		before := metav1.GetControllerOf(want)
		if err := controllerutil.SetControllerReference(s.owner, want, m.c.Scheme()); err != nil {
			var owned *controllerutil.AlreadyOwnedError
			if errors.As(err, &owned) {
				return nil, fmt.Errorf("%w: Secret %s is controlled by %s %s, not by %s %s",
					ErrInvalid, s.name, owned.Owner.Kind, owned.Owner.Name, kindOf(s.owner), s.owner.GetName())
			}
			return nil, fmt.Errorf("secrets: Secret %s: %w", s.name, err)
		}
		adopted = before == nil || before.UID != s.owner.GetUID()
	}
	if equality.Semantic.DeepEqual(&cur, want) {
		return want.Data, nil
	}
	if err := m.c.Update(ctx, want); err != nil {
		return nil, fmt.Errorf("secrets: updating Secret %s: %w", s.name, err)
	}
	log.Info("updated Secret", "generatedKeys", filled, "adopted", adopted)
	return want.Data, nil
}

// create makes the Secret with every field generated.
func (m *Manager) create(ctx context.Context, log logr.Logger, s spec) (map[string][]byte, error) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: m.namespace, Labels: install.ManagedLabels()},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{},
	}
	for _, f := range s.fields {
		v, err := f.generate()
		if err != nil {
			return nil, fmt.Errorf("secrets: generating Secret %s key %s: %w", s.name, f.key, err)
		}
		sec.Data[f.key] = []byte(v)
	}
	if s.owner != nil {
		if err := controllerutil.SetControllerReference(s.owner, sec, m.c.Scheme()); err != nil {
			return nil, fmt.Errorf("secrets: Secret %s: %w", s.name, err)
		}
	}
	if err := m.c.Create(ctx, sec); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("secrets: Secret %s already exists but is not in the cache yet (it is read on the next reconcile): %w", s.name, err)
		}
		return nil, fmt.Errorf("secrets: creating Secret %s: %w", s.name, err)
	}
	log.Info("created Secret", "keys", slices.Sorted(maps.Keys(sec.Data)))
	return sec.Data, nil
}

// Tokens implements agentclient.TokenSource: the agent token in the cache
// (white space trimmed, as the agent reads its file) and the one it
// replaced, if this process saw it change ("" otherwise). A missing
// Secret, an empty token or one that cannot be a bearer token is an error.
func (m *Manager) Tokens(ctx context.Context) (current, previous string, err error) {
	var s corev1.Secret
	if err := m.c.Get(ctx, client.ObjectKey{Namespace: m.namespace, Name: m.names.AgentTokenSecret}, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", fmt.Errorf("secrets: the agent token Secret %s does not exist yet", m.names.AgentTokenSecret)
		}
		return "", "", fmt.Errorf("secrets: reading the agent token Secret %s: %w", m.names.AgentTokenSecret, err)
	}
	tok := strings.TrimSpace(string(s.Data[install.AgentTokenKey]))
	switch {
	case tok == "":
		return "", "", fmt.Errorf("secrets: the agent token Secret %s has no %s", m.names.AgentTokenSecret, install.AgentTokenKey)
	case !validToken(tok):
		return "", "", fmt.Errorf("secrets: the agent token in Secret %s is not a bearer token (printable ASCII without spaces)", m.names.AgentTokenSecret)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if tok != m.current {
		if m.current != "" {
			m.previous = m.current
			m.log.Info("the agent token changed; the previous one is kept for the agent's grace period")
		}
		m.current = tok
	}
	return m.current, m.previous, nil
}

// alphanumeric is a field whose values are drivers.Alphanumeric.
func alphanumeric(key string, generate func() (string, error)) field {
	return field{
		key:      key,
		valid:    within(drivers.Alphanumeric),
		empty:    func(v []byte) bool { return len(v) == 0 },
		generate: generate,
	}
}

// within reports whether every byte of a value is in alphabet.
func within(alphabet string) func(v []byte) bool {
	return func(v []byte) bool {
		for _, b := range v {
			if strings.IndexByte(alphabet, b) < 0 {
				return false
			}
		}
		return true
	}
}

// validToken accepts what can follow "Bearer " in a header: printable
// ASCII without spaces (the generated token is base64url).
func validToken(tok string) bool {
	if tok == "" {
		return false
	}
	for i := range len(tok) {
		if tok[i] < 0x21 || tok[i] > 0x7e {
			return false
		}
	}
	return true
}

// kindOf names an owner's kind for messages.
func kindOf(o client.Object) string {
	switch o.(type) {
	case *v1alpha1.Phone:
		return string(drivers.KindPhone)
	case *v1alpha1.Softphone:
		return string(drivers.KindSoftphone)
	}
	return fmt.Sprintf("%T", o)
}
