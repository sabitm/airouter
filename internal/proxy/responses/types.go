// Package responses translates the OpenAI Responses API wire format into and out
// of the canonical IR, in both directions. As ingress (clients calling
// /v1/responses) it uses request decode, response encode, and stream encode; as
// a backend (a provider that only exposes /responses) it uses request encode,
// response decode, and stream decode.
package responses

import "encoding/json"

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
}

type tool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type inputItem struct {
	Type    string          `json:"type"` // message | function_call | function_call_output
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []contentPart

	// function_call
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`

	// function_call_output
	Output json.RawMessage `json:"output"` // string or []contentPart
}

type contentPart struct {
	Type     string          `json:"type"` // input_text | output_text | text | input_image
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"` // string or {url}
}
