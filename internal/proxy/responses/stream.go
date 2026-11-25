package responses

import (
	"encoding/json"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

// Responses argument retention matches the unary collector budgets. These are
// local so the codec does not import the proxy package. Count only recovered
// growth; an identical snapshot is not counted again.
const (
	maxRetainedToolArgBytes = 64 << 20
	maxTrackedToolCalls     = 1024
	// Message text tracking bounds are independent of tool arguments. Each
	// identified message retains at most one copy of its text for suffix
	// recovery; an identical final snapshot is not charged again.
	maxRetainedTextBytes = 64 << 20
	maxTrackedTextItems  = 1024
	maxTrackedTextParts  = 1024
	textItemOverhead     = 128
	textPartOverhead     = 128
	textKeyOverhead      = 32
	// textQueueEntryOverhead is the per-entry bookkeeping charged against the
	// pending text queue budget for one queued delta. Pending limits mirror the
	// encoder pending queue and stay below the text state budget.
	textQueueEntryOverhead = 48
	maxPendingTextBytes    = 8 << 20
	maxPendingTextEvents   = 1024
)

// streamEnvelope is the union of the Responses SSE event fields this decoder
// reads. The event kind is taken from the JSON "type" field rather than the SSE
// event name, so a producer that omits the name still decodes.
type streamEnvelope struct {
	Type            string            `json:"type"`
	OutputIndex     int               `json:"output_index"`
	OutputIndexSet  bool              `json:"-"`
	ItemID          string            `json:"item_id"`
	Delta           string            `json:"delta"`
	Arguments       string            `json:"arguments"`
	Name            string            `json:"name"`
	Text            string            `json:"text"`
	TextSet         bool              `json:"-"`
	Refusal         string            `json:"refusal"`
	RefusalSet      bool              `json:"-"`
	SummaryIndex    int               `json:"summary_index"`
	SummaryIndexSet bool              `json:"-"`
	ContentIndex    int               `json:"content_index"`
	ContentIndexSet bool              `json:"-"`
	Part            *reasoningSummary `json:"part"`
	Item            *streamItem       `json:"item"`
	Response        *respObject       `json:"response"`
	Error           *respErrorObject  `json:"error"`
}

func (e *streamEnvelope) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type         string            `json:"type"`
		OutputIndex  *int              `json:"output_index"`
		ItemID       string            `json:"item_id"`
		Delta        string            `json:"delta"`
		Arguments    string            `json:"arguments"`
		Name         string            `json:"name"`
		Text         *string           `json:"text"`
		Refusal      *string           `json:"refusal"`
		SummaryIndex *int              `json:"summary_index"`
		ContentIndex *int              `json:"content_index"`
		Part         *reasoningSummary `json:"part"`
		Item         *streamItem       `json:"item"`
		Response     *respObject       `json:"response"`
		Error        *respErrorObject  `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*e = streamEnvelope{
		Type: raw.Type, ItemID: raw.ItemID, Delta: raw.Delta,
		Arguments: raw.Arguments, Name: raw.Name,
		Part: raw.Part, Item: raw.Item, Response: raw.Response, Error: raw.Error,
	}
	if raw.Text != nil {
		e.Text = *raw.Text
		e.TextSet = true
	}
	if raw.Refusal != nil {
		e.Refusal = *raw.Refusal
		e.RefusalSet = true
	}
	if raw.OutputIndex != nil {
		e.OutputIndex = *raw.OutputIndex
		e.OutputIndexSet = true
	}
	if raw.SummaryIndex != nil {
		e.SummaryIndex = *raw.SummaryIndex
		e.SummaryIndexSet = true
	}
	if raw.ContentIndex != nil {
		e.ContentIndex = *raw.ContentIndex
		e.ContentIndexSet = true
	}
	return nil
}

type streamItem struct {
	ID               string              `json:"id"`
	Type             string              `json:"type"`
	CallID           string              `json:"call_id"`
	Name             string              `json:"name"`
	Arguments        string              `json:"arguments"`
	Content          []respOutputContent `json:"content"`
	Summary          []reasoningSummary  `json:"summary"`
	SummarySet       bool                `json:"-"`
	EncryptedContent *string             `json:"encrypted_content"`
	Status           string              `json:"status"`
}

// UnmarshalJSON decodes one item frame while recording whether the summary
// array was present and non-null. An absent summary on a complete item snapshot
// is a placeholder; an explicit array is authoritative and may not shrink.
func (it *streamItem) UnmarshalJSON(data []byte) error {
	type plain streamItem
	var raw struct {
		plain
		Summary json.RawMessage `json:"summary"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*it = streamItem(raw.plain)
	it.SummarySet = !isNullish(raw.Summary)
	if it.SummarySet {
		if err := json.Unmarshal(raw.Summary, &it.Summary); err != nil {
			return err
		}
	}
	return nil
}

// malformedReasoningEvent reports whether raw is a recognized reasoning-bearing
// SSE event whose strict decode failed on a supported field. Such a failure must
// not be swallowed as an unknown event: it would drop opaque state or a terminal
// frame. The returned message names the family only and never echoes a value.
func malformedReasoningEvent(raw []byte) string {
	var head struct {
		Type string `json:"type"`
		Item *struct {
			Type string `json:"type"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return ""
	}
	switch head.Type {
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return "responses: malformed reasoning stream event"
	case "response.output_item.added", "response.output_item.done":
		if head.Item != nil && head.Item.Type == "reasoning" {
			return "responses: malformed reasoning stream item"
		}
	case "response.completed", "response.incomplete":
		// The terminal frame carries the output snapshot. A malformed reasoning
		// item inside it must fail instead of being skipped without Finish.
		var resp struct {
			Response struct {
				Output []json.RawMessage `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal(raw, &resp) != nil {
			return ""
		}
		for _, rawItem := range resp.Response.Output {
			var itemHead struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(rawItem, &itemHead) != nil {
				continue
			}
			if itemHead.Type != "reasoning" {
				continue
			}
			var it streamItem
			if json.Unmarshal(rawItem, &it) != nil {
				return "responses: malformed reasoning stream item"
			}
		}
	}
	return ""
}

// malformedMessageEvent reports whether raw is a recognized message-bearing
// SSE event whose strict decode failed on a supported field. The returned
// message names the family only and never echoes a value.
func malformedMessageEvent(raw []byte) string {
	var head struct {
		Type string `json:"type"`
		Item *struct {
			Type string `json:"type"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return ""
	}
	switch head.Type {
	case "response.output_text.delta", "response.output_text.done",
		"response.refusal.delta", "response.refusal.done",
		"response.content_part.added", "response.content_part.done":
		return "responses: malformed message stream event"
	case "response.output_item.added", "response.output_item.done":
		if head.Item != nil && head.Item.Type == "message" {
			return "responses: malformed message stream item"
		}
	case "response.completed", "response.incomplete":
		// The terminal frame carries the output snapshot. A malformed message
		// item inside it must fail instead of being skipped without Finish.
		var resp struct {
			Response struct {
				Output []json.RawMessage `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal(raw, &resp) != nil {
			return ""
		}
		for _, rawItem := range resp.Response.Output {
			var itemHead struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(rawItem, &itemHead) != nil {
				continue
			}
			if itemHead.Type != "message" {
				continue
			}
			var it respOutputItem
			if json.Unmarshal(rawItem, &it) != nil {
				return "responses: malformed message stream item"
			}
		}
	}
	return ""
}

// DecodeStream reads an OpenAI Responses SSE stream and emits IR stream events.
// Used when Responses is the backend format. Each function call is tracked by
// output index, item ID, and call ID. Argument bytes are emitted only as
// EventToolCallDelta. The Finish event is deferred to end-of-stream so the
// response.completed usage is captured. Explicit error / response.failed frames
// return *ir.StreamFailure without Finish.
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	return decodeStreamLimited(r, emit, maxRetainedToolArgBytes, maxTrackedToolCalls)
}

// DecodeStreamAwaitingItemID is the Grok Build Responses policy. Tool start and
// argument events stay withheld until a native item id arrives, or a terminal
// snapshot or EOF proves that no id will arrive. A missing id still uses the
// caller's synthetic fallback. Withheld tools occupy a position in the shared
// ordering queue, so a later item cannot pass an earlier one. DecodeStream is
// unchanged.
func DecodeStreamAwaitingItemID(r io.Reader, emit func(ir.StreamEvent) error) error {
	return decodeStreamBoundedParts(r, emit, maxRetainedToolArgBytes, maxTrackedToolCalls, maxRetainedTextBytes, maxTrackedTextItems, maxTrackedTextParts, maxPendingTextBytes, maxPendingTextEvents, maxRetainedReasoningBytes, maxTrackedReasoningItems, maxTrackedReasoningParts, decodePolicy{awaitItemID: true})
}

// DecodeStreamCodexReconciled is the Codex-to-Responses policy. Codex may send
// the authoritative reasoning encrypted_content after the completion frame, or
// only in the terminal output array, so a completion without explicit bytes is
// withheld until the terminal pass. Provisional bytes on output_item.added are
// never promoted. Every other decode path is unchanged.
func DecodeStreamCodexReconciled(r io.Reader, emit func(ir.StreamEvent) error) error {
	return decodeStreamBoundedParts(r, emit, maxRetainedToolArgBytes, maxTrackedToolCalls, maxRetainedTextBytes, maxTrackedTextItems, maxTrackedTextParts, maxPendingTextBytes, maxPendingTextEvents, maxRetainedReasoningBytes, maxTrackedReasoningItems, maxTrackedReasoningParts, decodePolicy{reconcileCodex: true})
}

// DecodeStreamAwaitingItemIDObserved is the Grok Build Chat-bridge decode.
// observe receives source-order metadata and reasoning snapshots. It is not
// the client emit path. A nil observe is DecodeStreamAwaitingItemID. The
// observer cannot change item.wired, the ordering queue, or final-order checks.
func DecodeStreamAwaitingItemIDObserved(r io.Reader, emit func(ir.StreamEvent) error, observe func(ir.StreamEvent)) error {
	return decodeStreamBoundedParts(r, emit, maxRetainedToolArgBytes, maxTrackedToolCalls, maxRetainedTextBytes, maxTrackedTextItems, maxTrackedTextParts, maxPendingTextBytes, maxPendingTextEvents, maxRetainedReasoningBytes, maxTrackedReasoningItems, maxTrackedReasoningParts, decodePolicy{awaitItemID: true, observe: observe})
}

func decodeStreamLimited(r io.Reader, emit func(ir.StreamEvent) error, maxArgBytes, maxCalls int) error {
	return decodeStreamLimitedAll(r, emit, maxArgBytes, maxCalls, maxRetainedReasoningBytes, maxTrackedReasoningItems, maxTrackedReasoningParts)
}

func decodeStreamLimitedAll(r io.Reader, emit func(ir.StreamEvent) error, maxArgBytes, maxCalls, maxReasoningBytes, maxReasoningItems, maxReasoningParts int) error {
	return decodeStreamBounded(r, emit, maxArgBytes, maxCalls, maxRetainedTextBytes, maxTrackedTextItems, maxPendingTextBytes, maxPendingTextEvents, maxReasoningBytes, maxReasoningItems, maxReasoningParts)
}

// decodeStreamBounded is the full-injection entry point. All production paths
// keep their package constants; tests inject small budgets. The content-part
// budget uses the production cap.
func decodeStreamBounded(r io.Reader, emit func(ir.StreamEvent) error, maxArgBytes, maxCalls, maxTextBytes, maxTextItems, maxPendingBytes, maxPendingEvents, maxReasoningBytes, maxReasoningItems, maxReasoningParts int) error {
	return decodeStreamBoundedParts(r, emit, maxArgBytes, maxCalls, maxTextBytes, maxTextItems, maxTrackedTextParts, maxPendingBytes, maxPendingEvents, maxReasoningBytes, maxReasoningItems, maxReasoningParts, decodePolicy{})
}

// testCaptureDecoder, when set, receives the decoder for an in-flight decode.
// Tests inspect reasoning registration through it. Production leaves it nil.
var testCaptureDecoder func(*responsesToolDecoder)

// decodePolicy selects the backend-specific ordering rules for one decode.
// Every field defaults to the unchanged standard Responses behavior.
type decodePolicy struct {
	// awaitItemID withholds a function item until its native id arrives or a
	// terminal snapshot proves it absent (Grok Build).
	awaitItemID bool
	// reconcileCodex withholds provisional reasoning encrypted_content until a
	// completion frame or the terminal snapshot confirms it.
	reconcileCodex bool
	// observe receives source-order metadata without changing client emission.
	observe func(ir.StreamEvent)
}

// decodeStreamBoundedParts adds the content-part budget so tests can inject a
// small part cap independently of the item cap and byte budgets.
func decodeStreamBoundedParts(r io.Reader, emit func(ir.StreamEvent) error, maxArgBytes, maxCalls, maxTextBytes, maxTextItems, maxTextParts, maxPendingBytes, maxPendingEvents, maxReasoningBytes, maxReasoningItems, maxReasoningParts int, policy decodePolicy) error {
	awaitItemID, reconcileCodex, observe := policy.awaitItemID, policy.reconcileCodex, policy.observe
	reader := sse.NewReader(r)
	started := false
	startID, startModel := "", ""
	stop := ir.StopEndTurn
	inputTokens, outputTokens := 0, 0
	cacheRead, cacheWrite := 0, 0
	var pendingFail *ir.StreamFailure

	ensureStarted := func(id, model string) error {
		if id != "" {
			startID = id
		}
		if model != "" {
			startModel = model
		}
		if started {
			return nil
		}
		started = true
		return emit(ir.StreamEvent{Kind: ir.EventMessageStart, ID: startID, Model: startModel})
	}
	dec := &responsesToolDecoder{
		emit: emit, maxArgBytes: maxArgBytes, maxCalls: maxCalls,
		maxTextBytes: maxTextBytes, maxTextItems: maxTextItems, maxTextParts: maxTextParts,
		maxPendingBytes: maxPendingBytes, maxPendingEvents: maxPendingEvents,
		ensureStarted:     func() error { return ensureStarted("", "") },
		awaitNativeItemID: awaitItemID,
		observe:           observe,
	}
	dec.reasoning = newResponsesReasoningDecoder(emit, dec.ensureStarted, maxReasoningBytes, maxReasoningItems, maxReasoningParts)
	dec.reasoning.reconcileCodex = reconcileCodex
	if reconcileCodex {
		dec.reasoning.canOpenEarly = dec.canOpenCodexReasoningEarly
	}
	dec.reasoning.orderEmit = dec.orderEmitReasoning
	dec.reasoning.msgCount = func() int { return len(dec.texts) }
	if awaitItemID {
		dec.reasoning.bindSeq = dec.noteReasonBound
	}
	// Chat metadata stays off the emit path. Native decode passes a nil observer.
	if observe != nil {
		dec.reasoning.noteOrder = observe
	}
	if testCaptureDecoder != nil {
		testCaptureDecoder(dec)
	}

	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if len(ev.Data) == 0 {
			continue
		}
		var env streamEnvelope
		if json.Unmarshal(ev.Data, &env) != nil {
			// Unknown or unrelated malformed events keep their tolerated skip.
			// A recognized reasoning-bearing event with a malformed supported
			// field is a protocol failure, not a silent drop of opaque state.
			if msg := malformedReasoningEvent(ev.Data); msg != "" {
				return ir.ProtocolError(msg)
			}
			if msg := malformedMessageEvent(ev.Data); msg != "" {
				return ir.ProtocolError(msg)
			}
			continue
		}
		// Failure discriminators are authoritative even when nested metadata is
		// unusable. Check them before the success switch so a failed status on a
		// non-response.failed event cannot be ignored.
		if env.Type == "error" {
			pendingFail = streamFailureFrom(env.Error, nil)
			if pendingFail == nil {
				pendingFail = &ir.StreamFailure{Message: "upstream response failed"}
			}
			// Keep reading so a trailing response.failed can refine the failure;
			// if the stream ends here, pendingFail is returned below.
			continue
		}
		failedStatus := env.Response != nil && env.Response.Status == "failed"
		if env.Type == "response.failed" || failedStatus {
			fail := streamFailureFrom(nil, env.Response)
			if pendingFail != nil {
				if fail == nil {
					fail = pendingFail
				} else {
					// Prefer response.failed fields; fill gaps from a prior error event.
					if fail.Type == "" {
						fail.Type = pendingFail.Type
					}
					if fail.Code == "" {
						fail.Code = pendingFail.Code
					}
					if fail.Message == "" {
						fail.Message = pendingFail.Message
					}
				}
			}
			if fail == nil {
				fail = &ir.StreamFailure{Message: "upstream response failed"}
			}
			return fail
		}
		switch env.Type {
		case "response.created", "response.in_progress":
			if env.Response != nil {
				if err := ensureStarted(env.Response.ID, env.Response.Model); err != nil {
					return err
				}
			}
		case "response.output_item.added":
			if env.Item != nil {
				switch env.Item.Type {
				case "function_call":
					if err := dec.observeAdded(&env); err != nil {
						return err
					}
				case "reasoning":
					if err := dec.reasoning.observeAdded(&env); err != nil {
						return err
					}
				case "message":
					if err := dec.observeMessageAdded(&env); err != nil {
						return err
					}
				}
			}
		case "response.content_part.added":
			if err := dec.observeContentPartAdded(&env); err != nil {
				return err
			}
		case "response.output_text.delta":
			if err := dec.rejectContent(env.Delta); err != nil {
				return err
			}
			if err := dec.observeTextDelta(&env, false); err != nil {
				return err
			}
		case "response.refusal.delta":
			if err := dec.rejectContent(env.Delta); err != nil {
				return err
			}
			if err := dec.observeTextDelta(&env, true); err != nil {
				return err
			}
		case "response.output_text.done":
			if err := dec.observeTextDone(&env, false); err != nil {
				return err
			}
		case "response.refusal.done":
			if err := dec.observeRefusalDone(&env); err != nil {
				return err
			}
		case "response.content_part.done":
			if err := dec.observePartDone(&env); err != nil {
				return err
			}
		case "response.reasoning_summary_text.delta":
			if err := dec.rejectContent(env.Delta); err != nil {
				return err
			}
			if err := dec.reasoning.observeDelta(&env); err != nil {
				return err
			}
		case "response.reasoning_summary_text.done":
			if err := dec.reasoning.observeTextDone(&env); err != nil {
				return err
			}
		case "response.reasoning_summary_part.added":
			if err := dec.reasoning.observePart(&env, false); err != nil {
				return err
			}
		case "response.reasoning_summary_part.done":
			if err := dec.reasoning.observePart(&env, true); err != nil {
				return err
			}
		case "response.function_call_arguments.delta":
			if err := dec.observeDelta(&env); err != nil {
				return err
			}
		case "response.function_call_arguments.done":
			if err := dec.observeArgumentsDone(&env); err != nil {
				return err
			}
		case "response.output_item.done":
			if env.Item != nil {
				switch env.Item.Type {
				case "function_call":
					if err := dec.observeItemDone(&env); err != nil {
						return err
					}
				case "reasoning":
					if err := dec.reasoning.observeItemDone(&env); err != nil {
						return err
					}
				case "message":
					if err := dec.observeMessageDone(&env); err != nil {
						return err
					}
				}
			}
		case "response.completed", "response.incomplete":
			if env.Response != nil {
				if err := ensureStarted(env.Response.ID, env.Response.Model); err != nil {
					return err
				}
				// Both completed and incomplete close the response. An omitted status
				// is still terminal; only an explicit incomplete stop overrides tools.
				// observeFinal sets the response terminal flag after this snapshot, so
				// a valid final-only call in the first completion is still accepted.
				if err := dec.observeFinal(env.Response.Output); err != nil {
					return err
				}
				if env.Response.Usage != nil {
					u := usageFromWire(env.Response.Usage)
					inputTokens = u.InputTokens
					outputTokens = u.OutputTokens
					cacheRead = u.CacheReadTokens
					cacheWrite = u.CacheWriteTokens
				}
				stop = responsesStopReason(env.Response.Status, incompleteReason(env.Response.IncompleteDetails), dec.sawTool, dec.sawRefusal)
			}
		}
	}
	if pendingFail != nil {
		return pendingFail
	}
	if err := dec.flushPending(); err != nil {
		return err
	}
	if err := dec.flushTextPending(); err != nil {
		return err
	}
	if dec.sawTool {
		if err := ensureStarted("", ""); err != nil {
			return err
		}
		// An explicit incomplete status already won. A tool stop outranks a
		// refusal part, but it does not replace content-filter or max-token truncation.
		if stop != ir.StopMaxTokens && stop != ir.StopContentFilter {
			stop = ir.StopToolUse
		}
	}
	if stop == ir.StopEndTurn && dec.sawRefusal {
		stop = ir.StopRefusal
	}
	if !started {
		return nil
	}
	return emit(ir.StreamEvent{
		Kind: ir.EventFinish, StopReason: stop,
		InputTokens: inputTokens, OutputTokens: outputTokens,
		CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite,
	})
}

