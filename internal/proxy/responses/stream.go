package responses

import (
	"encoding/json"
	"io"
	"strings"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

// streamEnvelope is the union of the Responses SSE event fields this decoder
// reads. The event kind is taken from the JSON "type" field rather than the SSE
// event name, so a producer that omits the name still decodes.
type streamEnvelope struct {
	Type        string      `json:"type"`
	OutputIndex int         `json:"output_index"`
	Delta       string      `json:"delta"`
	Item        *streamItem `json:"item"`
	Response    *respObject `json:"response"`
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
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	reader := sse.NewReader(r)
	started := false
	sawTool := false
	stop := ir.StopEndTurn
	inputTokens, outputTokens := 0, 0

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
		case "response.function_call_arguments.delta":
			if env.Delta != "" {
				if err := emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: env.OutputIndex, ArgsFrag: env.Delta}); err != nil {
					return err
				}
			}
		case "response.completed", "response.incomplete", "response.failed":
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
	if !started {
		return nil
	}
	return emit(ir.StreamEvent{Kind: ir.EventFinish, StopReason: stop, InputTokens: inputTokens, OutputTokens: outputTokens})
}

const (
	openNone = iota
	openMessage
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

	textBuf  strings.Builder
	argBuf   strings.Builder
	fcCallID string
	fcName   string

	toolItem map[int]string
	toolIdx  map[int]int

	items []map[string]any // completed output items, for response.completed
}

func NewStreamEncoder(model string) *StreamEncoder {
	return &StreamEncoder{model: model, open: openNone, toolItem: map[int]string{}, toolIdx: map[int]int{}}
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
	case openFunction:
		full := e.argBuf.String()
		if err := e.emit(w, "response.function_call_arguments.done", map[string]any{
			"item_id": e.openItemID, "output_index": e.openOutIdx, "arguments": full,
		}); err != nil {
			return err
		}
		item := map[string]any{
			"id": e.openItemID, "type": "function_call", "status": "completed",
			"call_id": e.fcCallID, "name": e.fcName, "arguments": full,
		}
		e.items = append(e.items, item)
		if err := e.emit(w, "response.output_item.done", map[string]any{"output_index": e.openOutIdx, "item": item}); err != nil {
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

	case ir.EventToolCallStart:
		if err := e.ensureCreated(w); err != nil {
			return err
		}
		if err := e.closeOpen(w); err != nil {
			return err
		}
		e.openItemID = ir.NewID("fc_")
		e.openOutIdx = e.outputIndex
		e.outputIndex++
		e.open = openFunction
		e.argBuf.Reset()
		e.fcCallID = ev.ToolID
		e.fcName = ev.ToolName
		e.toolItem[ev.Index] = e.openItemID
		e.toolIdx[ev.Index] = e.openOutIdx
		return e.emit(w, "response.output_item.added", map[string]any{
			"output_index": e.openOutIdx,
			"item": map[string]any{
				"id": e.openItemID, "type": "function_call", "status": "in_progress",
				"call_id": ev.ToolID, "name": ev.ToolName, "arguments": "",
			},
		})

	case ir.EventToolCallDelta:
		itemID := e.openItemID
		outIdx := e.openOutIdx
		if id, ok := e.toolItem[ev.Index]; ok {
			itemID = id
			outIdx = e.toolIdx[ev.Index]
		}
		e.argBuf.WriteString(ev.ArgsFrag)
		return e.emit(w, "response.function_call_arguments.delta", map[string]any{
			"item_id": itemID, "output_index": outIdx, "delta": ev.ArgsFrag,
		})

	case ir.EventFinish:
		e.usageOut = ev.OutputTokens
		if err := e.ensureCreated(w); err != nil {
			return err
		}
		if err := e.closeOpen(w); err != nil {
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

func (e *StreamEncoder) openMessageItem(w *sse.Writer) error {
	if err := e.closeOpen(w); err != nil {
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
	return e.emit(w, "response.completed", map[string]any{"response": e.responseObj("completed", e.items, true)})
}
