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
	StreamOptions       *streamOptions  `json:"stream_options,omitempty"`
	Tools               []chatTool      `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"` // string or object
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	// Metadata carries portable caller metadata, including an Anthropic
	// metadata object translated to this format.
	Metadata json.RawMessage `json:"metadata,omitempty"`
	// ResponseFormat is the mapped output_config.format intent.
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content,omitempty"` // string or []chatPart, may be null
	// Refusal is encoded by MarshalJSON so an explicit empty string is not
	// dropped by omitempty. RefusalSet distinguishes that empty value from absence.
	Refusal          json.RawMessage   `json:"-"`
	RefusalSet       bool              `json:"-"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	Reasoning        json.RawMessage   `json:"reasoning,omitempty"`
	ReasoningDetails []json.RawMessage `json:"reasoning_details,omitempty"`
	ToolCalls        []chatToolCall    `json:"tool_calls,omitempty"`
	ToolCallID       string            `json:"tool_call_id,omitempty"`
}

func (m *chatMessage) UnmarshalJSON(data []byte) error {
	type plain chatMessage
	var raw struct {
		plain
		Refusal json.RawMessage `json:"refusal"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = chatMessage(raw.plain)
	if !isNullishRaw(raw.Refusal) {
		var s string
		if err := json.Unmarshal(raw.Refusal, &s); err != nil {
			return err
		}
		m.Refusal = append(json.RawMessage(nil), raw.Refusal...)
		m.RefusalSet = true
	}
	return nil
}

func (m chatMessage) MarshalJSON() ([]byte, error) {
	type plain chatMessage
	raw, err := json.Marshal(plain(m))
	if err != nil || !m.RefusalSet {
		return raw, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	obj["refusal"] = append(json.RawMessage(nil), m.Refusal...)
	return json.Marshal(obj)
}

type chatPart struct {
	Type     string        `json:"type"` // text | refusal | image_url | file
	Text     string        `json:"text,omitempty"`
	Refusal  string        `json:"refusal,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
	File     *chatFile     `json:"file,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

// chatFile is the OpenAI Chat Completions content part for type "file".
// file_data may be a data URL or bare base64; file_id is provider-owned.
type chatFile struct {
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
	FileID   string `json:"file_id,omitempty"`
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
	Strict      *bool           `json:"strict,omitempty"`
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
	Content *string `json:"content"`
	// Refusal is the native refusal field. Nil means absent. An explicit empty
	// string is retained and is not copied into Content.
	Refusal          *string           `json:"refusal,omitempty"` // pointer keeps an explicit empty string
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	Reasoning        json.RawMessage   `json:"reasoning,omitempty"`
	ReasoningDetails []json.RawMessage `json:"reasoning_details,omitempty"`
	ToolCalls        []chatToolCall    `json:"tool_calls,omitempty"`
}

type chatPromptTokensDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type chatUsage struct {
	PromptTokens        int                      `json:"prompt_tokens"`
	CompletionTokens    int                      `json:"completion_tokens"`
	TotalTokens         int                      `json:"total_tokens"`
	PromptTokensDetails *chatPromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}
