// Package anthropic translates the Anthropic Messages wire format to and from
// the canonical IR. It provides all four directions so the format can act as
// either the client-facing ingress or the upstream backend.
package anthropic

import "encoding/json"

// --- request ---

type messagesRequest struct {
	Model         string          `json:"model"`
	System        json.RawMessage `json:"system,omitempty"` // string or []block
	Messages      []anthMessage   `json:"messages"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []anthTool      `json:"tools,omitempty"`
	ToolChoice    *anthToolChoice `json:"tool_choice,omitempty"`
}

type anthMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []anthBlock
}

type anthBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	// image
	Source *anthSource `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string or []anthBlock
	IsError   bool            `json:"is_error,omitempty"`
}

type anthSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type anthToolChoice struct {
	Type string `json:"type"` // auto | any | tool | none
	Name string `json:"name,omitempty"`
}

// --- response ---

type messagesResponse struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"` // "message"
	Role         string      `json:"role"`
	Model        string      `json:"model"`
	Content      []anthBlock `json:"content"`
	StopReason   string      `json:"stop_reason"`
	StopSequence *string     `json:"stop_sequence"`
	Usage        anthUsage   `json:"usage"`
}

type anthUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
