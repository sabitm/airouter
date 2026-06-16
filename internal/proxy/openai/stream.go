package openai

import (
	"encoding/json"
	"io"
	"time"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

// --- chunk wire types (streaming) ---

type chatChunk struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *chatUsage    `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	ToolCalls []chunkToolCall `json:"tool_calls,omitempty"`
}

type chunkToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Function chunkFn `json:"function"`
}

type chunkFn struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// DecodeStream reads an OpenAI Chat Completions SSE stream and emits IR stream
// events. Used when OpenAI is the backend format. The Finish event is deferred
// to end-of-stream so a trailing usage-only chunk is captured.
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	reader := sse.NewReader(r)
	started := false
	var stopReason ir.StopReason = ir.StopEndTurn
	outputTokens := 0

	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if string(ev.Data) == "[DONE]" {
			break
		}
		var chunk chatChunk
		if json.Unmarshal(ev.Data, &chunk) != nil {
			continue
		}
		if !started {
			if err := emit(ir.StreamEvent{Kind: ir.EventMessageStart, ID: chunk.ID, Model: chunk.Model}); err != nil {
				return err
			}
			started = true
		}
		if chunk.Usage != nil {
			outputTokens = chunk.Usage.CompletionTokens
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				if err := emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: c.Delta.Content}); err != nil {
					return err
				}
			}
			for _, tc := range c.Delta.ToolCalls {
				if tc.ID != "" || tc.Function.Name != "" {
					if err := emit(ir.StreamEvent{Kind: ir.EventToolCallStart, Index: tc.Index, ToolID: tc.ID, ToolName: tc.Function.Name}); err != nil {
						return err
					}
				}
				if tc.Function.Arguments != "" {
					if err := emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: tc.Index, ArgsFrag: tc.Function.Arguments}); err != nil {
						return err
					}
				}
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				stopReason = stopReasonFromFinish(*c.FinishReason)
			}
		}
	}
	if !started {
		return nil
	}
	return emit(ir.StreamEvent{Kind: ir.EventFinish, StopReason: stopReason, OutputTokens: outputTokens})
}

// StreamEncoder renders IR stream events as an OpenAI Chat Completions SSE
// stream. Used when OpenAI is the ingress format.
type StreamEncoder struct {
	id        string
	created   int64
	model     string
	roleSent  bool
	usageOut  int
	toolIndex map[int]int // IR tool Index -> OpenAI tool_calls index
	nextTool  int
}

func NewStreamEncoder(model string) *StreamEncoder {
	return &StreamEncoder{model: model, created: time.Now().Unix(), toolIndex: map[int]int{}}
}

func (e *StreamEncoder) emit(w *sse.Writer, delta chunkDelta, finish *string) error {
	chunk := chatChunk{
		ID:      e.id,
		Model:   e.model,
		Choices: []chunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
	}
	raw, _ := marshalChunk(chunk, e.created)
	return w.WriteEvent("", raw)
}

func (e *StreamEncoder) Encode(ev ir.StreamEvent, w *sse.Writer) error {
	switch ev.Kind {
	case ir.EventMessageStart:
		e.id = ev.ID
		if e.id == "" {
			e.id = ir.NewID("chatcmpl-")
		}
		if ev.Model != "" {
			e.model = ev.Model
		}
		e.roleSent = true
		return e.emit(w, chunkDelta{Role: "assistant"}, nil)
	case ir.EventTextDelta:
		return e.emit(w, chunkDelta{Content: ev.Text}, nil)
	case ir.EventToolCallStart:
		idx := e.nextTool
		e.nextTool++
		e.toolIndex[ev.Index] = idx
		tc := chunkToolCall{Index: idx, ID: ev.ToolID}
		tc.Function.Name = ev.ToolName
		return e.emit(w, chunkDelta{ToolCalls: []chunkToolCall{tc}}, nil)
	case ir.EventToolCallDelta:
		idx, ok := e.toolIndex[ev.Index]
		if !ok {
			idx = ev.Index
		}
		tc := chunkToolCall{Index: idx}
		tc.Function.Arguments = ev.ArgsFrag
		return e.emit(w, chunkDelta{ToolCalls: []chunkToolCall{tc}}, nil)
	case ir.EventFinish:
		e.usageOut = ev.OutputTokens
		fr := finishFromStopReason(ev.StopReason)
		if err := e.emit(w, chunkDelta{}, &fr); err != nil {
			return err
		}
		return e.emitUsage(w)
	}
	return nil
}

// emitUsage sends a final choices-empty chunk carrying usage, mirroring OpenAI's
// stream_options.include_usage behavior. Sent by default; clients that don't
// expect it ignore the empty-choices chunk.
func (e *StreamEncoder) emitUsage(w *sse.Writer) error {
	chunk := chatChunk{
		ID:      e.id,
		Model:   e.model,
		Choices: []chunkChoice{},
		Usage:   &chatUsage{CompletionTokens: e.usageOut, TotalTokens: e.usageOut},
	}
	raw, _ := marshalChunk(chunk, e.created)
	return w.WriteEvent("", raw)
}

func (e *StreamEncoder) Close(w *sse.Writer) error {
	return w.WriteEvent("", []byte("[DONE]"))
}

// marshalChunk injects the fixed object/created fields the chunk struct omits.
func marshalChunk(chunk chatChunk, created int64) ([]byte, error) {
	type alias chatChunk
	return json.Marshal(struct {
		alias
		Object  string `json:"object"`
		Created int64  `json:"created"`
	}{alias: alias(chunk), Object: "chat.completion.chunk", Created: created})
}
