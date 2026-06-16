package ir

// StreamEventKind tags the streaming delta union.
type StreamEventKind int

const (
	EventMessageStart StreamEventKind = iota
	EventTextDelta
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

	// EventTextDelta
	Text string

	// EventToolCallStart / EventToolCallDelta
	Index    int
	ToolID   string
	ToolName string
	ArgsFrag string // partial JSON fragment for tool arguments

	// EventFinish
	StopReason   StopReason
	OutputTokens int
}
