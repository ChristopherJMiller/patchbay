// Package settings reads the install settings the controller runs with
// (docs/plans/release-a.md W3.1, §6.2.1 "Settings"): compile.Settings as
// JSON, rendered by the chart into the <fullname>-settings ConfigMap
// (install.SettingsKey) and mounted into the controller's pod.
//
// The controller reads the file once at start. There is no watch and no
// partial reload: the chart puts a checksum of the JSON on the controller's
// pod template, so a settings change rolls the (stateless) controller pod.
// The caller checks the result against the install's names with
// install.CheckSettings.
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ChristopherJMiller/patchbay/internal/compile"
)

// maxSize bounds the settings file: a ConfigMap holds at most 1 MiB.
const maxSize = 1 << 20

// Load reads the mounted settings file and parses it (Parse). Errors name
// the path.
func Load(path string) (compile.Settings, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the path is the controller's --settings-file flag.
	if err != nil {
		return compile.Settings{}, fmt.Errorf("settings: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return compile.Settings{}, fmt.Errorf("settings: reading %s: %w", path, err)
	}
	if len(raw) > maxSize {
		return compile.Settings{}, fmt.Errorf("settings: %s is larger than %d bytes", path, maxSize)
	}
	s, err := Parse(raw)
	if err != nil {
		return compile.Settings{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Parse decodes one JSON document into compile.Settings, strictly: an
// unknown field (at any depth), a value of the wrong type or anything but
// white space after the document is an error. The result must pass
// compile.Settings.Validate.
func Parse(raw []byte) (compile.Settings, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var s compile.Settings
	if err := dec.Decode(&s); err != nil {
		return compile.Settings{}, fmt.Errorf("settings: decoding: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return compile.Settings{}, errors.New("settings: trailing data after the JSON document")
	}
	if err := s.Validate(); err != nil {
		return compile.Settings{}, err
	}
	return s, nil
}
