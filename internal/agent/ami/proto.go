package ami

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// The AMI wire format: after a one-line banner ("Asterisk Call Manager/<v>"),
// both directions are messages of "Key: Value" lines ending in CRLF, each
// message ended by an empty line. Asterisk sometimes ends a line with a bare
// LF (a message text that holds its own "\n"), so the reader accepts both
// and treats a message with no fields as nothing.

// Limits on what the reader accepts from Asterisk: anything bigger ends the
// session rather than growing memory.
const (
	maxLine   = 64 << 10
	maxFields = 1024
)

// field is one "Key: Value" line.
type field struct{ key, value string }

// message is one AMI message, its fields in order.
type message []field

// get returns the first value of key, compared case-insensitively as
// Asterisk does, or "".
func (m message) get(key string) string {
	for _, f := range m {
		if strings.EqualFold(f.key, key) {
			return f.value
		}
	}
	return ""
}

var (
	errLineTooLong = errors.New("ami: a line from Asterisk is too long")
	errTooMany     = errors.New("ami: a message from Asterisk has too many fields")
)

// readLine reads one line without its CRLF or LF.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return "", errLineTooLong
	}
	if err != nil {
		return "", err
	}
	line = bytes.TrimSuffix(line[:len(line)-1], []byte("\r"))
	return string(line), nil
}

// readMessage reads one message (possibly empty: see the format notes).
func readMessage(r *bufio.Reader) (message, error) {
	var m message
	for {
		line, err := readLine(r)
		if err != nil {
			return nil, err
		}
		if line == "" {
			return m, nil
		}
		if len(m) == maxFields {
			return nil, errTooMany
		}
		key, value, _ := strings.Cut(line, ":")
		m = append(m, field{key: strings.TrimSpace(key), value: strings.TrimSpace(value)})
	}
}

// action is one request: Action: name, then its fields in order.
type action struct {
	name   string
	fields []field
}

// encode writes an action with its ActionID: "Action", "ActionID", then the
// fields as given, each "Key: Value\r\n", then "\r\n". No key or value may
// hold CR or LF (ErrUnsafeValue): AMI would read the rest as another field.
func (a action) encode(id string) ([]byte, error) {
	var b bytes.Buffer
	all := append([]field{{"Action", a.name}, {"ActionID", id}}, a.fields...)
	for _, f := range all {
		if strings.ContainsAny(f.key, "\r\n:") || f.key == "" || strings.ContainsAny(f.value, "\r\n") {
			return nil, fmt.Errorf("%w: %s field %q", ErrUnsafeValue, a.name, f.key)
		}
		b.WriteString(f.key)
		b.WriteString(": ")
		b.WriteString(f.value)
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	return b.Bytes(), nil
}

// responseError is Asterisk answering an action with anything but Success.
type responseError struct {
	action   string
	response string
	message  string
}

func (e *responseError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("ami: %s: Asterisk answered %q", e.action, e.response)
	}
	return fmt.Sprintf("ami: %s: %s", e.action, e.message)
}
