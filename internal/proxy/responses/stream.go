package responses

import (
	"strings"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

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
