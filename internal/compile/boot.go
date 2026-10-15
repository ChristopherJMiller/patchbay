package compile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Boot is the part of a bundle's Asterisk config that only a restart
// loads: asterisk.conf, modules.conf, and the PJSIP transports (res_pjsip
// keeps a running transport whose bind changed: "not reloadable,
// maintaining previous values"). A reload applies everything else.
//
// The PBX pod's Asterisk boots on whatever version is live when it starts,
// so the agent applies only versions whose Boot is this pod's: the one
// this release compiles for the pod's SIP bind (BootFor; the chart passes
// the agent pbx.address and pbx.ports.sip, the values whose change also
// restarts the pod). A version compiled for other settings (a controller
// that has not recompiled since they changed) or by another release (a
// different module list) is refused, so it can never go live with
// Asterisk still running the old transports or modules while the apply
// reports success. Comments and blank lines do not count.
type Boot struct {
	// SIPBind is the transports' bind address, "<address>:<port>".
	SIPBind string
	// Digest is the lower-case hex SHA-256 of the settings in
	// asterisk.conf, modules.conf and pjsip.conf's transport sections.
	Digest string
}

// BootFor is the Boot this release compiles for a PBX address and SIP
// port: the Settings fields that reach those files.
func BootFor(pbxAddress string, sipPort int) Boot {
	b, err := BootOf(asteriskFiles(Settings{PBXAddress: pbxAddress, SIPPort: sipPort}, nil))
	if err != nil {
		// asteriskFiles always renders all three files with both transports.
		panic(fmt.Sprintf("compile: BootFor: %v", err))
	}
	return b
}

// BootOf reads a bundle's Boot from its files (Bundle.Files, or the
// resolved files: none of the three holds a placeholder).
func BootOf(files map[string][]byte) (Boot, error) {
	var parts [3][]string
	for i, name := range []string{"asterisk.conf", "modules.conf", "pjsip.conf"} {
		b, ok := files[TreeAsterisk+"/"+name]
		if !ok {
			return Boot{}, fmt.Errorf("boot: the bundle has no %s/%s", TreeAsterisk, name)
		}
		parts[i] = settingLines(b)
	}
	transports, bind, err := transportSections(parts[2])
	if err != nil {
		return Boot{}, err
	}
	parts[2] = transports
	h := sha256.New()
	for i, lines := range parts {
		fmt.Fprintf(h, "%d %d\n", i, len(lines))
		for _, l := range lines {
			h.Write([]byte(l + "\n"))
		}
	}
	return Boot{SIPBind: bind, Digest: hex.EncodeToString(h.Sum(nil))}, nil
}

// settingLines is a config file's lines, trimmed, without comments and
// blank lines: what Asterisk reads.
func settingLines(conf []byte) []string {
	var out []string
	for l := range bytes.Lines(conf) {
		if s := strings.TrimSpace(string(l)); s != "" && !strings.HasPrefix(s, ";") {
			out = append(out, s)
		}
	}
	return out
}

// transportSections returns the lines of pjsip.conf's sections of type
// transport (settingLines), each "[name]" first, in file order, and their
// bind address, which every one must share.
func transportSections(lines []string) ([]string, string, error) {
	var out, section []string
	bind := ""
	flush := func() error {
		if len(section) == 0 || !slices.Contains(section[1:], "type = transport") {
			return nil
		}
		for _, l := range section {
			if v, ok := strings.CutPrefix(l, "bind = "); ok {
				if bind != "" && v != bind {
					return fmt.Errorf("boot: pjsip.conf's transports bind %s and %s", bind, v)
				}
				bind = v
			}
		}
		out = append(out, section...)
		return nil
	}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "["):
			if err := flush(); err != nil {
				return nil, "", err
			}
			section = []string{l}
		case section != nil:
			section = append(section, l)
		}
	}
	if err := flush(); err != nil {
		return nil, "", err
	}
	if bind == "" {
		return nil, "", errors.New("boot: pjsip.conf has no transport with a bind address")
	}
	return out, bind, nil
}
