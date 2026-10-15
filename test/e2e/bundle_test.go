//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"maps"
	"math/big"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ChristopherJMiller/patchbay/api/v1alpha1"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
	"github.com/ChristopherJMiller/patchbay/internal/drivers"
	"github.com/ChristopherJMiller/patchbay/internal/drivers/builtin"
)

// The compiled install the suite runs (docs/e2e.md, "Compiled config").
// Until the controller compiles (W3), the harness stands in for it: it
// compiles these fixed resources with compile.Compile and the built-in
// drivers, keeps their generated secrets in the Secrets the controller
// will own, and writes pbx-secrets, then pbx-config, as the controller
// will.
const (
	deskName     = "desk"
	deskMAC      = "0200E2E00001" // locally administered: no real phone has it
	deskNumber   = "100"
	laptopName   = "laptop"
	laptopNumber = "150"
	personNumber = "301"
	echoNumber   = "600"

	// unknownMAC asks for a SEP file no Phone has.
	unknownMAC = "0200E2E0FFFF"

	pbxConfigMap  = release + "-pbx-config"
	pbxSecrets    = release + "-pbx-secrets"
	amiSecretName = release + "-ami"
)

var (
	deskID   = drivers.DeviceID(drivers.KindPhone, deskName)
	laptopID = drivers.DeviceID(drivers.KindSoftphone, laptopName)
	deskSEP  = "SEP" + deskMAC + ".cnf.xml"
	amiRef   = compile.SecretRef{Name: amiSecretName, Key: "password"}
)

// e2eBundle is the compiled install and the secret values behind it.
type e2eBundle struct {
	compile.Bundle
	secrets map[compile.SecretRef][]byte
}

// e2eResources are the install's resources: Phone desk (the fakephone's
// MAC), Softphone laptop, and Person chris ringing both.
func e2eResources() compile.Inputs {
	created := metav1.NewTime(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	return compile.Inputs{
		Phones: []v1alpha1.Phone{{
			ObjectMeta: metav1.ObjectMeta{Name: deskName, CreationTimestamp: created},
			Spec: v1alpha1.PhoneSpec{
				Model: v1alpha1.PhoneModelCisco7945, MAC: deskMAC, Number: deskNumber, Label: "Desk",
			},
		}},
		Softphones: []v1alpha1.Softphone{{
			ObjectMeta: metav1.ObjectMeta{Name: laptopName, CreationTimestamp: created},
			Spec:       v1alpha1.SoftphoneSpec{Number: laptopNumber, Label: "Laptop"},
		}},
		People: []v1alpha1.Person{{
			ObjectMeta: metav1.ObjectMeta{Name: "chris", CreationTimestamp: created},
			Spec: v1alpha1.PersonSpec{Number: personNumber, Name: "Chris", Rings: []v1alpha1.RingTarget{
				{Phone: deskName}, {Softphone: laptopName},
			}},
		}},
	}
}

// e2eSettings are the install settings: the kind node is the PBX, and the
// only network allowed to register is the node's pod CIDR, where fakephone
// runs (the compiled ACL refuses everything else).
func (h *harness) e2eSettings() compile.Settings {
	return compile.Settings{
		PBXAddress:      h.nodeIP,
		SIPPort:         sipPort,
		RTPPorts:        compile.PortRange{First: 10000, Last: 10099},
		DeviceHTTPPort:  deviceHTTPPort,
		NTPServer:       "192.168.20.1",
		Timezone:        "America/Los_Angeles",
		Signalling:      drivers.SignallingPJSIP,
		NumberPlan:      compile.DefaultNumberPlan(),
		AllowedNetworks: []string{h.podCIDR},
		AMISecret:       amiRef,
	}
}

// secretSpec is one generated secret: where it lives and how it is made.
type secretSpec struct {
	ref    compile.SecretRef
	length int // alphanumeric characters
}

// e2eSecrets lists every secret the install needs, with the lengths the
// drivers' password policies and W3's generator use.
func e2eSecrets() []secretSpec {
	return []secretSpec{
		{compile.PasswordRef(deskID), 12}, // cisco79x5: 12 alphanumerics
		{compile.PasswordRef(laptopID), 24},
		{compile.ProvisioningTokenRef(laptopID), 32},
		{amiRef, 32},
	}
}

// applyBundle generates (or, on a reused cluster, keeps) every secret,
// compiles the install, and writes pbx-secrets and then pbx-config, the
// order the controller will use, so the agent never sees a bundle whose
// secrets are missing.
func (h *harness) applyBundle(ctx context.Context) (*e2eBundle, error) {
	values := map[compile.SecretRef][]byte{}
	for _, s := range e2eSecrets() {
		v, err := h.ensureSecretKey(ctx, s.ref, s.length)
		if err != nil {
			return nil, err
		}
		values[s.ref] = v
	}
	reg, err := builtin.Registry()
	if err != nil {
		return nil, err
	}
	in := e2eResources()
	in.Secrets = values
	b, err := compile.Compile(reg, in, h.e2eSettings())
	if err != nil {
		return nil, fmt.Errorf("compiling the e2e install: %w", err)
	}
	bundle := &e2eBundle{Bundle: b, secrets: values}
	if err := h.writeBundle(ctx, bundle.Bundle); err != nil {
		return nil, err
	}
	log.Printf("bundle %s written (%d files, devices %v)", b.Version, len(b.Files), slices.Sorted(maps.Keys(b.Devices)))
	return bundle, nil
}

// writeBundle writes pbx-secrets (every SecretRefs key, nothing else) and
// then pbx-config for b.
func (h *harness) writeBundle(ctx context.Context, b compile.Bundle) error {
	refs, err := b.SecretRefs()
	if err != nil {
		return err
	}
	projected := map[string][]byte{}
	for _, r := range refs {
		key, err := r.ProjectedKey()
		if err != nil {
			return err
		}
		v, err := h.secretValue(ctx, r)
		if err != nil {
			return err
		}
		projected[key] = v
	}
	if err := h.upsertSecret(ctx, pbxSecrets, projected); err != nil {
		return err
	}
	data, binary, err := b.ConfigMapData()
	if err != nil {
		return err
	}
	return h.upsertConfigMap(ctx, pbxConfigMap, data, binary)
}

// ensureSecretKey returns the value of ref, generating a random
// alphanumeric one of length n if its Secret or key does not exist yet.
func (h *harness) ensureSecretKey(ctx context.Context, ref compile.SecretRef, n int) ([]byte, error) {
	if v, err := h.secretValue(ctx, ref); err == nil {
		return v, nil
	} else if !apierrors.IsNotFound(err) && !isMissingKey(err) {
		return nil, err
	}
	v, err := randomAlphanumeric(n)
	if err != nil {
		return nil, err
	}
	secrets := h.cs.CoreV1().Secrets(namespace)
	sec, err := secrets.Get(ctx, ref.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: namespace},
			Data:       map[string][]byte{ref.Key: v},
		}, metav1.CreateOptions{})
	case err == nil:
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data[ref.Key] = v
		_, err = secrets.Update(ctx, sec, metav1.UpdateOptions{})
	}
	return v, err
}

