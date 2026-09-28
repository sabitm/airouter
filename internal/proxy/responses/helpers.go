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

// toolResultBlocks decodes a function_call_output value. A string is text;
// an array uses message-part decoding so nested media reaches InspectRequest.
func toolResultBlocks(raw json.RawMessage) []ir.ContentBlock {
	if len(raw) == 0 {
		return []ir.ContentBlock{{Type: ir.BlockText}}
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return []ir.ContentBlock{{Type: ir.BlockText}}
		}
		return []ir.ContentBlock{{Type: ir.BlockText, Text: s}}
	}
	blocks := decodeParts(raw)
	if len(blocks) == 0 {
		return []ir.ContentBlock{{Type: ir.BlockText}}
	}
	return blocks
}

// toolResultText concatenates the text blocks of an IR tool_result.
func toolResultText(b ir.ContentBlock) string {
	var sb strings.Builder
	for _, rb := range b.ToolResult {
		if rb.Type == ir.BlockText {
			sb.WriteString(rb.Text)
		}
	}
	return sb.String()
}

// toolResultHasMedia reports whether a tool result carries image or file blocks.
// Those cannot be flattened into the string form of function_call_output.output.
func toolResultHasMedia(b ir.ContentBlock) bool {
	for _, rb := range b.ToolResult {
		if rb.Type == ir.BlockImage || rb.Type == ir.BlockFile {
			return true
		}
	}
	return false
}

// toolResultOutput keeps a text-only result as a string. A result that contains
// image or file blocks becomes an ordered array of Responses input parts so
// representable text, image, and file blocks survive encode.
func toolResultOutput(b ir.ContentBlock) any {
	if !toolResultHasMedia(b) {
		return toolResultText(b)
	}
	var parts []map[string]any
	for _, rb := range b.ToolResult {
		switch rb.Type {
		case ir.BlockText:
			parts = append(parts, map[string]any{"type": "input_text", "text": rb.Text})
		case ir.BlockImage:
			parts = append(parts, inputImagePart(rb.Image))
		case ir.BlockFile:
			parts = append(parts, inputFilePart(rb.File))
		}
	}
	if parts == nil {
		parts = []map[string]any{}
	}
	return parts
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

// imageFromPart maps a Responses input_image onto IR. file_id is a source
// even when image_url is absent; both may be set and fail closed later.
func imageFromPart(p contentPart) *ir.Image {
	img := imageFromURL(imageURLString(p.ImageURL))
	img.ID = p.FileID
	return img
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

func inputImagePart(img *ir.Image) map[string]any {
	part := map[string]any{"type": "input_image"}
	if img == nil {
		return part
	}
	if img.ID != "" {
		part["file_id"] = img.ID
	}
	if url := imageToURL(img); url != "" {
		part["image_url"] = url
	}
	return part
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
