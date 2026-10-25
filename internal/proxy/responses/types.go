// Package responses translates the OpenAI Responses API wire format into and out
// of the canonical IR, in both directions. As ingress (clients calling
// /v1/responses) it uses request decode, response encode, and stream encode; as
// a backend (a provider that only exposes /responses) it uses request encode,
// response decode, and stream decode.
package responses

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// --- request ---

type request struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"` // string or []inputItem
	Instructions    string          `json:"instructions"`
	MaxOutputTokens *int            `json:"max_output_tokens"`
	Temperature     *float64        `json:"temperature"`
	TopP            *float64        `json:"top_p"`
	Stream          bool            `json:"stream"`
	Tools           []tool          `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"` // string or object
	Reasoning       *respReasoning  `json:"reasoning"`
	// PromptCacheKey is explicit caller intent. Nil when absent or null; the
	// wire value is kept verbatim, including an empty string.
	PromptCacheKey *string `json:"prompt_cache_key"`
	// Store and Include are explicit caller intent. Include elements stay raw
	// until validated, because decoding a JSON null element into a string
	// silently yields "" instead of an error.
	Store   *bool              `json:"store"`
	Include *[]json.RawMessage `json:"include"`
	// Metadata is portable caller metadata mapped from Anthropic metadata.
	Metadata json.RawMessage `json:"metadata,omitempty"`
	// Text carries the mapped output_config.format under text.format.
	Text json.RawMessage `json:"text,omitempty"`
}

type respReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type tool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

type inputItem struct {
	Type    string          `json:"type"` // message | reasoning | function_call | function_call_output
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []contentPart

	// reasoning
	Summary []reasoningSummary `json:"summary"`

	// function_call
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`

	// function_call_output
	Output json.RawMessage `json:"output"` // string or []contentPart
}

type reasoningSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// TextSet reports that the wire object carried a non-null "text" field,
	// including an explicit empty string. Absent text is a placeholder, not
	// proof that a retained part is empty.
	TextSet bool `json:"-"`
	// Refusal is the native refusal explanation on a content-part frame. The
	// same object also decodes reasoning summary parts, which ignore this field.
	Refusal    string `json:"refusal"`
	RefusalSet bool   `json:"-"`
}

// UnmarshalJSON rejects a null or non-object summary entry. A null entry would
// otherwise decode as an empty part and silently reinterpret malformed opaque
// state as absent fields. Supported fields keep their JSON type and presence.
func (p *reasoningSummary) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("responses: reasoning summary entry must be an object")
	}
	var raw struct {
		Type    json.RawMessage `json:"type"`
		Text    json.RawMessage `json:"text"`
		Refusal json.RawMessage `json:"refusal"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.Type, p.Text, p.TextSet, p.Refusal, p.RefusalSet = "", "", false, "", false
	if !isNullish(raw.Type) {
		if err := json.Unmarshal(raw.Type, &p.Type); err != nil {
			return err
		}
	}
	if !isNullish(raw.Text) {
		if err := json.Unmarshal(raw.Text, &p.Text); err != nil {
			return err
		}
		p.TextSet = true
	}
	if !isNullish(raw.Refusal) {
		if err := json.Unmarshal(raw.Refusal, &p.Refusal); err != nil {
			return err
		}
		p.RefusalSet = true
	}
	return nil
}

type contentPart struct {
	Type    string `json:"type"` // input_text | output_text | text | refusal | input_image | input_file
	Text    string `json:"text"`
	TextSet bool   `json:"-"`
	Refusal string `json:"refusal"`
	// RefusalSet reports a non-null refusal field, including an explicit empty
	// string. Absent and JSON null are not an empty refusal.
	RefusalSet bool            `json:"-"`
	ImageURL   json.RawMessage `json:"image_url"` // string or {url}
	// input_file fields (flat on the part in the public Responses shape).
	// FileID is also input_image.file_id. The two part types are exclusive, so
	// one JSON field is safe; a second file_id tag would drop the value.
	FileData string `json:"file_data,omitempty"`
	FileURL  string `json:"file_url,omitempty"`
	Filename string `json:"filename,omitempty"`
	FileID   string `json:"file_id,omitempty"`
}

// UnmarshalJSON records text and refusal presence. A refusal part with a
// non-string refusal field fails closed; other part kinds keep lenient field
// decoding so an unrelated malformed sibling is not a new rejection.
func (p *contentPart) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type     string          `json:"type"`
		Text     json.RawMessage `json:"text"`
		Refusal  json.RawMessage `json:"refusal"`
		ImageURL json.RawMessage `json:"image_url"`
		FileData string          `json:"file_data"`
		FileURL  string          `json:"file_url"`
		Filename string          `json:"filename"`
		FileID   string          `json:"file_id"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = contentPart{
		Type: raw.Type, ImageURL: raw.ImageURL, FileData: raw.FileData,
		FileURL: raw.FileURL, Filename: raw.Filename, FileID: raw.FileID,
	}
	if !isNullish(raw.Text) {
		if err := json.Unmarshal(raw.Text, &p.Text); err != nil {
			if raw.Type == "refusal" || raw.Type == "input_text" || raw.Type == "output_text" || raw.Type == "text" {
				return err
			}
		} else {
			p.TextSet = true
		}
	}
	if !isNullish(raw.Refusal) {
		if err := json.Unmarshal(raw.Refusal, &p.Refusal); err != nil {
			if raw.Type == "refusal" {
				return err
			}
		} else {
			p.RefusalSet = true
		}
	}
	return nil
}
