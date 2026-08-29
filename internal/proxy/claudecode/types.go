package claudecode

import "encoding/json"

// messagesBody mirrors the Anthropic Messages request shape produced by
// anthropic.EncodeRequest plus the metadata field this provider injects. It is
// defined locally (rather than reusing the anthropic package's unexported
// types) so the cloak step can mutate tools, message history, system, and
// metadata without exporting internal wire types. Every field EncodeRequest
// emits is modeled so remarshal drops nothing.
type messagesBody struct {
	Model         string            `json:"model"`
	System        json.RawMessage   `json:"system,omitempty"` // string or []block
	Messages      []wireMessage     `json:"messages"`
	MaxTokens     int               `json:"max_tokens"`
	Temperature   *float64          `json:"temperature,omitempty"`
	TopP          *float64          `json:"top_p,omitempty"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
	Stream        bool              `json:"stream,omitempty"`
	Tools         []wireTool        `json:"tools,omitempty"`
	ToolChoice    *wireToolChoice   `json:"tool_choice,omitempty"`
	Thinking      *wireThinking     `json:"thinking,omitempty"`
	OutputConfig  *wireOutputConfig `json:"output_config,omitempty"`
	Metadata      *wireMetadata     `json:"metadata,omitempty"`
}

// wireThinking mirrors anthropic.anthThinking. Budget thinking must survive
// the cloak remarshal or OAuth Claude Code silently loses extended reasoning
// while keeping applyThinking's inflated max_tokens.
type wireThinking struct {
	Type         string `json:"type"` // disabled | adaptive | enabled
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// wireOutputConfig mirrors anthropic.anthOutputConfig (adaptive-model effort).
type wireOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []wireBlock
}

// wireBlock covers every block kind the Anthropic format uses; only Type/Name
// are read by the cloak (tool_use rename) and the rest are preserved verbatim.
type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *wireSource     `json:"source,omitempty"`
	Title     string          `json:"title,omitempty"` // document blocks (optional filename-like label)
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type wireSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// wireTool models a tool declaration. Type is set only on Anthropic server tools
// (web_search_*, etc.) which require an exact reserved name and must never be
// suffixed; client tools carry no Type and get the _ide suffix when cloaking.
type wireTool struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type wireToolChoice struct {
	Type string `json:"type"` // auto | any | tool | none
	Name string `json:"name,omitempty"`
}

// wireMetadata carries the user_id field this provider injects. The user_id
// value is a JSON-encoded string ({"device_id","account_uuid","session_id"}),
// not a nested object, matching the Claude Code client format.
type wireMetadata struct {
	UserID string `json:"user_id,omitempty"`
}
