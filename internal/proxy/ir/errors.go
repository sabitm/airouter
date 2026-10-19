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

// RequestValidationError is a payload-free local rejection of caller request
// data. It marks a fault the client can fix, such as tool-call arguments that
// are not valid JSON, so the proxy returns HTTP 400 without contacting any
// upstream. The message carries only message/block location and must never
// include raw arguments, tool names, or tool ids.
type RequestValidationError struct {
	Message string
}

func (e *RequestValidationError) Error() string {
	if e == nil || e.Message == "" {
		return "invalid request"
	}
	return e.Message
}

// InvalidRequest returns a RequestValidationError with message. Callers must
// not place caller-supplied content in message.
func InvalidRequest(message string) error {
	return &RequestValidationError{Message: message}
}

// AsRequestValidation reports whether err is or wraps a *RequestValidationError.
func AsRequestValidation(err error) (*RequestValidationError, bool) {
	var rv *RequestValidationError
	if errors.As(err, &rv) {
		return rv, true
	}
	return nil, false
}
