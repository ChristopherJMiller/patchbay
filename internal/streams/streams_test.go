package streams_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherJMiller/patchbay/internal/streams"
)

// urlNamespace is RFC 9562's namespace ID for URLs,
// 6ba7b811-9dad-11d1-80b4-00c04fd430c8.
var urlNamespace = [16]byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}

// TestNameUUID: RFC 9562's version 5 example, and Python's uuid.uuid5 for
// the same input (checked 2026-10-16).
func TestNameUUID(t *testing.T) {
	dns, err := streams.ParseUUID("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	if err != nil {
		t.Fatal(err)
	}
	if got := streams.FormatUUID(streams.NameUUID(dns, "www.example.com")); got != "2ed6657d-e927-568b-95e1-2665a8aea6a2" {
		t.Errorf("uuid5(DNS, www.example.com) = %s, want RFC 9562's 2ed6657d-e927-568b-95e1-2665a8aea6a2", got)
	}
}

// TestNamespace: Namespace is uuid5(URL, NamespaceName), and stream UUIDs
// are what Python's uuid.uuid5(Namespace, name) gives (checked 2026-10-16),
// so they never change by accident.
func TestNamespace(t *testing.T) {
	if got := streams.FormatUUID(streams.NameUUID(urlNamespace, streams.NamespaceName)); got != streams.Namespace {
		t.Fatalf("uuid5(URL, %q) = %s, but Namespace is %s", streams.NamespaceName, got, streams.Namespace)
	}
	for name, want := range map[string]string{
		"king5":       "89fb6719-3e3d-5ef3-881a-b9f423a8a432",
		"groovesalad": "ede9025b-dc3f-5292-b3da-0092e7e1be22",
	} {
		if got := streams.UUID(name); got != want {
			t.Errorf("UUID(%q) = %s, want %s", name, got, want)
		}
	}
}

func TestParseUUID(t *testing.T) {
	u, err := streams.ParseUUID(streams.UUID("king5"))
	if err != nil || streams.FormatUUID(u) != streams.UUID("king5") {
		t.Fatalf("round trip: %v %v", u, err)
	}
	for _, bad := range []string{
		"", "89FB6719-3E3D-5EF3-881A-B9F423A8A432", "89fb67193e3d5ef3881ab9f423a8a432",
		"{89fb6719-3e3d-5ef3-881a-b9f423a8a432}", "89fb6719-3e3d-5ef3-881a-b9f423a8a43g", "89fb6719-3e3d-5ef3-881ab-9f423a8a432",
	} {
		if _, err := streams.ParseUUID(bad); err == nil {
			t.Errorf("ParseUUID(%q) = nil error", bad)
		}
	}
}

// TestAudio: one 20 ms frame of 16 kHz 16-bit mono is 640 bytes, the
// AudioSocket payload Asterisk 22.11's slin16 channel sends (plan §2(a)).
func TestAudio(t *testing.T) {
	if streams.FrameSamples != 320 || streams.FrameBytes != 640 || streams.Codec != "slin16" {
		t.Errorf("FrameSamples %d, FrameBytes %d, Codec %q", streams.FrameSamples, streams.FrameBytes, streams.Codec)
	}
}

func TestCheckSource(t *testing.T) {
	for _, ok := range []string{
		"http://192.168.0.58:5004/auto/v5.1",
		"https://ice2.somafm.com/groovesalad-128-mp3",
		"HTTPS://Example.com/live.m3u8",
		"http://fake-tuner.patchbay-e2e.svc:5004/auto/v7.1?hz=1300",
		"http://[2001:db8::1]:8000/radio",
	} {
		if err := streams.CheckSource(ok); err != nil {
			t.Errorf("CheckSource(%q) = %v", ok, err)
		}
	}
	for src, want := range map[string]string{
		"":                              "empty",
		"king 5":                        "white space",
		"http://x/a\tb":                 "white space",
		"/auto/v5.1":                    "scheme",
		"rtsp://192.168.0.58/auto/v5.1": "scheme",
		"file:///etc/passwd":            "scheme",
		"http://user:pass@192.168.0.58/auto/v5.1":                  "credentials",
		"http://user@192.168.0.58/auto/v5.1":                       "credentials",
		"http:///nohost":                                           "no host",
		"https://api.somafm.com/groovesalad.pls":                   "playlist",
		"https://example.com/RADIO.M3U":                            "playlist",
		"http://x/" + strings.Repeat("a", streams.MaxSourceLength): "long",
	} {
		err := streams.CheckSource(src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckSource(%q) = %v, want an error mentioning %q", src, err, want)
		}
	}
}

