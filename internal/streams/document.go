package streams

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/util/validation"
)

// FileName is streams.json: the key of the <fullname>-streams ConfigMap
// (install.StreamsKey) and the file name the bridge reads it as.
const FileName = "streams.json"

// Bounds of a document.
const (
	// MaxSourceLength is the longest source URL (the Stream CRD's
	// spec.source maxLength).
	MaxSourceLength = 2048
	// MaxPoolMax is the largest Pool.Max: how many sources of one pool may
	// play at once. A tuner device has a handful of tuners.
	MaxPoolMax = 16
	// MaxStreams is the most streams one document holds (the 7XX range has
	// 100 numbers).
	MaxStreams = 1000
)

// Document is streams.json: every Stream the compiled dialplan routes to the
// bridge, keyed by its UUID, and the tuner pools their sources count
// against. Compile builds it (compile.Bundle.Streams), Sealed; the
// controller writes it Encoded; the bridge Decodes it.
//
// The bridge plays one source per session, shared by every caller of every
// stream with that Source, and starts a session for a stream with a Pool
// only while fewer than that pool's Max sessions of the pool are running.
type Document struct {
	// Version is the lower-case hex SHA-256 of the document's encoding
	// with Version empty (Sealed sets it; Validate checks it). The bridge
	// reports the version it loaded (streambridge/api.BridgeStatus), so
	// the controller can tell when a new document has reached it.
	Version string `json:"version"`

	// Pools are the tuner pools (settings streams.tunerPools), sorted by
	// name, each once: all of them, used or not.
	Pools []Pool `json:"pools"`

	// Streams maps a stream's UUID (UUID(Stream.Name)) to it: every Stream
	// that holds its number and has an acceptable source.
	Streams map[string]Stream `json:"streams"`
}

// Pool is a tuner pool as the bridge needs it: a name and how many of its
// sources may play at once. Which sources belong to it (the pool's hosts)
// was decided by the compiler.
type Pool struct {
	// Name is the pool's name (a DNS label), e.g. "hdhomerun".
	Name string `json:"name"`
	// Max is how many sessions (distinct sources, not calls) of the pool
	// may run at once, 1 to MaxPoolMax.
	Max int `json:"max"`
}

// Stream is one stream as the bridge plays it.
type Stream struct {
	// Name is the Stream resource's name: how observations, metrics and
	// logs name it.
	Name string `json:"name"`
	// Number is the stream's number, for messages ("king5 (705)").
	Number string `json:"number"`
	// Source is the URL the bridge's decoder (ffmpeg) reads; CheckSource
	// accepts it.
	Source string `json:"source"`
	// Pool is the tuner pool the source counts against (a Pools name), or
	// empty: it needs no tuner (internet radio) and has no limit.
	Pool string `json:"pool,omitempty"`
}