// responsesToolCall tracks one function call. IR Index is assigned once and is
// not the Responses output index: completion output uses array order, while
// argument events may omit output_index or arrive before output_item.added.
//
// args is the one retained argument string. emitted is the byte offset already
// sent as EventToolCallDelta. Growth is appended once; a flush only advances
// emitted. terminal rejects every later change, including an identical-length
// replacement, but an identical snapshot is not a change.
//
// fromFinal marks a call created by a completion output item that did not match
// a streamed call. Those calls are keyed by output-array ordinal, not by the
// streamed output_index map.
type responsesToolCall struct {
	index       int
	hasIndex    bool
	outputIndex int
	itemID      string
	callID      string
	name        string
	started     bool
	fromFinal   bool
	args        strings.Builder
	emitted     int
	terminal    bool
	// awaitingItemID is set by the Grok Build policy while a native item id is
	// still possible. itemIDAbsent is set when a terminal snapshot proved that
	// the id will not arrive. EOF uses force instead of this flag.
	awaitingItemID bool
	itemIDAbsent   bool
	// seq is the stream registration order shared with text and reasoning.
	// A tool that still lacks a native item id is a barrier at this seq: no
	// item registered at or after it may be delivered first.
	seq int
	// orderMsgs is len(texts) when the call was created. A call registered
	// before an open message is not deferred by that message.
	orderMsgs int
	// wired records that at least one event reached the consumer, so a later
	// final-only slot at an earlier array ordinal cannot silently reorder it.
	wired bool
	// finalMatched and finalOrdinal track the terminal output-array position a
	// call was bound to, including a streamed call matched by item or call ID.
	finalMatched bool
	finalOrdinal int
}

// responsesToolDecoder reconciles argument deltas and complete snapshots for
// one Responses stream. Raw argument text is preserved; it is not parsed.
type responsesToolDecoder struct {
	emit          func(ir.StreamEvent) error
	ensureStarted func() error
	maxArgBytes   int
	maxCalls      int
	calls         []*responsesToolCall
	byIndex       map[int]*responsesToolCall
	byItem        map[string]*responsesToolCall
	byCall        map[string]*responsesToolCall
	byFinal       map[int]*responsesToolCall
	nextIndex     int
	argBytes      int
	sawTool       bool
	// sawRefusal records a native refusal part. It selects StopRefusal only when
	// no stronger terminal reason is set. It is not an upstream failure.
	sawRefusal bool
	// reasoning reconciles Responses reasoning items. It is nil only in tests
	// that construct the decoder directly for tool-only assertions.
	reasoning *responsesReasoningDecoder
	// texts tracks identified message text so a final-only or extended message
	// snapshot recovers only its own missing bytes. Legacy unkeyed deltas cannot
	// be attributed to one final item and keep the historical skip.
	texts          []*responsesTextItem
	textByItem     map[string]*responsesTextItem
	textByIndex    map[int]*responsesTextItem
	textByFinal    map[int]*responsesTextItem
	textBytes      int
	textParts      int
	maxTextBytes   int
	maxTextItems   int
	maxTextParts   int
	sawUnkeyedText bool
	// textQueue holds retained, not-yet-emitted ordering work from items behind
	// the earliest open message. All kinds share one bounded FIFO and one pending
	// budget: queueBytes and textQueueEvents charge payloads plus per-entry
	// bookkeeping. Pending limits are independent of, and smaller than, the
	// per-state budgets.
	textQueue        []pendingOrderWork
	queueBytes       int
	textQueueEvents  int
	maxPendingBytes  int
	maxPendingEvents int
	// bypassOrder disables deferral during the terminal array pass and during a
	// drain, where the decoder itself chooses the emission order.
	bypassOrder bool
	// responseDone is set after the first completed or incomplete snapshot is
	// accepted. It blocks new calls and new content even when output is empty.
	responseDone bool
	// awaitNativeItemID withholds tool start and arguments until a native item
	// id arrives or a terminal snapshot or EOF proves it absent. The withheld
	// tool is a barrier in textQueue, at its registration seq, so later items
	// cannot pass it. The barrier shares the pending text budget. drainingOrder
	// is set while that queue is being delivered, so a release does not queue
	// the same event again.
	awaitNativeItemID bool
	// observe is the Chat-bridge side channel. Nil leaves wire order unchanged.
	observe       func(ir.StreamEvent)
	drainingOrder bool
	nextSeq       int
	// seqByOutput and seqByID record a bound reasoning item's output index and
	// id. Keys are copied from the item after the reasoning decoder accepts it.
	// An envelope item_id that the decoder ignored is not a key.
	seqByOutput map[int]int
	seqByID     map[string]int
	reasonSeq   map[*responsesReasoningItem]int
	// itemWire records the order in which an item first reached the consumer.
	// A completed array whose ordinals disagree with this order is a protocol
	// error rather than a silent reorder of bytes already sent.
	itemWire  map[any]int
	wireClock int
}

// pendingOrderKind discriminates one deferred ordering work item. Text work
// emits a retained message suffix on release; tool and reasoning work emit a
// stored IR event.
type pendingOrderKind uint8

const (
	pendingOrderText pendingOrderKind = iota
	pendingOrderTool
	pendingOrderReasoning
	// pendingOrderItemBarrier reserves a tool's stream position until its
	// native item id arrives or is proved absent. It is not an IR event.
	pendingOrderItemBarrier
)

// pendingOrderWork is one ordering-sensitive emission retained behind an
// unresolved earlier message. cost is the exact pending-budget charge and is
// subtracted when the entry is released. textOwner, call, and reason identify
// the owning item so the terminal array pass can flush an item at its own slot.
type pendingOrderWork struct {
	kind      pendingOrderKind
	textOwner *responsesTextItem
	call      *responsesToolCall
	reason    *responsesReasoningItem
	event     ir.StreamEvent
	cost      int
	// heldForItem marks work queued behind an item-id barrier. While any such
	// entry or barrier remains, the queue drains as a strict prefix.
	heldForItem bool
	// raw is a stored text event with no text owner, used for unkeyed text that
	// arrived while a barrier was active.
	raw bool
}

// responsesTextPart is one content part of a message, keyed by content_index.
// end is the offset in responsesTextItem.text one past the part's bytes; the
// part's start is the previous part's end, so the parts partition the message
// text. done marks a part closed by its own complete snapshot; a whole-message
// snapshot closes every part.
type responsesTextPart struct {
	end int
	typ string
	// refusal is the part kind. A marker change splits recovered suffixes so
	// mixed ordinary text and refusal are not merged semantically. kindSet keeps
	// an explicit ordinary part distinct from a part whose kind is still unknown.
	refusal bool
	kindSet bool
	done    bool
}

// responsesTextItem tracks one message item for final text recovery. itemID and
// streamed output index identify it; the terminal output-array ordinal only
// disambiguates items without a usable identity. text retains the bytes needed
// to recover a final-only message or a final suffix without duplication; parts
// attributes those bytes to real content indices.
type responsesTextItem struct {
	itemID       string
	hasIndex     bool
	outputIndex  int
	fromFinal    bool
	finalOrdinal int
	finalMatched bool
	text         strings.Builder
	emitted      int
	parts        []*responsesTextPart
	// emptyRefusalEmitted records content indices whose explicit empty refusal
	// was already emitted. The index is bounded by the part cap.
	emptyRefusalEmitted []bool
	// loose records contiguous unindexed byte ranges and their markers.
	// A snapshot may claim those bytes only when every overlapping range has
	// the same marker. The ranges are coalesced, so bookkeeping stays bounded
	// by the part cap rather than by the delta count.
	loose []looseTextRange
	// indexedDelta records that a delta named a content index. A later loose
	// delta is then an unsupported mixture. A done snapshot does not set this.
	indexedDelta bool
	terminal     bool
	// seq is the stream registration order. Text registered before a blocked
	// tool keeps this seq and is not moved behind that tool.
	seq int
}

type textIdentity struct {
	outputIndex  int
	hasIndex     bool
	itemID       string
	finalOrdinal int
}

func finalMessageText(item *respOutputItem) string {
	var b strings.Builder
	for _, c := range item.Content {
		part, ok := outputContentPart(c)
		if !ok {
			continue
		}
		b.WriteString(part.Text)
	}
	return b.String()
}

func messageHasRefusal(item *respOutputItem) bool {
	if item == nil {
		return false
	}
	for _, c := range item.Content {
		part, ok := outputContentPart(c)
		if ok && part.Refusal {
			return true
		}
	}
	return false
}

func (d *responsesToolDecoder) retainText(n int) error {
	if n <= 0 {
		return nil
	}
	if n > d.maxTextBytes-d.textBytes {
		return ir.ProtocolError("message text too large")
	}
	d.textBytes += n
	return nil
}

// observeTextDelta streams one output_text delta. Identified deltas are also
// retained so a later final snapshot can recover only its own missing suffix;
// unkeyed deltas keep the legacy immediate path and suppress final recovery.
// While an earlier identified message is still waiting for its terminal
// snapshot, later identified text queues instead of emitting, so a recovered
// suffix can never be emitted after already emitted later text.
func (d *responsesToolDecoder) observeTextDelta(env *streamEnvelope, refusal bool) error {
	if env.Delta == "" {
		// An identified empty refusal delta still records the part kind so a later
		// done snapshot can preserve an explicit empty refusal.
		if refusal && (env.ItemID != "" || env.OutputIndexSet) {
			return d.noteRefusalPart(env)
		}
		return nil
	}
	if env.ItemID == "" && !env.OutputIndexSet {
		// A refusal delta needs an item or output index. Guessing its identity would
		// reclassify later snapshots. Ordinary unkeyed text keeps the legacy path.
		if refusal {
			return ir.ProtocolError("refusal delta has no content identity")
		}
		d.sawUnkeyedText = true
		if err := d.ensureMessageStart(); err != nil {
			return err
		}
		ev := ir.StreamEvent{Kind: ir.EventTextDelta, Text: env.Delta}
		if d.deferItemID(d.nextSeq) {
			return d.enqueueRaw(ev)
		}
		return d.emit(ev)
	}
	item, err := d.bindText(textIdentity{outputIndex: env.OutputIndex, hasIndex: env.OutputIndexSet, itemID: env.ItemID}, true, false)
	if err != nil {
		return err
	}
	if item.terminal || d.responseDone {
		return ir.ProtocolError("conflicting message text")
	}
	// Decide before retention whether this delta queues, so an over-budget
	// queued event is rejected without retaining or emitting it. A tool barrier
	// at or before this message also queues the delta; an earlier message keeps
	// emitting so it cannot move behind that tool.
	open := d.firstOpenText()
	if item == open && !d.shouldHoldText(item) {
		if err := d.appendTextDelta(item, env, refusal); err != nil {
			return err
		}
		return d.emitTextSuffix(item)
	}
	cost := len(env.Delta) + textQueueEntryOverhead
	if err := d.checkPending(cost); err != nil {
		return err
	}
	if err := d.appendTextDelta(item, env, refusal); err != nil {
		return err
	}
	d.insertPending(pendingOrderWork{
		kind: pendingOrderText, textOwner: item, cost: cost,
		heldForItem: d.deferItemID(item.seq),
	})
	return nil
}

// appendTextDelta appends one delta to the message aggregate. A delta with an
// explicit content_index is attributed to that part so a later complete
// snapshot for the same index validates against the right bytes. A delta that
// names an already closed part, or that would grow a non-tail part, is
// rejected so a late delta cannot reorder completed content.
func (d *responsesToolDecoder) appendTextDelta(item *responsesTextItem, env *streamEnvelope, refusal bool) error {
	if env.ContentIndexSet {
		index := env.ContentIndex
		if err := d.ensureTextParts(item, index); err != nil {
			return err
		}
		part := item.parts[index]
		if err := d.notePartKind(part, refusal); err != nil {
			return err
		}
		if part.done {
			return ir.ProtocolError("conflicting message text")
		}
		if index != len(item.parts)-1 || part.end != item.text.Len() {
			return ir.ProtocolError("conflicting message text order")
		}
		if err := d.retainText(len(env.Delta)); err != nil {
			return err
		}
		item.text.WriteString(env.Delta)
		part.end += len(env.Delta)
		item.indexedDelta = true
		if refusal && env.Delta != "" {
			// A nonempty indexed delta records the refusal without closing the part.
			d.sawRefusal = true
		}
		return nil
	}
	// Unindexed deltas stay in the historical aggregate. They do not move an
	// indexed part boundary. A loose delta after an indexed delta is a different
	// unsupported mixture: its marker cannot be attributed without guessing.
	// A part created only by a done snapshot is not that mixture.
	if item.indexedDelta {
		return ir.ProtocolError("conflicting message text")
	}
	start := item.text.Len()
	// Marker acceptance is checked before retention. A rejected delta leaves
	// neither a range nor retained bytes for a later snapshot to claim.
	if err := looseRangeAccepted(item, start, start+len(env.Delta), refusal); err != nil {
		return err
	}
	if err := d.retainText(len(env.Delta)); err != nil {
		return err
	}
	if err := d.noteLooseKind(item, start, start+len(env.Delta), refusal); err != nil {
		return err
	}
	item.text.WriteString(env.Delta)
	return nil
}

