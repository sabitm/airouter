package anthropic

import (
	"encoding/json"
	"io"
	"sort"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

// --- streaming event wire types ---

type streamMessageStart struct {
	Message struct {
		ID    string    `json:"id"`
		Model string    `json:"model"`
		Usage anthUsage `json:"usage"`
	} `json:"message"`
}

type streamContentBlockStart struct {
	Index        int `json:"index"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
}

type streamContentBlockDelta struct {
	Index int `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
}

type streamMessageDelta struct {
	Delta struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage anthUsage `json:"usage"`
}

// DecodeStream reads an Anthropic Messages SSE stream and emits IR stream
// events. Used when Anthropic is the backend format. The block index from the
// wire is carried through as StreamEvent.Index so the ingress encoder can
// attribute tool argument fragments correctly.
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	reader := sse.NewReader(r)
	var stopReason ir.StopReason = ir.StopEndTurn
	inputTokens := 0
	outputTokens := 0
	finished := false
	started := false

	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch ev.Name {
		case "error":
			var env struct {
				Error *struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(ev.Data, &env) != nil || env.Error == nil {
				return &ir.StreamFailure{Message: "upstream stream failed"}
			}
			sf := &ir.StreamFailure{Type: env.Error.Type, Message: env.Error.Message}
			if sf.Message == "" {
				sf.Message = "upstream stream failed"
			}
			return sf
		case "message_start":
			var m streamMessageStart
			if json.Unmarshal(ev.Data, &m) != nil {
				continue
			}
			inputTokens = m.Message.Usage.TotalInput()
			if err := emit(ir.StreamEvent{
				Kind: ir.EventMessageStart, ID: m.Message.ID, Model: m.Message.Model,
				InputTokens: inputTokens,
			}); err != nil {
				return err
			}
			started = true
		case "content_block_start":
			var s streamContentBlockStart
			if json.Unmarshal(ev.Data, &s) != nil {
				continue
			}
			if s.ContentBlock.Type == "tool_use" {
				if err := emit(ir.StreamEvent{
					Kind: ir.EventToolCallStart, Index: s.Index,
					ToolID: s.ContentBlock.ID, ToolName: s.ContentBlock.Name,
				}); err != nil {
					return err
				}
				started = true
			}
		case "content_block_delta":
			var d streamContentBlockDelta
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			switch d.Delta.Type {
			case "thinking_delta":
				if err := emit(ir.StreamEvent{Kind: ir.EventReasoningDelta, Text: d.Delta.Thinking}); err != nil {
					return err
				}
				started = true
			case "text_delta":
				if err := emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: d.Delta.Text}); err != nil {
					return err
				}
				started = true
			case "input_json_delta":
				if err := emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: d.Index, ArgsFrag: d.Delta.PartialJSON}); err != nil {
					return err
				}
				started = true
			}
		case "message_delta":
			var m streamMessageDelta
			if json.Unmarshal(ev.Data, &m) != nil {
				continue
			}
			stopReason = stopReason2(m.Delta.StopReason)
			// Some Anthropic-compatible providers defer all usage until the final
			// delta instead of reporting input at message_start.
			if in := m.Usage.TotalInput(); in != 0 {
				inputTokens = in
			}
			outputTokens = m.Usage.OutputTokens
		case "message_stop":
			if err := emit(ir.StreamEvent{
				Kind: ir.EventFinish, StopReason: stopReason,
				InputTokens: inputTokens, OutputTokens: outputTokens,
			}); err != nil {
				return err
			}
			finished = true
		}
	}
	if !finished {
		if !started {
			// Empty / comment-only stream: do not fabricate a successful completion.
			return nil
		}
		return emit(ir.StreamEvent{
			Kind: ir.EventFinish, StopReason: stopReason,
			InputTokens: inputTokens, OutputTokens: outputTokens,
		})
	}
	return nil
}

func stopReason2(s string) ir.StopReason {
	if s == "" {
		return ir.StopEndTurn
	}
	return stopReason(s)
}

// open block kinds tracked by the encoder.
const (
	blockNone = iota
	blockText
	blockReasoning
	blockTool
)

