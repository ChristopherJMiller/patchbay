package compile

import (
	"fmt"
	"regexp"
	"strings"
)

// The projected Secret (docs/plans/release-a.md §5.1, §7.2). The agent
// reads no Secret but one: <fullname>-pbx-secrets, which the controller
// writes before each pbx-config ConfigMap and which holds the value of every
// secret the bundle references (SecretRefs), each under its ProjectedKey.
// The agent's Role names exactly that object and the ConfigMap, so the agent
// can read no other Secret in the namespace.

// ProjectedKeySeparator joins a SecretRef's Name and Key into its key in
// the projected Secret. A Secret's name is a DNS subdomain and never holds
// it, so the first one splits the key again.
const ProjectedKeySeparator = "_"

// maxSecretKeyLength is the longest key a Secret's data may have.
const maxSecretKeyLength = 253

var (
	secretNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	secretKeyPattern  = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
)

// ProjectedKey is the key under which the projected Secret holds this
// secret's value: Name + "_" + Key, e.g. patchbay-phone-desk_sip-password.
// The controller writes it and the agent's intake reads it; both call this.
// It fails for a ref that is not a valid Secret name and data key (exactly
// what PlaceholderPattern accepts) or whose projected key would be longer
// than a Secret's data key may be.
func (r SecretRef) ProjectedKey() (string, error) {
	switch {
	case len(r.Name) > maxSecretKeyLength || !secretNamePattern.MatchString(r.Name):
		return "", fmt.Errorf("compile: secret name %q is not a DNS subdomain", r.Name)
	case !secretKeyPattern.MatchString(r.Key):
		return "", fmt.Errorf("compile: secret %s: key %q is not a valid Secret data key", r.Name, r.Key)
	}
	key := r.Name + ProjectedKeySeparator + r.Key
	if len(key) > maxSecretKeyLength {
		return "", fmt.Errorf("compile: secret %s key %s: the projected key is longer than %d characters",
			r.Name, r.Key, maxSecretKeyLength)
	}
	return key, nil
}

// SecretRefFromProjectedKey is the inverse of ProjectedKey: it splits a key
// of the projected Secret at its first "_".
func SecretRefFromProjectedKey(key string) (SecretRef, error) {
	name, k, ok := strings.Cut(key, ProjectedKeySeparator)
	r := SecretRef{Name: name, Key: k}
	if !ok {
		return SecretRef{}, fmt.Errorf("compile: projected key %q has no %q", key, ProjectedKeySeparator)
	}
	if again, err := r.ProjectedKey(); err != nil || again != key {
		return SecretRef{}, fmt.Errorf("compile: %q is not a projected key: %w", key, err)
	}
	return r, nil
}

// SecretRefs lists every secret the bundle references, sorted by (Name,
// Key) and unique: each placeholder in its files, each softphone's
// provisioning token in the index, and the AMI and push passwords. They are exactly
// the secrets Version covers, the keys the controller must project into
// pbx-secrets, and the values the agent must resolve before it applies the
// bundle (it refuses the whole version if one is missing). It fails if a
// file holds text that starts like a placeholder but is not one.
func (b Bundle) SecretRefs() ([]SecretRef, error) {
	refs, err := placeholders(b.Files)
	if err != nil {
		return nil, err
	}
	if b.AMI.Password != (SecretRef{}) {
		refs = append(refs, b.AMI.Password)
	}
	if b.Push.Password != (SecretRef{}) {
		refs = append(refs, b.Push.Password)
	}
	for _, d := range b.Devices {
		if d.ProvisioningToken != nil {
			refs = append(refs, *d.ProvisioningToken)
		}
	}
	return sortRefs(refs), nil
}

// Index is the bundle without its files: what the pbx-config ConfigMap
// holds under IndexKey, and what the agent keeps beside each applied
// version (the version's bundle.json) to attribute fetches of that version.
func (b Bundle) Index() Index {
	return Index{Version: b.Version, Devices: b.Devices, AMI: b.AMI, Push: b.Push}
}
