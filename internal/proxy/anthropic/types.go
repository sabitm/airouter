// Package anthropic translates the Anthropic Messages wire format to and from
// the canonical IR. It provides all four directions so the format can act as
// either the client-facing ingress or the upstream backend.
package anthropic

import "encoding/json"

// --- request ---

type messagesRequest struct {
	Model             string            `json:"model"`
	System            json.RawMessage   `json:"system,omitempty"` // string or []block
	Messages          []anthMessage     `json:"messages"`
	MaxTokens         int               `json:"max_tokens"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	StopSequences     []string          `json:"stop_sequences,omitempty"`
	Stream            bool              `json:"stream,omitempty"`
	Tools             []anthTool        `json:"tools,omitempty"`
	ToolChoice        *anthToolChoice   `json:"tool_choice,omitempty"`
	Thinking          *anthThinking     `json:"thinking,omitempty"`
	OutputConfig      *anthOutputConfig `json:"output_config,omitempty"`
	Metadata          json.RawMessage   `json:"metadata,omitempty"`
	ContextManagement json.RawMessage   `json:"context_management,omitempty"`
	CacheControl      json.RawMessage   `json:"cache_control,omitempty"`
	Fallbacks         json.RawMessage   `json:"fallbacks,omitempty"`
	Container         json.RawMessage   `json:"container,omitempty"`
	// Speed is captured raw so absent, null, and any non-null value stay
	// distinguishable. The encoder never writes it; it rejects a non-null value
	// instead of dropping caller intent.
	Speed json.RawMessage `json:"speed,omitempty"`
}

type anthThinking struct {
	Type         string `json:"type"` // disabled | adaptive | enabled
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"`
}

type anthOutputConfig struct {
	Effort     string          `json:"effort,omitempty"`
	Format     json.RawMessage `json:"format,omitempty"`
	TaskBudget *anthTaskBudget `json:"task_budget,omitempty"`
}

// anthTaskBudget is the verified output_config.task_budget shape. Total and
// Remaining are pointers so a missing total is distinguishable from an
// explicit zero, and an explicit zero remaining is not dropped.
type anthTaskBudget struct {
	Type      string `json:"type"`
	Total     *int   `json:"total"`
	Remaining *int   `json:"remaining,omitempty"`
}

type anthMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []anthBlock
}

type anthBlock struct {
	Type string `json:"type"`
	// Raw, when set, is the original object. MarshalJSON emits it unchanged so
	// server-block fields that this struct does not model are not dropped.
	Raw json.RawMessage `json:"-"`

	Text string `json:"text,omitempty"`
	// Citations is the wire value for a text citation list or the
	// search_result {enabled} object. Text decoding validates only the array
	// form; the search_result form stays inside its preserved server object.
	Citations json.RawMessage `json:"citations,omitempty"`
	// Thinking is a pointer so a signed empty thinking block can emit
	// "thinking":"" without putting that field on text or tool blocks.
	Thinking  *string `json:"thinking,omitempty"`
	Signature string  `json:"signature,omitempty"`
	// Data is the opaque redacted_thinking payload.
	Data string `json:"data,omitempty"`

	// image | document
	Source *anthSource `json:"source,omitempty"`
	// document title (optional filename-like label)
	Title string `json:"title,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string or []anthBlock
	IsError   bool            `json:"is_error,omitempty"`

	// CacheControl is kept raw so a malformed excluded or nested marker cannot
	// fail the whole block-array decode. Only request.go validates and converts
	// it, and only at eligible request positions.
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

func (b anthBlock) MarshalJSON() ([]byte, error) {
	if len(b.Raw) > 0 {
		return append([]byte(nil), b.Raw...), nil
	}
	type plain anthBlock
	return json.Marshal(plain(b))
}

func thinkingText(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func thinkingPtr(s string) *string {
	return &s
}

type anthSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthTool struct {
	Type                string          `json:"type,omitempty"`
	Name                string          `json:"name"`
	Description         string          `json:"description,omitempty"`
	InputSchema         json.RawMessage `json:"input_schema,omitempty"`
	CacheControl        json.RawMessage `json:"cache_control,omitempty"`
	Strict              *bool           `json:"strict,omitempty"`
	EagerInputStreaming *bool           `json:"eager_input_streaming,omitempty"`
	DeferLoading        *bool           `json:"defer_loading,omitempty"`
	MaxUses             *int            `json:"max_uses,omitempty"`
	AllowedDomains      []string        `json:"allowed_domains,omitempty"`
	BlockedDomains      []string        `json:"blocked_domains,omitempty"`
}

// anthCacheControl is the wire shape of one canonical explicit cache hint. An
// empty TTL is omitted so a marker without one re-emits without one. Scope is
// emitted only for the verified "global" value. EvictOnComplete is a pointer
// so an explicit false is emitted and an omitted or null flag is not.
type anthCacheControl struct {
	Type            string `json:"type"`
	TTL             string `json:"ttl,omitempty"`
	Scope           string `json:"scope,omitempty"`
	EvictOnComplete *bool  `json:"evict_on_complete,omitempty"`
}

type anthToolChoice struct {
	Type                   string `json:"type"` // auto | any | tool | none
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
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
	// Provider-owned continuity fields. Each is a validated JSON object or array
	// shape from the pinned source; they round-trip only through Anthropic.
	Container            json.RawMessage `json:"container,omitempty"`
	ContextManagement    json.RawMessage `json:"context_management,omitempty"`
	StopDetails          json.RawMessage `json:"stop_details,omitempty"`
	InputTransformations json.RawMessage `json:"input_transformations,omitempty"`
}

type anthUsage struct {
	InputTokens int `json:"input_tokens"`
	// Cache tokens are billed input that Anthropic reports separately: on a cache
	// write input_tokens drops to the uncached remainder and the bulk lands in
	// cache_creation; on a cache read it lands in cache_read. TotalInput folds all
	// three so logged usage reflects the real input regardless of cache state.
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

func (u anthUsage) TotalInput() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}
