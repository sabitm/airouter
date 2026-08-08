// Package media provides shared attachment helpers for the inference proxy:
// data-URL parsing, base64 size accounting, MIME classification, magic-byte
// checks, and request-scoped remote image materialization with SSRF guards.
package media

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	// MaxAttachmentBytes is the per-attachment decoded payload cap.
	MaxAttachmentBytes = 10 << 20 // 10 MiB
	// MaxAttachments is the maximum number of recognized image/file blocks
	// allowed on one request.
	MaxAttachments = 8
	// FetchTimeout bounds a single remote image download.
	FetchTimeoutSeconds = 10
)

// Kind classifies a MIME type for transport capability checks.
type Kind int

const (
	KindUnknown Kind = iota
	KindImage
	KindPDF
	KindGeneric
)

// Errors returned to ingress as client-facing attachment failures.
var (
	ErrTooManyAttachments = errors.New("too many attachments")
	ErrAttachmentTooLarge = errors.New("attachment exceeds size limit")
	ErrInvalidDataURL     = errors.New("invalid data URL")
	ErrInvalidBase64      = errors.New("invalid base64 attachment payload")
	ErrUnsupportedMedia   = errors.New("unsupported media type")
	ErrSignatureMismatch  = errors.New("attachment content does not match declared type")
	ErrEmptyAttachment    = errors.New("attachment has no usable source")
	ErrMultipleSources    = errors.New("attachment specifies multiple source forms")
	ErrUnsafeURL          = errors.New("attachment URL is not allowed")
	ErrFetchFailed        = errors.New("failed to fetch remote attachment")
	ErrRedirect           = errors.New("remote attachment redirects are not followed")
)

// NormalizeMIME lowercases and strips parameters (e.g. "; charset=utf-8").
func NormalizeMIME(mt string) string {
	mt = strings.TrimSpace(strings.ToLower(mt))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	return mt
}

// Classify returns the attachment kind for a MIME type.
func Classify(mt string) Kind {
	switch NormalizeMIME(mt) {
	case "image/png", "image/jpeg", "image/jpg", "image/gif", "image/webp":
		return KindImage
	case "application/pdf":
		return KindPDF
	case "":
		return KindUnknown
	default:
		if strings.HasPrefix(NormalizeMIME(mt), "image/") {
			// Unsupported image subtypes (svg, heic, ...) are not KindImage for
			// the allowed-image set; treat as unknown so callers fail closed.
			return KindUnknown
		}
		return KindGeneric
	}
}

// IsSupportedImageMIME reports whether mt is one of the four allowed image types.
func IsSupportedImageMIME(mt string) bool {
	switch NormalizeMIME(mt) {
	case "image/png", "image/jpeg", "image/jpg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

// CanonicalImageMIME maps image/jpg -> image/jpeg and leaves others normalized.
func CanonicalImageMIME(mt string) string {
	mt = NormalizeMIME(mt)
	if mt == "image/jpg" {
		return "image/jpeg"
	}
	return mt
}

// DecodeBase64Size validates standard or raw-std base64 and returns decoded size.
// It does not retain the decoded bytes beyond the size check.
func DecodeBase64Size(data string) (int, error) {
	raw, err := DecodeBase64(data, MaxAttachmentBytes)
	if err != nil {
		return 0, err
	}
	return len(raw), nil
}

// DecodeBase64 returns decoded bytes after size validation.
func DecodeBase64(data string, max int) ([]byte, error) {
	if data == "" {
		return nil, ErrInvalidBase64
	}
	if strings.ContainsAny(data, " \t\r\n") {
		return nil, ErrInvalidBase64
	}
	// Rough upper bound before allocating: base64 expands ~4/3.
	if max > 0 && len(data) > (max/3)*4+8 {
		return nil, ErrAttachmentTooLarge
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidBase64, err)
		}
	}
	if max > 0 && len(raw) > max {
		return nil, ErrAttachmentTooLarge
	}
	return raw, nil
}

// EncodeBase64 returns standard padded base64.
func EncodeBase64(raw []byte) string {
	return base64.StdEncoding.EncodeToString(raw)
}