var (
	numberPattern  = regexp.MustCompile(`^[0-9]{1,16}$`)
	versionPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// CheckSource is the rule every stream source keeps, in the compiler (a
// Stream whose source breaks it is left out of the bundle, not failed), in
// Validate (so the bridge never starts a decoder on anything else), and,
// as CEL, in the Stream CRD:
//   - an absolute http or https URL with a host, at most MaxSourceLength
//     bytes, with no white space or control character;
//   - no credentials (user:password@): the source is shown in status,
//     Events, observations and logs;
//   - not a playlist: a path ending in .pls or .m3u (any case) is refused,
//     because the decoder reads streams, not playlists. Put one of the
//     playlist's stream URLs in the source instead (docs/streams.md).
//     An HLS playlist (.m3u8) is a stream to ffmpeg, and is accepted.
func CheckSource(source string) error {
	switch {
	case source == "":
		return errors.New("streams: the source is empty")
	case len(source) > MaxSourceLength:
		return fmt.Errorf("streams: the source is %d bytes long, more than %d", len(source), MaxSourceLength)
	case strings.ContainsFunc(source, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
		return errors.New("streams: the source contains white space or a control character")
	}
	u, err := url.Parse(source)
	if err != nil {
		return fmt.Errorf("streams: the source is not a URL: %w", err)
	}
	switch scheme := strings.ToLower(u.Scheme); {
	case scheme != "http" && scheme != "https":
		return fmt.Errorf("streams: the source's scheme is %q; only http and https are read", u.Scheme)
	case u.User != nil:
		return errors.New("streams: the source carries credentials (user@ or user:password@); sources are shown in status and logs")
	case u.Hostname() == "":
		return errors.New("streams: the source has no host")
	}
	if p := strings.ToLower(u.Path); strings.HasSuffix(p, ".pls") || strings.HasSuffix(p, ".m3u") {
		return errors.New("streams: the source is a playlist (.pls or .m3u); use one of the stream URLs it lists")
	}
	return nil
}

// Validate checks a document: its Version matches its content; pools are
// sorted by name, each a DNS label once, with Max 1 to MaxPoolMax; at most
// MaxStreams streams, each keyed by UUID(Name), named by a DNS subdomain
// once, with a number of 1-16 digits, a source CheckSource accepts and a
// pool that is empty or listed; and streams with one source agree on its
// pool (a session is per source).
func (d Document) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("streams: "+format, args...)) }

	pools := map[string]bool{}
	for i, p := range d.Pools {
		for _, problem := range validation.IsDNS1123Label(p.Name) {
			bad("pool %q: %s", p.Name, problem)
		}
		if pools[p.Name] {
			bad("pool %q is listed twice", p.Name)
		}
		pools[p.Name] = true
		if i > 0 && d.Pools[i-1].Name >= p.Name {
			bad("pools are not sorted by name (%q before %q)", d.Pools[i-1].Name, p.Name)
		}
		if p.Max < 1 || p.Max > MaxPoolMax {
			bad("pool %q: max %d is not 1-%d", p.Name, p.Max, MaxPoolMax)
		}
	}

	if len(d.Streams) > MaxStreams {
		bad("%d streams, more than %d", len(d.Streams), MaxStreams)
	}
	names := map[string]string{}        // name -> UUID
	sourcePool := map[string]string{}   // source -> pool
	sourceStream := map[string]string{} // source -> the first stream with it
	for _, id := range slices.Sorted(maps.Keys(d.Streams)) {
		s := d.Streams[id]
		for _, problem := range validation.IsDNS1123Subdomain(s.Name) {
			bad("stream %q: %s", s.Name, problem)
		}
		if want := UUID(s.Name); id != want {
			bad("stream %q is keyed %q, not its UUID %s", s.Name, id, want)
		}
		if other, dup := names[s.Name]; dup {
			bad("stream %q is listed twice (%s and %s)", s.Name, other, id)
		}
		names[s.Name] = id
		if !numberPattern.MatchString(s.Number) {
			bad("stream %q: number %q is not 1-16 digits", s.Name, s.Number)
		}
		if err := CheckSource(s.Source); err != nil {
			bad("stream %q: %v", s.Name, strings.TrimPrefix(err.Error(), "streams: "))
		}
		if s.Pool != "" && !pools[s.Pool] {
			bad("stream %q: pool %q is not among the pools", s.Name, s.Pool)
		}
		if pool, seen := sourcePool[s.Source]; seen && pool != s.Pool {
			bad("streams %q and %q share a source but not a pool (%q, %q)", sourceStream[s.Source], s.Name, pool, s.Pool)
		} else if !seen {
			sourcePool[s.Source], sourceStream[s.Source] = s.Pool, s.Name
		}
	}

	if len(errs) == 0 {
		switch want, err := d.version(); {
		case err != nil:
			errs = append(errs, err)
		case !versionPattern.MatchString(d.Version):
			bad("version %q is not a lower-case hex SHA-256 (the document is not sealed)", d.Version)
		case d.Version != want:
			bad("version %s does not match the content (%s)", d.Version, want)
		}
	}
	return errors.Join(errs...)
}

// Sealed returns a copy of d ready to write: nil Pools and Streams become
// empty, Pools are sorted by name, and Version is set from the content.
// It fails if the result does not Validate.
func (d Document) Sealed() (Document, error) {
	out := Document{Pools: slices.Clone(d.Pools), Streams: maps.Clone(d.Streams)}
	if out.Pools == nil {
		out.Pools = []Pool{}
	}
	if out.Streams == nil {
		out.Streams = map[string]Stream{}
	}
	slices.SortStableFunc(out.Pools, func(a, b Pool) int { return cmp.Compare(a.Name, b.Name) })
	v, err := out.version()
	if err != nil {
		return Document{}, err
	}
	out.Version = v
	if err := out.Validate(); err != nil {
		return Document{}, err
	}
	return out, nil
}

// Encode is the document as streams.json: indented JSON (map keys sorted),
// ending in a newline. It fails unless the document Validates, so an
// unsealed document is never written.
func (d Document) Encode() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return d.encode()
}

// Decode reads streams.json strictly (an unknown field or trailing data is
// an error) and Validates it.
func Decode(data []byte) (Document, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var d Document
	if err := dec.Decode(&d); err != nil {
		return Document{}, fmt.Errorf("streams: decoding %s: %w", FileName, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Document{}, fmt.Errorf("streams: decoding %s: trailing data after the document", FileName)
	}
	if err := d.Validate(); err != nil {
		return Document{}, err
	}
	return d, nil
}

func (d Document) encode() ([]byte, error) {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("streams: encoding: %w", err)
	}
	return append(b, '\n'), nil
}

// version is the SHA-256 of the encoding with Version empty, nil Pools and
// Streams written as empty (so a sealed document and its decoded copy
// agree).
func (d Document) version() (string, error) {
	d.Version = ""
	if d.Pools == nil {
		d.Pools = []Pool{}
	}
	if d.Streams == nil {
		d.Streams = map[string]Stream{}
	}
	b, err := d.encode()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
