package media

import (
	"errors"
	"fmt"

	"airouter/internal/proxy/ir"
)

// Attachment describes one recognized image or file block found on a request.
type Attachment struct {
	Kind      Kind
	IsImage   bool
	HasData   bool
	HasURL    bool
	HasID     bool
	MediaType string
	Filename  string
	// InToolResult is true when the block is nested inside a tool_result. Most
	// backends flatten tool results to plain text and would silently drop nested
	// media; only transports that recursively encode tool_result content can
	// accept these (Anthropic / Claude Code).
	InToolResult bool
}

// InspectRequest walks the IR for image/file blocks, validates locally available
// payloads, and returns the attachment inventory. Malformed attachments return
// a client error without contacting providers. Inline fields on the request may
// be normalized in place (data-URL split, MIME canonicalization).
func InspectRequest(req *ir.Request) ([]Attachment, error) {
	if req == nil {
		return nil, nil
	}
	var out []Attachment
	var walk func(blocks []ir.ContentBlock, inToolResult bool) error
	walk = func(blocks []ir.ContentBlock, inToolResult bool) error {
		for i := range blocks {
			b := &blocks[i]
			switch b.Type {
			case ir.BlockImage:
				att, err := inspectImage(b)
				if err != nil {
					return err
				}
				if att != nil {
					att.InToolResult = inToolResult
					out = append(out, *att)
				}
			case ir.BlockFile:
				att, err := inspectFile(b)
				if err != nil {
					return err
				}
				if att != nil {
					att.InToolResult = inToolResult
					out = append(out, *att)
				}
			case ir.BlockToolResult:
				if err := walk(b.ToolResult, true); err != nil {
					return err
				}
			}
			if len(out) > MaxAttachments {
				return fmt.Errorf("%w: maximum %d", ErrTooManyAttachments, MaxAttachments)
			}
		}
		return nil
	}
	for i := range req.Messages {
		if err := walk(req.Messages[i].Content, false); err != nil {
			return nil, err
		}
	}
	if len(out) > MaxAttachments {
		return nil, fmt.Errorf("%w: maximum %d", ErrTooManyAttachments, MaxAttachments)
	}
	return out, nil
}

func inspectImage(b *ir.ContentBlock) (*Attachment, error) {
	if b.Image == nil {
		return nil, fmt.Errorf("%w: empty image block", ErrEmptyAttachment)
	}
	img := b.Image
	if img.Data == "" && IsDataURL(img.URL) {
		mt, data, _, err := ParseImageURL(img.URL)
		if err != nil {
			return nil, err
		}
		img.MediaType = mt
		img.Data = data
		img.URL = ""
	}
	if img.Data != "" && img.URL != "" {
		return nil, fmt.Errorf("%w: image has both data and url", ErrMultipleSources)
	}
	att := &Attachment{Kind: KindImage, IsImage: true, MediaType: CanonicalImageMIME(img.MediaType)}
	if img.Data != "" {
		att.HasData = true
		mt, err := ValidateInlinePayload(img.Data, img.MediaType, KindImage)
		if err != nil {
			return nil, err
		}
		img.MediaType = mt
		att.MediaType = mt
	} else if img.URL != "" {
		att.HasURL = true
		if IsDataURL(img.URL) {
			return nil, ErrInvalidDataURL
		}
		// Syntax-only check for native URL passthrough backends; no DNS.
		if _, err := ParseHTTPURL(img.URL); err != nil {
			return nil, err
		}
		if att.MediaType != "" && !IsSupportedImageMIME(att.MediaType) {
			return nil, fmt.Errorf("%w: %s", ErrUnsupportedMedia, att.MediaType)
		}
	} else {
		return nil, ErrEmptyAttachment
	}
	return att, nil
}

func inspectFile(b *ir.ContentBlock) (*Attachment, error) {
	if b.File == nil {
		return nil, fmt.Errorf("%w: empty file block", ErrEmptyAttachment)
	}
	f := b.File
	if f.Data == "" && IsDataURL(f.URL) {
		mt, data, err := ParseFileData(f.URL)
		if err != nil {
			return nil, err
		}
		if f.MediaType == "" {
			f.MediaType = mt
		}
		f.Data = data
		f.URL = ""
	}
	if IsDataURL(f.Data) {
		mt, data, err := ParseFileData(f.Data)
		if err != nil {
			return nil, err
		}
		if f.MediaType == "" {
			f.MediaType = mt
		}
		f.Data = data
	}

	mt := NormalizeMIME(f.MediaType)
	kind := Classify(mt)
	switch {
	case mt == "application/pdf" || kind == KindPDF:
		kind = KindPDF
	case kind == KindImage:
		// Image MIME on a file block stays a file attachment (generic path for
		// capability); bytes still validated as image when present.
		kind = KindGeneric
	case mt == "" && f.Data != "":
		raw, err := DecodeBase64(f.Data, MaxAttachmentBytes)
		if err != nil {
			return nil, err
		}
		switch {
		case IsPDF(raw):
			kind = KindPDF
			mt = "application/pdf"
			f.MediaType = mt
		case DetectImageMIME(raw) != "":
			kind = KindGeneric
			mt = DetectImageMIME(raw)
			f.MediaType = mt
		default:
			kind = KindGeneric
			mt = "application/octet-stream"
			f.MediaType = mt
		}
	case mt != "" && kind == KindUnknown:
		kind = KindGeneric
	case mt == "" && (f.URL != "" || f.ID != ""):
		kind = KindGeneric
	}

	att := &Attachment{Kind: kind, MediaType: mt, Filename: f.Filename}
	sources := 0
	if f.Data != "" {
		sources++
		att.HasData = true
	}
	if f.URL != "" {
		sources++
		att.HasURL = true
	}
	if f.ID != "" {
		sources++
		att.HasID = true
	}
	if sources == 0 {
		return nil, ErrEmptyAttachment
	}
	if sources > 1 {
		return nil, fmt.Errorf("%w: file has more than one of data/url/id", ErrMultipleSources)
	}
	if f.Data != "" {
		switch kind {
		case KindPDF:
			if _, err := ValidateInlinePayload(f.Data, mt, KindPDF); err != nil {
				return nil, err
			}
			att.MediaType = "application/pdf"
			f.MediaType = "application/pdf"
		default:
			if IsSupportedImageMIME(mt) {
				det, err := ValidateInlinePayload(f.Data, mt, KindImage)
				if err != nil {
					return nil, err
				}
				att.MediaType = det
				f.MediaType = det
			} else if _, err := ValidateInlinePayload(f.Data, mt, KindGeneric); err != nil {
				return nil, err
			}
		}
	} else if f.URL != "" {
		if IsDataURL(f.URL) {
			return nil, ErrInvalidDataURL
		}
		// Syntax-only check; remote PDF/file fetch is out of scope for v1.
		if _, err := ParseHTTPURL(f.URL); err != nil {
			return nil, err
		}
	}
	return att, nil
}

// ClientErrorStatus maps a media error to an HTTP status.
func ClientErrorStatus(err error) int {
	if err == nil {
		return 400
	}
	if errors.Is(err, ErrAttachmentTooLarge) {
		return 413
	}
	return 400
}
