package media

import (
	"bytes"
	"fmt"
)

// DetectImageMIME identifies PNG/JPEG/GIF/WebP from magic bytes. Empty string
// means unrecognized.
func DetectImageMIME(raw []byte) string {
	if len(raw) >= 8 && bytes.Equal(raw[:8], []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}) {
		return "image/png"
	}
	if len(raw) >= 3 && raw[0] == 0xFF && raw[1] == 0xD8 && raw[2] == 0xFF {
		return "image/jpeg"
	}
	if len(raw) >= 6 && (bytes.Equal(raw[:6], []byte("GIF87a")) || bytes.Equal(raw[:6], []byte("GIF89a"))) {
		return "image/gif"
	}
	// RIFF....WEBP
	if len(raw) >= 12 && bytes.Equal(raw[:4], []byte("RIFF")) && bytes.Equal(raw[8:12], []byte("WEBP")) {
		return "image/webp"
	}
	return ""
}

// IsPDF reports whether raw begins with the PDF magic header.
func IsPDF(raw []byte) bool {
	return len(raw) >= 5 && bytes.Equal(raw[:5], []byte("%PDF-"))
}

// ValidateImageBytes checks decoded image bytes against an optional declared
// MIME. Recognized PNG/JPEG/GIF/WebP magic is required and authoritative.
// Declared image MIME, when present, must match the magic. Unrecognized or
// conflicting bytes (including PDF labeled as an image) fail closed.
func ValidateImageBytes(raw []byte, declared string) (detected string, err error) {
	if len(raw) == 0 {
		return "", ErrEmptyAttachment
	}
	if len(raw) > MaxAttachmentBytes {
		return "", ErrAttachmentTooLarge
	}
	detected = DetectImageMIME(raw)
	if detected == "" {
		if declared != "" && IsSupportedImageMIME(declared) {
			return "", ErrSignatureMismatch
		}
		if IsPDF(raw) {
			return "", ErrSignatureMismatch
		}
		if declared != "" {
			return "", fmt.Errorf("%w: %s", ErrUnsupportedMedia, declared)
		}
		return "", ErrUnsupportedMedia
	}
	if declared != "" && CanonicalImageMIME(declared) != detected {
		return "", ErrSignatureMismatch
	}
	return detected, nil
}

// ValidatePDFBytes checks decoded PDF bytes.
func ValidatePDFBytes(raw []byte) error {
	if len(raw) == 0 {
		return ErrEmptyAttachment
	}
	if len(raw) > MaxAttachmentBytes {
		return ErrAttachmentTooLarge
	}
	if !IsPDF(raw) {
		return ErrSignatureMismatch
	}
	return nil
}

// ValidateInlinePayload validates locally available base64 for the given kind.
// kind should be KindImage, KindPDF, or KindGeneric. For images and PDFs the
// magic bytes are checked; generic files only get size/base64 validation.
func ValidateInlinePayload(base64Data, mediaType string, kind Kind) (canonMIME string, n int, err error) {
	raw, err := DecodeBase64(base64Data, MaxAttachmentBytes)
	if err != nil {
		return "", 0, err
	}
	n = len(raw)
	switch kind {
	case KindImage:
		det, err := ValidateImageBytes(raw, mediaType)
		if err != nil {
			return "", 0, err
		}
		return det, n, nil
	case KindPDF:
		if err := ValidatePDFBytes(raw); err != nil {
			return "", 0, err
		}
		return "application/pdf", n, nil
	default:
		// Generic: size already checked. Prefer declared MIME when present.
		mt := NormalizeMIME(mediaType)
		if mt == "" {
			mt = "application/octet-stream"
		}
		return mt, n, nil
	}
}
