package ir

import (
	"errors"
	"fmt"
)

// StreamFailure is a structured upstream stream (or stream-collected unary)
// failure parsed from a protocol error frame. Only typed fields are stored so
// the message can reach terminal logs without carrying raw event bodies.
type StreamFailure struct {
	Type    string // e.g. service_unavailable_error
	Code    string // e.g. server_is_overloaded
	Message string
}

// ProtocolError is a payload-free stream sequencing failure. Message must
// not contain signature bytes, redacted data, or response bodies.
func ProtocolError(message string) error {
	if message == "" {
		message = "invalid stream event sequence"
	}
	return &StreamFailure{Type: "invalid_request_error", Message: message}
}

func (e *StreamFailure) Error() string {
	if e == nil {
		return "upstream stream failed"
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return fmt.Sprintf("upstream stream failed: %s", e.Code)
	}
	if e.Type != "" {
		return fmt.Sprintf("upstream stream failed: %s", e.Type)
	}
	return "upstream stream failed"
}

// AsStreamFailure reports whether err is or wraps a *StreamFailure.
func AsStreamFailure(err error) (*StreamFailure, bool) {
	var sf *StreamFailure
	if errors.As(err, &sf) {
		return sf, true
	}
	return nil, false
}
