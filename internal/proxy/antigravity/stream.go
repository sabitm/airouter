package antigravity

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

// DecodeStream reads Antigravity/Cloud Code SSE (Gemini response, optionally
// wrapped in {response:...}) and emits IR stream events. Tool names are decloaked.
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	reader := sse.NewReader(r)
	started := false
	toolIndex := 0
	sawTools := false
	var stopReason ir.StopReason = ir.StopEndTurn
	inputTokens := 0
	outputTokens := 0
	msgID := ""
	model := ""

	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		data := strings.TrimSpace(string(ev.Data))
		if data == "" || data == "[DONE]" {
			continue
		}
		raw := []byte(data)
		if sf, ok := antigravityStreamError(raw); ok {
			return sf
		}
		resp, ok := unwrapChunk(raw)
		if !ok {
			continue
		}
		if resp.ResponseID != "" {
			msgID = resp.ResponseID
		}
		if resp.ModelVersion != "" {
			model = resp.ModelVersion
		}
		if resp.UsageMetadata != nil {
			// Latest authoritative usage wins, including late-only metadata chunks.
			// Prompt stays positive-only: Gemini usage is cumulative and a bare
			// zero is not a meaningful overwrite of earlier prompt counts.
			if in := resp.UsageMetadata.inputTokens(); in > 0 {
				inputTokens = in
			}
			// Output accepts explicit candidates (including zero), a present total
			// used for derivation, or positive thoughts reported on their own.
			if resp.UsageMetadata.hasAuthoritativeOutput() {
				outputTokens = resp.UsageMetadata.outputTokens()
			}
		}
		if !started {
			if msgID == "" {
				msgID = ir.NewID("msg_")
			}
			if err := emit(ir.StreamEvent{Kind: ir.EventMessageStart, ID: msgID, Model: model, InputTokens: inputTokens}); err != nil {
				return err
			}
			started = true
		} else if inputTokens > 0 {
			// Message already started; input tokens may arrive later — nothing to do.
		}

		if len(resp.Candidates) == 0 {
			continue
		}
		cand := resp.Candidates[0]
		if cand.Content != nil {
			for _, part := range cand.Content.Parts {
				if part.Thought {
					// Drop model-internal thinking; not modeled in IR.
					continue
				}
				if part.Text != "" {
					if err := emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: part.Text}); err != nil {
						return err
					}
				}
				if part.FunctionCall != nil && part.FunctionCall.Name != "" {
					sawTools = true
					name := DecloakName(part.FunctionCall.Name)
					id := part.FunctionCall.ID
					if id == "" {
						id = fmt.Sprintf("%s-%d", name, toolIndex)
					}
					args := "{}"
					if part.FunctionCall.Args != nil {
						if b, err := json.Marshal(part.FunctionCall.Args); err == nil {
							args = string(b)
						}
					}
					idx := toolIndex
					toolIndex++
					if err := emit(ir.StreamEvent{
						Kind:     ir.EventToolCallStart,
						Index:    idx,
						ToolID:   id,
						ToolName: name,
					}); err != nil {
						return err
					}
					if err := emit(ir.StreamEvent{
						Kind:     ir.EventToolCallDelta,
						Index:    idx,
						ToolID:   id,
						ToolName: name,
						ArgsFrag: args,
					}); err != nil {
						return err
					}
				}
			}
		}
		if cand.FinishReason != "" {
			stopReason = mapFinish(cand.FinishReason, sawTools)
		}
	}

	if !started {
		// Empty / error-only stream: do not fabricate a successful completion.
		return nil
	}
	return emit(ir.StreamEvent{
		Kind:         ir.EventFinish,
		StopReason:   stopReason,
		OutputTokens: outputTokens,
		InputTokens:  inputTokens,
	})
}

// antigravityStreamError detects a top-level error envelope that unwrapChunk
// would otherwise drop. Parsed fields only.
func antigravityStreamError(data []byte) (*ir.StreamFailure, bool) {
	if len(data) == 0 || data[0] != '{' {
		return nil, false
	}
	var env struct {
		Error *struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &env) != nil || env.Error == nil {
		return nil, false
	}
	sf := &ir.StreamFailure{Message: env.Error.Message, Type: env.Error.Status}
	if sf.Message == "" {
		sf.Message = "upstream stream failed"
	}
	switch c := env.Error.Code.(type) {
	case string:
		sf.Code = c
	case float64:
		sf.Code = fmt.Sprintf("%.0f", c)
	}
	return sf, true
}

func unwrapChunk(data []byte) (*geminiResponse, bool) {
	var wrap streamChunk
	if json.Unmarshal(data, &wrap) != nil {
		return nil, false
	}
	if wrap.Response != nil {
		return wrap.Response, true
	}
	// Bare response fields on the same object.
	if len(wrap.Candidates) > 0 || wrap.UsageMetadata != nil || wrap.ResponseID != "" {
		return &geminiResponse{
			Candidates:    wrap.Candidates,
			UsageMetadata: wrap.UsageMetadata,
			ResponseID:    wrap.ResponseID,
			ModelVersion:  wrap.ModelVersion,
		}, true
	}
	return nil, false
}

func mapFinish(reason string, sawTools bool) ir.StopReason {
	switch strings.ToUpper(reason) {
	case "MAX_TOKENS":
		return ir.StopMaxTokens
	case "STOP":
		if sawTools {
			return ir.StopToolUse
		}
		return ir.StopEndTurn
	default:
		if sawTools {
			return ir.StopToolUse
		}
		return ir.StopEndTurn
	}
}