// checkPending validates the combined pending queue budget. The check runs
// before retention so a rejected event leaves no partial state.
func (d *responsesToolDecoder) checkPending(cost int) error {
	if d.textQueueEvents >= d.maxPendingEvents || cost > d.maxPendingBytes-d.queueBytes {
		return ir.ProtocolError("pending message text exceeded limit")
	}
	return nil
}

// appendPending records one already-budget-checked ordering entry. The cost is
// the exact charge released when the entry drains.
func (d *responsesToolDecoder) appendPending(w pendingOrderWork) {
	d.textQueue = append(d.textQueue, w)
	d.queueBytes += w.cost
	d.textQueueEvents++
}

// insertPending records one already-budget-checked entry at its owner's stream
// position. A late event for an earlier item must not land behind a later tool
// that is only waiting in the queue.
func (d *responsesToolDecoder) insertPending(w pendingOrderWork) {
	seq, ok := d.pendingSeq(w)
	if !d.awaitNativeItemID || !ok {
		d.appendPending(w)
		return
	}
	idx := len(d.textQueue)
	for i, existing := range d.textQueue {
		es, eok := d.pendingSeq(existing)
		if eok && es > seq {
			idx = i
			break
		}
	}
	d.textQueue = append(d.textQueue, pendingOrderWork{})
	copy(d.textQueue[idx+1:], d.textQueue[idx:])
	d.textQueue[idx] = w
	d.queueBytes += w.cost
	d.textQueueEvents++
}

func (d *responsesToolDecoder) pendingSeq(w pendingOrderWork) (int, bool) {
	switch w.kind {
	case pendingOrderItemBarrier, pendingOrderTool:
		if w.call != nil {
			return w.call.seq, true
		}
	case pendingOrderText:
		if w.textOwner != nil {
			return w.textOwner.seq, true
		}
	case pendingOrderReasoning:
		if w.reason != nil {
			return d.reasonOrdinal(w.reason), true
		}
	}
	return 0, false
}

// enqueueOrder validates and retains one ordering entry. A failure retains
// nothing, so an offending event is never queued or emitted.
func (d *responsesToolDecoder) enqueueOrder(w pendingOrderWork) error {
	if err := d.checkPending(w.cost); err != nil {
		return err
	}
	d.insertPending(w)
	return nil
}

// firstOpenText returns the earliest streamed message whose terminal snapshot
// has not been seen. Only that message may emit directly; later messages queue
// until it closes so a recovered suffix cannot reorder behind them.
func (d *responsesToolDecoder) firstOpenText() *responsesTextItem {
	for _, item := range d.texts {
		if item.fromFinal || item.terminal {
			continue
		}
		return item
	}
	return nil
}

func (d *responsesToolDecoder) textIndex(item *responsesTextItem) (int, bool) {
	for i, known := range d.texts {
		if known == item {
			return i, true
		}
	}
	return 0, false
}

// pendingOrderEventBytes is the combined pending charge of a queued tool or
// reasoning event: payload strings, the reasoning summary slice and per-part
// overhead, and one entry of bookkeeping.
func pendingOrderEventBytes(ev ir.StreamEvent) int {
	n := textQueueEntryOverhead
	n += len(ev.Text) + len(ev.ArgsFrag) + len(ev.ToolID) + len(ev.ToolName) + len(ev.ResponsesItemID) + len(ev.ReasoningID)
	if ev.Reasoning != nil {
		n += len(ev.Reasoning.ID) + len(ev.Reasoning.Status)
		if ev.Reasoning.EncryptedContent != nil {
			n += len(*ev.Reasoning.EncryptedContent)
		}
		n += len(ev.Reasoning.Summary) * reasoningPartOverhead
		for _, p := range ev.Reasoning.Summary {
			n += len(p.Type) + len(p.Text)
		}
	}
	return n
}

// flushClosedText emits a closed message's retained tail when no earlier
// message is still open, then releases queued work that may now flow. The
// emitted tail advances emitted, so the queued portion is never repeated.
func (d *responsesToolDecoder) flushClosedText(item *responsesTextItem) error {
	for _, known := range d.texts {
		if known == item {
			break
		}
		if !known.fromFinal && !known.terminal {
			return nil
		}
	}
	if d.shouldHoldText(item) {
		return d.holdTextSuffix(item)
	}
	if err := d.emitTextSuffix(item); err != nil {
		return err
	}
	return d.drainOrdered()
}

// orderReleasable reports that one queued entry may emit now. Text of the
// current barrier message or of a closed message flows; text of a later
// message waits. A tool or reasoning event flows once every message registered
// before it is closed, so an event registered before an open message is not
// deferred by it.
func (d *responsesToolDecoder) orderReleasable(w pendingOrderWork, barrier *responsesTextItem) bool {
	switch w.kind {
	case pendingOrderItemBarrier:
		return false
	case pendingOrderText:
		if w.textOwner == nil || barrier == nil {
			return true
		}
		ownerIdx, ownerOK := d.textIndex(w.textOwner)
		barrierIdx, barrierOK := d.textIndex(barrier)
		if !ownerOK || !barrierOK {
			return true
		}
		// A later message stays behind every unresolved earlier message. The
		// barrier is the earliest open message, so the owner may emit only when
		// no open message precedes it: owner index <= barrier index. A terminal
		// flag alone must not release text past the barrier.
		return ownerIdx <= barrierIdx
	case pendingOrderTool:
		if barrier == nil {
			return true
		}
		idx, ok := d.textIndex(barrier)
		return !ok || idx >= w.call.orderMsgs
	case pendingOrderReasoning:
		if barrier == nil {
			return true
		}
		idx, ok := d.textIndex(barrier)
		return !ok || idx >= w.reason.orderMsgs
	}
	return true
}

// emitOrderWork emits one released entry: a message suffix for text work, or
// the stored event verbatim for tool and reasoning work.
func (d *responsesToolDecoder) emitOrderWork(w pendingOrderWork) error {
	if w.kind == pendingOrderItemBarrier {
		return nil
	}
	if w.kind == pendingOrderText {
		if w.raw {
			return d.emit(w.event)
		}
		return d.emitTextSuffix(w.textOwner)
	}
	if w.call != nil {
		w.call.wired = true
		d.noteItemWire(w.call)
	}
	if w.reason != nil {
		w.reason.wired = true
		d.noteItemWire(w.reason)
	}
	return d.emitObserved(w.call, w.event)
}

func (d *responsesToolDecoder) releaseOrderCost(w pendingOrderWork) {
	d.queueBytes -= w.cost
	if d.queueBytes < 0 {
		d.queueBytes = 0
	}
	if d.textQueueEvents > 0 {
		d.textQueueEvents--
	}
}

// drainOrdered emits every releasable entry in arrival order and keeps the
// rest. A releasable entry behind a blocked one is safe to emit first because
// a blocked entry is always later in stream order than the current barrier.
func (d *responsesToolDecoder) drainOrdered() error {
	if len(d.textQueue) == 0 {
		return nil
	}
	if d.itemQueueStrict() {
		return d.drainItemPrefix()
	}
	barrier := d.firstOpenText()
	var kept []pendingOrderWork
	for _, w := range d.textQueue {
		if !d.orderReleasable(w, barrier) {
			kept = append(kept, w)
			continue
		}
		d.releaseOrderCost(w)
		if err := d.emitOrderWork(w); err != nil {
			return err
		}
	}
	d.textQueue = kept
	d.resetEmptyQueue()
	return nil
}

// drainItemPrefix delivers a leading run of releasable work and stops at the
// first item-id barrier or message-blocked entry. A later ready tool must not
// pass either one.
func (d *responsesToolDecoder) drainItemPrefix() error {
	if d.drainingOrder {
		return nil
	}
	d.drainingOrder = true
	defer func() { d.drainingOrder = false }()
	barrier := d.firstOpenText()
	var kept []pendingOrderWork
	for i, w := range d.textQueue {
		if w.kind == pendingOrderItemBarrier || !d.orderReleasable(w, barrier) {
			kept = append(kept, d.textQueue[i:]...)
			break
		}
		d.releaseOrderCost(w)
		if err := d.emitOrderWork(w); err != nil {
			return err
		}
	}
	d.textQueue = kept
	d.resetEmptyQueue()
	return nil
}

// flushOrderedForCall emits the queued events of one call at its terminal
// array slot, so a final-only item cannot overtake streamed work.
func (d *responsesToolDecoder) flushOrderedForCall(call *responsesToolCall) error {
	if len(d.textQueue) == 0 {
		return nil
	}
	var kept []pendingOrderWork
	for _, w := range d.textQueue {
		if w.kind == pendingOrderTool && w.call == call {
			d.releaseOrderCost(w)
			if w.call != nil {
				w.call.wired = true
				d.noteItemWire(w.call)
			}
			if err := d.emitObserved(w.call, w.event); err != nil {
				return err
			}
			continue
		}
		kept = append(kept, w)
	}
	d.textQueue = kept
	d.resetEmptyQueue()
	return nil
}

// flushOrderedForReason emits the queued events of one reasoning item at its
// terminal array slot, so its summaries and done never overtake it.
func (d *responsesToolDecoder) flushOrderedForReason(reason *responsesReasoningItem) error {
	if len(d.textQueue) == 0 {
		return nil
	}
	var kept []pendingOrderWork
	for _, w := range d.textQueue {
		if w.kind == pendingOrderReasoning && w.reason == reason {
			d.releaseOrderCost(w)
			if w.reason != nil {
				w.reason.wired = true
				d.noteItemWire(w.reason)
			}
			if err := d.emit(w.event); err != nil {
				return err
			}
			continue
		}
		kept = append(kept, w)
	}
	d.textQueue = kept
	d.resetEmptyQueue()
	return nil
}

func (d *responsesToolDecoder) resetEmptyQueue() {
	if len(d.textQueue) != 0 {
		return
	}
	d.textQueue = nil
	d.queueBytes = 0
	d.textQueueEvents = 0
}

// flushTextPending releases all retained ordering work at EOF when no terminal
// snapshot arrived. Work drains in arrival order and each streamed message's
// retained tail follows, so an incomplete stream drops no valid content.
func (d *responsesToolDecoder) flushTextPending() error {
	if err := d.releaseItemBarriers(); err != nil {
		return err
	}
	d.drainingOrder = true
	for _, w := range d.textQueue {
		d.releaseOrderCost(w)
		if w.kind == pendingOrderItemBarrier {
			continue
		}
		if err := d.emitOrderWork(w); err != nil {
			d.drainingOrder = false
			return err
		}
	}
	d.drainingOrder = false
	d.textQueue = nil
	d.queueBytes = 0
	d.textQueueEvents = 0
	for _, item := range d.texts {
		if item.fromFinal {
			continue
		}
		if err := d.emitTextSuffix(item); err != nil {
			return err
		}
	}
	return nil
}

// bindText looks up a message item by streamed output index and item ID. create
// allocates only when no key matches; after the response is terminal a new item
// is refused so a repeated completion cannot append content.
func (d *responsesToolDecoder) bindText(id textIdentity, create, fromFinal bool) (*responsesTextItem, error) {
	var found *responsesTextItem
	match := func(item *responsesTextItem) error {
		if item == nil {
			return nil
		}
		if found == nil {
			found = item
			return nil
		}
		if found != item {
			return ir.ProtocolError("conflicting message item identity")
		}
		return nil
	}
	if id.hasIndex {
		if err := match(d.textByIndex[id.outputIndex]); err != nil {
			return nil, err
		}
	}
	if id.itemID != "" {
		if err := match(d.textByItem[id.itemID]); err != nil {
			return nil, err
		}
	}
	if found == nil {
		if !create {
			return nil, nil
		}
		if d.responseDone {
			return nil, ir.ProtocolError("conflicting message item identity")
		}
		if len(d.texts) >= d.maxTextItems {
			return nil, ir.ProtocolError("too many message text items")
		}
		if err := d.retainText(textItemOverhead); err != nil {
			return nil, err
		}
		found = &responsesTextItem{fromFinal: fromFinal, seq: d.allocSeq()}
		d.texts = append(d.texts, found)
		if fromFinal {
			if err := d.retainText(textKeyOverhead); err != nil {
				return nil, err
			}
			if d.textByFinal == nil {
				d.textByFinal = map[int]*responsesTextItem{}
			}
			d.textByFinal[id.finalOrdinal] = found
			found.finalMatched = true
			found.finalOrdinal = id.finalOrdinal
		}
	}
	if err := d.assignText(found, id); err != nil {
		return nil, err
	}
	return found, nil
}

func (d *responsesToolDecoder) assignText(item *responsesTextItem, id textIdentity) error {
	if id.hasIndex {
		if item.hasIndex && item.outputIndex != id.outputIndex {
			return ir.ProtocolError("conflicting message item identity")
		}
		if other := d.textByIndex[id.outputIndex]; other != nil && other != item {
			return ir.ProtocolError("conflicting message item identity")
		}
	}
	if id.itemID != "" {
		if item.itemID != "" && item.itemID != id.itemID {
			return ir.ProtocolError("conflicting message item identity")
		}
		if other := d.textByItem[id.itemID]; other != nil && other != item {
			return ir.ProtocolError("conflicting message item identity")
		}
	}
	if d.textByItem == nil {
		d.textByItem = map[string]*responsesTextItem{}
		d.textByIndex = map[int]*responsesTextItem{}
	}
	if id.hasIndex && !item.hasIndex {
		if err := d.retainText(textKeyOverhead); err != nil {
			return err
		}
		item.hasIndex = true
		item.outputIndex = id.outputIndex
		d.textByIndex[id.outputIndex] = item
	}
	if id.itemID != "" && item.itemID == "" {
		if err := d.retainText(len(id.itemID) + textKeyOverhead); err != nil {
			return err
		}
		item.itemID = id.itemID
		d.textByItem[id.itemID] = item
	}
	return nil
}

func (d *responsesToolDecoder) claimTextFinal(item *responsesTextItem, ordinal int) error {
	if item.finalMatched && item.finalOrdinal != ordinal {
		return ir.ProtocolError("conflicting message item identity")
	}
	item.finalMatched = true
	item.finalOrdinal = ordinal
	return nil
}

// uniqueTextIndexMatch returns the sole streamed message whose output_index
// equals ordinal. A final-only ordinal is never treated as a streamed index.
func (d *responsesToolDecoder) uniqueTextIndexMatch(ordinal int) *responsesTextItem {
	var found *responsesTextItem
	for _, item := range d.texts {
		if item.fromFinal || !item.hasIndex || item.outputIndex != ordinal {
			continue
		}
		if found != nil {
			return nil
		}
		found = item
	}
	return found
}

// blocksNewTextFinal reports that a final-only message cannot be proved
// distinct from a streamed message. An identified streamed message is distinct
// when its ID differs; an unidentified one is distinct only through a different
// streamed output index.
func (d *responsesToolDecoder) blocksNewTextFinal(ordinal int, item *respOutputItem) bool {
	for _, known := range d.texts {
		if known.fromFinal {
			continue
		}
		if known.itemID != "" && known.itemID != item.ID {
			continue
		}
		if known.itemID == "" && known.hasIndex && known.outputIndex != ordinal {
			continue
		}
		return true
	}
	return false
}

func (d *responsesToolDecoder) hasStreamedTextItem() bool {
	for _, item := range d.texts {
		if !item.fromFinal {
			return true
		}
	}
	return false
}