// StreamEncoder renders IR stream events as an Anthropic Messages SSE stream.
// It manages content-block indices and start/stop framing, which the flat
// OpenAI delta stream does not carry. Used when Anthropic is the ingress format.
type StreamEncoder struct {
	id            string
	model         string
	inputTokens   int
	started       bool
	openKind      int
	openIndex     int
	nextIndex     int
	tools         map[int]*streamTool
	openToolIdx   []int // anthropic block indices started and not yet stopped
	pendingBytes  int
	finishEmitted bool
}

// streamTool is one IR-index tool call. block is -1 until content_block_start
// has been written; identity may arrive across repeated Starts and argument
// fragments may precede the first Start.
type streamTool struct {
	block   int
	id      string
	name    string
	pending string
}

func NewStreamEncoder(model string) *StreamEncoder {
	return &StreamEncoder{model: model, openKind: blockNone, tools: map[int]*streamTool{}}
}

// maxPendingArgs bounds fragments buffered for an index whose Start has not
// arrived yet, so a stream that never resolves identity cannot retain
// unbounded bytes.
const maxPendingArgs = 1 << 20

func (e *StreamEncoder) event(w *sse.Writer, name string, payload map[string]any) error {
	payload["type"] = name
	raw, _ := json.Marshal(payload)
	return w.WriteEvent(name, raw)
}

func (e *StreamEncoder) ensureStart(w *sse.Writer) error {
	if e.started {
		return nil
	}
	e.started = true
	id := e.id
	if id == "" {
		id = ir.NewID("msg_")
	}
	return e.event(w, "message_start", map[string]any{
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": e.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": e.inputTokens, "output_tokens": 0},
		},
	})
}

func (e *StreamEncoder) closeBlock(w *sse.Writer) error {
	if e.openKind != blockText && e.openKind != blockReasoning {
		return nil
	}
	idx := e.openIndex
	e.openKind = blockNone
	return e.event(w, "content_block_stop", map[string]any{"index": idx})
}

func (e *StreamEncoder) tool(irIndex int) *streamTool {
	if t, ok := e.tools[irIndex]; ok {
		return t
	}
	t := &streamTool{block: -1}
	e.tools[irIndex] = t
	return t
}

func (e *StreamEncoder) bufferPending(t *streamTool, frag string) {
	if frag == "" || e.pendingBytes+len(frag) > maxPendingArgs {
		return
	}
	t.pending += frag
	e.pendingBytes += len(frag)
}

// openToolBlock writes content_block_start once the tool name is known.
// Fragments buffered before that are flushed immediately after so the wire
// stays message/block-before-delta. Id-only Starts wait for the name so a
// split identity does not open a nameless block.
func (e *StreamEncoder) openToolBlock(w *sse.Writer, t *streamTool) error {
	if t.block >= 0 || t.name == "" {
		return nil
	}
	if e.openKind != blockNone && e.openKind != blockTool {
		if err := e.closeBlock(w); err != nil {
			return err
		}
	}
	e.openKind = blockTool
	t.block = e.nextIndex
	e.nextIndex++
	e.openIndex = t.block
	e.openToolIdx = append(e.openToolIdx, t.block)
	if t.id == "" {
		t.id = ir.NewID("toolu_")
	}
	if err := e.event(w, "content_block_start", map[string]any{
		"index": t.block,
		"content_block": map[string]any{
			"type": "tool_use", "id": t.id, "name": t.name, "input": map[string]any{},
		},
	}); err != nil {
		return err
	}
	return e.flushToolPending(w, t)
}

func (e *StreamEncoder) flushToolPending(w *sse.Writer, t *streamTool) error {
	if t.block < 0 || t.pending == "" {
		return nil
	}
	frag := t.pending
	t.pending = ""
	e.pendingBytes -= len(frag)
	return e.event(w, "content_block_delta", map[string]any{
		"index": t.block, "delta": map[string]any{"type": "input_json_delta", "partial_json": frag},
	})
}

