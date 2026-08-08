package media

import (
	"fmt"
	"strings"
)

// DataURL is a parsed data: URI.
type DataURL struct {
	MediaType string
	// Data is the payload portion after the comma. When Base64 is true it is
	// base64 text (no data-URI wrapper); otherwise it is the raw percent-decoded
	// form as provided (we only accept base64 attachments in practice).
	Data   string
	Base64 bool
}

// ParseDataURL splits a data: URI into media type and payload. Only the
// ";base64" transfer encoding is accepted for attachment use; non-base64 data
// URLs return ErrInvalidDataURL so callers do not silently treat raw text as
// binary content.
func ParseDataURL(s string) (*DataURL, error) {
	const prefix = "data:"
	if !strings.HasPrefix(s, prefix) {
		return nil, ErrInvalidDataURL
	}
	rest := s[len(prefix):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return nil, ErrInvalidDataURL
	}
	meta := rest[:comma]
	payload := rest[comma+1:]
	if payload == "" {
		return nil, ErrInvalidDataURL
	}
	base64Flag := false
	mediaType := meta
	if meta == "" {
		mediaType = "text/plain"
	} else {
		parts := strings.Split(meta, ";")
		mediaType = parts[0]
		for _, p := range parts[1:] {
			if strings.EqualFold(strings.TrimSpace(p), "base64") {
				base64Flag = true
			}
		}
	}
	if !base64Flag {
		return nil, fmt.Errorf("%w: missing base64 marker", ErrInvalidDataURL)
	}
	return &DataURL{
		MediaType: NormalizeMIME(mediaType),
		Data:      payload,
		Base64:    true,
	}, nil
}

// IsDataURL reports whether s looks like a data: URI.
func IsDataURL(s string) bool {
	return strings.HasPrefix(s, "data:")
}

// RenderDataURL builds a data:<mt>;base64,<data> string.
func RenderDataURL(mediaType, base64Data string) string {
	mt := mediaType
	if mt == "" {
		mt = "application/octet-stream"
	}
	return "data:" + mt + ";base64," + base64Data
}

// ParseImageURL converts an OpenAI-style image URL (remote or data URI) into
// media type + base64 data or a remote URL. Malformed data URIs return an error
// rather than falling back to treating the whole string as a URL.
func ParseImageURL(url string) (mediaType, data, remote string, err error) {
	if url == "" {
		return "", "", "", ErrEmptyAttachment
	}
	if IsDataURL(url) {
		du, err := ParseDataURL(url)
		if err != nil {
			return "", "", "", err
		}
		if _, err := DecodeBase64Size(du.Data); err != nil {
			return "", "", "", err
		}
		return CanonicalImageMIME(du.MediaType), du.Data, "", nil
	}
	return "", "", url, nil
}

// ParseFileData accepts either a bare base64 payload or a data URL and returns
// normalized media type + base64 data. When the input is bare base64, mediaType
// is left empty for the caller to supply.
func ParseFileData(s string) (mediaType, data string, err error) {
	if s == "" {
		return "", "", ErrEmptyAttachment
	}
	if IsDataURL(s) {
		du, err := ParseDataURL(s)
		if err != nil {
			return "", "", err
		}
		if _, err := DecodeBase64Size(du.Data); err != nil {
			return "", "", err
		}
		return du.MediaType, du.Data, nil
	}
	if _, err := DecodeBase64Size(s); err != nil {
		return "", "", err
	}
	return "", s, nil
}
