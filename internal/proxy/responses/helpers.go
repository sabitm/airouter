package responses

import (
	"encoding/json"
	"strings"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/media"
)

func contentToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var parts []contentPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// outputToText flattens a function_call_output value (string or content parts).
func outputToText(raw json.RawMessage) string {
	return contentToText(raw)
}

// toolResultText concatenates the text blocks of an IR tool_result for emission
// as a Responses function_call_output, which carries a plain string output.
func toolResultText(b ir.ContentBlock) string {
	var sb strings.Builder
	for _, rb := range b.ToolResult {
		if rb.Type == ir.BlockText {
			sb.WriteString(rb.Text)
		}
	}
	return sb.String()
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

func rawArgs(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}

// imageURLString extracts the URL from a Responses input_image field, which may
// be a bare string or a {"url": "..."} object.
func imageURLString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var obj struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(raw, &obj)
	return obj.URL
}

func imageFromURL(url string) *ir.Image {
	if url == "" {
		return &ir.Image{}
	}
	if media.IsDataURL(url) {
		mt, data, _, err := media.ParseImageURL(url)
		if err != nil {
			return &ir.Image{URL: url}
		}
		return &ir.Image{MediaType: mt, Data: data}
	}
	return &ir.Image{URL: url}
}

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

func fileFromPart(p contentPart) *ir.File {
	f := &ir.File{Filename: p.Filename, ID: p.FileID, URL: p.FileURL}
	if p.FileData != "" {
		mt, data, err := media.ParseFileData(p.FileData)
		if err != nil {
			if media.IsDataURL(p.FileData) {
				f.URL = p.FileData
			} else {
				f.Data = p.FileData
			}
			return f
		}
		f.MediaType = mt
		f.Data = data
		if f.MediaType == "" && f.Filename != "" {
			f.MediaType = mimeFromFilename(f.Filename)
		}
	}
	return f
}

func inputFilePart(f *ir.File) map[string]any {
	part := map[string]any{"type": "input_file"}
	if f == nil {
		return part
	}
	if f.Filename != "" {
		part["filename"] = f.Filename
	}
	if f.ID != "" {
		part["file_id"] = f.ID
	}
	if f.Data != "" {
		mt := f.MediaType
		if mt == "" {
			mt = "application/octet-stream"
		}
		part["file_data"] = media.RenderDataURL(mt, f.Data)
	} else if f.URL != "" {
		if media.IsDataURL(f.URL) {
			part["file_data"] = f.URL
		} else {
			part["file_url"] = f.URL
		}
	}
	return part
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
