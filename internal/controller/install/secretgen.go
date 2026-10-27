package install

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"

	"github.com/ChristopherJMiller/patchbay/internal/drivers"
)

// Generated secrets (design §4, plan W3.2). The controller is the only
// writer of every one of them; each Generate function reads its randomness
// from r: nil means crypto/rand.Reader (production), and tests pass a
// reader of their own.
const (
	// ProvisioningTokenLength is a softphone's provisioning token:
	// alphanumerics, so it is URL-safe as it is (device HTTP compares the
	// decoded path).
	ProvisioningTokenLength = 32
	// AMIPasswordLength is the AMI password: alphanumerics, so it needs no
	// escaping in manager.conf.
	AMIPasswordLength = 32
	// PushPasswordLength is the phones' push password: alphanumerics, and
	// short: the 7945 refused a 32-character SIP password, and it may echo
	// this one in a query string.
	PushPasswordLength = 16
	// AgentTokenBytes is the control API token's entropy; the token is
	// these bytes in unpadded base64url (43 characters).
	AgentTokenBytes = 32
)

// Generate returns length characters drawn uniformly from alphabet (each
// byte of r is used or rejected, never reduced with a bias). The alphabet
// must hold 2 to 256 distinct ASCII characters.
func Generate(r io.Reader, length int, alphabet string) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	if length <= 0 {
		return "", fmt.Errorf("install: secret length %d", length)
	}
	seen := map[byte]bool{}
	for i := range len(alphabet) {
		c := alphabet[i]
		if c >= 0x80 || seen[c] {
			return "", fmt.Errorf("install: alphabet %q is not distinct ASCII characters", alphabet)
		}
		seen[c] = true
	}
	n := len(alphabet)
	if n < 2 {
		return "", fmt.Errorf("install: alphabet %q is too small", alphabet)
	}
	// Bytes at or above limit would favour the alphabet's first characters.
	limit := 256 - 256%n
	out := make([]byte, 0, length)
	buf := make([]byte, 2*length)
	for len(out) < length {
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", fmt.Errorf("install: reading randomness: %w", err)
		}
		for _, b := range buf {
			if int(b) < limit && len(out) < length {
				out = append(out, alphabet[int(b)%n])
			}
		}
	}
	return string(out), nil
}

// GeneratePassword is a SIP password under a driver's policy (12
// alphanumerics for the 79x5, 24 for softphones). The policy must pass
// drivers.PasswordPolicy.Validate.
func GeneratePassword(r io.Reader, p drivers.PasswordPolicy) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	return Generate(r, p.Length, p.Alphabet)
}

// GenerateProvisioningToken is a softphone's one-time provisioning token:
// ProvisioningTokenLength alphanumerics.
func GenerateProvisioningToken(r io.Reader) (string, error) {
	return Generate(r, ProvisioningTokenLength, drivers.Alphanumeric)
}

// GenerateAMIPassword is the AMI password: AMIPasswordLength alphanumerics.
func GenerateAMIPassword(r io.Reader) (string, error) {
	return Generate(r, AMIPasswordLength, drivers.Alphanumeric)
}

// GeneratePushPassword is the push password: PushPasswordLength
// alphanumerics.
func GeneratePushPassword(r io.Reader) (string, error) {
	return Generate(r, PushPasswordLength, drivers.Alphanumeric)
}

// GenerateAgentToken is the control API token: AgentTokenBytes random
// bytes in unpadded base64url.
func GenerateAgentToken(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, AgentTokenBytes)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("install: reading randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
