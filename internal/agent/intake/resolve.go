package intake

import (
	"errors"
	"fmt"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// Resolved is a bundle with its secrets: what apply writes and serves.
type Resolved struct {
	// Bundle is the bundle as decoded: placeholders intact.
	Bundle compile.Bundle
	// Files are every file of the bundle with its placeholders resolved.
	Files map[string][]byte
	// AMIPassword is the AMI account's password.
	AMIPassword string
	// Tokens maps each rendered softphone's device ID to its current
	// one-time provisioning token.
	Tokens map[string]string
}

// errSecretMismatch marks a Secret that is not the one written for the
// bundle: a value its version covers is missing, or the values are not the
// ones it was compiled with (compile.Bundle.VersionWith).
var errSecretMismatch = errors.New("pbx-secrets does not hold the secret values this bundle version was compiled with")

// unresolvedError is a secret the bundle references that has no value.
type unresolvedError struct{ ref compile.SecretRef }

func (e unresolvedError) Error() string {
	return "unresolved placeholder " + e.ref.Name + "/" + e.ref.Key
}

// Is makes a missing value a Secret mismatch: the Secret written for the
// version holds every value.
func (e unresolvedError) Is(target error) bool { return target == errSecretMismatch }

// Resolve resolves every placeholder of b with the projected Secret's data
// (keys compile.SecretRef.ProjectedKey). It is pure. Each file is scanned
// once with compile.PlaceholderPattern and each match replaced by its value,
// so a value that itself looks like a placeholder stays as written; nothing
// else in a file changes, whatever its encoding.
//
// Every secret the bundle references (b.SecretRefs: placeholders,
// provisioning tokens, the AMI password) must have a non-empty value, or the
// whole bundle is refused with "unresolved placeholder <name>/<key>", naming
// the first missing one in SecretRefs order. No error names a value.
func Resolve(b compile.Bundle, secretData map[string][]byte) (Resolved, error) {
	refs, err := b.SecretRefs()
	if err != nil {
		return Resolved{}, err
	}
	values := make(map[compile.SecretRef][]byte, len(refs))
	for _, r := range refs {
		key, err := r.ProjectedKey()
		if err != nil {
			return Resolved{}, fmt.Errorf("unresolved placeholder %s/%s: %w", r.Name, r.Key, err)
		}
		v := secretData[key]
		if len(v) == 0 {
			return Resolved{}, unresolvedError{r}
		}
		values[r] = v
	}

	files := make(map[string][]byte, len(b.Files))
	for p, content := range b.Files {
		var missing error
		files[p] = compile.PlaceholderPattern.ReplaceAllFunc(content, func(m []byte) []byte {
			sub := compile.PlaceholderPattern.FindSubmatch(m)
			r := compile.SecretRef{Name: string(sub[1]), Key: string(sub[2])}
			v, ok := values[r]
			if !ok && missing == nil {
				// SecretRefs lists every placeholder, so this cannot
				// happen; refuse rather than write the placeholder.
				missing = unresolvedError{r}
			}
			return v
		})
		if missing != nil {
			return Resolved{}, missing
		}
	}

	r := Resolved{Bundle: b, Files: files, Tokens: map[string]string{}}
	if b.AMI.Password == (compile.SecretRef{}) {
		return Resolved{}, errors.New("the bundle names no AMI password")
	}
	r.AMIPassword = string(values[b.AMI.Password])
	for id, d := range b.Devices {
		if d.ProvisioningToken != nil && d.Version != "" {
			r.Tokens[id] = string(values[*d.ProvisioningToken])
		}
	}
	return r, nil
}

// secretsMatch is nil when secretData holds exactly the secret values b's
// version was compiled with, and errSecretMismatch when it holds others
// (or lacks one): pbx-secrets written for another version.
func secretsMatch(b compile.Bundle, secretData map[string][]byte) error {
	refs, err := b.SecretRefs()
	if err != nil {
		return err
	}
	values := make(map[compile.SecretRef][]byte, len(refs))
	for _, r := range refs {
		key, err := r.ProjectedKey()
		if err != nil {
			return err
		}
		values[r] = secretData[key]
	}
	v, err := b.VersionWith(values)
	switch {
	case errors.Is(err, compile.ErrMissingSecret):
		return errSecretMismatch
	case err != nil:
		return err
	case v != b.Version:
		return errSecretMismatch
	}
	return nil
}