// matchTextFinal binds one terminal output-array message. ordinal is the array
// position; it is not assumed to equal a streamed output_index. Identity is
// matched by item ID first, then by a unique streamed index. A final-only item
// is safe only when every streamed message is provably distinct.
func (d *responsesToolDecoder) matchTextFinal(ordinal int, item *respOutputItem, allowCreate bool) (*responsesTextItem, error) {
	if known := d.textByFinal[ordinal]; known != nil {
		if err := d.assignText(known, textIdentity{itemID: item.ID}); err != nil {
			return nil, err
		}
		return known, nil
	}
	if item.ID != "" {
		if found := d.textByItem[item.ID]; found != nil {
			if err := d.claimTextFinal(found, ordinal); err != nil {
				return nil, err
			}
			return found, nil
		}
	}
	if found := d.uniqueTextIndexMatch(ordinal); found != nil {
		if item.ID != "" {
			if err := d.assignText(found, textIdentity{itemID: item.ID}); err != nil {
				return nil, err
			}
		}
		if err := d.claimTextFinal(found, ordinal); err != nil {
			return nil, err
		}
		return found, nil
	}
	if d.blocksNewTextFinal(ordinal, item) {
		return nil, ir.ProtocolError("conflicting message item identity")
	}
	if !allowCreate {
		return nil, nil
	}
	return d.bindText(textIdentity{itemID: item.ID, finalOrdinal: ordinal}, true, true)
}

// ensureTextParts grows item.parts with empty placeholders through index. Each
// placeholder is charged before allocation and the total part count is bounded.
func (d *responsesToolDecoder) ensureTextParts(item *responsesTextItem, index int) error {
	// A decoder built directly in a test may omit the injected cap; fall back to
	// the production bound instead of treating every index as out of range.
	limit := d.maxTextParts
	if limit <= 0 {
		limit = maxTrackedTextParts
	}
	if index < 0 || index >= limit {
		return ir.ProtocolError("message text part index out of range")
	}
	for len(item.parts) <= index {
		pos := 0
		if n := len(item.parts); n > 0 {
			pos = item.parts[n-1].end
		}
		if d.textParts >= limit {
			return ir.ProtocolError("too many message text parts")
		}
		if err := d.retainText(textPartOverhead); err != nil {
			return err
		}
		item.parts = append(item.parts, &responsesTextPart{end: pos})
		d.textParts++
	}
	return nil
}

// partStart returns the offset where part index begins. Parts partition the
// message text, so a part starts where the previous one ended.
func (item *responsesTextItem) partStart(index int) int {
	if index <= 0 {
		return 0
	}
	return item.parts[index-1].end
}

// looseTextRange is one contiguous run of identified bytes that arrived
// without a content_index. start is inclusive and end is exclusive in the
// message aggregate. Adjacent runs with the same marker coalesce.
type looseTextRange struct {
	start   int
	end     int
	refusal bool
}

// looseRangeAccepted reports whether [start, end) can join the loose ranges.
// It does not mutate. A marker change inside one contiguous run is rejected
// before the delta bytes are retained.
func looseRangeAccepted(item *responsesTextItem, start, end int, refusal bool) error {
	if item == nil || start > end || start < 0 {
		return ir.ProtocolError("conflicting message text")
	}
	if start == end {
		return nil
	}
	n := len(item.loose)
	if n == 0 {
		return nil
	}
	last := item.loose[n-1]
	if last.end > start {
		return ir.ProtocolError("conflicting message text")
	}
	if last.end == start && last.refusal != refusal {
		return ir.ProtocolError("conflicting message text")
	}
	if last.end != start && n >= maxTrackedTextParts {
		return ir.ProtocolError("too many message text parts")
	}
	return nil
}

// noteLooseKind records one already accepted unindexed range. Adjacent ranges
// with the same marker coalesce, so the slice stays bounded by the part cap.
func (d *responsesToolDecoder) noteLooseKind(item *responsesTextItem, start, end int, refusal bool) error {
	if start == end {
		return nil
	}
	n := len(item.loose)
	if n > 0 && item.loose[n-1].end == start {
		item.loose[n-1].end = end
	} else {
		if err := d.retainText(textPartOverhead); err != nil {
			return err
		}
		item.loose = append(item.loose, looseTextRange{start: start, end: end, refusal: refusal})
	}
	if refusal {
		// A loose identified refusal is recorded even though its part stays open.
		d.sawRefusal = true
	}
	return nil
}

// looseMarkerAt reports the marker of the unindexed range that contains off.
// A byte with no loose range is not an identified unindexed byte.
func looseMarkerAt(item *responsesTextItem, off int) (refusal bool, ok bool) {
	for i := range item.loose {
		r := &item.loose[i]
		if off < r.start {
			return false, false
		}
		if off < r.end {
			return r.refusal, true
		}
	}
	return false, false
}

// claimLooseRange requires every identified unindexed byte in [start, end) to
// carry marker. Bytes with no loose range are not relabeled. The check reads
// only the coalesced ranges, so it does not copy the message text.
func claimLooseRange(item *responsesTextItem, start, end int, refusal bool) error {
	if start >= end {
		return nil
	}
	for i := range item.loose {
		r := &item.loose[i]
		if r.end <= start {
			continue
		}
		if r.start >= end {
			break
		}
		if r.refusal != refusal {
			return ir.ProtocolError("conflicting message text")
		}
	}
	return nil
}

// shiftLoose moves every loose boundary at or after at right by delta. A
// boundary inside a loose range is a contradiction: growth must not split an
// identified unindexed run.
func shiftLoose(item *responsesTextItem, at, delta int) error {
	if delta == 0 {
		return nil
	}
	for i := range item.loose {
		r := &item.loose[i]
		if r.start >= at {
			r.start += delta
			r.end += delta
			continue
		}
		if r.end > at {
			return ir.ProtocolError("conflicting message text")
		}
	}
	return nil
}

func (d *responsesToolDecoder) notePartKind(part *responsesTextPart, refusal bool) error {
	if part.kindSet && part.refusal != refusal {
		return ir.ProtocolError("conflicting message text")
	}
	part.kindSet = true
	part.refusal = refusal
	if refusal {
		part.typ = "refusal"
	} else if part.typ == "" {
		part.typ = "output_text"
	}
	return nil
}

func (d *responsesToolDecoder) reconcileTextPart(item *responsesTextItem, index int, snapshot string, refusal bool) error {
	if err := d.ensureTextParts(item, index); err != nil {
		return err
	}
	part := item.parts[index]
	start := item.partStart(index)
	// Claim the existing overlap before kind or suffix state changes. An exact
	// snapshot is the same claim: it must not relabel loose bytes.
	if err := claimLooseRange(item, start, part.end, refusal); err != nil {
		return err
	}
	if err := d.notePartKind(part, refusal); err != nil {
		return err
	}
	retained := item.text.String()[start:part.end]
	var suffix string
	switch {
	case retained == snapshot:
	case strings.HasPrefix(snapshot, retained):
		suffix = snapshot[len(retained):]
	default:
		return ir.ProtocolError("conflicting message text")
	}
	if suffix != "" {
		if part.done || item.terminal || d.responseDone {
			return ir.ProtocolError("conflicting message text")
		}
		// Growth is safe only when no later part already holds bytes; empty
		// placeholders do not block it because their derived offsets follow.
		for j := index + 1; j < len(item.parts); j++ {
			// A later part that already holds bytes owns a boundary. An empty closed
			// placeholder does not, because its offset follows this growth.
			if item.parts[j].end > item.partStart(j) {
				return ir.ProtocolError("conflicting message text order")
			}
		}
		following := item.text.String()[part.end:item.text.Len()]
		match := len(suffix)
		if match > len(following) {
			match = len(following)
		}
		if following[:match] != suffix[:match] {
			return ir.ProtocolError("conflicting message text order")
		}
		// The suffix may cover a later loose run before it adds new bytes. Reject
		// that relabel before the extra bytes are retained.
		if err := claimLooseRange(item, part.end, part.end+len(suffix), refusal); err != nil {
			return err
		}
		if len(suffix) > len(following) {
			extra := suffix[len(following):]
			growthAt := item.text.Len()
			if err := shiftLoose(item, growthAt, len(extra)); err != nil {
				return err
			}
			if err := d.retainText(len(extra)); err != nil {
				return err
			}
			item.text.WriteString(extra)
		}
		shift := len(suffix)
		part.end += shift
		// Bytes after the old boundary moved right by shift. Every later part
		// was zero-width above, so shifting its end keeps it at its own start.
		// Without the shift a later reconcile slices text[start:end] with a
		// stale end and panics when an earlier part grows again.
		for j := index + 1; j < len(item.parts); j++ {
			item.parts[j].end += shift
		}
	}
	part.done = true
	if part.refusal {
		// Closing the part records the refusal. An added placeholder does not.
		d.sawRefusal = true
	}
	return nil
}

// applyMessageContent reconciles one authoritative complete message snapshot.
// The content array carves the message text into real content indices; the
// snapshot must extend the retained aggregate and account for every retained
// part, so an explicit empty or shorter array cannot silently erase text. A
// missing content field is a placeholder and is handled by the caller.
func (d *responsesToolDecoder) applyMessageContent(item *responsesTextItem, content []respOutputContent) error {
	retained := item.text.String()
	texts := make([]string, len(content))
	kinds := make([]bool, len(content))
	known := make([]bool, len(content))
	var full strings.Builder
	for i, c := range content {
		part, ok := outputContentPart(c)
		if !ok {
			continue
		}
		texts[i] = part.Text
		kinds[i] = part.Refusal
		known[i] = true
		full.WriteString(part.Text)
	}
	if !strings.HasPrefix(full.String(), retained) {
		return ir.ProtocolError("conflicting message text")
	}
	for i, text := range texts {
		if !known[i] {
			continue
		}
		if err := d.reconcileTextPart(item, i, text, kinds[i]); err != nil {
			return err
		}
	}
	// A complete snapshot must account for every retained part. A shorter array
	// that drops bytes is a contradiction, not a silent truncation.
	for i := len(content); i < len(item.parts); i++ {
		if item.parts[i].end > item.partStart(i) {
			return ir.ProtocolError("conflicting message text")
		}
	}
	item.terminal = true
	return nil
}

// emitTextSuffix sends the retained bytes not yet streamed. Repeated terminal
// snapshots emit nothing new.
func (d *responsesToolDecoder) emitTextSuffix(item *responsesTextItem) error {
	if item == nil {
		return nil
	}
	if d.shouldHoldText(item) {
		return d.holdTextSuffix(item)
	}
	if len(item.parts) == 0 {
		return d.emitLooseTail(item, item.text.Len())
	}
	// Walk retained part boundaries so a recovered suffix keeps the part marker.
	for i, part := range item.parts {
		start := item.partStart(i)
		end := part.end
		if end < start || end > item.text.Len() {
			return ir.ProtocolError("conflicting message text")
		}
		if item.emitted < start {
			// Bytes before this part were not claimed by an earlier part. They keep
			// the loose marker recorded when they arrived.
			if err := d.emitLooseTail(item, start); err != nil {
				return err
			}
		}
		if item.emitted < end {
			if err := d.emitClaimedSpan(item, item.emitted, end, part.refusal); err != nil {
				return err
			}
		}
		if part.refusal && part.done && start == end && !item.emptyRefusalSeen(i) {
			if err := d.ensureMessageStart(); err != nil {
				return err
			}
			if err := d.emit(ir.StreamEvent{Kind: ir.EventTextDelta, Refusal: true}); err != nil {
				return err
			}
			d.noteItemWire(item)
			item.markEmptyRefusal(i)
		}
	}
	return d.emitLooseTail(item, item.text.Len())
}

// emitClaimedSpan emits [from, to) with the snapshot marker. The caller must
// already have claimed every loose byte in that span. A mismatch here is a
// missed claim and fails before the bytes are emitted under the wrong marker.
func (d *responsesToolDecoder) emitClaimedSpan(item *responsesTextItem, from, to int, refusal bool) error {
	if from >= to {
		return nil
	}
	if err := claimLooseRange(item, from, to, refusal); err != nil {
		return err
	}
	if err := d.ensureMessageStart(); err != nil {
		return err
	}
	if err := d.emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: item.text.String()[from:to], Refusal: refusal}); err != nil {
		return err
	}
	d.noteItemWire(item)
	d.noteTextOrder(item, item.text.String()[from:to], refusal)
	item.emitted = to
	return nil
}

// emitLooseTail emits retained bytes that no indexed part owns. Each byte must
// belong to one loose range. A gap, or a range boundary inside the tail, fails
// before any of those bytes are emitted unmarked.
func (d *responsesToolDecoder) emitLooseTail(item *responsesTextItem, limit int) error {
	if item.emitted >= limit {
		return nil
	}
	if err := d.ensureMessageStart(); err != nil {
		return err
	}
	text := item.text.String()
	for item.emitted < limit {
		refusal, ok := looseMarkerAt(item, item.emitted)
		if !ok {
			return ir.ProtocolError("conflicting message text")
		}
		end := limit
		for i := range item.loose {
			r := &item.loose[i]
			if item.emitted >= r.start && item.emitted < r.end {
				if r.end < end {
					end = r.end
				}
				break
			}
		}
		if end <= item.emitted {
			return ir.ProtocolError("conflicting message text")
		}
		frag := text[item.emitted:end]
		if err := d.emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: frag, Refusal: refusal}); err != nil {
			return err
		}
		d.noteItemWire(item)
		d.noteTextOrder(item, frag, refusal)
		item.emitted = end
	}
	return nil
}

func (item *responsesTextItem) emptyRefusalSeen(index int) bool {
	return index >= 0 && index < len(item.emptyRefusalEmitted) && item.emptyRefusalEmitted[index]
}

func textItemWillEmit(item *responsesTextItem) bool {
	if item == nil {
		return false
	}
	if item.emitted < item.text.Len() {
		return true
	}
	for i, part := range item.parts {
		if part.refusal && part.done && item.partStart(i) == part.end && !item.emptyRefusalSeen(i) {
			return true
		}
	}
	return false
}

func (item *responsesTextItem) markEmptyRefusal(index int) {
	if index < 0 || index >= maxTrackedTextParts {
		return
	}
	if len(item.emptyRefusalEmitted) <= index {
		next := make([]bool, index+1)
		copy(next, item.emptyRefusalEmitted)
		item.emptyRefusalEmitted = next
	}
	item.emptyRefusalEmitted[index] = true
}

// observeMessageAdded registers a streamed message identity before its deltas.
// The added frame is a placeholder; content arrives through deltas and the
// message done frame.
func (d *responsesToolDecoder) observeMessageAdded(env *streamEnvelope) error {
	if d.sawUnkeyedText || env.Item == nil {
		return nil
	}
	_, err := d.bindText(textIdentity{
		outputIndex: env.OutputIndex, hasIndex: env.OutputIndexSet, itemID: env.Item.ID,
	}, true, false)
	return err
}

// observeMessageDone applies a complete message snapshot from its lifecycle
// done frame. A missing content field is a placeholder, not a contradiction;
// the final snapshot stays the recovery point. An explicit content array is
// authoritative: it must extend the streamed bytes, account for every retained
// part, and it terminalizes the item.
func (d *responsesToolDecoder) observeMessageDone(env *streamEnvelope) error {
	if d.sawUnkeyedText || env.Item == nil {
		return nil
	}
	item, err := d.bindText(textIdentity{
		outputIndex: env.OutputIndex, hasIndex: env.OutputIndexSet, itemID: env.Item.ID,
	}, true, false)
	if err != nil {
		return err
	}
	if env.Item.Content == nil {
		return nil
	}
	if err := d.applyMessageContent(item, env.Item.Content); err != nil {
		return err
	}
	return d.flushClosedText(item)
}

// observeTextDone reconciles one content part's complete text by content_index.
// A later part's done is not compared against the full concatenation, and its
// missing suffix is recovered when it is the tail.
func (d *responsesToolDecoder) observeTextDone(env *streamEnvelope, refusal bool) error {
	if d.sawUnkeyedText || !env.TextSet {
		return nil
	}
	return d.observePartSnapshot(env, env.Text, refusal)
}

func (d *responsesToolDecoder) observeRefusalDone(env *streamEnvelope) error {
	if d.sawUnkeyedText || !env.RefusalSet {
		return nil
	}
	return d.observePartSnapshot(env, env.Refusal, true)
}

func (d *responsesToolDecoder) observePartSnapshot(env *streamEnvelope, snapshot string, refusal bool) error {
	item, err := d.bindText(textIdentity{
		outputIndex: env.OutputIndex, hasIndex: env.OutputIndexSet, itemID: env.ItemID,
	}, true, false)
	if err != nil {
		return err
	}
	index := 0
	if env.ContentIndexSet {
		index = env.ContentIndex
	}
	if err := d.reconcileTextPart(item, index, snapshot, refusal); err != nil {
		return err
	}
	return d.flushClosedText(item)
}