func house() streams.Document {
	return streams.Document{
		CallLimitSeconds: 7200,
		Pools:            []streams.Pool{{Name: "hdhomerun", Max: 1}, {Name: "attic", Max: 2}},
		Streams: map[string]streams.Stream{
			streams.UUID("king5"):       {Name: "king5", Number: "705", Source: "http://192.168.0.58:5004/auto/v5.1", Pool: "hdhomerun"},
			streams.UUID("groovesalad"): {Name: "groovesalad", Number: "750", Source: "https://ice2.somafm.com/groovesalad-128-mp3"},
		},
	}
}

// TestSealEncodeDecode: Sealed sorts pools and sets the version, Encode
// and Decode round-trip it, and the version follows the content only.
func TestSealEncodeDecode(t *testing.T) {
	d, err := house().Sealed()
	if err != nil {
		t.Fatal(err)
	}
	if d.Pools[0].Name != "attic" || len(d.Version) != 64 {
		t.Fatalf("sealed: %+v", d)
	}
	raw, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), "}\n") || !strings.Contains(string(raw), `"version": "`+d.Version+`"`) {
		t.Errorf("encoding:\n%s", raw)
	}
	back, err := streams.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := back.Encode(); string(again) != string(raw) {
		t.Errorf("decode then encode differs:\n%s\n%s", raw, again)
	}
	again, err := house().Sealed()
	if err != nil || again.Version != d.Version {
		t.Errorf("sealing the same content twice: %s, %s (%v)", d.Version, again.Version, err)
	}
	changed := house()
	changed.Pools[0].Max = 2
	if c, err := changed.Sealed(); err != nil || c.Version == d.Version {
		t.Errorf("a changed pool max kept version %s (%v)", c.Version, err)
	}
	changed = house()
	changed.CallLimitSeconds = 60
	if c, err := changed.Sealed(); err != nil || c.Version == d.Version {
		t.Errorf("a changed call limit kept version %s (%v)", c.Version, err)
	}
	noLimit := house()
	noLimit.CallLimitSeconds = 0 // the bridge then sets no limit of its own
	if c, err := noLimit.Sealed(); err != nil {
		t.Errorf("no call limit: %v", err)
	} else if raw, _ := c.Encode(); strings.Contains(string(raw), "callLimitSeconds") {
		t.Errorf("a zero call limit is written:\n%s", raw)
	}

	empty, err := streams.Document{}.Sealed()
	if err != nil {
		t.Fatal(err)
	}
	raw, err = empty.Encode()
	if err != nil || !strings.Contains(string(raw), `"pools": []`) || !strings.Contains(string(raw), `"streams": {}`) {
		t.Errorf("an empty document encodes as\n%s (%v)", raw, err)
	}
}

// TestDecodeStrict: unknown fields, trailing data, an unsealed or tampered
// document are refused.
func TestDecodeStrict(t *testing.T) {
	d, err := house().Sealed()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"unknown field": strings.Replace(string(raw), `"pools"`, `"extra": 1, "pools"`, 1),
		"trailing data": string(raw) + "{}",
		"tampered":      strings.Replace(string(raw), `auto/v5.1`, `auto/v7.1`, 1),
		"unsealed":      strings.Replace(string(raw), d.Version, "", 1),
	} {
		if _, err := streams.Decode([]byte(data)); err == nil {
			t.Errorf("%s: Decode accepted it", name)
		}
	}
}

