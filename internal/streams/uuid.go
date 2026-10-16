package streams

import (
	"crypto/sha1" //nolint:gosec // G505: UUID version 5 is defined over SHA-1 (RFC 9562 §5.5); it names, it does not protect.
	"encoding/hex"
	"errors"
	"fmt"
)

// UUIDs (design §9): a Stream is named on the AudioSocket wire by
// uuid5(Namespace, its resource name), so its UUID stays the same across
// renders, restarts and renumbering, and the dialplan and streams.json,
// compiled together, always agree.

// NamespaceName is the name the stream namespace is derived from:
// Namespace = uuid5(the RFC 9562 URL namespace, NamespaceName).
const NamespaceName = "https://github.com/ChristopherJMiller/patchbay/streams"

// Namespace is the fixed UUID namespace of every stream UUID. It is derived
// from NamespaceName (TestNamespace pins both). Changing it changes every
// stream's UUID: harmless to calls, because the dialplan and streams.json
// are compiled and written together, but every UUID in logs and metrics
// history would stop matching.
const Namespace = "211a92d2-913d-5c00-af69-82af81c3daf0"

// UUID is the stream UUID of the Stream named name (its metadata.name), in
// canonical form: lower-case hex, 8-4-4-4-12.
func UUID(name string) string {
	ns, err := ParseUUID(Namespace)
	if err != nil {
		panic("streams: Namespace is not a UUID: " + err.Error())
	}
	return FormatUUID(NameUUID(ns, name))
}

// NameUUID is the RFC 9562 version 5 UUID of name in namespace ns: the first
// 16 bytes of SHA-1(ns || name), with the version (5) and variant (10) bits
// set.
func NameUUID(ns [16]byte, name string) [16]byte {
	h := sha1.New() //nolint:gosec // G401: see the import.
	h.Write(ns[:])
	h.Write([]byte(name))
	var u [16]byte
	copy(u[:], h.Sum(nil))
	u[6] = u[6]&0x0f | 0x50
	u[8] = u[8]&0x3f | 0x80
	return u
}

// FormatUUID writes a UUID in canonical form, as Asterisk's dialplan and
// streams.json carry it. The AudioSocket ID frame carries the same 16
// bytes in binary.
func FormatUUID(u [16]byte) string {
	var b [36]byte
	hex.Encode(b[0:8], u[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], u[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], u[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], u[8:10])
	b[23] = '-'
	hex.Encode(b[24:36], u[10:16])
	return string(b[:])
}

// ParseUUID reads a UUID in canonical form only (lower-case hex,
// 8-4-4-4-12), the one form FormatUUID writes, so a UUID has exactly one
// spelling in streams.json.
func ParseUUID(s string) ([16]byte, error) {
	var u [16]byte
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return u, fmt.Errorf("streams: %q is not a UUID in canonical form (8-4-4-4-12 lower-case hex)", s)
	}
	j := 0
	for _, span := range [][2]int{{0, 8}, {9, 13}, {14, 18}, {19, 23}, {24, 36}} {
		part := s[span[0]:span[1]]
		for _, c := range part {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return u, fmt.Errorf("streams: %q is not a UUID in canonical form (8-4-4-4-12 lower-case hex)", s)
			}
		}
		n, err := hex.Decode(u[j:], []byte(part))
		if err != nil {
			return u, errors.Join(fmt.Errorf("streams: %q is not a UUID", s), err)
		}
		j += n
	}
	return u, nil
}