func (e *StreamEncoder) closeOpenTools(w *sse.Writer) error {
	if len(e.openToolIdx) == 0 {
		return nil
	}
	idxs := append([]int(nil), e.openToolIdx...)
	sort.Ints(idxs)
	e.openToolIdx = nil
	e.openKind = blockNone
	for _, idx := range idxs {
		if err := e.event(w, "content_block_stop", map[string]any{"index": idx}); err != nil {
			return err
		}
	}
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
		return e.ensureStart(w)

	case ir.EventTextDelta:
		if err := e.ensureStart(w); err != nil {
			return err
		}
		if e.openKind != blockText {
			if err := e.closeBlock(w); err != nil {
				return err
			}
			if err := e.closeOpenTools(w); err != nil {
				return err
			}
			e.openIndex = e.nextIndex
			e.nextIndex++
			e.openKind = blockText
			if err := e.event(w, "content_block_start", map[string]any{
				"index": e.openIndex, "content_block": map[string]any{"type": "text", "text": ""},
			}); err != nil {
				return err
			}
		}
		return e.event(w, "content_block_delta", map[string]any{
			"index": e.openIndex, "delta": map[string]any{"type": "text_delta", "text": ev.Text},
		})

	case ir.EventReasoningDelta:
		if err := e.ensureStart(w); err != nil {
			return err
		}
		if e.openKind != blockReasoning {
			if err := e.closeBlock(w); err != nil {
				return err
			}
			if err := e.closeOpenTools(w); err != nil {
				return err
			}
			e.openIndex = e.nextIndex
			e.nextIndex++
			e.openKind = blockReasoning
			if err := e.event(w, "content_block_start", map[string]any{
				"index": e.openIndex, "content_block": map[string]any{"type": "thinking", "thinking": ""},
			}); err != nil {
				return err
			}
		}
		return e.event(w, "content_block_delta", map[string]any{
			"index": e.openIndex, "delta": map[string]any{"type": "thinking_delta", "thinking": ev.Text},
		})

	case ir.EventToolCallStart:
		if err := e.ensureStart(w); err != nil {
			return err
		}
		t := e.tool(ev.Index)
		if ev.ToolID != "" {
			t.id = ev.ToolID
		}
		if ev.ToolName != "" {
			t.name = ev.ToolName
		}
		return e.openToolBlock(w, t)

	case ir.EventToolCallDelta:
		t := e.tool(ev.Index)
		if t.block < 0 {
			// No content_block_start yet: buffer instead of emitting a delta
			// outside any content block (or onto whichever block happens to be
			// open, which would merge another call's arguments into it).
			e.bufferPending(t, ev.ArgsFrag)
			return nil
		}
		return e.event(w, "content_block_delta", map[string]any{
			"index": t.block, "delta": map[string]any{"type": "input_json_delta", "partial_json": ev.ArgsFrag},
		})

	case ir.EventFinish:
		startEmitted := e.started
		startInput := e.inputTokens
		// OpenAI-family backends report input at Finish. Capture it before
		// ensureStart so a Finish-first stream puts the count on message_start.
		if ev.InputTokens != 0 {
			e.inputTokens = ev.InputTokens
		}
		if err := e.ensureStart(w); err != nil {
			return err
		}
		if err := e.closeBlock(w); err != nil {
			return err
		}
		if err := e.closeOpenTools(w); err != nil {
			return err
		}
		usage := map[string]any{"output_tokens": ev.OutputTokens}
		// message_start already went out: expose a late/changed input count
		// on the terminal delta. Skip when it would duplicate the start frame.
		if startEmitted && ev.InputTokens != 0 && ev.InputTokens != startInput {
			usage["input_tokens"] = ev.InputTokens
		}
		if err := e.event(w, "message_delta", map[string]any{
			"delta": map[string]any{"stop_reason": stopReasonWire(ev.StopReason), "stop_sequence": nil},
			"usage": usage,
		}); err != nil {
			return err
		}
		e.finishEmitted = true
		return e.event(w, "message_stop", map[string]any{})
	}
	return nil
}

// EncodeError writes a terminal Anthropic error event. No message_stop follows.
func (e *StreamEncoder) EncodeError(w *sse.Writer, message, errType string) error {
	if errType == "" {
		errType = "api_error"
	}
	raw := EncodeError(message, errType)
	return w.WriteEvent("error", raw)
}

func (e *StreamEncoder) Close(w *sse.Writer) error {
	if !e.started || e.finishEmitted {
		return nil
	}
	if err := e.closeBlock(w); err != nil {
		return err
	}
	if err := e.closeOpenTools(w); err != nil {
		return err
	}
	if err := e.event(w, "message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 0},
	}); err != nil {
		return err
	}
	return e.event(w, "message_stop", map[string]any{})
}
