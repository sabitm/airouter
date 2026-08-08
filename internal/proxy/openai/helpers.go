package openai

import (
	"encoding/json"
	"strings"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/media"
)

// parseStop normalizes the OpenAI `stop` field (string or []string) to a slice.
func parseStop(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return []string{s}
		}
		return nil
	}
	var arr []string
	_ = json.Unmarshal(raw, &arr)
	return arr
}

// contentToText flattens a message content field (string, null, or array of
// parts) into plain text, concatenating any text parts.
func contentToText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var parts []chatPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func toolResultText(b ir.ContentBlock) string {
	var sb strings.Builder
	for _, r := range b.ToolResult {
		if r.Type == ir.BlockText {
			sb.WriteString(r.Text)
		}
	}
	return sb.String()
}

// imageFromURL parses an OpenAI image_url, splitting an inline data URI into
// media type + base64 data, or keeping a remote URL as-is. Malformed data URIs
// still produce an Image so InspectRequest can reject them with a clear error;
// the URL field is left as the original string only when it is not a data URI.
func imageFromURL(url string) *ir.Image {
	if url == "" {
		return &ir.Image{}
	}
	if media.IsDataURL(url) {
		mt, data, _, err := media.ParseImageURL(url)
		if err != nil {
			// Preserve the raw value under Data empty + URL so validation sees it.
			return &ir.Image{URL: url}
		}
		return &ir.Image{MediaType: mt, Data: data}
	}
	return &ir.Image{URL: url}
}

// imageToURL renders an IR image back to an OpenAI image_url string.
func imageToURL(img *ir.Image) string {
	if img == nil {
		return ""
	}
	if img.Data != "" {
		mt := img.MediaType
		if mt == "" {
			mt = "image/png"
		}
		return media.RenderDataURL(mt, img.Data)
	}
	return img.URL
}

func fileFromChat(f *chatFile) *ir.File {
	if f == nil {
		return &ir.File{}
	}
	out := &ir.File{Filename: f.Filename, ID: f.FileID}
	if f.FileData == "" {
		return out
	}
	mt, data, err := media.ParseFileData(f.FileData)
	if err != nil {
		// Leave Data as provided so InspectRequest can surface the error; if it
		// looks like a data URL keep it in URL for the same reason.
		if media.IsDataURL(f.FileData) {
			out.URL = f.FileData
		} else {
			out.Data = f.FileData
		}
		return out
	}
	out.MediaType = mt
	out.Data = data
	if out.MediaType == "" && out.Filename != "" {
		out.MediaType = mimeFromFilename(out.Filename)
	}
	return out
}

func chatFileFromIR(f *ir.File) *chatFile {
	if f == nil {
		return nil
	}
	out := &chatFile{Filename: f.Filename, FileID: f.ID}
	if f.Data != "" {
		mt := f.MediaType
		if mt == "" {
			mt = "application/octet-stream"
		}
		out.FileData = media.RenderDataURL(mt, f.Data)
	} else if f.URL != "" && media.IsDataURL(f.URL) {
		out.FileData = f.URL
	}
	// Remote file URLs are not a stable Chat Completions field; only inline/id.
	return out
}

func mimeFromFilename(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".txt"):
		return "text/plain"
	case strings.HasSuffix(lower, ".json"):
		return "application/json"
	default:
		return ""
	}
}

func mustText(s string) json.RawMessage {
	raw, _ := json.Marshal(s)
	return raw
}

// rawOrNull returns valid JSON for a tool argument payload, defaulting empty to
// an empty object so downstream parsers never choke on "".
func rawOrNull(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}
