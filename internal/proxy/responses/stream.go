package responses

import (
	"encoding/json"
	"io"
	"sort"
	"strings"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

// streamEnvelope is the union of the Responses SSE event fields this decoder
// reads. The event kind is taken from the JSON "type" field rather than the SSE
// event name, so a producer that omits the name still decodes.
type streamEnvelope struct {
	Type        string           `json:"type"`
	OutputIndex int              `json:"output_index"`
	Delta       string           `json:"delta"`
	Item        *streamItem      `json:"item"`
	Response    *respObject      `json:"response"`
	Error       *respErrorObject `json:"error"`
}

type streamItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
}

// DecodeStream reads an OpenAI Responses SSE stream and emits IR stream events.
// Used when Responses is the backend format. Tool calls are keyed by the event
// output_index so argument fragments attribute to the right call. The Finish
// event is deferred to end-of-stream so the response.completed usage is captured.
// Explicit error / response.failed frames return *ir.StreamFailure without Finish.
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	reader := sse.NewReader(r)
	started := false
	sawTool := false
	stop := ir.StopEndTurn
	inputTokens, outputTokens := 0, 0
	var pendingFail *ir.StreamFailure

	ensureStarted := func(id, model string) error {
		if started {
			return nil
		}
		started = true
		return emit(ir.StreamEvent{Kind: ir.EventMessageStart, ID: id, Model: model})
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
				if err := ensureStarted("", ""); err != nil {
					return err
				}
				sawTool = true
				if err := emit(ir.StreamEvent{
					Kind: ir.EventToolCallStart, Index: env.OutputIndex,
					ToolID: env.Item.CallID, ToolName: env.Item.Name,
				}); err != nil {
					return err
				}
			}
		case "response.output_text.delta":
			if env.Delta != "" {
				if err := ensureStarted("", ""); err != nil {
					return err
				}
				if err := emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: env.Delta}); err != nil {
					return err
				}
			}
		case "response.reasoning_summary_text.delta":
			if env.Delta != "" {
				if err := ensureStarted("", ""); err != nil {
					return err
				}
				if err := emit(ir.StreamEvent{Kind: ir.EventReasoningDelta, Text: env.Delta}); err != nil {
					return err
				}
			}
		case "response.function_call_arguments.delta":
			if env.Delta != "" {
				if err := emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: env.OutputIndex, ArgsFrag: env.Delta}); err != nil {
					return err
				}
			}
		case "response.completed", "response.incomplete":
			if env.Response != nil {
				if env.Response.Usage != nil {
					inputTokens = env.Response.Usage.InputTokens
					outputTokens = env.Response.Usage.OutputTokens
				}
				if env.Response.Status == "incomplete" {
					stop = ir.StopMaxTokens
				} else if sawTool {
					stop = ir.StopToolUse
				}
			}
		}
	}
	if pendingFail != nil {
		return pendingFail
	}
	if !started {
		return nil
	}
	return emit(ir.StreamEvent{Kind: ir.EventFinish, StopReason: stop, InputTokens: inputTokens, OutputTokens: outputTokens})
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
		obj["usage"] = map[string]any{
			"input_tokens":  e.inputTokens,
			"output_tokens": e.usageOut,
			"total_tokens":  e.inputTokens + e.usageOut,
		}
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

func (e *StreamEncoder) Encode(ev ir.StreamEvent, w *sse.Writer) error {
	switch ev.Kind {
	case ir.EventMessageStart:
		e.id = ev.ID
		if ev.Model != "" {
			e.model = ev.Model
		}
		e.inputTokens = ev.InputTokens
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

	case ir.EventReasoningDelta:
		if err := e.ensureCreated(w); err != nil {
			return err
		}
		if e.open != openReasoning {
			if err := e.openReasoningItem(w); err != nil {
				return err
			}
		}
		e.reasoningBuf.WriteString(ev.Text)
		return e.emit(w, "response.reasoning_summary_text.delta", map[string]any{
			"item_id": e.openItemID, "output_index": e.openOutIdx, "summary_index": 0, "delta": ev.Text,
		})

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
		// input value can still be reflected.
		if ev.InputTokens != 0 {
			e.inputTokens = ev.InputTokens
		}
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
