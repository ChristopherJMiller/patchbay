package sensor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// The file server's socket protocol (docs/plans/release-a.md, W2 task
// level). The file server runs as another process, so it reports its
// transfers to the agent over the unix socket layout.FetchSocket:
//
//   - the agent listens (it starts first, as the pod's native sidecar),
//     removes a stale socket file before binding, chmods the socket to
//     layout.FetchSocketMode, and accepts any number of connections;
//   - the file server connects and writes one line per event: the JSON
//     encoding of one FetchEvent, its fields only, then "\n"
//     (WriteFetchEvent). Nothing is ever sent back;
//   - the agent reads with a FetchEventReader. A line longer than
//     MaxWireLine, or one that does not decode into exactly one valid event
//     with only known fields, ends that connection; the events before it
//     stand;
//   - the file server queues events while it is not connected (bounded;
//     the oldest is dropped first, and drops are counted and logged) and
//     reconnects about once a second. Nothing is resent: an event the agent
//     missed while it restarted is lost, like the rest of its memory.
//
// Both ends are the same binary, so the protocol has no version handshake;
// a new field is added to FetchEvent and to both ends in one change.

// MaxWireLine is the longest line, newline included, either end accepts.
// A valid event encodes to well under it.
const MaxWireLine = 16 << 10

// ErrLineTooLong is returned by FetchEventReader.Next for a line over
// MaxWireLine.
var ErrLineTooLong = errors.New("sensor: fetch event line too long")

// WriteFetchEvent validates e and writes it as one line, in one Write.
func WriteFetchEvent(w io.Writer, e FetchEvent) error {
	if err := e.Validate(); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil { // Encode appends the "\n"
		return fmt.Errorf("sensor: encoding a fetch event: %w", err)
	}
	if buf.Len() > MaxWireLine {
		return ErrLineTooLong
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// FetchEventReader reads the events WriteFetchEvent wrote.
type FetchEventReader struct {
	s *bufio.Scanner
}

// NewFetchEventReader reads events from r.
func NewFetchEventReader(r io.Reader) *FetchEventReader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 4096), MaxWireLine)
	s.Split(scanWholeLines)
	return &FetchEventReader{s: s}
}

// errPartialLine is a stream that ended inside a line: a writer that died
// mid-event. Its last bytes are not an event, whatever they hold.
var errPartialLine = errors.New("sensor: the stream ended inside a fetch event line")

// scanWholeLines is bufio.ScanLines without its leniency: a line must end
// in "\n", and a "\r" before it is not stripped.
func scanWholeLines(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return 0, nil, errPartialLine
	}
	return 0, nil, nil
}

// Next returns the next event. It returns io.EOF at the end of a stream
// that ended after a whole line, ErrLineTooLong for an oversized line, and
// another error for a line that is not exactly one valid FetchEvent (an
// unknown field, trailing data, a failed Validate) or a stream that ended
// inside a line. After an error the reader is done.
func (r *FetchEventReader) Next() (FetchEvent, error) {
	if !r.s.Scan() {
		switch err := r.s.Err(); {
		case errors.Is(err, bufio.ErrTooLong):
			return FetchEvent{}, ErrLineTooLong
		case err != nil:
			return FetchEvent{}, err
		}
		return FetchEvent{}, io.EOF
	}
	line := r.s.Bytes()
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var e FetchEvent
	if err := dec.Decode(&e); err != nil {
		return FetchEvent{}, fmt.Errorf("sensor: decoding a fetch event: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return FetchEvent{}, errors.New("sensor: trailing data after a fetch event")
	}
	if err := e.Validate(); err != nil {
		return FetchEvent{}, err
	}
	return e, nil
}
