package ir

// StreamEventKind tags the streaming delta union.
type StreamEventKind int

const (
	EventMessageStart StreamEventKind = iota
	EventTextDelta
	// EventReasoningDelta is the legacy unindexed, text-only reasoning delta
	// used by OpenAI, Responses, Kiro, and other non-Anthropic producers.
	// Index is not a source block index on this event. Anthropic thinking uses
	// the explicit lifecycle events below so a zero source index stays distinct
	// from this legacy shape. A Responses summary delta may additionally set
	// ReasoningID/ReasoningIndexSet and SummaryIndex; consumers that project
	// readable text only ignore those fields.
	EventReasoningDelta
	// EventReasoningItemDone carries one complete OpenAI Responses reasoning
	// item snapshot in Reasoning. It closes the item identified by ReasoningID
	// or ReasoningIndexSet. Identical repeat snapshots must not be emitted
	// twice. Encrypted bytes are never placed on Text.
	EventReasoningItemDone
	// EventReasoningStart opens one indexed thinking block. Index is the
	// source content-block index.
	EventReasoningStart
	// EventReasoningSignature is an Anthropic signature_delta for the thinking
	// block identified by Index. Signature holds the opaque fragment. Text is
	// not used for signature bytes.
	EventReasoningSignature
	// EventReasoningEnd closes the indexed thinking block. A signature for that
	// block must already have been emitted when the source supplied one.
	EventReasoningEnd
	// EventRedactedReasoning is one complete Anthropic redacted_thinking block.
	// Data is the opaque redacted payload. Text is not a readable substitute.
	EventRedactedReasoning
	EventToolCallStart
	EventToolCallDelta
	EventFinish
)

// StreamEvent is one decoded delta from a backend stream, in a format-neutral
// shape that any ingress encoder can render. Tool calls are identified by Index
// so argument fragments can be attributed to the right call without buffering.
type StreamEvent struct {
	Kind StreamEventKind

	// EventMessageStart
	ID          string
	Model       string
	InputTokens int
	// CacheReadTokens and CacheWriteTokens are subsets of InputTokens when
	// present on MessageStart or Finish. InputTokens is inclusive; cache
	// fields must never be added again.
	CacheReadTokens  int
	CacheWriteTokens int

	// EventTextDelta / EventReasoningDelta / EventReasoningStart text.
	Text string
	// Indexed distinguishes an Anthropic thinking_delta, which carries a source
	// block Index, from a legacy EventReasoningDelta. Legacy producers leave
	// this false; their Index is not a source block index.
	Indexed bool

	// ReasoningID is the source OpenAI Responses reasoning item ID when known.
	// It is not an Anthropic block identity and not a function-call identity.
	ReasoningID string
	// ReasoningIndexSet reports that ReasoningIndex is the source streamed
	// output_index for a Responses reasoning item. It is not a terminal
	// output-array ordinal, and it is not an Anthropic content-block index.
	ReasoningIndexSet bool
	// ReasoningIndex is the source streamed output_index when ReasoningIndexSet.
	ReasoningIndex int
	// SummaryIndex is the ordered summary part ordinal within a Responses
	// reasoning item. It is meaningful only with ReasoningID/ReasoningIndexSet.
	SummaryIndex int
	// Reasoning is the complete structured item on EventReasoningItemDone.
	// The same pointer must not be mutated after emission.
	Reasoning *ResponsesReasoning

	// Signature is the opaque Anthropic signature fragment on
	// EventReasoningSignature. It is not OpenAI encrypted_content.
	Signature string
	// Data is the opaque Anthropic redacted_thinking payload on
	// EventRedactedReasoning.
	Data string

	// EventToolCallStart / EventToolCallDelta
	Index    int
	ToolID   string
	ToolName string
	// ArgsFrag is consumed from EventToolCallDelta only. Start is identity
	// (id/name/index); putting complete args on Start drops them at every
	// ingress encoder and the unary collector.
	ArgsFrag string

	// EventFinish
	StopReason   StopReason
	OutputTokens int
	// UsageReported is set only by the Cursor agent decoder on a normal
	// Connect completion whose turn_ended supplied both input and output
	// counters, including explicit zero. It is not encoded. Other codecs
	// leave it false, which must not be read as missing usage.
	UsageReported bool
}
