// Package api is the wire contract of the controller's messages endpoint:
// the JSON Home Assistant (and any caller holding the token) POSTs to
// DefaultPort to put a message on desk phones, take it back, and tell the
// controller the house's state (quiet, away).
//
// The limits of MessageRequest mirror the Message CRD
// (api/v1alpha1/message_types.go: title 0-32, text 1-256, key a DNS label,
// no control characters); the endpoint rejects with 400 what the CRD would
// refuse, so a caller sees the reason rather than a create error. Change
// them together.
//
// The package holds types, their validation and the House file's parser, so
// the endpoint, the reconciler (which reads House from the HouseConfigMap)
// and the e2e suite import it without either implementation. Callers
// import it as messagesapi.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Ports and paths.
const (
	// DefaultPort is the messages endpoint's port.
	DefaultPort = 9441

	// PathMessages is POST: a MessageRequest; 201 with a MessageResponse.
	PathMessages = "/v1/messages"
	// PathWithdraw is POST: a WithdrawRequest; 200 with a WithdrawResponse.
	PathWithdraw = "/v1/withdraw"
	// PathHouse is GET (200 with the House) and PUT (a HouseUpdate; 200
	// with the House as stored).
	PathHouse = "/v1/house"
)

// Limits, mirroring the Message CRD.
const (
	// MaxTitle is the longest title, in characters.
	MaxTitle = 32
	// MaxText is the longest text, in characters.
	MaxText = 256
	// DefaultExpires is the lifetime of a message that does not say.
	DefaultExpires = time.Hour
	// MaxExpires is the longest lifetime.
	MaxExpires = 24 * time.Hour
)

// MessageRequest is POST PathMessages' body: one message to one or more
// phones (the endpoint creates one Message per phone).
type MessageRequest struct {
	// To names the Phones, each a DNS-1123 subdomain. At least one.
	To []string `json:"to"`
	// Title is the heading, 0-32 characters, no control characters.
	Title string `json:"title"`
	// Text is the message, 1-256 characters, no control characters.
	Text string `json:"text"`
	// Urgent messages are shown in quiet hours and away; a call in
	// progress holds them like any other.
	Urgent bool `json:"urgent"`
	// Expires is how long the message stays worth showing, a Go duration
	// from 0s to 24h; "" means "1h".
	Expires string `json:"expires"`
	// Key makes a newer message with the same key replace this one on the
	// same phone. "" or a DNS-1123 label.
	Key string `json:"key"`
}

// ExpiresIn is Expires as a duration, DefaultExpires when empty. It does
// not check the range; Validate does.
func (r MessageRequest) ExpiresIn() (time.Duration, error) {
	if r.Expires == "" {
		return DefaultExpires, nil
	}
	d, err := time.ParseDuration(r.Expires)
	if err != nil {
		return 0, fmt.Errorf("expires %q: %w", r.Expires, err)
	}
	return d, nil
}

// Validate checks the request against the limits above.
func (r MessageRequest) Validate() error {
	var errs []error
	if len(r.To) == 0 {
		errs = append(errs, errors.New("to: at least one phone is required"))
	}
	errs = append(errs, checkPhones(r.To)...)
	if n := utf8.RuneCountInString(r.Text); n < 1 || n > MaxText {
		errs = append(errs, fmt.Errorf("text: must be 1-%d characters, is %d", MaxText, n))
	}
	if hasControl(r.Text) {
		errs = append(errs, errors.New("text: control characters are not allowed"))
	}
	if n := utf8.RuneCountInString(r.Title); n > MaxTitle {
		errs = append(errs, fmt.Errorf("title: must be at most %d characters, is %d", MaxTitle, n))
	}
	if hasControl(r.Title) {
		errs = append(errs, errors.New("title: control characters are not allowed"))
	}
	if d, err := r.ExpiresIn(); err != nil {
		errs = append(errs, err)
	} else if d < 0 || d > MaxExpires {
		errs = append(errs, fmt.Errorf("expires %q: must be between 0s and %s", r.Expires, MaxExpires))
	}
	if r.Key != "" {
		errs = append(errs, checkKey(r.Key)...)
	}
	return errors.Join(errs...)
}

// MessageResponse is the 201 body: the names of the Messages created, one
// per phone.
type MessageResponse struct {
	Names []string `json:"names"`
}

// WithdrawRequest is POST PathWithdraw's body: take back the messages with
// a key.
type WithdrawRequest struct {
	// Key is required, a DNS-1123 label.
	Key string `json:"key"`
	// To limits it to these Phones; empty means every phone.
	To []string `json:"to"`
}

// Validate checks the request.
func (r WithdrawRequest) Validate() error {
	var errs []error
	if r.Key == "" {
		errs = append(errs, errors.New("key: required"))
	} else {
		errs = append(errs, checkKey(r.Key)...)
	}
	return errors.Join(append(errs, checkPhones(r.To)...)...)
}

// WithdrawResponse is the 200 body: how many Messages were withdrawn.
type WithdrawResponse struct {
	Withdrawn int `json:"withdrawn"`
}

// HouseUpdate is PUT PathHouse's body; a field left out is left as it is.
type HouseUpdate struct {
	// Quiet turns quiet hours on or off.
	Quiet *bool `json:"quiet,omitempty"`
	// Away says nobody is home.
	Away *bool `json:"away,omitempty"`
}

// Validate checks that the update sets something.
func (u HouseUpdate) Validate() error {
	if u.Quiet == nil && u.Away == nil {
		return errors.New("house: set quiet, away or both")
	}
	return nil
}

// House is the house state: PUT's and GET's response body, and the JSON in
// the HouseConfigMap's HouseKey.
type House struct {
	Quiet     bool      `json:"quiet"`
	Away      bool      `json:"away"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ParseHouse reads a House strictly: an unknown field or anything after
// the object is an error.
func ParseHouse(raw []byte) (House, error) {
	var h House
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return House{}, fmt.Errorf("house: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return House{}, errors.New("house: unexpected data after the object")
	}
	return h, nil
}

// Error is the body of every non-2xx answer.
type Error struct {
	Error string `json:"error"`
}

func checkPhones(to []string) []error {
	var errs []error
	for _, p := range to {
		for _, msg := range validation.IsDNS1123Subdomain(p) {
			errs = append(errs, fmt.Errorf("to: phone %q: %s", p, msg))
		}
	}
	return errs
}

func checkKey(key string) []error {
	var errs []error
	for _, msg := range validation.IsDNS1123Label(key) {
		errs = append(errs, fmt.Errorf("key %q: %s", key, msg))
	}
	return errs
}

// hasControl reports a C0 control, DEL or a C1 control, the CRD's
// [\x00-\x1F\x7F-\x9F] pattern.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return true
		}
	}
	return false
}
