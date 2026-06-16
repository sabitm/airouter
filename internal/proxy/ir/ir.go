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
	BlockText       BlockType = "text"
	BlockImage      BlockType = "image"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
)

// Image holds an image either as a remote URL or as inline base64 data. Exactly
// one form is populated depending on what the source format provided.
type Image struct {
	URL       string
	MediaType string
	Data      string // base64, no data-URI prefix
}

// ContentBlock is a tagged union over the block kinds. Only the fields relevant
// to Type are meaningful.
type ContentBlock struct {
	Type BlockType

	Text  string // BlockText
	Image *Image // BlockImage

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
}

type StopReason string

const (
	StopEndTurn      StopReason = "end_turn"
	StopMaxTokens    StopReason = "max_tokens"
	StopStopSequence StopReason = "stop_sequence"
	StopToolUse      StopReason = "tool_use"
)

type Usage struct {
	InputTokens  int
	OutputTokens int
}

type Response struct {
	ID         string
	Model      string
	Content    []ContentBlock // text and tool_use blocks
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
