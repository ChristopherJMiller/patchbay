package compile

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
)

// The pbx-config ConfigMap (design §5) carries one bundle: the controller
// writes it with ConfigMapData and the agent reads it back with
// BundleFromConfigMap. Every file is one key, so `kubectl get configmap
// pbx-config -o yaml` shows each file in full (there are no secrets in it);
// the bundle's version, device index and AMI account are one JSON key,
// IndexKey.
//
// A file's key is its bundle path with "/" written as "__" (ConfigMap keys
// allow only letters, digits, ".", "-" and "_"): asterisk/pjsip.conf is
// asterisk__pjsip.conf, tftp/SEPD0C78915E8C8.cnf.xml is
// tftp__SEPD0C78915E8C8.cnf.xml. TreeOf's portable-name rule makes that
// reversible. A UTF-8 file goes in data, anything else in binaryData.
//
// The API server refuses a ConfigMap whose keys and values add up to more
// than MaxConfigMapBytes. Release A's bundle is tens of kilobytes; release
// C's wallpapers and ringtones are what could approach the limit, and they
// will need their own objects then. Until then ConfigMapData refuses an
// oversized bundle with ErrBundleTooLarge rather than leave the API server
// to fail the write.
const (
	// IndexKey is the key holding the bundle's Index as JSON. No file's key
	// can equal it: every file key holds "__".
	IndexKey = "bundle.json"

	// MaxConfigMapBytes is the API server's limit on a ConfigMap: the keys
	// and values of data and binaryData together.
	MaxConfigMapBytes = 1 << 20

	// maxKeyLength is the longest ConfigMap key the API server accepts.
	maxKeyLength = 253
)

// ErrBundleTooLarge is returned by ConfigMapData for a bundle over
// MaxConfigMapBytes.
var ErrBundleTooLarge = errors.New("compile: bundle too large for one ConfigMap")

// Index is everything in a bundle but its files: the JSON under IndexKey.
type Index struct {
	Version string                  `json:"version"`
	Devices map[string]BundleDevice `json:"devices"`
	AMI     AMIAccount              `json:"ami"`
}

// ConfigMapKey is the ConfigMap key of a bundle path: the path with "/"
// written as "__". It fails for a path TreeOf refuses.
func ConfigMapKey(p string) (string, error) {
	if _, err := TreeOf(p); err != nil {
		return "", err
	}
	key := strings.ReplaceAll(p, "/", "__")
	if len(key) > maxKeyLength {
		return "", fmt.Errorf("bundle: %q: longer than a ConfigMap key may be (%d)", p, maxKeyLength)
	}
	return key, nil
}

// PathFromConfigMapKey is the bundle path a ConfigMap key stands for: the
// inverse of ConfigMapKey.
func PathFromConfigMapKey(key string) (string, error) {
	p := strings.ReplaceAll(key, "__", "/")
	if _, err := TreeOf(p); err != nil {
		return "", fmt.Errorf("bundle: ConfigMap key %q is not a bundle file: %w", key, err)
	}
	return p, nil
}

// ConfigMapData encodes the bundle as a ConfigMap's data and binaryData. It
// fails if the bundle does not Validate, or is over MaxConfigMapBytes.
func (b Bundle) ConfigMapData() (data map[string]string, binaryData map[string][]byte, err error) {
	if err := b.Validate(); err != nil {
		return nil, nil, err
	}
	if b.Version == "" {
		return nil, nil, errors.New("bundle: no version")
	}
	index, err := json.Marshal(b.Index())
	if err != nil {
		return nil, nil, fmt.Errorf("bundle: encoding the index: %w", err)
	}
	data = map[string]string{IndexKey: string(index)}
	size := len(IndexKey) + len(index)
	for _, p := range slices.Sorted(maps.Keys(b.Files)) {
		key, err := ConfigMapKey(p)
		if err != nil {
			return nil, nil, err
		}
		content := b.Files[p]
		size += len(key) + len(content)
		if utf8.Valid(content) {
			data[key] = string(content)
			continue
		}
		if binaryData == nil {
			binaryData = map[string][]byte{}
		}
		binaryData[key] = slices.Clone(content)
	}
	if size > MaxConfigMapBytes {
		return nil, nil, fmt.Errorf("%w: %d bytes, the limit is %d", ErrBundleTooLarge, size, MaxConfigMapBytes)
	}
	return data, binaryData, nil
}

// BundleFromConfigMap decodes what ConfigMapData encoded, and validates the
// result. It does not recompute Version (that needs the secret values); the
// agent trusts the controller's.
func BundleFromConfigMap(data map[string]string, binaryData map[string][]byte) (Bundle, error) {
	raw, ok := data[IndexKey]
	if !ok {
		return Bundle{}, fmt.Errorf("bundle: no %s key", IndexKey)
	}
	var idx Index
	if err := json.Unmarshal([]byte(raw), &idx); err != nil {
		return Bundle{}, fmt.Errorf("bundle: decoding %s: %w", IndexKey, err)
	}
	if idx.Version == "" {
		return Bundle{}, fmt.Errorf("bundle: %s has no version", IndexKey)
	}
	b := Bundle{Files: map[string][]byte{}, Version: idx.Version, Devices: idx.Devices, AMI: idx.AMI}
	for key, v := range data {
		if key == IndexKey {
			continue
		}
		p, err := PathFromConfigMapKey(key)
		if err != nil {
			return Bundle{}, err
		}
		b.Files[p] = []byte(v)
	}
	for key, v := range binaryData {
		p, err := PathFromConfigMapKey(key)
		if err != nil {
			return Bundle{}, err
		}
		if _, dup := b.Files[p]; dup {
			return Bundle{}, fmt.Errorf("bundle: %s is in both data and binaryData", key)
		}
		b.Files[p] = slices.Clone(v)
	}
	if err := b.Validate(); err != nil {
		return Bundle{}, err
	}
	return b, nil
}
