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
	// from this legacy shape.
	EventReasoningDelta
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
}