func (d *responsesToolDecoder) noteRefusalPart(env *streamEnvelope) error {
	item, err := d.bindText(textIdentity{
		outputIndex: env.OutputIndex, hasIndex: env.OutputIndexSet, itemID: env.ItemID,
	}, true, false)
	if err != nil {
		return err
	}
	if item.terminal || d.responseDone {
		return ir.ProtocolError("conflicting message text")
	}
	index := 0
	if env.ContentIndexSet {
		index = env.ContentIndex
	}
	if err := d.ensureTextParts(item, index); err != nil {
		return err
	}
	return d.notePartKind(item.parts[index], true)
}

// observePartDone reconciles one content_part.done frame whose part is a text
// part, keyed by actual content_index.
// observeContentPartAdded records a refusal part without closing it. An empty
// added part is placeholder metadata and is not terminal. Nonempty readable
// content uses the same bounded ordering queue as a text delta, so a later
// message cannot emit ahead of an unresolved earlier message.
func (d *responsesToolDecoder) observeContentPartAdded(env *streamEnvelope) error {
	if env.Part == nil || env.Part.Type != "refusal" {
		return nil
	}
	if !env.Part.RefusalSet && !env.Part.TextSet {
		return d.noteRefusalPart(env)
	}
	snapshot := ""
	if env.Part.RefusalSet {
		snapshot = env.Part.Refusal
	} else if env.Part.TextSet {
		snapshot = env.Part.Text
	}
	if snapshot == "" {
		// An explicit empty added part remains a placeholder. It records the kind
		// without inventing visible output or selecting a completed refusal.
		return d.noteRefusalPart(env)
	}
	item, err := d.bindText(textIdentity{
		outputIndex: env.OutputIndex, hasIndex: env.OutputIndexSet, itemID: env.ItemID,
	}, true, false)
	if err != nil {
		return err
	}
	if item.terminal || d.responseDone {
		return ir.ProtocolError("conflicting message text")
	}
	index := 0
	if env.ContentIndexSet {
		index = env.ContentIndex
	}
	if err := d.ensureTextParts(item, index); err != nil {
		return err
	}
	part := item.parts[index]
	if err := d.notePartKind(part, true); err != nil {
		return err
	}
	if part.done {
		return ir.ProtocolError("conflicting message text")
	}
	start := item.partStart(index)
	retained := ""
	if part.end >= start && part.end <= item.text.Len() {
		retained = item.text.String()[start:part.end]
	}
	if !strings.HasPrefix(snapshot, retained) {
		return ir.ProtocolError("conflicting message text")
	}
	extra := snapshot[len(retained):]
	if extra == "" {
		// A repeated added snapshot is metadata. It must not emit again or close
		// the part; a later delta can still extend it.
		return nil
	}
	if index != len(item.parts)-1 || part.end != item.text.Len() {
		return ir.ProtocolError("conflicting message text order")
	}
	// Budget and identity are decided before retention. A rejected added part
	// leaves neither the bytes nor a queued emission.
	open := d.firstOpenText()
	queued := item != open || d.shouldHoldText(item)
	cost := 0
	if queued {
		cost = len(extra) + textQueueEntryOverhead
		if err := d.checkPending(cost); err != nil {
			return err
		}
	}
	if err := d.retainText(len(extra)); err != nil {
		return err
	}
	item.text.WriteString(extra)
	part.end += len(extra)
	d.sawRefusal = true
	if queued {
		d.insertPending(pendingOrderWork{
			kind: pendingOrderText, textOwner: item, cost: cost,
			heldForItem: d.deferItemID(item.seq),
		})
		return nil
	}
	return d.emitTextSuffix(item)
}

func (d *responsesToolDecoder) observePartDone(env *streamEnvelope) error {
	if d.sawUnkeyedText || env.Part == nil {
		return nil
	}
	refusal := env.Part.Type == "refusal"
	if refusal {
		if !env.Part.TextSet && !env.Part.RefusalSet {
			return nil
		}
	} else if !env.Part.TextSet {
		return nil
	}
	if env.Part.Type != "" && env.Part.Type != "output_text" && env.Part.Type != "text" && !refusal {
		return nil
	}
	snapshot := env.Part.Text
	if refusal && env.Part.RefusalSet {
		snapshot = env.Part.Refusal
	}
	return d.observePartSnapshot(env, snapshot, refusal)
}

func (d *responsesToolDecoder) observeAdded(env *streamEnvelope) error {
	call, err := d.bind(toolIdentity{
		outputIndex: env.OutputIndex, hasIndex: env.OutputIndexSet,
		itemID: env.Item.ID, callID: env.Item.CallID, name: env.Item.Name,
	}, true, false)
	if err != nil {
		return err
	}
	// An empty added arguments field is the normal pending placeholder, not a
	// terminal empty snapshot. A later delta or done event supplies the bytes.
	if env.Item.Arguments == "" {
		return d.emitReady(call)
	}
	return d.reconcile(call, env.Item.Arguments, false)
}

func (d *responsesToolDecoder) observeDelta(env *streamEnvelope) error {
	if env.Delta == "" && env.ItemID == "" && env.Name == "" && !env.OutputIndexSet {
		return nil
	}
	call, err := d.bind(toolIdentity{
		outputIndex: env.OutputIndex, hasIndex: env.OutputIndexSet,
		itemID: env.ItemID, name: env.Name,
	}, true, false)
	if err != nil {
		return err
	}
	if env.Delta == "" {
		// A late item id with no new argument bytes must still release a withheld start.
		if d.awaitNativeItemID && call.itemID != "" && !call.started {
			return d.emitReady(call)
		}
		return nil
	}
	if call.terminal {
		return ir.ProtocolError("conflicting function call arguments")
	}
	if err := d.appendArgs(call, env.Delta); err != nil {
		return err
	}
	return d.emitReady(call)
}

func (d *responsesToolDecoder) observeArgumentsDone(env *streamEnvelope) error {
	call, err := d.bind(toolIdentity{
		outputIndex: env.OutputIndex, hasIndex: env.OutputIndexSet,
		itemID: env.ItemID, name: env.Name,
	}, true, false)
	if err != nil {
		return err
	}
	// args_done has no call_id. Mark the snapshot terminal immediately so a
	// later delta cannot extend it, but hold emission until identity arrives.
	return d.reconcile(call, env.Arguments, true)
}

func (d *responsesToolDecoder) observeItemDone(env *streamEnvelope) error {
	call, err := d.bind(toolIdentity{
		outputIndex: env.OutputIndex, hasIndex: env.OutputIndexSet,
		itemID: env.Item.ID, callID: env.Item.CallID, name: env.Item.Name,
	}, true, false)
	if err != nil {
		return err
	}
	return d.reconcile(call, env.Item.Arguments, true)
}

func (d *responsesToolDecoder) observeFinal(items []respOutputItem) error {
	// Apply every item before emitting. An ID-less final-only call cannot start
	// under the normal identity rule, so emitting inside the loop would let a
	// later identified item pass it and reorder the output array. Reasoning
	// snapshots are applied quietly in the same pass and emit in array order
	// during the second pass, so recovered deltas never jump ahead of an
	// interleaved final-only tool or message.
	type finalSlot struct {
		call      *responsesToolCall
		reasoning *responsesReasoningItem
		text      *responsesTextItem
	}
	slots := make([]finalSlot, len(items))
	for i := range items {
		item := &items[i]
		switch item.Type {
		case "function_call":
			call, err := d.matchFinal(i, item)
			if err != nil {
				return err
			}
			if err := d.applySnapshot(call, item.Arguments, true); err != nil {
				return err
			}
			slots[i].call = call
		case "reasoning":
			if d.reasoning == nil {
				continue
			}
			ri, err := d.reasoning.matchFinal(i, item)
			if err != nil {
				return err
			}
			if err := d.reasoning.applyFinal(ri, item); err != nil {
				return err
			}
			slots[i].reasoning = ri
		case "message":
			// Legacy unkeyed deltas cannot be attributed to one final message.
			// Keep the historical skip instead of guessing by text.
			if d.sawUnkeyedText {
				continue
			}
			full := finalMessageText(item)
			ti, err := d.matchTextFinal(i, item, full != "" || messageHasRefusal(item))
			if err != nil {
				return err
			}
			if ti == nil {
				continue
			}
			if item.Content != nil {
				if err := d.applyMessageContent(ti, item.Content); err != nil {
					return err
				}
			} else {
				ti.terminal = true
			}
			slots[i].text = ti
		}
	}
	// A final snapshot is the last chance for a native item id. Calls that
	// still lack one are proven absent and may use the synthetic fallback.
	if d.awaitNativeItemID {
		for _, call := range d.calls {
			if call.itemID == "" {
				call.itemIDAbsent = true
				call.awaitingItemID = false
			}
		}
	}
	// The terminal array pass emits in array order. bypassOrder stops a forced
	// emission from deferring again, and each slot flushes its own queued
	// streamed work first so a final-only item cannot overtake it.
	d.bypassOrder = true
	defer func() { d.bypassOrder = false }()
	for i := range items {
		willEmit := false
		switch {
		case slots[i].call != nil:
			willEmit = d.callWillEmit(slots[i].call)
		case slots[i].reasoning != nil:
			willEmit = d.reasonWillEmit(slots[i].reasoning)
		case slots[i].text != nil:
			willEmit = textItemWillEmit(slots[i].text)
		}
		if willEmit {
			if err := d.rejectReorderedEmission(i); err != nil {
				return err
			}
		}
		if call := slots[i].call; call != nil {
			if err := d.flushOrderedForCall(call); err != nil {
				return err
			}
			// Final-only calls are complete in this snapshot, even with an empty
			// call ID. Streamed calls still wait for a call ID unless EOF forces them.
			if err := d.emitCall(call, call.fromFinal); err != nil {
				return err
			}
		}
		if ti := slots[i].text; ti != nil {
			if err := d.emitTextSuffix(ti); err != nil {
				return err
			}
		}
		if ri := slots[i].reasoning; ri != nil {
			if err := d.flushOrderedForReason(ri); err != nil {
				return err
			}
			if err := d.reasoning.emitRecoveredDeltas(ri); err != nil {
				return err
			}
			if err := d.reasoning.emitFinal(ri); err != nil {
				return err
			}
		}
	}
	// Close after this snapshot so a final-only call in the first completion is
	// accepted. A later event cannot create a call or append content. An empty
	// output array is still terminal.
	d.responseDone = true
	for _, call := range d.calls {
		call.terminal = true
	}
	for _, ti := range d.texts {
		ti.terminal = true
	}
	if d.reasoning != nil {
		d.reasoning.markDone()
	}
	// Drain entries whose item had no terminal slot, so an unmatched streamed
	// item loses no content. Every message is closed, so nothing stays blocked.
	if err := d.releaseItemBarriers(); err != nil {
		return err
	}
	if err := d.drainOrdered(); err != nil {
		return err
	}
	if err := d.rejectEmittedFinalOrder(); err != nil {
		return err
	}
	// Terminal ordinals are metadata for the Chat observer. They are recorded
	// only after the native order check accepts this array.
	if d.observe != nil {
		for i := range slots {
			ev := ir.StreamEvent{TerminalSet: true, TerminalIndex: i}
			switch {
			case slots[i].call != nil:
				call := slots[i].call
				ev.Kind = ir.EventToolCallStart
				ev.ToolID = call.callID
				ev.ToolName = call.name
				ev.ResponsesItemID = call.itemID
				if call.hasIndex {
					ev.SourceOutputSet = true
					ev.SourceOutputIndex = call.outputIndex
				}
			case slots[i].reasoning != nil:
				item := slots[i].reasoning
				ev.Kind = ir.EventReasoningReserve
				ev.ReasoningID = item.id
				ev.ReasoningIndexSet = item.hasIndex
				ev.ReasoningIndex = item.outputIndex
			case slots[i].text != nil:
				item := slots[i].text
				ev.Kind = ir.EventTextDelta
				ev.ID = item.itemID
				if item.hasIndex {
					ev.SourceOutputSet = true
					ev.SourceOutputIndex = item.outputIndex
				}
			default:
				continue
			}
			d.observe(ev)
		}
	}
	return nil
}

// rejectReorderedEmission fails when a terminal slot at ordinal still has new
// ordering work and an item that already reached the consumer owns a later
// terminal array ordinal. The final array is authoritative, so such an order
// cannot be represented without silently reordering bytes already sent. The
// check spans text, tool, and reasoning because their terminal ordinals share
// one output-array order. An item with no emitted content owns no position yet
// and is skipped; consistent repeats add no new work and never reach here.
func (d *responsesToolDecoder) rejectReorderedEmission(ordinal int) error {
	for _, item := range d.texts {
		if !item.finalMatched || item.emitted == 0 {
			continue
		}
		if item.finalOrdinal > ordinal {
			return ir.ProtocolError("conflicting message text order")
		}
	}
	for _, call := range d.calls {
		if !call.finalMatched || !call.wired {
			continue
		}
		if call.finalOrdinal > ordinal {
			return ir.ProtocolError("conflicting function call order")
		}
	}
	if d.reasoning != nil {
		for _, item := range d.reasoning.items {
			if !item.finalMatched || !item.wired {
				continue
			}
			if item.finalOrdinal > ordinal {
				return ir.ProtocolError("conflicting reasoning item order")
			}
		}
	}
	return nil
}

// rejectEmittedFinalOrder fails when items already delivered disagree with the
// completed output array. The check runs even when those items have no new
// bytes, so a consistent stream is not silently kept against the final order.
func (d *responsesToolDecoder) rejectEmittedFinalOrder() error {
	if !d.awaitNativeItemID {
		return nil
	}
	type ranked struct {
		wire, ordinal int
		msg           string
	}
	var ranks []ranked
	for _, call := range d.calls {
		if !call.finalMatched || d.itemWire[call] == 0 {
			continue
		}
		ranks = append(ranks, ranked{d.itemWire[call], call.finalOrdinal, "conflicting function call order"})
	}
	for _, item := range d.texts {
		if !item.finalMatched || d.itemWire[item] == 0 {
			continue
		}
		ranks = append(ranks, ranked{d.itemWire[item], item.finalOrdinal, "conflicting message text order"})
	}
	if d.reasoning != nil {
		for _, item := range d.reasoning.items {
			if !item.finalMatched || d.itemWire[item] == 0 {
				continue
			}
			ranks = append(ranks, ranked{d.itemWire[item], item.finalOrdinal, "conflicting reasoning item order"})
		}
	}
	for i := range ranks {
		for j := range ranks {
			if ranks[i].wire < ranks[j].wire && ranks[i].ordinal > ranks[j].ordinal {
				return ir.ProtocolError(ranks[i].msg)
			}
		}
	}
	return nil
}

// callWillEmit reports that a terminal call slot still has bytes to send: an
// unwired start, an unemitted argument suffix, or work waiting in the queue.
func (d *responsesToolDecoder) callWillEmit(call *responsesToolCall) bool {
	if !call.wired || call.emitted < call.args.Len() {
		return true
	}
	for _, w := range d.textQueue {
		if w.kind == pendingOrderTool && w.call == call {
			return true
		}
	}
	return false
}

// reasonWillEmit reports that a terminal reasoning slot still has new output
// order to send. An unwired item, including a new opaque-only item, has no
// emitted position yet. An unemitted summary suffix or queued reasoning event
// is also new order. item.emitted is only the last completion snapshot, so a
// wired item whose readable parts are already emitted may receive late
// metadata without claiming a new array position.
func (d *responsesToolDecoder) reasonWillEmit(item *responsesReasoningItem) bool {
	if !item.wired {
		return true
	}
	for _, p := range item.parts {
		if p != nil && p.emitted < p.text.Len() {
			return true
		}
	}
	for _, w := range d.textQueue {
		if w.kind == pendingOrderReasoning && w.reason == item {
			return true
		}
	}
	return false
}

