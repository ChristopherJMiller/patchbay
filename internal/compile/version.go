package compile

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"maps"
	"slices"
)

// ErrMissingSecret is returned by Compile when a placeholder, a provisioning
// token or the AMI account names a secret Inputs.Secrets has no value for
// (or an empty one).
var ErrMissingSecret = errors.New("compile: a referenced secret has no value")

// VersionWith is the bundle version b's files and index have with these
// secret values: exactly what Compile sets as Version (Compile calls it),
// over b.Files, b's Index without its Version, and the SHA-256 of the value
// of each of b.SecretRefs (other entries of secrets are ignored). A ref
// without a non-empty value is an error wrapping ErrMissingSecret.
//
// The agent's intake compares it with Version before it applies a bundle,
// so a version is only ever applied with the very secret values it was
// compiled with: the controller writes pbx-secrets before pbx-config, and
// the agent sees the two objects change independently, so for a moment it
// may hold one version's ConfigMap and another version's Secret.
func (b Bundle) VersionWith(secrets map[SecretRef][]byte) (string, error) {
	index, err := json.Marshal(Index{Devices: b.Devices, AMI: b.AMI, Push: b.Push})
	if err != nil {
		return "", fmt.Errorf("compile: encoding the index: %w", err)
	}
	refs, err := b.SecretRefs()
	if err != nil {
		return "", err
	}
	return version(b.Files, index, refs, secrets)
}

// placeholderStart is how every placeholder begins; text that begins like
// one but does not match PlaceholderPattern would never be resolved.
var placeholderStart = []byte("@@secret:")

// placeholders returns the secrets the files reference, sorted by (Name,
// Key) and unique. It fails if a file holds text that starts like a
// placeholder but is not one: the agent would leave it in the file.
func placeholders(files map[string][]byte) ([]SecretRef, error) {
	var refs []SecretRef
	for _, p := range slices.Sorted(maps.Keys(files)) {
		content := files[p]
		matches := PlaceholderPattern.FindAllSubmatch(content, -1)
		if n := bytes.Count(content, placeholderStart); n != len(matches) {
			return nil, fmt.Errorf("compile: %s: %d of its %d secret placeholders are malformed", p, n-len(matches), n)
		}
		for _, m := range matches {
			refs = append(refs, SecretRef{Name: string(m[1]), Key: string(m[2])})
		}
	}
	return sortRefs(refs), nil
}

// sortRefs sorts by (Name, Key) and drops duplicates.
func sortRefs(refs []SecretRef) []SecretRef {
	slices.SortFunc(refs, func(a, b SecretRef) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Key, b.Key))
	})
	return slices.Compact(refs)
}

// version is the SHA-256, in lower-case hex, over a framed stream:
//
//	"file\x00" + path + "\x00" + uint64be(len(content)) + content   per file, in path order
//	"index\x00" + uint64be(len(index)) + index                      when index is not nil
//	"secret\x00" + name + "\x00" + key + "\x00" + hex(sha256(value)) per ref, sorted by (Name, Key)
//
// The lengths make the framing unambiguous; secret values enter only as
// their own digests. A device version passes no index; the bundle version
// passes the JSON of its Index without Version. Every ref must have a
// non-empty value in secrets, or the error wraps ErrMissingSecret.
func version(files map[string][]byte, index []byte, refs []SecretRef, secrets map[SecretRef][]byte) (string, error) {
	h := sha256.New()
	for _, p := range slices.Sorted(maps.Keys(files)) {
		frame(h, "file\x00"+p+"\x00", files[p])
	}
	if index != nil {
		frame(h, "index\x00", index)
	}
	var missing []error
	for _, r := range sortRefs(slices.Clone(refs)) {
		v := secrets[r]
		if len(v) == 0 {
			missing = append(missing, fmt.Errorf("%w: %s key %s", ErrMissingSecret, r.Name, r.Key))
			continue
		}
		sum := sha256.Sum256(v)
		h.Write([]byte("secret\x00" + r.Name + "\x00" + r.Key + "\x00" + hex.EncodeToString(sum[:])))
	}
	if len(missing) > 0 {
		return "", errors.Join(missing...)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// frame writes a header, the content's length as a big-endian uint64, and
// the content.
func frame(h hash.Hash, header string, content []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(content)))
	h.Write([]byte(header))
	h.Write(n[:])
	h.Write(content)
}
