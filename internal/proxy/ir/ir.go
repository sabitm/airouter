// Package ir defines the canonical intermediate representation that every wire
// format (OpenAI chat completions, Anthropic messages, and later OpenAI
// responses) decodes into and encodes out of. Translating N formats through one
// IR keeps the converter count linear instead of quadratic.
package ir

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type BlockType string

const (
	BlockText      BlockType = "text"
	BlockReasoning BlockType = "reasoning"
	// BlockRedactedReasoning is Anthropic redacted_thinking. It is a distinct
	// block, not a flag on BlockReasoning, because it has no readable text and
	// must not be projected as ordinary reasoning.
	BlockRedactedReasoning BlockType = "redacted_reasoning"
	BlockImage             BlockType = "image"
	BlockFile              BlockType = "file"
	BlockToolUse           BlockType = "tool_use"
	BlockToolResult        BlockType = "tool_result"
)

// Image holds an image from exactly one source: inline base64 Data, a remote
// URL, or a provider-owned ID. Filename is not carried. ID is scoped to the
// provider that issued it and is not portable across codec IDs.
type Image struct {
	URL       string
	MediaType string
	Data      string // base64, no data-URI prefix
	ID        string // provider-owned image/file id; not portable across protocols
}

// File holds a non-image attachment. Exactly one source is normally present:
// Data (raw base64 payload, no data-URI prefix), URL, or a provider-owned ID.
// Filename and MediaType are metadata and may accompany any source form.
type File struct {
	Filename  string
	MediaType string
	Data      string // base64, no data-URI prefix
	URL       string
	ID        string // provider-owned file id; not portable across protocols
}

// ContentBlock is a tagged union over the block kinds. Only the fields relevant
// to Type are meaningful.
type ContentBlock struct {
	Type BlockType

	Text  string // BlockText or BlockReasoning. Empty thinking text is valid.
	Image *Image // BlockImage
	File  *File  // BlockFile

	// AnthropicSignature is the opaque Anthropic thinking signature. It is not
	// an OpenAI encrypted_content value or a Gemini thought signature. Empty
	// means the reasoning block is unsigned.
	AnthropicSignature string
	// RedactedData is the opaque Anthropic redacted_thinking data string.
	// Meaningful only on BlockRedactedReasoning.
	RedactedData string

	// BlockToolUse: a model-issued call to a tool.
	ToolID    string
	ToolName  string
	ToolInput json.RawMessage

	// BlockToolResult: the caller's response to a prior tool use. Tool results
	// are normalized Anthropic-style: carried as blocks inside a user message.
	ToolUseID  string
	ToolResult []ContentBlock // typically a single text block
	IsError    bool
}

type Message struct {
	Role    Role
	Content []ContentBlock
}

type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON schema
}

type ToolChoiceType string

const (
	ToolChoiceAuto ToolChoiceType = "auto"
	ToolChoiceAny  ToolChoiceType = "any" // model must call some tool
	ToolChoiceTool ToolChoiceType = "tool"
	ToolChoiceNone ToolChoiceType = "none"
)

type ToolChoice struct {
	Type ToolChoiceType
	Name string // set when Type == ToolChoiceTool
}

// ThinkingMode is the unified reasoning/thinking intent kind on a request.
type ThinkingMode string

const (
	ThinkingNone   ThinkingMode = "none"
	ThinkingAuto   ThinkingMode = "auto"
	ThinkingLevel  ThinkingMode = "level"
	ThinkingBudget ThinkingMode = "budget"
)

// ThinkingEnable is an independent enable or disable request. It is absent
// when the client did not state one. Intensity fields do not imply it.
type ThinkingEnable string

const (
	ThinkingEnableUnset    ThinkingEnable = ""
	ThinkingEnableEnabled  ThinkingEnable = "enabled"
	ThinkingEnableDisabled ThinkingEnable = "disabled"
)

// Thinking captures request-side reasoning effort. Nil on Request means the
// client expressed no intent; Mode none is an explicit intensity disable.
// Effort, Enable, and BudgetSet retain independent intent for schema-driven
// consumers. Existing writers keep Mode, Level, and Budget precedence.
type Thinking struct {
	Mode      ThinkingMode
	Level     string // minimal|low|medium|high|xhigh|max when Mode==level
	Budget    int    // token budget when Mode==budget
	Enable    ThinkingEnable
	Effort    string
	BudgetSet bool
}

type Request struct {
	Model         string
	System        string
	Messages      []Message
	MaxTokens     int
	Temperature   *float64
	TopP          *float64
	StopSequences []string
	Stream        bool
	Tools         []Tool
	ToolChoice    *ToolChoice
	Thinking      *Thinking
}

type StopReason string

const (
	StopEndTurn      StopReason = "end_turn"
	StopMaxTokens    StopReason = "max_tokens"
	StopStopSequence StopReason = "stop_sequence"
	StopToolUse      StopReason = "tool_use"
)

// Usage is token accounting for a completed turn.
//
// InputTokens is the inclusive prompt/input total. CacheReadTokens and
// CacheWriteTokens are subsets of that total and must never be added again.
//
// UsageReported is an internal marker, not a wire field. Today only the
// Cursor agent decoder sets it, and only on a normal Connect completion
// whose turn_ended carried both input and output counters. Explicit zero is
// reported. A missing counter, a partial pair, or an immediate MCP handoff
// leaves it false so callers can estimate instead of mixing buckets. Other
// codecs leave it false; that does not mean their counts are missing.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	UsageReported    bool
}

// ClampCacheTokens bounds cache read/write so they cannot exceed inclusive
// input. Negative buckets become 0. An overlarge sum keeps reads first, then
// clamps writes to the remainder. Input itself is not modified.
func ClampCacheTokens(input, read, write int) (int, int) {
	if read < 0 {
		read = 0
	}
	if write < 0 {
		write = 0
	}
	if input < 0 {
		return 0, 0
	}
	if read > input {
		read = input
	}
	rem := input - read
	if write > rem {
		write = rem
	}
	return read, write
}

// Clamped returns a copy with cache buckets bounded by inclusive InputTokens.
func (u Usage) Clamped() Usage {
	u.CacheReadTokens, u.CacheWriteTokens = ClampCacheTokens(u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	return u
}

// OrdinaryInputTokens is inclusive input minus clamped cache read/write.
func (u Usage) OrdinaryInputTokens() int {
	u = u.Clamped()
	n := u.InputTokens - u.CacheReadTokens - u.CacheWriteTokens
	if n < 0 {
		return 0
	}
	return n
}

type Response struct {
	ID         string
	Model      string
	Content    []ContentBlock // text, reasoning, and tool_use blocks
	StopReason StopReason
	Usage      Usage
}

// NewID returns a random hex id with the given prefix, used when a target
// format requires an id the source format did not supply.
func NewID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