// matchFinal binds a completion item without guessing by name or argument text.// Item ID or call ID identifies a streamed call. An output_index equals this
// array ordinal only when that index has one streamed call; assign then fills
// identity or reports a nonempty ID conflict. Otherwise a new final-only call
// is safe only when every streamed call already has both IDs and at least one
// ID proves it is a different call. An index-only call, or a call that has only
// one ID, blocks creation. Final-only calls stay on byFinal, so repeated
// snapshots resolve the same ordinal and same-name items stay separate.
func (d *responsesToolDecoder) matchFinal(ordinal int, item *respOutputItem) (*responsesToolCall, error) {
	if call := d.byFinal[ordinal]; call != nil {
		if err := d.assign(call, toolIdentity{itemID: item.ID, callID: item.CallID, name: item.Name}); err != nil {
			return nil, err
		}
		return call, d.claimFinal(call, ordinal)
	}
	if item.ID != "" || item.CallID != "" {
		call, err := d.bind(toolIdentity{itemID: item.ID, callID: item.CallID}, false, false)
		if err != nil {
			return nil, err
		}
		if call != nil {
			if err := d.assign(call, toolIdentity{itemID: item.ID, callID: item.CallID, name: item.Name}); err != nil {
				return nil, err
			}
			return call, d.claimFinal(call, ordinal)
		}
		if call := d.uniqueIndexMatch(ordinal); call != nil {
			if err := d.assign(call, toolIdentity{itemID: item.ID, callID: item.CallID, name: item.Name}); err != nil {
				return nil, err
			}
			return call, d.claimFinal(call, ordinal)
		}
		if d.blocksNewFinal(item) {
			return nil, ir.ProtocolError("conflicting function call identity")
		}
		return d.bind(toolIdentity{
			itemID: item.ID, callID: item.CallID, name: item.Name, finalOrdinal: ordinal,
		}, true, true)
	}
	if call := d.uniqueIndexMatch(ordinal); call != nil {
		if err := d.assign(call, toolIdentity{name: item.Name}); err != nil {
			return nil, err
		}
		return call, d.claimFinal(call, ordinal)
	}
	if d.hasStreamedCall() {
		return nil, ir.ProtocolError("conflicting function call identity")
	}
	return d.bind(toolIdentity{
		name: item.Name, finalOrdinal: ordinal,
	}, true, true)
}

// claimFinal records one terminal array position for a call. A second,
// different ordinal for the same call is a provider contradiction.
func (d *responsesToolDecoder) claimFinal(call *responsesToolCall, ordinal int) error {
	if call.finalMatched && call.finalOrdinal != ordinal {
		return ir.ProtocolError("conflicting function call identity")
	}
	call.finalMatched = true
	call.finalOrdinal = ordinal
	return nil
}

// blocksNewFinal reports that a streamed call cannot be proved distinct from
// item. A call with both IDs is distinct when either ID differs. A call missing
// either ID can still be this item, so creating another call would be a guess.
func (d *responsesToolDecoder) blocksNewFinal(item *respOutputItem) bool {
	for _, call := range d.calls {
		if call.fromFinal {
			continue
		}
		if call.itemID != "" && call.callID != "" && finalDistinct(call, item) {
			continue
		}
		return true
	}
	return false
}

func finalDistinct(call *responsesToolCall, item *respOutputItem) bool {
	if item.ID != "" && call.itemID != "" && item.ID != call.itemID {
		return true
	}
	if item.CallID != "" && call.callID != "" && item.CallID != call.callID {
		return true
	}
	return false
}

func (d *responsesToolDecoder) hasStreamedCall() bool {
	for _, call := range d.calls {
		if !call.fromFinal {
			return true
		}
	}
	return false
}

// uniqueIndexMatch returns the sole streamed call whose output_index equals
// ordinal. Equality with the completion array position is not assumed for any
// other call, and a final-only ordinal is never an output_index.
func (d *responsesToolDecoder) uniqueIndexMatch(ordinal int) *responsesToolCall {
	var found *responsesToolCall
	for _, call := range d.calls {
		if call.fromFinal || !call.hasIndex || call.outputIndex != ordinal {
			continue
		}
		if found != nil {
			return nil
		}
		found = call
	}
	return found
}

// flushPending releases every call that never started, including a zero-length
// or ID-less call. EOF does not invent a call ID, item ID, or JSON object.
// Already started calls are left unchanged.
func (d *responsesToolDecoder) flushPending() error {
	for _, call := range d.calls {
		if call.started {
			continue
		}
		if call.itemID == "" {
			call.itemIDAbsent = true
		}
		if err := d.emitCall(call, true); err != nil {
			return err
		}
	}
	return nil
}

type toolIdentity struct {
	outputIndex  int
	hasIndex     bool
	itemID       string
	callID       string
	name         string
	finalOrdinal int
}

// bind looks up a streamed call by output index, item ID, and call ID. create
// allocates only when no key matches. After the response is terminal, create is
// refused so a new delta or a repeated completion cannot add a call. fromFinal
// keeps the new call out of the streamed output_index map.
func (d *responsesToolDecoder) bind(id toolIdentity, create, fromFinal bool) (*responsesToolCall, error) {
	var found *responsesToolCall
	match := func(call *responsesToolCall) error {
		if call == nil {
			return nil
		}
		if found == nil {
			found = call
			return nil
		}
		if found != call {
			return ir.ProtocolError("conflicting function call identity")
		}
		return nil
	}
	if id.hasIndex {
		if err := match(d.byIndex[id.outputIndex]); err != nil {
			return nil, err
		}
	}
	if id.itemID != "" {
		if err := match(d.byItem[id.itemID]); err != nil {
			return nil, err
		}
	}
	if id.callID != "" {
		if err := match(d.byCall[id.callID]); err != nil {
			return nil, err
		}
	}
	if found == nil {
		if !create {
			return nil, nil
		}
		// The response flag blocks creation here, not only on calls that already
		// exist. Identity fill of a known call uses create=false or matches above.
		if d.responseDone {
			return nil, ir.ProtocolError("conflicting function call identity")
		}
		if len(d.calls) >= d.maxCalls {
			return nil, ir.ProtocolError("too many function calls")
		}
		found = &responsesToolCall{index: d.nextIndex, fromFinal: fromFinal, orderMsgs: len(d.texts), seq: d.allocSeq()}
		d.nextIndex++
		d.calls = append(d.calls, found)
		if fromFinal {
			found.finalMatched = true
			found.finalOrdinal = id.finalOrdinal
			if d.byFinal == nil {
				d.byFinal = map[int]*responsesToolCall{}
			}
			d.byFinal[id.finalOrdinal] = found
		}
	}
	if err := d.assign(found, id); err != nil {
		return nil, err
	}
	return found, nil
}

func (d *responsesToolDecoder) assign(call *responsesToolCall, id toolIdentity) error {
	if id.hasIndex {
		if call.hasIndex && call.outputIndex != id.outputIndex {
			return ir.ProtocolError("conflicting function call index")
		}
		if other := d.byIndex[id.outputIndex]; other != nil && other != call {
			return ir.ProtocolError("conflicting function call index")
		}
	}
	if id.itemID != "" {
		if call.itemID != "" && call.itemID != id.itemID {
			return ir.ProtocolError("conflicting function call item")
		}
		if other := d.byItem[id.itemID]; other != nil && other != call {
			return ir.ProtocolError("conflicting function call item")
		}
	}
	if id.callID != "" {
		if call.callID != "" && call.callID != id.callID {
			return ir.ProtocolError("conflicting function call id")
		}
		if other := d.byCall[id.callID]; other != nil && other != call {
			return ir.ProtocolError("conflicting function call id")
		}
	}
	if id.name != "" && call.name != "" && call.name != id.name {
		return ir.ProtocolError("conflicting function call name")
	}
	if d.byIndex == nil {
		d.byIndex = map[int]*responsesToolCall{}
		d.byItem = map[string]*responsesToolCall{}
		d.byCall = map[string]*responsesToolCall{}
	}
	if id.hasIndex && !call.hasIndex {
		call.hasIndex = true
		call.outputIndex = id.outputIndex
		d.byIndex[id.outputIndex] = call
	}
	if id.itemID != "" && call.itemID == "" {
		call.itemID = id.itemID
		d.byItem[id.itemID] = call
	}
	if id.callID != "" && call.callID == "" {
		call.callID = id.callID
		d.byCall[id.callID] = call
	}
	if id.name != "" && call.name == "" {
		call.name = id.name
	}
	return nil
}

func (d *responsesToolDecoder) appendArgs(call *responsesToolCall, frag string) error {
	if frag == "" {
		return nil
	}
	if err := d.retain(len(frag)); err != nil {
		return err
	}
	call.args.WriteString(frag)
	return nil
}

// emitReady sends a tool start and any unsent argument suffix. A call stays
// buffered until it has a name and call ID, unless EOF forces release. EOF does
// not invent the missing identity. Every tool event follows EventMessageStart.
// A call registered after an unresolved message is deferred by orderEmitTool;
// its started/emitted bookkeeping still advances so a release never repeats it.
func (d *responsesToolDecoder) emitReady(call *responsesToolCall) error {
	return d.emitCall(call, false)
}

// orderEmitTool emits a tool event now, or queues it behind an earlier open
// message. bypassOrder is set by the terminal array pass, where the caller
// already chose the position. A live emission marks the call wired.
func (d *responsesToolDecoder) orderEmitTool(call *responsesToolCall, ev ir.StreamEvent) error {
	if !d.bypassOrder && call != nil && d.messagePrecedes(call.orderMsgs) {
		return d.enqueueOrder(pendingOrderWork{
			kind: pendingOrderTool, call: call, event: ev,
			cost: pendingOrderEventBytes(ev),
		})
	}
	if call != nil {
		call.wired = true
		d.noteItemWire(call)
	}
	return d.emit(ev)
}

// messagePrecedes reports that the earliest open message was registered before
// orderMsgs, so work at that position must wait for the message to close.
func (d *responsesToolDecoder) messagePrecedes(orderMsgs int) bool {
	barrier := d.firstOpenText()
	if barrier == nil {
		return false
	}
	idx, ok := d.textIndex(barrier)
	return ok && idx < orderMsgs
}

// An added-only reasoning placeholder must not close a preceding function
// call while its argument fragments still arrive.
func (d *responsesToolDecoder) canOpenCodexReasoningEarly(item *responsesReasoningItem) bool {
	for _, call := range d.calls {
		if !call.terminal && (!call.hasIndex || call.outputIndex < item.outputIndex) {
			return false
		}
	}
	return true
}

// orderEmitReasoning is the reasoning decoder hook. It defers one reasoning
// item's event behind a message registered after the item, using the same
// bounded queue and the same pending budget as tool and text work.
func (d *responsesToolDecoder) orderEmitReasoning(item *responsesReasoningItem, ev ir.StreamEvent) error {
	seq := d.reasonOrdinal(item)
	if !d.bypassOrder && d.deferItemID(seq) {
		return d.enqueueOrder(pendingOrderWork{
			kind: pendingOrderReasoning, reason: item, event: ev,
			cost: pendingOrderEventBytes(ev), heldForItem: true,
		})
	}
	if !d.bypassOrder && item != nil && d.messagePrecedes(item.orderMsgs) {
		return d.enqueueOrder(pendingOrderWork{
			kind: pendingOrderReasoning, reason: item, event: ev,
			cost: pendingOrderEventBytes(ev),
		})
	}
	if item != nil {
		item.wired = true
		d.noteItemWire(item)
	}
	return d.emit(ev)
}

func (d *responsesToolDecoder) allocSeq() int {
	seq := d.nextSeq
	d.nextSeq++
	return seq
}

func (d *responsesToolDecoder) findSeq(hasIndex bool, index int, id string) (int, bool) {
	if hasIndex && d.seqByOutput != nil {
		if seq, ok := d.seqByOutput[index]; ok {
			return seq, true
		}
	}
	if id != "" && d.seqByID != nil {
		if seq, ok := d.seqByID[id]; ok {
			return seq, true
		}
	}
	return 0, false
}

func (d *responsesToolDecoder) rememberSeq(seq int, hasIndex bool, index int, id string) {
	if hasIndex {
		if d.seqByOutput == nil {
			d.seqByOutput = map[int]int{}
		}
		if _, ok := d.seqByOutput[index]; !ok {
			d.seqByOutput[index] = seq
		}
	}
	if id != "" {
		if d.seqByID == nil {
			d.seqByID = map[string]int{}
		}
		if _, ok := d.seqByID[id]; !ok {
			d.seqByID[id] = seq
		}
	}
}

// noteToolOrder reports an accepted tool source index to the Chat observer.
// The emitted start event is unchanged.
func (d *responsesToolDecoder) noteToolOrder(call *responsesToolCall) {
	if d == nil || d.observe == nil || call == nil || !call.hasIndex {
		return
	}
	d.observe(ir.StreamEvent{
		Kind:              ir.EventToolCallStart,
		ToolID:            call.callID,
		ToolName:          call.name,
		ResponsesItemID:   call.itemID,
		SourceOutputSet:   true,
		SourceOutputIndex: call.outputIndex,
	})
}

// noteTextOrder reports an accepted text source index to the Chat observer.
// Refusal bytes stay out of the layout. The emitted delta is unchanged.
func (d *responsesToolDecoder) noteTextOrder(item *responsesTextItem, text string, refusal bool) {
	if d == nil || d.observe == nil || item == nil || refusal || text == "" || !item.hasIndex {
		return
	}
	d.observe(ir.StreamEvent{
		Kind:              ir.EventTextDelta,
		ID:                item.itemID,
		Text:              text,
		SourceOutputSet:   true,
		SourceOutputIndex: item.outputIndex,
	})
}

// noteReasonBound reserves the stream position of a reasoning item after that
// decoder has accepted it. added and done frames bind only the nested item id.
// Summary frames bind the envelope item_id, then the nested id. A repeated bind
// reuses this item's sequence and does not store any other id. A final-only
// item does not take a live sequence; the terminal pass emits it by array slot.
func (d *responsesToolDecoder) noteReasonBound(item *responsesReasoningItem) {
	if !d.awaitNativeItemID || item == nil || item.fromFinal {
		return
	}
	var seq int
	ok := false
	if d.reasonSeq != nil {
		seq, ok = d.reasonSeq[item]
	}
	if !ok {
		seq = d.allocSeq()
		if d.reasonSeq == nil {
			d.reasonSeq = map[*responsesReasoningItem]int{}
		}
		d.reasonSeq[item] = seq
	}
	d.rememberSeq(seq, item.hasIndex, item.outputIndex, item.id)
}

func (d *responsesToolDecoder) reasonOrdinal(item *responsesReasoningItem) int {
	if item == nil {
		return d.nextSeq
	}
	if d.reasonSeq != nil {
		if seq, ok := d.reasonSeq[item]; ok {
			return seq
		}
	}
	seq, ok := d.findSeq(item.hasIndex, item.outputIndex, item.id)
	if !ok {
		seq = d.allocSeq()
	}
	if d.reasonSeq == nil {
		d.reasonSeq = map[*responsesReasoningItem]int{}
	}
	d.reasonSeq[item] = seq
	d.rememberSeq(seq, item.hasIndex, item.outputIndex, item.id)
	return seq
}

// deferItemID reports that an unresolved tool barrier occupies this seq or an
// earlier one. An item registered before that barrier is not deferred.
func (d *responsesToolDecoder) deferItemID(seq int) bool {
	if !d.awaitNativeItemID || d.drainingOrder || d.bypassOrder {
		return false
	}
	for _, w := range d.textQueue {
		if w.kind == pendingOrderItemBarrier && w.call != nil && w.call.seq <= seq {
			return true
		}
	}
	return false
}

func (d *responsesToolDecoder) shouldHoldText(item *responsesTextItem) bool {
	if item == nil {
		return false
	}
	return d.deferItemID(item.seq)
}

func (d *responsesToolDecoder) itemQueueStrict() bool {
	if !d.awaitNativeItemID {
		return false
	}
	for _, w := range d.textQueue {
		if w.kind == pendingOrderItemBarrier || w.heldForItem {
			return true
		}
	}
	return false
}

func (d *responsesToolDecoder) noteItemWire(key any) {
	// Only the Grok Build final-order check reads these ranks.
	if !d.awaitNativeItemID || key == nil {
		return
	}
	if d.itemWire == nil {
		d.itemWire = map[any]int{}
	}
	if d.itemWire[key] != 0 {
		return
	}
	d.wireClock++
	d.itemWire[key] = d.wireClock
}

func (d *responsesToolDecoder) noteAwaitingItem(call *responsesToolCall) error {
	if call == nil || call.awaitingItemID {
		return nil
	}
	cost := textQueueEntryOverhead
	if err := d.checkPending(cost); err != nil {
		return err
	}
	call.awaitingItemID = true
	d.insertPending(pendingOrderWork{
		kind: pendingOrderItemBarrier, call: call, cost: cost, heldForItem: true,
	})
	return nil
}

func (d *responsesToolDecoder) enqueueRaw(ev ir.StreamEvent) error {
	return d.enqueueOrder(pendingOrderWork{
		kind: pendingOrderText, event: ev, cost: pendingOrderEventBytes(ev),
		heldForItem: true, raw: true,
	})
}

func (d *responsesToolDecoder) hasQueuedText(item *responsesTextItem) bool {
	for _, w := range d.textQueue {
		if w.kind == pendingOrderText && w.textOwner == item {
			return true
		}
	}
	return false
}

