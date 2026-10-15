package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ChristopherJMiller/patchbay/internal/agent/layout"
	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// The compiled bundle in the cluster (docs/plans/release-a.md §7.2). The
// bundle reconciler writes two objects, in this order, each only when its
// content changed:
//
//  1. ProjectedSecret: <fullname>-pbx-secrets, exactly b.SecretRefs()
//     under their ProjectedKeys, with exactly the values b was compiled
//     with (the agent applies a version only with them:
//     compile.Bundle.VersionWith);
//  2. ConfigMap: <fullname>-pbx-config, compile.Bundle.ConfigMapData.
//
// Both carry ManagedLabels and AnnotationBundleVersion = b.Version. Every
// other component reads them back with ReadCompiled, from the cache, on
// every reconcile: the cluster, not the controller's memory, holds what was
// compiled, so a restarted controller, the agent and the CLI all see the
// same thing.

// ErrNotCompiled is ReadCompiled's error while ConfigMap does not exist:
// the controller has not compiled the install yet (or cannot). A device
// reconciler writes no status then; it is enqueued again when the
// ConfigMap appears.
var ErrNotCompiled = errors.New("install: the bundle has not been compiled yet (no pbx-config ConfigMap)")

// ConfigMap builds the pbx-config ConfigMap for b: b.ConfigMapData(),
// ManagedLabels and AnnotationBundleVersion. It fails as ConfigMapData does
// (an invalid bundle, compile.ErrBundleTooLarge).
func ConfigMap(n Names, namespace string, b compile.Bundle) (*corev1.ConfigMap, error) {
	data, binary, err := b.ConfigMapData()
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{
		ObjectMeta: objectMeta(n.ConfigMap, namespace, b.Version),
		Data:       data,
		BinaryData: binary,
	}, nil
}

// ProjectedSecret builds the pbx-secrets Secret for b: for every ref in
// b.SecretRefs(), values[ref] under ref.ProjectedKey() and nothing else,
// with ManagedLabels and AnnotationBundleVersion. It fails if a value is
// missing or empty (wrapping compile.ErrMissingSecret), or if the values do
// not give b.Version (b.VersionWith), so a Secret whose values do not match
// its bundle can never be written.
func ProjectedSecret(n Names, namespace string, b compile.Bundle, values map[compile.SecretRef][]byte) (*corev1.Secret, error) {
	v, err := b.VersionWith(values)
	if err != nil {
		return nil, err
	}
	if v != b.Version {
		return nil, fmt.Errorf("install: these secret values compile to version %s, not the bundle's %s", v, b.Version)
	}
	refs, err := b.SecretRefs()
	if err != nil {
		return nil, err
	}
	data := make(map[string][]byte, len(refs))
	for _, r := range refs {
		key, err := r.ProjectedKey()
		if err != nil {
			return nil, err
		}
		data[key] = slices.Clone(values[r])
	}
	return &corev1.Secret{
		ObjectMeta: objectMeta(n.ProjectedSecret, namespace, b.Version),
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}, nil
}

func objectMeta(name, namespace, version string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        name,
		Namespace:   namespace,
		Labels:      ManagedLabels(),
		Annotations: map[string]string{AnnotationBundleVersion: version},
	}
}

// IndexOf decodes the bundle index (compile.IndexKey) of a pbx-config
// ConfigMap without decoding its files. The version must be a bundle
// version (64 lower-case hex digits); a nil device map becomes empty.
func IndexOf(cm *corev1.ConfigMap) (compile.Index, error) {
	raw, ok := cm.Data[compile.IndexKey]
	if !ok {
		return compile.Index{}, fmt.Errorf("install: ConfigMap %s has no %s", cm.Name, compile.IndexKey)
	}
	var idx compile.Index
	if err := json.Unmarshal([]byte(raw), &idx); err != nil {
		return compile.Index{}, fmt.Errorf("install: ConfigMap %s: decoding %s: %w", cm.Name, compile.IndexKey, err)
	}
	if !layout.ValidVersion(idx.Version) {
		return compile.Index{}, fmt.Errorf("install: ConfigMap %s: %q is not a bundle version", cm.Name, idx.Version)
	}
	if idx.Devices == nil {
		idx.Devices = map[string]compile.BundleDevice{}
	}
	return idx, nil
}

