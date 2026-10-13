package responses

import (
	"encoding/json"
	"io"
	"sort"
	"strings"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

// Responses argument retention matches the unary collector budgets. These are
// local so the codec does not import the proxy package. Count only recovered
// growth; an identical snapshot is not counted again.
const (
	maxRetainedToolArgBytes = 64 << 20
	maxTrackedToolCalls     = 1024
)

// streamEnvelope is the union of the Responses SSE event fields this decoder
// reads. The event kind is taken from the JSON "type" field rather than the SSE
// event name, so a producer that omits the name still decodes.
type streamEnvelope struct {
	Type           string           `json:"type"`
	OutputIndex    int              `json:"output_index"`
	OutputIndexSet bool             `json:"-"`
	ItemID         string           `json:"item_id"`
	Delta          string           `json:"delta"`
	Arguments      string           `json:"arguments"`
	Name           string           `json:"name"`
	Item           *streamItem      `json:"item"`
	Response       *respObject      `json:"response"`
	Error          *respErrorObject `json:"error"`
}

func (e *streamEnvelope) UnmarshalJSON(data []byte) error {
	var raw struct {
		Type        string           `json:"type"`
		OutputIndex *int             `json:"output_index"`
		ItemID      string           `json:"item_id"`
		Delta       string           `json:"delta"`
		Arguments   string           `json:"arguments"`
		Name        string           `json:"name"`
		Item        *streamItem      `json:"item"`
		Response    *respObject      `json:"response"`
		Error       *respErrorObject `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*e = streamEnvelope{
		Type: raw.Type, ItemID: raw.ItemID, Delta: raw.Delta,
		Arguments: raw.Arguments, Name: raw.Name, Item: raw.Item,
		Response: raw.Response, Error: raw.Error,
	}
	if raw.OutputIndex != nil {
		e.OutputIndex = *raw.OutputIndex
		e.OutputIndexSet = true
	}
	return nil
}

type streamItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
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

func decodeStreamLimited(r io.Reader, emit func(ir.StreamEvent) error, maxArgBytes, maxCalls int) error {
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
		ensureStarted: func() error { return ensureStarted("", "") },
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
			if env.Item != nil && env.Item.Type == "function_call" {
				if err := dec.observeAdded(&env); err != nil {
					return err
				}
			}
		case "response.output_text.delta":
			if err := dec.rejectContent(env.Delta); err != nil {
				return err
			}
			if env.Delta != "" {
				if err := ensureStarted("", ""); err != nil {
					return err
				}
				if err := emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: env.Delta}); err != nil {
					return err
				}
			}
		case "response.reasoning_summary_text.delta":
			if err := dec.rejectContent(env.Delta); err != nil {
				return err
			}
			if env.Delta != "" {
				if err := ensureStarted("", ""); err != nil {
					return err
				}
				if err := emit(ir.StreamEvent{Kind: ir.EventReasoningDelta, Text: env.Delta}); err != nil {
					return err
				}
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
			if env.Item != nil && env.Item.Type == "function_call" {
				if err := dec.observeItemDone(&env); err != nil {
					return err
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
				if env.Response.Status == "incomplete" {
					stop = ir.StopMaxTokens
				} else if dec.sawTool {
					stop = ir.StopToolUse
				}
			}
		}
	}
	if pendingFail != nil {
		return pendingFail
	}
	if err := dec.flushPending(); err != nil {
		return err
	}
	if dec.sawTool {
		if err := ensureStarted("", ""); err != nil {
			return err
		}
		if stop != ir.StopMaxTokens {
			stop = ir.StopToolUse
		}
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
	// responseDone is set after the first completed or incomplete snapshot is
	// accepted. It blocks new calls and new content even when output is empty.
	responseDone bool
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
	// later identified item pass it and reorder the output array.
	touched := make([]*responsesToolCall, 0, len(items))
	for i := range items {
		item := &items[i]
		if item.Type != "function_call" {
			continue
		}
		call, err := d.matchFinal(i, item)
		if err != nil {
			return err
		}
		if err := d.applySnapshot(call, item.Arguments, true); err != nil {
			return err
		}
		touched = append(touched, call)
	}
	for _, call := range touched {
		// Final-only calls are complete in this snapshot, even with an empty call
		// ID. Streamed calls still wait for a call ID unless EOF forces them.
		if err := d.emitCall(call, call.fromFinal); err != nil {
			return err
		}
	}
	// Close after this snapshot so a final-only call in the first completion is
	// accepted. A later event cannot create a call or append content. An empty
	// output array is still terminal.
	d.responseDone = true
	for _, call := range d.calls {
		call.terminal = true
	}
	return nil
}

// matchFinal binds a completion item without guessing by name or argument text.
// Item ID or call ID identifies a streamed call. An output_index equals this
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
		return call, nil
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
			return call, nil
		}
		if call := d.uniqueIndexMatch(ordinal); call != nil {
			if err := d.assign(call, toolIdentity{itemID: item.ID, callID: item.CallID, name: item.Name}); err != nil {
				return nil, err
			}
			return call, nil
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
		return call, nil
	}
	if d.hasStreamedCall() {
		return nil, ir.ProtocolError("conflicting function call identity")
	}
	return d.bind(toolIdentity{
		name: item.Name, finalOrdinal: ordinal,
	}, true, true)
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
		found = &responsesToolCall{index: d.nextIndex, fromFinal: fromFinal}
		d.nextIndex++
		d.calls = append(d.calls, found)
		if fromFinal {
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
func (d *responsesToolDecoder) emitReady(call *responsesToolCall) error {
	return d.emitCall(call, false)
}

func (d *responsesToolDecoder) emitCall(call *responsesToolCall, force bool) error {
	if !force && (call.name == "" || call.callID == "") {
		return nil
	}
	if err := d.ensureMessageStart(); err != nil {
		return err
	}
	if !call.started {
		call.started = true
		d.sawTool = true
		if err := d.emit(ir.StreamEvent{
			Kind: ir.EventToolCallStart, Index: call.index,
			ToolID: call.callID, ToolName: call.name,
		}); err != nil {
			return err
		}
	}
	if call.emitted >= call.args.Len() {
		return nil
	}
	frag := call.args.String()[call.emitted:]
	call.emitted = call.args.Len()
	return d.emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: call.index, ArgsFrag: frag})
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

	createdEmitted bool
	finishEmitted  bool

	outputIndex int
	open        int
	openItemID  string
	openOutIdx  int

	textBuf      strings.Builder
	reasoningBuf strings.Builder

	tools      map[int]*respTool
	pendingLen int

	items []map[string]any // completed output items, for response.completed
}

// respTool is one IR-index function call. itemID is empty until
// output_item.added has been written; identity may arrive across repeated
// Starts and argument fragments may precede the first Start.
type respTool struct {
	itemID  string
	outIdx  int
	callID  string
	name    string
	pending string
	args    strings.Builder
	done    bool
}

func NewStreamEncoder(model string) *StreamEncoder {
	return &StreamEncoder{model: model, open: openNone, tools: map[int]*respTool{}}
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

func (e *StreamEncoder) responseObj(status string, output []map[string]any, withUsage bool) map[string]any {
	out := any([]any{})
	if output != nil {
		out = output
	}
	obj := map[string]any{
		"id":     e.id,
		"object": "response",
		"status": status,
		"model":  e.model,
		"output": out,
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
	e.createdEmitted = true
	if e.id == "" {
		e.id = ir.NewID("resp_")
	}
	return e.emit(w, "response.created", map[string]any{"response": e.responseObj("in_progress", nil, false)})
}

func (e *StreamEncoder) closeOpen(w *sse.Writer) error {
	switch e.open {
	case openMessage:
		full := e.textBuf.String()
		if err := e.emit(w, "response.output_text.done", map[string]any{
			"item_id": e.openItemID, "output_index": e.openOutIdx, "content_index": 0, "text": full,
		}); err != nil {
			return err
		}
		if err := e.emit(w, "response.content_part.done", map[string]any{
			"item_id": e.openItemID, "output_index": e.openOutIdx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": full, "annotations": []any{}},
		}); err != nil {
			return err
		}
		item := map[string]any{
			"id": e.openItemID, "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": full, "annotations": []any{}}},
		}
		e.items = append(e.items, item)
		if err := e.emit(w, "response.output_item.done", map[string]any{"output_index": e.openOutIdx, "item": item}); err != nil {
			return err
		}
	case openReasoning:
		full := e.reasoningBuf.String()
		if err := e.emit(w, "response.reasoning_summary_text.done", map[string]any{
			"item_id": e.openItemID, "output_index": e.openOutIdx, "summary_index": 0, "text": full,
		}); err != nil {
			return err
		}
		part := map[string]any{"type": "summary_text", "text": full}
		if err := e.emit(w, "response.reasoning_summary_part.done", map[string]any{
			"item_id": e.openItemID, "output_index": e.openOutIdx, "summary_index": 0, "part": part,
		}); err != nil {
			return err
		}
		item := map[string]any{"id": e.openItemID, "type": "reasoning", "summary": []any{part}}
		e.items = append(e.items, item)
		if err := e.emit(w, "response.output_item.done", map[string]any{"output_index": e.openOutIdx, "item": item}); err != nil {
			return err
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
	e.open = openFunction
	t.itemID = ir.NewID("fc_")
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

func (e *StreamEncoder) encodeReasoningText(w *sse.Writer, text string) error {
	if text == "" {
		return nil
	}
	if err := e.ensureCreated(w); err != nil {
		return err
	}
	if e.open != openReasoning {
		if err := e.openReasoningItem(w); err != nil {
			return err
		}
	}
	e.reasoningBuf.WriteString(text)
	return e.emit(w, "response.reasoning_summary_text.delta", map[string]any{
		"item_id": e.openItemID, "output_index": e.openOutIdx, "summary_index": 0, "delta": text,
	})
}

func (e *StreamEncoder) Encode(ev ir.StreamEvent, w *sse.Writer) error {
	switch ev.Kind {
	case ir.EventMessageStart:
		e.id = ev.ID
		if ev.Model != "" {
			e.model = ev.Model
		}
		e.applyUsage(ev)
		return e.ensureCreated(w)

	case ir.EventTextDelta:
		if err := e.ensureCreated(w); err != nil {
			return err
		}
		if e.open != openMessage {
			if err := e.openMessageItem(w); err != nil {
				return err
			}
		}
		e.textBuf.WriteString(ev.Text)
		return e.emit(w, "response.output_text.delta", map[string]any{
			"item_id": e.openItemID, "output_index": e.openOutIdx, "content_index": 0, "delta": ev.Text,
		})

	case ir.EventReasoningDelta, ir.EventReasoningStart:
		return e.encodeReasoningText(w, ev.Text)
	case ir.EventReasoningSignature, ir.EventReasoningEnd, ir.EventRedactedReasoning:
		// Anthropic signature and redacted payloads are not Responses fields.
		// Do not place them in encrypted_content.
		return nil

	case ir.EventToolCallStart:
		if err := e.ensureCreated(w); err != nil {
			return err
		}
		t := e.tool(ev.Index)
		if ev.ToolID != "" {
			t.callID = ev.ToolID
		}
		if ev.ToolName != "" {
			t.name = ev.ToolName
		}
		return e.openFunctionItem(w, t)

	case ir.EventToolCallDelta:
		t := e.tool(ev.Index)
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
		status := "completed"
		if ev.StopReason == ir.StopMaxTokens {
			status = "incomplete"
		}
		e.finishEmitted = true
		return e.emit(w, "response.completed", map[string]any{"response": e.responseObj(status, e.items, true)})
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

func (e *StreamEncoder) openMessageItem(w *sse.Writer) error {
	if err := e.closeOpen(w); err != nil {
		return err
	}
	if err := e.closeOpenFunctions(w); err != nil {
		return err
	}
	e.openItemID = ir.NewID("msg_")
	e.openOutIdx = e.outputIndex
	e.outputIndex++
	e.open = openMessage
	e.textBuf.Reset()
	if err := e.emit(w, "response.output_item.added", map[string]any{
		"output_index": e.openOutIdx,
		"item": map[string]any{
			"id": e.openItemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{},
		},
	}); err != nil {
		return err
	}
	return e.emit(w, "response.content_part.added", map[string]any{
		"item_id": e.openItemID, "output_index": e.openOutIdx, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (e *StreamEncoder) openReasoningItem(w *sse.Writer) error {
	if err := e.closeOpen(w); err != nil {
		return err
	}
	if err := e.closeOpenFunctions(w); err != nil {
		return err
	}
	e.openItemID = ir.NewID("rs_")
	e.openOutIdx = e.outputIndex
	e.outputIndex++
	e.open = openReasoning
	e.reasoningBuf.Reset()
	if err := e.emit(w, "response.output_item.added", map[string]any{
		"output_index": e.openOutIdx,
		"item":         map[string]any{"id": e.openItemID, "type": "reasoning", "summary": []any{}},
	}); err != nil {
		return err
	}
	return e.emit(w, "response.reasoning_summary_part.added", map[string]any{
		"item_id": e.openItemID, "output_index": e.openOutIdx, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": ""},
	})
}

func (e *StreamEncoder) Close(w *sse.Writer) error {
	if e.finishEmitted {
		return nil
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
	return e.emit(w, "response.completed", map[string]any{"response": e.responseObj("completed", e.items, true)})
}