// TestValidate: every rule, one broken at a time.
func TestValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*streams.Document)
		want string
	}{
		"pool not a label":   {func(d *streams.Document) { d.Pools[0].Name = "Attic" }, `pool "Attic"`},
		"pool twice":         {func(d *streams.Document) { d.Pools[1].Name = "attic" }, "listed twice"},
		"pool max 0":         {func(d *streams.Document) { d.Pools[0].Max = 0 }, "max 0"},
		"pool max too large": {func(d *streams.Document) { d.Pools[0].Max = streams.MaxPoolMax + 1 }, "max 17"},
		"pools unsorted":     {func(d *streams.Document) { d.Pools[0], d.Pools[1] = d.Pools[1], d.Pools[0] }, "not sorted"},
		"negative limit":     {func(d *streams.Document) { d.CallLimitSeconds = -1 }, "callLimitSeconds -1"},
		"limit too long":     {func(d *streams.Document) { d.CallLimitSeconds = streams.MaxCallLimitSeconds + 1 }, "callLimitSeconds 86401"},
		"wrong key": {func(d *streams.Document) {
			s := d.Streams[streams.UUID("king5")]
			s.Name = "king5b"
			d.Streams[streams.UUID("king5")] = s
		}, "not its UUID"},
		"number not digits": {func(d *streams.Document) {
			s := d.Streams[streams.UUID("king5")]
			s.Number = "70a"
			d.Streams[streams.UUID("king5")] = s
		}, `number "70a"`},
		"bad source": {func(d *streams.Document) {
			s := d.Streams[streams.UUID("king5")]
			s.Source = "ftp://x/y"
			d.Streams[streams.UUID("king5")] = s
		}, "scheme"},
		"unknown pool": {func(d *streams.Document) {
			s := d.Streams[streams.UUID("king5")]
			s.Pool = "cellar"
			d.Streams[streams.UUID("king5")] = s
		}, `pool "cellar"`},
		"one source two pools": {func(d *streams.Document) {
			s := d.Streams[streams.UUID("groovesalad")]
			s.Source = "http://192.168.0.58:5004/auto/v5.1"
			d.Streams[streams.UUID("groovesalad")] = s
		}, "share a source"},
		"bad name": {func(d *streams.Document) {
			d.Streams[streams.UUID("Bad")] = streams.Stream{Name: "Bad", Number: "709", Source: "http://x/y"}
		}, `stream "Bad"`},
	} {
		t.Run(name, func(t *testing.T) {
			d, err := house().Sealed()
			if err != nil {
				t.Fatal(err)
			}
			d.Pools = append([]streams.Pool(nil), d.Pools...)
			tc.edit(&d)
			if err := d.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
	var zero streams.Document
	if err := zero.Validate(); err == nil || !strings.Contains(err.Error(), "not sealed") {
		t.Errorf("a zero document: %v, want not sealed", err)
	}
}

// TestJSONFieldNames pins streams.json's field names, which the bridge and
// docs/streams.md rely on.
func TestJSONFieldNames(t *testing.T) {
	d, err := house().Sealed()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "callLimitSeconds", "pools", "streams"} {
		if _, ok := generic[k]; !ok {
			t.Errorf("no top-level %q in\n%s", k, raw)
		}
	}
	king := generic["streams"].(map[string]any)[streams.UUID("king5")].(map[string]any)
	for _, k := range []string{"name", "number", "source", "pool"} {
		if _, ok := king[k]; !ok {
			t.Errorf("no %q in king5's entry %v", k, king)
		}
	}
	if _, ok := generic["streams"].(map[string]any)[streams.UUID("groovesalad")].(map[string]any)["pool"]; ok {
		t.Errorf("a stream without a pool writes one")
	}
}
