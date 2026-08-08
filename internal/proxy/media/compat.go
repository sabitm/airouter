package media

// BackendCaps describes what attachment forms a backend transport can represent.
// This is wire/transport capability, not a per-model registry.
type BackendCaps struct {
	// Images: inline base64 and/or remote URL.
	ImageInline bool
	ImageURL    bool
	// When ImageURL is false but ImageInline is true, remote images must be
	// fetched and inlined before encode (Codex, Kiro, Antigravity).
	MaterializeImageURL bool

	// PDF document support (Anthropic document blocks, etc.).
	PDFInline bool
	PDFURL    bool

	// Generic non-PDF files (OpenAI Chat file parts, Responses input_file,
	// Antigravity inlineData).
	FileInline bool
	FileURL    bool

	// Provider-owned file IDs are only safe on same-codec-id passthrough.
	// Translated attempts never accept HasID-only attachments.
	FileID bool

	// ToolResultMedia is true when the backend encoder recursively preserves
	// image/file blocks nested inside tool_result. Encoders that flatten tool
	// results to plain text must leave this false so nested media fails closed.
	ToolResultMedia bool
}

// CapsForCodecID returns transport attachment capabilities for a backend codec id.
func CapsForCodecID(id string) BackendCaps {
	switch id {
	case "oai-chat":
		// Chat file_data carries PDFs and generic files the same way; there is
		// no stable public file_url field.
		return BackendCaps{
			ImageInline: true,
			ImageURL:    true,
			PDFInline:   true,
			FileInline:  true,
			FileID:      true, // passthrough-only enforced by caller
		}
	case "anth-msg", "claude-code":
		// encodeBlocks recurses into tool_result, preserving nested image/PDF.
		return BackendCaps{
			ImageInline:     true,
			ImageURL:        true,
			PDFInline:       true,
			PDFURL:          true,
			ToolResultMedia: true,
		}
	case "oai-responses":
		// input_file carries PDFs via the same inline/URL forms as generic files.
		return BackendCaps{
			ImageInline: true,
			ImageURL:    true,
			PDFInline:   true,
			PDFURL:      true,
			FileInline:  true,
			FileURL:     true,
			FileID:      true, // passthrough-only enforced by caller
		}
	case "oai-codex":
		// Codex reuses Responses image mapping but rejects PDF/files in v1.
		// Remote images are fetched inline when required by the wire path.
		return BackendCaps{
			ImageInline:         true,
			ImageURL:            false,
			MaterializeImageURL: true,
		}
	case "kiro":
		return BackendCaps{
			ImageInline:         true,
			ImageURL:            false,
			MaterializeImageURL: true,
		}
	case "antigravity":
		return BackendCaps{
			ImageInline:         true,
			ImageURL:            false,
			MaterializeImageURL: true,
			PDFInline:           true,
			FileInline:          true,
			// Remote file/PDF URLs are not fetched in v1.
		}
	case "qoder", "cursor":
		return BackendCaps{} // no image/file support
	default:
		return BackendCaps{}
	}
}

// Incompatible reports why the backend cannot represent the attachment set.
// Empty string means compatible (possibly after materialization). translated
// is true when the attempt is IR-translated (not same-codec-id passthrough);
// provider-owned file IDs are rejected on translated attempts.
func (c BackendCaps) Incompatible(atts []Attachment, translated bool) string {
	if len(atts) == 0 {
		return ""
	}
	for _, a := range atts {
		if a.InToolResult && !c.ToolResultMedia {
			return "backend does not support media inside tool_result blocks"
		}
		if a.IsImage || a.Kind == KindImage {
			if a.HasData && !c.ImageInline {
				return "backend does not support inline images"
			}
			if a.HasURL && !c.ImageURL && !c.MaterializeImageURL {
				return "backend does not support image URLs"
			}
			if !a.HasData && !a.HasURL {
				return "image attachment is missing a usable source"
			}
			if !c.ImageInline && !c.ImageURL && !c.MaterializeImageURL {
				return "backend does not support images"
			}
			continue
		}
		// File / document
		switch a.Kind {
		case KindPDF:
			if a.HasID {
				if translated || !c.FileID {
					return "provider file IDs cannot be translated to this backend"
				}
			}
			if a.HasData && !c.PDFInline {
				return "backend does not support PDF documents"
			}
			if a.HasURL && !c.PDFURL {
				// Do not fetch remote PDFs in v1.
				return "backend does not support remote PDF URLs"
			}
			if !a.HasData && !a.HasURL && !a.HasID {
				return "PDF attachment is missing a usable source"
			}
			if !c.PDFInline && !c.PDFURL && !(c.FileID && a.HasID && !translated) {
				return "backend does not support PDF documents"
			}
		default: // generic file
			if a.HasID {
				if translated || !c.FileID {
					return "provider file IDs cannot be translated to this backend"
				}
				continue
			}
			if a.HasData && !c.FileInline {
				return "backend does not support file attachments"
			}
			if a.HasURL && !c.FileURL {
				return "backend does not support remote file URLs"
			}
			if !a.HasData && !a.HasURL && !a.HasID {
				return "file attachment is missing a usable source"
			}
			if !c.FileInline && !c.FileURL && !(c.FileID && a.HasID && !translated) {
				return "backend does not support file attachments"
			}
		}
	}
	return ""
}

// NeedsImageMaterialization reports whether any image URL must be fetched
// inline for this backend.
func (c BackendCaps) NeedsImageMaterialization(atts []Attachment) bool {
	if !c.MaterializeImageURL {
		return false
	}
	for _, a := range atts {
		if (a.IsImage || a.Kind == KindImage) && a.HasURL && !a.HasData {
			return true
		}
	}
	return false
}
