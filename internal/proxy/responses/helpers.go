package responses

import (
	"bytes"
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

// rejectProviderOwnedTool keeps an Anthropic server tool or provider-only
// control from being converted into a callable custom function. Strict is
// portable and is not rejected here.
func rejectProviderOwnedTool(t ir.Tool) error {
	at := t.AnthropicTool
	if at == nil {
		return nil
	}
	if at.Type != "" {
		return ir.InvalidRequest("Anthropic server tools are not supported by this format")
	}
	if at.EagerInput != nil && *at.EagerInput {
		return ir.InvalidRequest("eager input streaming is not supported by this format")
	}
	if at.DeferLoading != nil && *at.DeferLoading {
		return ir.InvalidRequest("deferred tool loading is not supported by this format")
	}
	if at.MaxUses != nil || len(at.AllowedDomains) > 0 || len(at.BlockedDomains) > 0 {
		return ir.InvalidRequest("provider-only tool controls are not supported by this format")
	}
	return nil
}

// decodeResponseTextFormat extracts the portable format from the Responses text
// object. Other text siblings such as verbosity are not interpreted here and
// stay untouched on a same-codec passthrough. A malformed text or format keeps
// invalid intent so translated requests fail instead of dropping it.
func decodeResponseTextFormat(raw json.RawMessage) *ir.OutputFormat {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] != '{' || !json.Valid(trimmed) {
		return &ir.OutputFormat{Source: "oai-responses", InvalidReason: "text must be an object"}
	}
	var text struct {
		Format json.RawMessage `json:"format"`
	}
	if json.Unmarshal(trimmed, &text) != nil {
		return &ir.OutputFormat{Source: "oai-responses", InvalidReason: "text must be an object"}
	}
	format := bytes.TrimSpace(text.Format)
	if len(format) == 0 || bytes.Equal(format, []byte("null")) {
		return nil
	}
	return ir.DecodeResponsesFormat(format, "oai-responses")
}

// encodeTextFormat maps portable intent first, then falls back to the existing
// Anthropic-owned output_config.format mapping. Schema and envelope fields stay
// raw so number tokens survive and explicit false remains present.
func encodeTextFormat(req *ir.Request) (json.RawMessage, error) {
	portable, err := req.EffectivePortableOutputFormat()
	if err != nil {
		return nil, err
	}
	if portable == nil {
		if req.Anthropic == nil {
			return nil, nil
		}
		return responseTextFormat(req.Anthropic)
	}
	if err := portable.Validate(); err != nil {
		return nil, err
	}
	format := map[string]any{}
	switch portable.Type {
	case ir.OutputFormatJSONSchema:
		format["type"] = "json_schema"
		format["name"] = portable.Name
		format["schema"] = json.RawMessage(portable.Schema)
		if portable.Description != nil {
			format["description"] = *portable.Description
		}
		switch {
		case portable.Strict != nil:
			format["strict"] = *portable.Strict
		case portable.StrictNull:
			// Explicit null stays null: Optional[bool] null must survive, not
			// collapse to omission.
			format["strict"] = nil
		}
	case ir.OutputFormatJSONObject:
		format["type"] = "json_object"
	case ir.OutputFormatText:
		format["type"] = "text"
	default:
		return nil, ir.InvalidRequest("output format type is not supported")
	}
	body := map[string]any{"format": format}
	return json.Marshal(body)
}

// responseTextFormat maps a validated Anthropic output_config.format to the
// standard Responses text.format object. The schema is preserved. A schema
// title supplies the required name when it is a safe identifier.
func responseTextFormat(a *ir.AnthropicRequest) (json.RawMessage, error) {
	schema, ok, err := a.OutputFormatJSONSchema()
	if err != nil || !ok {
		return nil, err
	}
	if len(schema) == 0 {
		return json.RawMessage(`{"format":{"type":"json_object"}}`), nil
	}
	body := map[string]any{
		"format": map[string]any{
			"type":   "json_schema",
			"name":   structuredOutputName(schema),
			"schema": json.RawMessage(schema),
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func structuredOutputName(schema json.RawMessage) string {
	var head struct {
		Title string `json:"title"`
	}
	if json.Unmarshal(schema, &head) == nil {
		name := head.Title
		if len(name) > 0 && len(name) <= 64 {
			valid := true
			for _, r := range name {
				if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
					continue
				}
				valid = false
				break
			}
			if valid {
				return name
			}
		}
	}
	return "structured_output"
}
