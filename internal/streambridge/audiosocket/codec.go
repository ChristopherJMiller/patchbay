package audiosocket

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ReadFrame reads one frame: its header, then its payload. It returns
// io.EOF only when r ends at a frame boundary (before a header), and
// io.ErrUnexpectedEOF when r ends inside a frame. A zero-length payload is
// nil. Each call allocates the payload it returns (at most MaxPayload
// bytes, which the length field cannot exceed).
func ReadFrame(r io.Reader) (Frame, error) {
	var h [HeaderSize]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return Frame{}, err
	}
	f := Frame{Kind: Kind(h[0])}
	if n := binary.BigEndian.Uint16(h[1:]); n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return Frame{}, err
		}
	}
	return f, nil
}

// WriteFrame writes f in one Write call, header and payload together, so
// frames written by different goroutines to one connection never
// interleave. A payload longer than MaxPayload is an error, and nothing is
// written.
func WriteFrame(w io.Writer, f Frame) error {
	if len(f.Payload) > MaxPayload {
		return fmt.Errorf("audiosocket: a %s payload of %d bytes is longer than %d", f.Kind, len(f.Payload), MaxPayload)
	}
	b := AppendFrame(make([]byte, 0, HeaderSize+len(f.Payload)), f.Kind, f.Payload)
	n, err := w.Write(b)
	if err == nil && n < len(b) {
		err = io.ErrShortWrite
	}
	return err
}

// AppendFrame appends the frame of kind k with payload to dst and returns
// the extended slice. It panics if payload is longer than MaxPayload (a
// caller's bug: WriteFrame returns an error instead).
func AppendFrame(dst []byte, k Kind, payload []byte) []byte {
	if len(payload) > MaxPayload {
		panic(fmt.Sprintf("audiosocket: a payload of %d bytes is longer than %d", len(payload), MaxPayload))
	}
	dst = append(dst, byte(k), 0, 0)
	binary.BigEndian.PutUint16(dst[len(dst)-2:], uint16(len(payload)))
	return append(dst, payload...)
}

// UUIDOf is the UUID a KindUUID frame carries: the 16 bytes that
// streams.FormatUUID writes as text. Any other kind, or a payload that is
// not UUIDSize bytes, is an error.
func UUIDOf(f Frame) ([16]byte, error) {
	var u [16]byte
	switch {
	case f.Kind != KindUUID:
		return u, fmt.Errorf("audiosocket: a %s frame, not a uuid frame", f.Kind)
	case len(f.Payload) != UUIDSize:
		return u, fmt.Errorf("audiosocket: a uuid frame of %d bytes, not %d", len(f.Payload), UUIDSize)
	}
	copy(u[:], f.Payload)
	return u, nil
}