func (d *responsesToolDecoder) holdTextSuffix(item *responsesTextItem) error {
	if item == nil || !textItemWillEmit(item) || d.hasQueuedText(item) {
		return nil
	}
	cost := item.text.Len() - item.emitted + textQueueEntryOverhead
	if cost < textQueueEntryOverhead {
		cost = textQueueEntryOverhead
	}
	if err := d.checkPending(cost); err != nil {
		return err
	}
	d.insertPending(pendingOrderWork{
		kind: pendingOrderText, textOwner: item, cost: cost, heldForItem: true,
	})
	return nil
}

func (d *responsesToolDecoder) barrierIndex(call *responsesToolCall) int {
	for i, w := range d.textQueue {
		if w.kind == pendingOrderItemBarrier && w.call == call {
			return i
		}
	}
	return -1
}

func (d *responsesToolDecoder) removeBarrierAt(idx int) {
	if idx < 0 || idx >= len(d.textQueue) {
		return
	}
	d.releaseOrderCost(d.textQueue[idx])
	d.textQueue = append(d.textQueue[:idx], d.textQueue[idx+1:]...)
	d.resetEmptyQueue()
}

func (d *responsesToolDecoder) removeBarrier(call *responsesToolCall) {
	d.removeBarrierAt(d.barrierIndex(call))
}

func (d *responsesToolDecoder) checkPendingDelta(byteDelta, eventDelta int) error {
	if eventDelta > 0 && d.textQueueEvents > d.maxPendingEvents-eventDelta {
		return ir.ProtocolError("pending message text exceeded limit")
	}
	if byteDelta > 0 && byteDelta > d.maxPendingBytes-d.queueBytes {
		return ir.ProtocolError("pending message text exceeded limit")
	}
	return nil
}

func (d *responsesToolDecoder) replaceBarrier(idx int, call *responsesToolCall, events []ir.StreamEvent) error {
	if idx < 0 || idx >= len(d.textQueue) {
		return nil
	}
	old := d.textQueue[idx]
	works := make([]pendingOrderWork, len(events))
	addBytes := 0
	for i, ev := range events {
		cost := pendingOrderEventBytes(ev)
		addBytes += cost
		works[i] = pendingOrderWork{
			kind: pendingOrderTool, call: call, event: ev, cost: cost, heldForItem: true,
		}
	}
	if err := d.checkPendingDelta(addBytes-old.cost, len(events)-1); err != nil {
		return err
	}
	d.releaseOrderCost(old)
	tail := append([]pendingOrderWork(nil), d.textQueue[idx+1:]...)
	d.textQueue = d.textQueue[:idx]
	for _, w := range works {
		d.appendPending(w)
	}
	d.textQueue = append(d.textQueue, tail...)
	d.resetEmptyQueue()
	return nil
}

func (d *responsesToolDecoder) appendHeld(call *responsesToolCall, events []ir.StreamEvent) error {
	works := make([]pendingOrderWork, 0, len(events))
	addBytes := 0
	for _, ev := range events {
		cost := pendingOrderEventBytes(ev)
		addBytes += cost
		works = append(works, pendingOrderWork{
			kind: pendingOrderTool, call: call, event: ev, cost: cost, heldForItem: true,
		})
	}
	if err := d.checkPendingDelta(addBytes, len(works)); err != nil {
		return err
	}
	for _, w := range works {
		d.insertPending(w)
	}
	return nil
}

func (d *responsesToolDecoder) blockedByEarlierBarrier(call *responsesToolCall) bool {
	for _, w := range d.textQueue {
		if w.kind == pendingOrderItemBarrier && w.call != nil && w.call != call && w.call.seq < call.seq {
			return true
		}
	}
	return false
}

func (d *responsesToolDecoder) canEmitCallNow(call *responsesToolCall, idx int) bool {
	if d.messagePrecedes(call.orderMsgs) || d.blockedByEarlierBarrier(call) {
		return false
	}
	if idx > 0 {
		return false
	}
	if idx < 0 && d.itemQueueStrict() {
		return false
	}
	return true
}

func (d *responsesToolDecoder) pendingCallEvents(call *responsesToolCall) []ir.StreamEvent {
	var events []ir.StreamEvent
	if !call.started {
		events = append(events, ir.StreamEvent{
			Kind: ir.EventToolCallStart, Index: call.index,
			ToolID: call.callID, ToolName: call.name, ResponsesItemID: call.itemID,
		})
	}
	if call.emitted < call.args.Len() {
		events = append(events, ir.StreamEvent{
			Kind: ir.EventToolCallDelta, Index: call.index,
			ArgsFrag:        call.args.String()[call.emitted:],
			ResponsesItemID: call.itemID,
		})
	}
	return events
}

func (d *responsesToolDecoder) commitCallEvents(call *responsesToolCall) {
	if !call.started {
		call.started = true
		d.sawTool = true
	}
	call.emitted = call.args.Len()
}

func (d *responsesToolDecoder) emitEvents(call *responsesToolCall, events []ir.StreamEvent) error {
	for _, ev := range events {
		if err := d.emitObserved(call, ev); err != nil {
			return err
		}
		if call != nil {
			call.wired = true
			d.noteItemWire(call)
		}
	}
	return nil
}

// emitObserved emits one client event and, for a tool start, tells the Chat
// observer the accepted source index. The emitted event is unchanged.
func (d *responsesToolDecoder) emitObserved(call *responsesToolCall, ev ir.StreamEvent) error {
	if err := d.emit(ev); err != nil {
		return err
	}
	if ev.Kind == ir.EventToolCallStart {
		d.noteToolOrder(call)
	}
	return nil
}

// placeCallEvents inserts a ready call at its barrier. A front call is delivered
// now. A call behind an earlier barrier or an open message stays there, so a
// later id cannot pass it.
func (d *responsesToolDecoder) placeCallEvents(call *responsesToolCall, events []ir.StreamEvent) error {
	if d.bypassOrder {
		d.removeBarrier(call)
		return d.emitEvents(call, events)
	}
	if err := d.drainOrdered(); err != nil {
		return err
	}
	idx := d.barrierIndex(call)
	if d.canEmitCallNow(call, idx) {
		if idx >= 0 {
			d.removeBarrierAt(idx)
		}
		return d.emitEvents(call, events)
	}
	if idx >= 0 {
		return d.replaceBarrier(idx, call, events)
	}
	if len(events) == 0 {
		return nil
	}
	if d.messagePrecedes(call.orderMsgs) && !d.deferItemID(call.seq) && !d.itemQueueStrict() {
		for _, ev := range events {
			if err := d.orderEmitTool(call, ev); err != nil {
				return err
			}
		}
		return nil
	}
	return d.appendHeld(call, events)
}

// releaseItemBarriers turns resolved barriers into queued events at the same
// position. EOF and the terminal drain use it so a missing id still emits in
// stream order, with an empty item id for the synthetic fallback.
func (d *responsesToolDecoder) releaseItemBarriers() error {
	for {
		idx := -1
		var call *responsesToolCall
		for i, w := range d.textQueue {
			if w.kind != pendingOrderItemBarrier || w.call == nil {
				continue
			}
			if w.call.awaitingItemID && w.call.itemID == "" && !w.call.itemIDAbsent {
				continue
			}
			idx = i
			call = w.call
			break
		}
		if call == nil {
			return nil
		}
		if call.itemID == "" {
			call.itemIDAbsent = true
		}
		call.awaitingItemID = false
		// A call that still has no name or call id is not emitted here. Dropping
		// the barrier lets later queued work drain; EOF force-emits the call.
		if call.name == "" || call.callID == "" {
			d.removeBarrierAt(idx)
			continue
		}
		events := d.pendingCallEvents(call)
		if err := d.replaceBarrier(idx, call, events); err != nil {
			return err
		}
		d.commitCallEvents(call)
	}
}

func (d *responsesToolDecoder) emitCall(call *responsesToolCall, force bool) error {
	// Grok Build waits for a native item id. Emitting a start without one lets
	// the encoder publish a synthetic id that a later frame cannot rename.
	if d.awaitNativeItemID && call.itemID == "" && !force && !call.itemIDAbsent {
		return d.noteAwaitingItem(call)
	}
	if !force && (call.name == "" || call.callID == "") {
		return nil
	}
	call.awaitingItemID = false
	if err := d.ensureMessageStart(); err != nil {
		return err
	}
	events := d.pendingCallEvents(call)
	d.commitCallEvents(call)
	if err := d.placeCallEvents(call, events); err != nil {
		return err
	}
	if d.bypassOrder {
		return nil
	}
	return d.drainOrdered()
}

func (d *responsesToolDecoder) ensureMessageStart() error {
	if d.ensureStarted == nil {
		return nil
	}
	return d.ensureStarted()
}

// rejectContent fails a nonempty text or reasoning delta after the response
// has completed or become incomplete. Empty lifecycle metadata is ignored.
// The error carries no payload.
func (d *responsesToolDecoder) rejectContent(delta string) error {
	if delta != "" && d.responseDone {
		return ir.ProtocolError("conflicting response content")
	}
	return nil
}

func (d *responsesToolDecoder) reconcile(call *responsesToolCall, snapshot string, terminal bool) error {
	if err := d.applySnapshot(call, snapshot, terminal); err != nil {
		return err
	}
	return d.emitReady(call)
}

// applySnapshot records snapshot growth without emitting. An identical snapshot
// adds nothing. A terminal call rejects any suffix, including a repeated
// completion that changes a known call.
func (d *responsesToolDecoder) applySnapshot(call *responsesToolCall, snapshot string, terminal bool) error {
	retained := call.args.String()
	var suffix string
	switch {
	case retained == snapshot:
		// An identical snapshot, including a legitimate empty call, adds nothing.
	case strings.HasPrefix(snapshot, retained):
		suffix = snapshot[len(retained):]
	default:
		return ir.ProtocolError("conflicting function call arguments")
	}
	if call.terminal && suffix != "" {
		return ir.ProtocolError("conflicting function call arguments")
	}
	if err := d.appendArgs(call, suffix); err != nil {
		return err
	}
	if terminal {
		call.terminal = true
	}
	return nil
}

func (d *responsesToolDecoder) retain(n int) error {
	if n <= 0 {
		return nil
	}
	if n > d.maxArgBytes-d.argBytes {
		return ir.ProtocolError("function call arguments too large")
	}
	d.argBytes += n
	return nil
}

// streamFailureFrom builds a StreamFailure from a top-level error object and/or
// a response payload. Parsed fields only; never embeds raw JSON.
func streamFailureFrom(errObj *respErrorObject, resp *respObject) *ir.StreamFailure {
	sf := &ir.StreamFailure{}
	if errObj != nil {
		sf.Type = errObj.Type
		sf.Code = errObj.Code
		sf.Message = errObj.Message
	}
	if resp != nil && resp.Error != nil {
		if sf.Type == "" {
			sf.Type = resp.Error.Type
		}
		if sf.Code == "" {
			sf.Code = resp.Error.Code
		}
		if sf.Message == "" {
			sf.Message = resp.Error.Message
		}
	}
	if sf.Type == "" && sf.Code == "" && sf.Message == "" {
		return nil
	}
	if sf.Message == "" {
		sf.Message = "upstream response failed"
	}
	return sf
}

const (
	openNone = iota
	openMessage
	openReasoning
	openFunction
)

// StreamEncoder renders IR stream events as an OpenAI Responses SSE event
// sequence. It buffers text and tool arguments so it can emit the matching
// terminal (.done) events and a final response.completed snapshot, which the
// flat backend deltas do not provide.
type StreamEncoder struct {
	model       string
	id          string
	seq         int
	inputTokens int
	usageOut    int
	cacheRead   int
	cacheWrite  int

	// createdAt is the Unix seconds shared by every synthesized response
	// object in this stream. Zero until the first positive upstream value or
	// the fallback clock is locked with the first response object.
	createdAt      int64
	createdEmitted bool
	finishEmitted  bool

	outputIndex int
	open        int
	openItemID  string
	openOutIdx  int

	// openParts records each content part of the open message in order. A marker
	// change closes the current part and opens the next; earlier bytes stay in
	// their original part. Pointers keep each strings.Builder stable across append.
	openParts []*respOpenPart
	partIdx   int

	// Structured Responses reasoning state. reasoningOpen is the item currently
	// emitting summary frames; reasoningOpenPart is its open summary part.
	// reasoningDeferred queues items whose summary bytes were retained before
	// their identity was known; they replay in arrival order. The max fields
	// mirror the package constants and exist so tests can inject small limits.
	reasoningItems    map[string]*respReasoningItem
	reasoningOpen     *respReasoningItem
	reasoningOpenPart int
	reasoningDeferred []*respReasoningItem
	reasoningSeq      int
	reasoningBytes    int
	reasoningParts    int
	maxReasoningBytes int
	maxReasoningItems int
	maxReasoningParts int

	// pendingEvents queues later work behind a deferred reasoning item whose
	// source ID has not arrived, so a generated wire ID is never emitted while
	// a source ID is still possible. pendingBytes charges payloads and
	// bookkeeping against the same pending budget used elsewhere. flushing
	// disables deferral and the queue at finish/close, where generation is the
	// only remaining option.
	pendingEvents []ir.StreamEvent
	pendingBytes  int
	flushing      bool

	// reconcileCodex is the narrow Codex-to-Responses policy. A reasoning
	// completion without explicit encrypted bytes is held open instead of
	// closed, and later text, tools, or another reasoning item wait in
	// pendingEvents, so the terminal snapshot can still supply the bytes.
	reconcileCodex bool

	tools      map[int]*respTool
	pendingLen int

	items []map[string]any // completed output items, for response.completed
}

// respTool is one IR-index function call. itemID is empty until
// output_item.added has been written; identity may arrive across repeated
// Starts and argument fragments may precede the first Start.
type respOpenPart struct {
	refusal bool
	text    strings.Builder
	closed  bool
}

type respTool struct {
	itemID     string
	sourceItem string
	outIdx     int
	callID     string
	name       string
	pending    string
	args       strings.Builder
	done       bool
}

// NewCodexStreamEncoder pairs with DecodeStreamCodexReconciled for a
// translated Codex stream on Responses ingress.
func NewCodexStreamEncoder(model string) *StreamEncoder {
	e := NewStreamEncoder(model)
	e.reconcileCodex = true
	return e
}

func NewStreamEncoder(model string) *StreamEncoder {
	return &StreamEncoder{
		model: model, open: openNone, tools: map[int]*respTool{},
		reasoningItems: map[string]*respReasoningItem{}, reasoningOpenPart: -1,
		maxReasoningBytes: maxEncoderReasoningBytes,
		maxReasoningItems: maxEncoderReasoningItems,
		maxReasoningParts: maxEncoderReasoningParts,
	}
}

// maxPendingArgs bounds fragments buffered for an index whose Start has not
// arrived yet, so a stream that never resolves identity cannot retain
// unbounded bytes.
const maxPendingArgs = 1 << 20

func (e *StreamEncoder) applyUsage(ev ir.StreamEvent) {
	if ev.InputTokens != 0 {
		e.inputTokens = ev.InputTokens
	}
	if ev.CacheReadTokens != 0 {
		e.cacheRead = ev.CacheReadTokens
	}
	if ev.CacheWriteTokens != 0 {
		e.cacheWrite = ev.CacheWriteTokens
	}
}

func (e *StreamEncoder) emit(w *sse.Writer, name string, data map[string]any) error {
	data["type"] = name
	data["sequence_number"] = e.seq
	e.seq++
	raw := mustJSON(data)
	return w.WriteEvent(name, raw)
}

// responsesCreatedAt returns one positive Unix-second timestamp. A positive
// upstream value is kept. Zero and negative are unspecified, so the wall clock
// is used. A caller that emits more than one object must store the result.
func responsesCreatedAt(upstream int64) int64 {
	if upstream > 0 {
		return upstream
	}
	return time.Now().Unix()
}

// noteCreatedAt records a positive upstream timestamp before the first response
// object is emitted. Zero does not clear a saved value. A value that arrives
// after the first object is ignored so every envelope shares one time.
func (e *StreamEncoder) noteCreatedAt(upstream int64) {
	if e.createdEmitted || upstream <= 0 || e.createdAt > 0 {
		return
	}
	e.createdAt = upstream
}

func (e *StreamEncoder) responseObj(status string, output []map[string]any, withUsage bool) map[string]any {
	out := any([]any{})
	if output != nil {
		out = output
	}
	if e.createdAt <= 0 {
		e.createdAt = responsesCreatedAt(0)
	}
	obj := map[string]any{
		"id":         e.id,
		"object":     "response",
		"created_at": e.createdAt,
		"status":     status,
		"model":      e.model,
		"output":     out,
	}
	if withUsage {
		obj["usage"] = usageToWire(ir.Usage{
			InputTokens:      e.inputTokens,
			OutputTokens:     e.usageOut,
			CacheReadTokens:  e.cacheRead,
			CacheWriteTokens: e.cacheWrite,
		})
	}
	return obj
}

