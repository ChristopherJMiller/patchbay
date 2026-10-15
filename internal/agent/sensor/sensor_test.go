package sensor_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherJMiller/patchbay/internal/agent/api"
	"github.com/ChristopherJMiller/patchbay/internal/agent/sensor"
)

var (
	t0 = time.Date(2026, 10, 14, 12, 0, 0, 123456789, time.UTC)
	v1 = strings.Repeat("ab", 32)
	d1 = strings.Repeat("cd", 32)
)

// sep is a served TFTP fetch of a phone's config, as the file server
// reports it.
func sep() sensor.FetchEvent {
	return sensor.FetchEvent{
		Transport:     api.TransportTFTP,
		File:          "SEPD0C78915E8C8.cnf.xml",
		Result:        api.FetchServed,
		At:            t0,
		Source:        "192.168.20.101",
		BundleVersion: v1,
		BundlePath:    "tftp/SEPD0C78915E8C8.cnf.xml",
	}
}

func TestValidate(t *testing.T) {
	valid := map[string]sensor.FetchEvent{
		"served SEP": sep(),
		"not found sgn": func() sensor.FetchEvent {
			e := sep()
			e.File, e.Result, e.BundlePath = "SEPD0C78915E8C8.cnf.xml.sgn", api.FetchNotFound, "tftp/SEPD0C78915E8C8.cnf.xml.sgn"
			return e
		}(),
		"firmware over 6970": {
			Transport: api.TransportHTTP6970, File: "apps45.9-4-2SR4-3.sbn", Result: api.FetchServed,
			At: t0, Source: "192.168.20.101",
		},
		"failed transfer": func() sensor.FetchEvent {
			e := sep()
			e.Result, e.Error = api.FetchError, "timeout after block 12"
			return e
		}(),
		"provisioning served": {
			Transport: api.TransportHTTPDevice, File: "provisioning/softphone-den.xml", Result: api.FetchServed,
			At: t0, Source: "192.168.0.50", BundleVersion: v1, BundlePath: "provisioning/softphone-den.xml",
			Device: "softphone-den", Version: d1,
		},
		"provisioning spent": {
			Transport: api.TransportHTTPDevice, File: "provisioning/softphone-den.xml", Result: api.FetchNotFound,
			At: t0, Source: "192.168.0.50", BundleVersion: v1, BundlePath: "provisioning/softphone-den.xml",
			Device: "softphone-den",
		},
		"provisioning unknown token": {
			Transport: api.TransportHTTPDevice, File: sensor.ProvisioningUnknownFile, Result: api.FetchNotFound,
			At: t0, Source: "192.168.0.50",
		},
		"IPv6 source": func() sensor.FetchEvent { e := sep(); e.Source = "fd00::1"; return e }(),
	}
	for name, e := range valid {
		if err := e.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	invalid := map[string]func(*sensor.FetchEvent){
		"unknown transport":       func(e *sensor.FetchEvent) { e.Transport = "ftp" },
		"unknown result":          func(e *sensor.FetchEvent) { e.Result = "partial" },
		"error on served":         func(e *sensor.FetchEvent) { e.Error = "x" },
		"empty file":              func(e *sensor.FetchEvent) { e.File = "" },
		"leading slash":           func(e *sensor.FetchEvent) { e.File = "/SEPD0C78915E8C8.cnf.xml" },
		"long file":               func(e *sensor.FetchEvent) { e.File = strings.Repeat("a", sensor.MaxFileLength+1) },
		"control in file":         func(e *sensor.FetchEvent) { e.File = "SEP\nx" },
		"invalid UTF-8 file":      func(e *sensor.FetchEvent) { e.File = "SEP\xff" },
		"long error":              func(e *sensor.FetchEvent) { e.Result, e.Error = api.FetchError, strings.Repeat("e", 513) },
		"no time":                 func(e *sensor.FetchEvent) { e.At = time.Time{} },
		"source with port":        func(e *sensor.FetchEvent) { e.Source = "192.168.20.101:69" },
		"mapped source":           func(e *sensor.FetchEvent) { e.Source = "::ffff:192.168.20.101" },
		"non-canonical source":    func(e *sensor.FetchEvent) { e.Source = "fd00:0::1" },
		"empty source":            func(e *sensor.FetchEvent) { e.Source = "" },
		"bad bundle version":      func(e *sensor.FetchEvent) { e.BundleVersion = "../v1" },
		"path without version":    func(e *sensor.FetchEvent) { e.BundleVersion = "" },
		"path outside the trees":  func(e *sensor.FetchEvent) { e.BundlePath = "firmware/x.sbn" },
		"path escaping":           func(e *sensor.FetchEvent) { e.BundlePath = "tftp/../asterisk/pjsip.conf" },
		"bad device":              func(e *sensor.FetchEvent) { e.Device = "Phone_Desk" },
		"long device":             func(e *sensor.FetchEvent) { e.Device = "phone-" + strings.Repeat("a", 25) },
		"version without device":  func(e *sensor.FetchEvent) { e.Version = d1 },
		"bad version with device": func(e *sensor.FetchEvent) { e.Device, e.Version = "phone-desk", "v1" },
	}
	for name, mutate := range invalid {
		e := sep()
		mutate(&e)
		if err := e.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, e)
		}
	}
}