// SecretValues decodes a pbx-secrets Secret: every key back to its
// SecretRef (compile.SecretRefFromProjectedKey). A key that is not a
// projected key is an error.
func SecretValues(s *corev1.Secret) (map[compile.SecretRef][]byte, error) {
	out := make(map[compile.SecretRef][]byte, len(s.Data))
	for _, key := range slices.Sorted(maps.Keys(s.Data)) {
		r, err := compile.SecretRefFromProjectedKey(key)
		if err != nil {
			return nil, fmt.Errorf("install: Secret %s: %w", s.Name, err)
		}
		out[r] = slices.Clone(s.Data[key])
	}
	return out, nil
}

// Compiled is what the controller last wrote, as read back by ReadCompiled.
type Compiled struct {
	// Index is pbx-config's index: the bundle version (drivers.Observations
	// CompiledVersion) and every device's entry (its Version is
	// drivers.Device.ConfigVersion).
	Index compile.Index

	// ConfigMap is the pbx-config object as read (a copy): the target of
	// the controller's install-wide Events.
	ConfigMap *corev1.ConfigMap

	// Secrets are pbx-secrets' values when its AnnotationBundleVersion is
	// Index.Version, i.e. exactly the values the bundle was compiled with.
	// Nil when pbx-secrets is missing or belongs to another version (the
	// controller writes it before the ConfigMap, so it is briefly ahead).
	Secrets map[compile.SecretRef][]byte
}

// ReadCompiled reads pbx-config and pbx-secrets through c (the manager's
// cached client). It returns ErrNotCompiled when pbx-config does not exist,
// and an error when pbx-config's index or pbx-secrets' keys do not decode.
func ReadCompiled(ctx context.Context, c client.Reader, namespace string, n Names) (Compiled, error) {
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: n.ConfigMap}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return Compiled{}, ErrNotCompiled
		}
		return Compiled{}, fmt.Errorf("install: reading ConfigMap %s: %w", n.ConfigMap, err)
	}
	idx, err := IndexOf(&cm)
	if err != nil {
		return Compiled{}, err
	}
	out := Compiled{Index: idx, ConfigMap: &cm}

	var s corev1.Secret
	switch err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: n.ProjectedSecret}, &s); {
	case apierrors.IsNotFound(err):
		return out, nil
	case err != nil:
		return Compiled{}, fmt.Errorf("install: reading Secret %s: %w", n.ProjectedSecret, err)
	}
	if s.Annotations[AnnotationBundleVersion] != idx.Version {
		return out, nil
	}
	if out.Secrets, err = SecretValues(&s); err != nil {
		return Compiled{}, err
	}
	return out, nil
}

// Device returns a device's index entry.
func (c Compiled) Device(id string) (compile.BundleDevice, bool) {
	d, ok := c.Index.Devices[id]
	return d, ok
}

// ProvisioningToken is the provisioning token the compiled bundle grants a
// softphone: the value of its index entry's ProvisioningToken in Secrets.
// ok is false for a device that is not a rendered softphone, or while
// Secrets is nil; status.provisioningURL then keeps its previous value.
func (c Compiled) ProvisioningToken(deviceID string) (token string, ok bool) {
	d, found := c.Index.Devices[deviceID]
	if !found || d.ProvisioningToken == nil || c.Secrets == nil {
		return "", false
	}
	v := c.Secrets[*d.ProvisioningToken]
	if len(v) == 0 {
		return "", false
	}
	return string(v), true
}
