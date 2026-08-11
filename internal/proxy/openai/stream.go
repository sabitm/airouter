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
	Index    int     `json:"index"`
	ID       string  `json:"id,omitempty"`
	Function chunkFn `json:"function"`
}

type chunkFn struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// DecodeStream reads an OpenAI Chat Completions SSE stream and emits IR stream
// events. Used when OpenAI is the backend format. The Finish event is deferred
// to end-of-stream so a trailing usage-only chunk is captured. A top-level
// error object in a data frame returns *ir.StreamFailure without Finish.
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	reader := sse.NewReader(r)
	started := false
	var stopReason ir.StopReason = ir.StopEndTurn
	inputTokens := 0
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
		if len(ev.Data) == 0 || ev.Data[0] != '{' {
			continue
		}
		// Error frames are data-only JSON with a top-level "error" object and no
		// choices. Detect before treating as a chat chunk so zero-valued error JSON
		// does not fabricate EventMessageStart.
		var errProbe struct {
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    string `json:"code"`
			} `json:"error"`
			Choices json.RawMessage `json:"choices"`
		}
		if json.Unmarshal(ev.Data, &errProbe) == nil && errProbe.Error != nil && len(errProbe.Choices) == 0 {
			sf := &ir.StreamFailure{
				Type:    errProbe.Error.Type,
				Code:    errProbe.Error.Code,
				Message: errProbe.Error.Message,
			}
			if sf.Message == "" {
				sf.Message = "upstream stream failed"
			}
			return sf
		}
		var chunk chatChunk
		if json.Unmarshal(ev.Data, &chunk) != nil {
			continue
		}
		// Skip frames that carry neither identity, content, finish, nor usage so
		// arbitrary empty JSON does not open a fabricated successful stream.
		hasContent := chunk.ID != "" || chunk.Model != "" || chunk.Usage != nil
		if !hasContent {
			for _, c := range chunk.Choices {
				if c.Delta.Role != "" || c.Delta.Content != "" || len(c.Delta.ToolCalls) > 0 ||
					(c.FinishReason != nil && *c.FinishReason != "") {
					hasContent = true
					break
				}
			}
		}
		if !hasContent {
			continue
		}
		if !started {
			if err := emit(ir.StreamEvent{Kind: ir.EventMessageStart, ID: chunk.ID, Model: chunk.Model}); err != nil {
				return err
			}
			started = true
		}
		if chunk.Usage != nil {
			inputTokens = chunk.Usage.PromptTokens
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
	return emit(ir.StreamEvent{Kind: ir.EventFinish, StopReason: stopReason, InputTokens: inputTokens, OutputTokens: outputTokens})
}

// StreamEncoder renders IR stream events as an OpenAI Chat Completions SSE
// stream. Used when OpenAI is the ingress format.
type StreamEncoder struct {
	id        string
	created   int64
	model     string
	roleSent  bool
	usageIn   int
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
		// Anthropic backends report input on message start; OpenAI-family backends
		// report it on finish. Take it from whichever event carries a nonzero value.
		if ev.InputTokens != 0 {
			e.usageIn = ev.InputTokens
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
		if ev.InputTokens != 0 {
			e.usageIn = ev.InputTokens
		}
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
		Usage:   &chatUsage{PromptTokens: e.usageIn, CompletionTokens: e.usageOut, TotalTokens: e.usageIn + e.usageOut},
	}
	raw, _ := marshalChunk(chunk, e.created)
	return w.WriteEvent("", raw)
}

// EncodeError writes a terminal OpenAI-style data-only error frame. No finish,
// usage, or [DONE] follows; official SDKs raise on this shape.
func (e *StreamEncoder) EncodeError(w *sse.Writer, message, errType string) error {
	if errType == "" {
		errType = "api_error"
	}
	raw := EncodeError(message, errType)
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