func (e *StreamEncoder) ensureCreated(w *sse.Writer) error {
	if e.createdEmitted {
		return nil
	}
	if e.id == "" {
		e.id = ir.NewID("resp_")
	}
	// Lock the timestamp before the first object. Later envelopes, including
	// completed, incomplete, and Close, must reuse this value.
	if e.createdAt <= 0 {
		e.createdAt = responsesCreatedAt(0)
	}
	e.createdEmitted = true
	return e.emit(w, "response.created", map[string]any{"response": e.responseObj("in_progress", nil, false)})
}

func (e *StreamEncoder) closeOpen(w *sse.Writer) error {
	switch e.open {
	case openMessage:
		if err := e.closeOpenPart(w); err != nil {
			return err
		}
		content := make([]any, 0, len(e.openParts))
		for _, part := range e.openParts {
			text := part.text.String()
			if part.refusal {
				content = append(content, map[string]any{"type": "refusal", "refusal": text})
				continue
			}
			if text == "" {
				continue
			}
			content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
		}
		if content == nil {
			content = []any{}
		}
		item := map[string]any{
			"id": e.openItemID, "type": "message", "status": "completed", "role": "assistant",
			"content": content,
		}
		e.items = append(e.items, item)
		if err := e.emit(w, "response.output_item.done", map[string]any{"output_index": e.openOutIdx, "item": item}); err != nil {
			return err
		}
		e.openParts = nil
		e.partIdx = 0
	case openReasoning:
		if e.reasoningOpen != nil {
			if err := e.closeReasoningItem(w, e.reasoningOpen); err != nil {
				return err
			}
		}
	}
	e.open = openNone
	return nil
}

func (e *StreamEncoder) tool(irIndex int) *respTool {
	if t, ok := e.tools[irIndex]; ok {
		return t
	}
	t := &respTool{}
	e.tools[irIndex] = t
	return t
}

func (e *StreamEncoder) bufferPending(t *respTool, frag string) {
	if frag == "" || e.pendingLen+len(frag) > maxPendingArgs {
		return
	}
	t.pending += frag
	e.pendingLen += len(frag)
}

// openFunctionItem writes output_item.added once identity is known. Fragments
// buffered before that are flushed immediately after so the client never sees
// a delta keyed to a nonexistent item_id.
func (e *StreamEncoder) openFunctionItem(w *sse.Writer, t *respTool) error {
	if t.itemID != "" || t.name == "" {
		return nil
	}
	if e.open == openMessage || e.open == openReasoning {
		if err := e.closeOpen(w); err != nil {
			return err
		}
	}
	if err := e.closeDeferredReasoning(w); err != nil {
		return err
	}
	e.open = openFunction
	// A provider item id is distinct from call_id. Missing identity keeps the
	// historical synthetic fallback rather than inventing a Grok-specific id.
	if t.sourceItem != "" {
		t.itemID = t.sourceItem
	} else {
		t.itemID = ir.NewID("fc_")
	}
	t.outIdx = e.outputIndex
	e.outputIndex++
	e.openItemID = t.itemID
	e.openOutIdx = t.outIdx
	if t.callID == "" {
		t.callID = ir.NewID("call_")
	}
	if err := e.emit(w, "response.output_item.added", map[string]any{
		"output_index": t.outIdx,
		"item": map[string]any{
			"id": t.itemID, "type": "function_call", "status": "in_progress",
			"call_id": t.callID, "name": t.name, "arguments": "",
		},
	}); err != nil {
		return err
	}
	return e.flushToolPending(w, t)
}

func (e *StreamEncoder) flushToolPending(w *sse.Writer, t *respTool) error {
	if t.itemID == "" || t.pending == "" {
		return nil
	}
	frag := t.pending
	t.pending = ""
	e.pendingLen -= len(frag)
	t.args.WriteString(frag)
	return e.emit(w, "response.function_call_arguments.delta", map[string]any{
		"item_id": t.itemID, "output_index": t.outIdx, "delta": frag,
	})
}

func (e *StreamEncoder) closeOpenFunctions(w *sse.Writer) error {
	idxs := make([]int, 0, len(e.tools))
	for irIndex, t := range e.tools {
		if t.itemID != "" && !t.done {
			idxs = append(idxs, irIndex)
		}
	}
	sort.Slice(idxs, func(i, j int) bool {
		return e.tools[idxs[i]].outIdx < e.tools[idxs[j]].outIdx
	})
	for _, irIndex := range idxs {
		t := e.tools[irIndex]
		t.done = true
		full := t.args.String()
		if err := e.emit(w, "response.function_call_arguments.done", map[string]any{
			"item_id": t.itemID, "output_index": t.outIdx, "arguments": full,
		}); err != nil {
			return err
		}
		item := map[string]any{
			"id": t.itemID, "type": "function_call", "status": "completed",
			"call_id": t.callID, "name": t.name, "arguments": full,
		}
		e.items = append(e.items, item)
		if err := e.emit(w, "response.output_item.done", map[string]any{"output_index": t.outIdx, "item": item}); err != nil {
			return err
		}
	}
	e.open = openNone
	return nil
}

// Encode renders one IR stream event. While a deferred reasoning item is still
// waiting for its source ID, later ordering work is queued so no generated wire
// ID is emitted while a source ID is still possible.
func (e *StreamEncoder) Encode(ev ir.StreamEvent, w *sse.Writer) error {
	if len(e.pendingEvents) > 0 {
		switch {
		case ev.Kind == ir.EventMessageStart:
			// Metadata only; never reorders output. Record the timestamp before
			// queued events emit the first response object.
			e.noteCreatedAt(ev.CreatedAt)
		case ev.Kind == ir.EventFinish:
			// Finish flushes the whole queue in arrival order before closing, so
			// encoding it now cannot reorder output and must not risk the cap.
			return e.encodeEvent(ev, w)
		case e.passesAwaitingReasoning(ev):
			// The awaited item's own frames and its completion must pass the
			// queue, or a queued tool or text event at the head could never be
			// released. A separately deferred identity still binds from the
			// queue in arrival order.
			if err := e.encodeEvent(ev, w); err != nil {
				return err
			}
			return e.drainPending(w)
		default:
			// FIFO: a resolving event is appended like any other so its content
			// never overtakes queued predecessors.
			if err := e.queueEvent(ev); err != nil {
				return err
			}
		}
		return e.drainPending(w)
	}
	if !e.flushing && (e.mustQueueForDeferred(ev) || e.mustQueueForAwaitingReasoning(ev)) {
		return e.queueEvent(ev)
	}
	return e.encodeEvent(ev, w)
}

// drainPending keeps item order while letting the current Codex reasoning
// item consume its own queued updates. Bind a deferred ID before emitting and
// release each queue charge before encoding that event.
func (e *StreamEncoder) drainPending(w *sse.Writer) error {
	for len(e.pendingEvents) > 0 {
		i := 0
		ev := e.pendingEvents[i]
		if blocking := e.blockingDeferredReasoning(); blocking != nil {
			if err := e.bindPendingIdentity(blocking); err != nil {
				return err
			}
		}
		if e.mustQueueForAwaitingReasoning(ev) {
			// A second reasoning item can become the head while its final
			// snapshot is already queued. Reconcile its own frames in order
			// before opening the next item, including during the final drain.
			for i++; i < len(e.pendingEvents); i++ {
				if e.passesAwaitingReasoning(e.pendingEvents[i]) {
					break
				}
			}
			if i == len(e.pendingEvents) {
				if !e.flushing {
					return nil
				}
				i = 0 // Successful end of stream. No more snapshots can arrive.
			}
		}
		if i == 0 && !e.flushing && !e.passesAwaitingReasoning(ev) && e.mustQueueForDeferred(ev) {
			return nil
		}
		ev = e.pendingEvents[i]
		if i == 0 {
			e.pendingEvents[0] = ir.StreamEvent{}
			e.pendingEvents = e.pendingEvents[1:]
		} else {
			e.pendingEvents = slices.Delete(e.pendingEvents, i, i+1)
		}
		e.pendingBytes -= e.pendingEventBytes(ev)
		if err := e.encodeEvent(ev, w); err != nil {
			return err
		}
	}
	e.pendingEvents = nil
	e.pendingBytes = 0
	return nil
}

func (e *StreamEncoder) encodeEvent(ev ir.StreamEvent, w *sse.Writer) error {
	switch ev.Kind {
	case ir.EventMessageStart:
		e.id = ev.ID
		if ev.Model != "" {
			e.model = ev.Model
		}
		e.applyUsage(ev)
		e.noteCreatedAt(ev.CreatedAt)
		return e.ensureCreated(w)

	case ir.EventTextEnd:
		return nil

	case ir.EventTextStart, ir.EventTextDelta:
		if ev.Text == "" && !ev.Refusal {
			return nil
		}
		if err := e.ensureCreated(w); err != nil {
			return err
		}
		if e.open != openMessage {
			if err := e.openMessageItem(w); err != nil {
				return err
			}
		}
		if err := e.ensureMessagePart(w, ev.Refusal); err != nil {
			return err
		}
		if ev.Text == "" {
			return nil
		}
		e.openParts[e.partIdx].text.WriteString(ev.Text)
		name := "response.output_text.delta"
		if ev.Refusal {
			name = "response.refusal.delta"
		}
		return e.emit(w, name, map[string]any{
			"item_id": e.openItemID, "output_index": e.openOutIdx, "content_index": e.partIdx, "delta": ev.Text,
		})

	case ir.EventReasoningItemStart:
		return e.encodeReasoningItemStart(w, ev)
	case ir.EventReasoningDelta, ir.EventReasoningStart:
		return e.encodeReasoningDelta(w, ev)
	case ir.EventReasoningItemDone:
		return e.encodeReasoningDone(w, ev)
	case ir.EventReasoningSignature, ir.EventReasoningEnd, ir.EventRedactedReasoning:
		// Anthropic signature and redacted payloads are not Responses fields.
		// Do not place them in encrypted_content. They never reorder output, so
		// they pass through even while identity work is queued.
		return nil
	case ir.EventServerBlock, ir.EventServerStart, ir.EventServerDelta, ir.EventServerEnd, ir.EventServerUpdate:
		return ir.ProtocolError("server block is not supported by this format")
	case ir.EventTextCitation:
		// A read-only citation annotation has no field in this format. It is not
		// a tool call and must not invent a server block.
		return nil

	case ir.EventToolCallStart:
		if err := e.ensureCreated(w); err != nil {
			return err
		}
		t := e.tool(ev.Index)
		if ev.ToolID != "" {
			t.callID = ev.ToolID
		}
		if ev.ResponsesItemID != "" {
			t.sourceItem = ev.ResponsesItemID
		}
		if ev.ToolName != "" {
			t.name = ev.ToolName
		}
		return e.openFunctionItem(w, t)

	case ir.EventToolCallDelta:
		t := e.tool(ev.Index)
		if ev.ResponsesItemID != "" && t.sourceItem == "" {
			t.sourceItem = ev.ResponsesItemID
		}
		if t.itemID == "" {
			// No item for this index yet: buffer instead of emitting a delta
			// keyed to a nonexistent item or writing into another item's args.
			e.bufferPending(t, ev.ArgsFrag)
			return nil
		}
		if t.done {
			return nil
		}
		t.args.WriteString(ev.ArgsFrag)
		return e.emit(w, "response.function_call_arguments.delta", map[string]any{
			"item_id": t.itemID, "output_index": t.outIdx, "delta": ev.ArgsFrag,
		})

	case ir.EventFinish:
		// Everything still queued must be rendered before the response closes.
		// Generation is the only remaining option for unresolved identities.
		e.flushing = true
		if err := e.drainPending(w); err != nil {
			return err
		}
		// OpenAI-family backends report input at finish; response.completed is the
		// only Responses event carrying usage and it is emitted below, so a late
		// input value can still be reflected. Do not reset nonzero start cache
		// with absent later fields.
		e.applyUsage(ev)
		e.usageOut = ev.OutputTokens
		if err := e.ensureCreated(w); err != nil {
			return err
		}
		if err := e.closeOpen(w); err != nil {
			return err
		}
		if err := e.closeOpenFunctions(w); err != nil {
			return err
		}
		if err := e.closeDeferredReasoning(w); err != nil {
			return err
		}
		e.finishEmitted = true
		name, status, reason := responseTerminal(ev.StopReason)
		obj := e.responseObj(status, e.items, true)
		if reason != "" {
			obj["incomplete_details"] = map[string]any{"reason": reason}
		}
		return e.emit(w, name, map[string]any{"response": obj})
	}
	return nil
}

// EncodeError writes a terminal Responses error event. No completed/finish follows.
func (e *StreamEncoder) EncodeError(w *sse.Writer, message, errType string) error {
	if errType == "" {
		errType = "api_error"
	}
	return e.emit(w, "error", map[string]any{
		"error": map[string]any{
			"type":    errType,
			"message": message,
			"code":    nil,
			"param":   nil,
		},
	})
}

func responseTerminal(stop ir.StopReason) (name, status, reason string) {
	switch stop {
	case ir.StopMaxTokens:
		return "response.incomplete", "incomplete", "max_output_tokens"
	case ir.StopContentFilter:
		return "response.incomplete", "incomplete", "content_filter"
	default:
		return "response.completed", "completed", ""
	}
}

func (e *StreamEncoder) openMessageItem(w *sse.Writer) error {
	if err := e.closeOpen(w); err != nil {
		return err
	}
	if err := e.closeOpenFunctions(w); err != nil {
		return err
	}
	if err := e.closeDeferredReasoning(w); err != nil {
		return err
	}
	e.openItemID = ir.NewID("msg_")
	e.openOutIdx = e.outputIndex
	e.outputIndex++
	e.open = openMessage
	e.openParts = nil
	e.partIdx = 0
	return e.emit(w, "response.output_item.added", map[string]any{
		"output_index": e.openOutIdx,
		"item": map[string]any{
			"id": e.openItemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{},
		},
	})
}

func (e *StreamEncoder) ensureMessagePart(w *sse.Writer, refusal bool) error {
	if len(e.openParts) > 0 && e.openParts[e.partIdx].refusal == refusal {
		return nil
	}
	if len(e.openParts) > 0 {
		if err := e.closeOpenPart(w); err != nil {
			return err
		}
		e.partIdx++
	}
	e.openParts = append(e.openParts, &respOpenPart{refusal: refusal})
	part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
	if refusal {
		part = map[string]any{"type": "refusal", "refusal": ""}
	}
	return e.emit(w, "response.content_part.added", map[string]any{
		"item_id": e.openItemID, "output_index": e.openOutIdx, "content_index": e.partIdx, "part": part,
	})
}

func (e *StreamEncoder) closeOpenPart(w *sse.Writer) error {
	if len(e.openParts) == 0 || e.openParts[e.partIdx].closed {
		return nil
	}
	part := e.openParts[e.partIdx]
	e.openParts[e.partIdx].closed = true
	text := part.text.String()
	if part.refusal {
		if err := e.emit(w, "response.refusal.done", map[string]any{
			"item_id": e.openItemID, "output_index": e.openOutIdx, "content_index": e.partIdx, "refusal": text,
		}); err != nil {
			return err
		}
		return e.emit(w, "response.content_part.done", map[string]any{
			"item_id": e.openItemID, "output_index": e.openOutIdx, "content_index": e.partIdx,
			"part": map[string]any{"type": "refusal", "refusal": text},
		})
	}
	if err := e.emit(w, "response.output_text.done", map[string]any{
		"item_id": e.openItemID, "output_index": e.openOutIdx, "content_index": e.partIdx, "text": text,
	}); err != nil {
		return err
	}
	return e.emit(w, "response.content_part.done", map[string]any{
		"item_id": e.openItemID, "output_index": e.openOutIdx, "content_index": e.partIdx,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	})
}

func (e *StreamEncoder) Close(w *sse.Writer) error {
	if e.finishEmitted {
		return nil
	}
	e.flushing = true
	if err := e.drainPending(w); err != nil {
		return err
	}
	if err := e.ensureCreated(w); err != nil {
		return err
	}
	if err := e.closeOpen(w); err != nil {
		return err
	}
	if err := e.closeOpenFunctions(w); err != nil {
		return err
	}
	if err := e.closeDeferredReasoning(w); err != nil {
		return err
	}
	return e.emit(w, "response.completed", map[string]any{"response": e.responseObj("completed", e.items, true)})
}
