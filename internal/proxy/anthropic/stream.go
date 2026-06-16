package anthropic

import (
	"encoding/json"
	"io"

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
	outputTokens := 0
	finished := false

	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch ev.Name {
		case "message_start":
			var m streamMessageStart
			if json.Unmarshal(ev.Data, &m) != nil {
				continue
			}
			if err := emit(ir.StreamEvent{
				Kind: ir.EventMessageStart, ID: m.Message.ID, Model: m.Message.Model,
				InputTokens: m.Message.Usage.InputTokens,
			}); err != nil {
				return err
			}
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
			}
		case "content_block_delta":
			var d streamContentBlockDelta
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			switch d.Delta.Type {
			case "text_delta":
				if err := emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: d.Delta.Text}); err != nil {
					return err
				}
			case "input_json_delta":
				if err := emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: d.Index, ArgsFrag: d.Delta.PartialJSON}); err != nil {
					return err
				}
			}
		case "message_delta":
			var m streamMessageDelta
			if json.Unmarshal(ev.Data, &m) != nil {
				continue
			}
			stopReason = stopReason2(m.Delta.StopReason)
			outputTokens = m.Usage.OutputTokens
		case "message_stop":
			if err := emit(ir.StreamEvent{Kind: ir.EventFinish, StopReason: stopReason, OutputTokens: outputTokens}); err != nil {
				return err
			}
			finished = true
		}
	}
	if !finished {
		return emit(ir.StreamEvent{Kind: ir.EventFinish, StopReason: stopReason, OutputTokens: outputTokens})
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
	toolBlock     map[int]int // IR tool Index -> anthropic block index
	finishEmitted bool
}

func NewStreamEncoder(model string) *StreamEncoder {
	return &StreamEncoder{model: model, openKind: blockNone, toolBlock: map[int]int{}}
}

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
	if e.openKind == blockNone {
		return nil
	}
	idx := e.openIndex
	e.openKind = blockNone
	return e.event(w, "content_block_stop", map[string]any{"index": idx})
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

	case ir.EventToolCallStart:
		if err := e.ensureStart(w); err != nil {
			return err
		}
		if err := e.closeBlock(w); err != nil {
			return err
		}
		e.openIndex = e.nextIndex
		e.nextIndex++
		e.openKind = blockTool
		e.toolBlock[ev.Index] = e.openIndex
		return e.event(w, "content_block_start", map[string]any{
			"index": e.openIndex,
			"content_block": map[string]any{
				"type": "tool_use", "id": ev.ToolID, "name": ev.ToolName, "input": map[string]any{},
			},
		})

	case ir.EventToolCallDelta:
		idx, ok := e.toolBlock[ev.Index]
		if !ok {
			idx = e.openIndex
		}
		return e.event(w, "content_block_delta", map[string]any{
			"index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": ev.ArgsFrag},
		})

	case ir.EventFinish:
		if err := e.ensureStart(w); err != nil {
			return err
		}
		if err := e.closeBlock(w); err != nil {
			return err
		}
		if err := e.event(w, "message_delta", map[string]any{
			"delta": map[string]any{"stop_reason": stopReasonWire(ev.StopReason), "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": ev.OutputTokens},
		}); err != nil {
			return err
		}
		e.finishEmitted = true
		return e.event(w, "message_stop", map[string]any{})
	}
	return nil
}

func (e *StreamEncoder) Close(w *sse.Writer) error {
	if !e.started || e.finishEmitted {
		return nil
	}
	if err := e.closeBlock(w); err != nil {
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
