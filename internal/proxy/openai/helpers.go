package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// chatReasoningText normalizes the common OpenAI-compatible reasoning aliases.
// The first populated scalar wins; reasoning_details fragments are concatenated.
func chatReasoningText(content string, reasoning json.RawMessage, details []json.RawMessage) string {
	if content != "" {
		return content
	}
	var scalar string
	if len(reasoning) > 0 && json.Unmarshal(reasoning, &scalar) == nil && scalar != "" {
		return scalar
	}
	var out strings.Builder
	for _, raw := range details {
		var detail struct {
			Text    string `json:"text"`
			Content string `json:"content"`
		}
		if json.Unmarshal(raw, &detail) == nil {
			if detail.Text != "" {
				out.WriteString(detail.Text)
			} else {
				out.WriteString(detail.Content)
			}
			continue
		}
		if json.Unmarshal(raw, &scalar) == nil {
			out.WriteString(scalar)
		}
	}
	return out.String()
}

// captureChatRefusal records whether the assistant refusal field was present
// and non-null, including an explicit empty string. A non-string value fails
// closed. Null and absence stay distinct from an empty refusal.
func captureChatRefusal(m *chatMessage) error {
	if m == nil || isNullishRaw(m.Refusal) {
		return nil
	}
	var s string
	if err := json.Unmarshal(m.Refusal, &s); err != nil {
		return fmt.Errorf("openai: decode request: refusal must be a string")
	}
	m.RefusalSet = true
	m.Refusal = mustText(s)
	return nil
}

func isNullishRaw(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	return string(bytes.TrimSpace(raw)) == "null"
}

// decodeChatTextAndRefusal preserves ordinary content and the native refusal
// field independently. Chat cannot express exact cross-field interleaving, so
// each field keeps its own order. Equal refusal text is not duplicated.
func decodeChatTextAndRefusal(content, refusal json.RawMessage, refusalSet bool) []ir.ContentBlock {
	var blocks []ir.ContentBlock
	if text := contentToText(content); text != "" {
		blocks = append(blocks, ir.ContentBlock{Type: ir.BlockText, Text: text})
	}
	for _, part := range decodeRefusalParts(content) {
		blocks = append(blocks, part)
	}
	if !refusalSet {
		return blocks
	}
	var field string
	_ = json.Unmarshal(refusal, &field)
	for _, b := range blocks {
		if b.Refusal && b.Text == field {
			return blocks
		}
	}
	return append(blocks, ir.ContentBlock{Type: ir.BlockText, Text: field, Refusal: true})
}

func decodeRefusalParts(raw json.RawMessage) []ir.ContentBlock {
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var parts []chatPart
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var out []ir.ContentBlock
	for _, p := range parts {
		if p.Type == "refusal" {
			out = append(out, ir.ContentBlock{Type: ir.BlockText, Text: p.Refusal, Refusal: true})
		}
	}
	return out
}

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

// encodeResponseFormat maps portable intent first, then falls back to the
// existing Anthropic-owned output_config.format mapping. A portable format keeps
// OpenAI envelope fields and raw schema tokens. Unknown or malformed intent
// fails before the request is marshaled.
func encodeResponseFormat(req *ir.Request) (json.RawMessage, error) {
	portable, err := req.EffectivePortableOutputFormat()
	if err != nil {
		return nil, err
	}
	if portable == nil {
		if req.Anthropic == nil {
			return nil, nil
		}
		return responseFormat(req.Anthropic)
	}
	if err := portable.Validate(); err != nil {
		return nil, err
	}
	switch portable.Type {
	case ir.OutputFormatJSONSchema:
		js := map[string]any{
			"name":   portable.Name,
			"schema": json.RawMessage(portable.Schema),
		}
		if portable.Description != nil {
			js["description"] = *portable.Description
		}
		switch {
		case portable.Strict != nil:
			js["strict"] = *portable.Strict
		case portable.StrictNull:
			// Explicit null stays null: OpenAI request types type strict as
			// Optional[bool], so null must survive, not collapse to omission.
			js["strict"] = nil
		}
		body := map[string]any{
			"type":        "json_schema",
			"json_schema": js,
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		return raw, nil
	case ir.OutputFormatJSONObject:
		return json.RawMessage(`{"type":"json_object"}`), nil
	case ir.OutputFormatText:
		return json.RawMessage(`{"type":"text"}`), nil
	default:
		return nil, ir.InvalidRequest("output format type is not supported")
	}
}

// responseFormat maps a validated Anthropic output_config.format to the
// standard OpenAI json_schema response_format. The schema is preserved. OpenAI
// requires a name, so a schema title is used when it is a safe identifier and
// "structured_output" otherwise. Unknown format types fail before send.
func responseFormat(a *ir.AnthropicRequest) (json.RawMessage, error) {
	schema, ok, err := a.OutputFormatJSONSchema()
	if err != nil || !ok {
		return nil, err
	}
	if len(schema) == 0 {
		return json.RawMessage(`{"type":"json_object"}`), nil
	}
	body := map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
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