type missingKeyError struct{ ref compile.SecretRef }

func (e missingKeyError) Error() string {
	return fmt.Sprintf("secret %s has no key %s", e.ref.Name, e.ref.Key)
}

func isMissingKey(err error) bool {
	_, ok := err.(missingKeyError) //nolint:errorlint // returned unwrapped by secretValue.
	return ok
}

// secretValue reads one key of one Secret.
func (h *harness) secretValue(ctx context.Context, ref compile.SecretRef) ([]byte, error) {
	sec, err := h.cs.CoreV1().Secrets(namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	v, ok := sec.Data[ref.Key]
	if !ok || len(v) == 0 {
		return nil, missingKeyError{ref}
	}
	return v, nil
}

func (h *harness) upsertSecret(ctx context.Context, name string, data map[string][]byte) error {
	secrets := h.cs.CoreV1().Secrets(namespace)
	sec, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Data: data,
		}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	sec.Data, sec.StringData = data, nil
	_, err = secrets.Update(ctx, sec, metav1.UpdateOptions{})
	return err
}

func (h *harness) upsertConfigMap(ctx context.Context, name string, data map[string]string, binary map[string][]byte) error {
	cms := h.cs.CoreV1().ConfigMaps(namespace)
	cm, err := cms.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Data: data, BinaryData: binary,
		}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	cm.Data, cm.BinaryData = data, binary
	_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

const alphanumeric = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// randomAlphanumeric is n characters from crypto/rand, like the controller's
// generated passwords and tokens.
func randomAlphanumeric(n int) ([]byte, error) {
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphanumeric)))
	for i := range out {
		k, err := rand.Int(rand.Reader, max)
		if err != nil {
			return nil, err
		}
		out[i] = alphanumeric[k.Int64()]
	}
	return out, nil
}