// TestWireFormat pins the bytes on the socket: one JSON object per line,
// these field names, empty optional fields left out, nanosecond RFC 3339
// time, and no HTML escaping.
func TestWireFormat(t *testing.T) {
	e := sep()
	e.File, e.BundlePath = "SEP<&>.cnf.xml", ""
	e.Result = api.FetchNotFound
	var buf bytes.Buffer
	if err := sensor.WriteFetchEvent(&buf, e); err != nil {
		t.Fatal(err)
	}
	want := `{"transport":"tftp","file":"SEP<&>.cnf.xml","result":"notFound","at":"2026-10-14T12:00:00.123456789Z",` +
		`"source":"192.168.20.101","bundleVersion":"` + v1 + `"}` + "\n"
	if got := buf.String(); got != want {
		t.Fatalf("wire line\n got %s\nwant %s", got, want)
	}
}

func TestWireRoundTrip(t *testing.T) {
	events := []sensor.FetchEvent{sep(), {
		Transport: api.TransportHTTP6970, File: "term45.default.loads", Result: api.FetchNotFound,
		At: t0.Add(time.Second), Source: "192.168.20.102",
	}}
	var buf bytes.Buffer
	for _, e := range events {
		if err := sensor.WriteFetchEvent(&buf, e); err != nil {
			t.Fatal(err)
		}
	}
	r := sensor.NewFetchEventReader(&buf)
	for i, want := range events {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if !got.At.Equal(want.At) {
			t.Errorf("event %d: At %v, want %v", i, got.At, want.At)
		}
		got.At = want.At
		if got != want {
			t.Errorf("event %d:\n got %+v\nwant %+v", i, got, want)
		}
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("after the last event: %v, want io.EOF", err)
	}
}

func TestWriteRefusesInvalidEvents(t *testing.T) {
	e := sep()
	e.Source = "not-an-ip"
	var buf bytes.Buffer
	if err := sensor.WriteFetchEvent(&buf, e); err == nil || buf.Len() != 0 {
		t.Fatalf("WriteFetchEvent(invalid) = %v and wrote %q", err, buf.String())
	}
}

func TestReaderRefuses(t *testing.T) {
	var good bytes.Buffer
	if err := sensor.WriteFetchEvent(&good, sep()); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSuffix(good.String(), "\n")
	for name, tc := range map[string]struct {
		input   string
		tooLong bool
	}{
		"unknown field":  {input: strings.TrimSuffix(line, "}") + `,"token":"x"}` + "\n"},
		"trailing data":  {input: line + ` {}` + "\n"},
		"two objects":    {input: line + line + "\n"},
		"invalid event":  {input: strings.Replace(line, `"tftp"`, `"ftp"`, 1) + "\n"},
		"not JSON":       {input: "hello\n"},
		"partial line":   {input: line},
		"truncated line": {input: line[:len(line)/2]},
		"too long":       {input: `{"file":"` + strings.Repeat("a", sensor.MaxWireLine) + "\"}\n", tooLong: true},
	} {
		// A good event first: it stands even though the connection then ends.
		r := sensor.NewFetchEventReader(strings.NewReader(good.String() + tc.input))
		if _, err := r.Next(); err != nil {
			t.Fatalf("%s: the good event before it: %v", name, err)
		}
		_, err := r.Next()
		switch {
		case err == nil || errors.Is(err, io.EOF):
			t.Errorf("%s: Next() = %v, want an error", name, err)
		case tc.tooLong != errors.Is(err, sensor.ErrLineTooLong):
			t.Errorf("%s: Next() = %v, ErrLineTooLong expected %t", name, err, tc.tooLong)
		}
	}
}

func TestRecorderFunc(t *testing.T) {
	var got []sensor.FetchEvent
	var rec sensor.FetchRecorder = sensor.FetchRecorderFunc(func(e sensor.FetchEvent) { got = append(got, e) })
	rec.RecordFetch(sep())
	if len(got) != 1 || got[0] != sep() {
		t.Fatalf("recorded %+v", got)
	}
}
