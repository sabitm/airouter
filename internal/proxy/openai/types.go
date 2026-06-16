// Package openai translates the OpenAI Chat Completions wire format to and from
// the canonical IR. It provides all four directions so the format can act as
// either the client-facing ingress or the upstream backend.
package openai

import "encoding/json"

// --- request ---

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"` // string or []string
	Stream              bool            `json:"stream,omitempty"`
	Tools               []chatTool      `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"` // string or object
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"` // string or []chatPart, may be null
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type chatPart struct {
	Type     string        `json:"type"` // text | image_url
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded string
}

type chatTool struct {
	Type     string          `json:"type"` // "function"
	Function chatFunctionDef `json:"function"`
}

type chatFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// --- response ---

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

type chatChoice struct {
	Index        int             `json:"index"`
	Message      chatRespMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type chatRespMessage struct {
	Role string `json:"role"`
	// Pointer so an absent text body marshals to null (OpenAI's convention when
	// the turn is only tool calls).
	Content   *string        `json:"content"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
