package proxy

import (
	"encoding/json"
	"strconv"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

// passthroughClass is the commit-gate decision for one same-codec SSE event.
type passthroughClass int

const (
	// passLifecycle is setup/usage/keepalive with no client-visible output.
	// Buffer it until a later event commits the stream.
	passLifecycle passthroughClass = iota
	// passVisible is actual output (text or tool-call activity).
	passVisible
	// passTerminal is a successful completion marker ([DONE], message_stop,
	// response.completed/incomplete).
	passTerminal
	// passFailure is an explicit protocol error frame.
	passFailure
)

// classifyPassthroughEvent inspects one upstream SSE event for same-codec
// streaming. Unknown non-error events are treated as visible so they commit
// rather than being held forever.
func classifyPassthroughEvent(codecID string, ev sse.Event) (passthroughClass, *ir.StreamFailure) {
	switch codecID {
	case "oai-chat":
		return classifyOpenAIChatPassthrough(ev)
	case "oai-responses":
		return classifyResponsesPassthrough(ev)
	case "anth-msg":
		return classifyAnthropicPassthrough(ev)
	default:
		return passVisible, nil
	}
}

func classifyOpenAIChatPassthrough(ev sse.Event) (passthroughClass, *ir.StreamFailure) {
	if string(ev.Data) == "[DONE]" {
		return passTerminal, nil
	}
	if len(ev.Data) == 0 || ev.Data[0] != '{' {
		return passVisible, nil
	}

	var errProbe struct {
		Error   json.RawMessage `json:"error"`
		Choices json.RawMessage `json:"choices"`
	}
	if json.Unmarshal(ev.Data, &errProbe) == nil && len(errProbe.Error) > 0 && string(errProbe.Error) != "null" && emptyOrAbsentJSONArray(errProbe.Choices) {
		return passFailure, failureFromErrorField(errProbe.Error)
	}

	var chunk struct {
		Choices []struct {
			Delta struct {
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(ev.Data, &chunk) != nil {
		return passVisible, nil
	}

	for _, c := range chunk.Choices {
		if jsonValuePresent(c.Delta.Content) || jsonValuePresent(c.Delta.ToolCalls) {
			return passVisible, nil
		}
	}
	for _, c := range chunk.Choices {
		if c.FinishReason != nil && *c.FinishReason != "" {
			return passTerminal, nil
		}
	}
	// Role-only / empty setup chunks and usage-only trailers stay buffered.
	if len(chunk.Choices) > 0 || jsonValuePresent(chunk.Usage) {
		return passLifecycle, nil
	}
	return passVisible, nil
}

func classifyResponsesPassthrough(ev sse.Event) (passthroughClass, *ir.StreamFailure) {
	var env struct {
		Type  string          `json:"type"`
		Delta json.RawMessage `json:"delta"`
		Item  *struct {
			Type string `json:"type"`
		} `json:"item"`
		Response *struct {
			Status string          `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"response"`
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(ev.Data, &env)
	typ := env.Type
	if typ == "" {
		typ = ev.Name
	}

	failedStatus := env.Response != nil && env.Response.Status == "failed"
	if ev.Name == "error" || typ == "error" || ev.Name == "response.failed" || typ == "response.failed" || failedStatus {
		var fail *ir.StreamFailure
		if len(env.Error) > 0 && string(env.Error) != "null" {
			fail = failureFromErrorField(env.Error)
		}
		if env.Response != nil && len(env.Response.Error) > 0 && string(env.Response.Error) != "null" {
			rf := failureFromErrorField(env.Response.Error)
			if fail == nil {
				fail = rf
			} else {
				if fail.Type == "" {
					fail.Type = rf.Type
				}
				if fail.Code == "" {
					fail.Code = rf.Code
				}
				if fail.Message == "" || fail.Message == "upstream stream failed" {
					fail.Message = rf.Message
				}
			}
		}
		if fail == nil {
			fail = &ir.StreamFailure{Message: "upstream response failed"}
		}
		return passFailure, fail
	}

	switch ev.Name {
	case "response.completed", "response.incomplete":
		return passTerminal, nil
	case "response.created", "response.in_progress":
		return passLifecycle, nil
	}
	switch typ {
	case "response.completed", "response.incomplete":
		return passTerminal, nil
	case "response.created", "response.in_progress", "response.content_part.added":
		return passLifecycle, nil
	case "response.output_item.added":
		if env.Item != nil && env.Item.Type != "" && env.Item.Type != "message" {
			return passVisible, nil
		}
		return passLifecycle, nil
	case "response.output_text.delta", "response.function_call_arguments.delta":
		if jsonValuePresent(env.Delta) {
			return passVisible, nil
		}
		return passLifecycle, nil
	case "ping":
		return passLifecycle, nil
	}
	if ev.Name == "ping" {
		return passLifecycle, nil
	}
	return passVisible, nil
}

func classifyAnthropicPassthrough(ev sse.Event) (passthroughClass, *ir.StreamFailure) {
	var env struct {
		Type         string          `json:"type"`
		Error        json.RawMessage `json:"error"`
		ContentBlock *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content_block"`
		Delta *struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	_ = json.Unmarshal(ev.Data, &env)
	typ := env.Type
	if typ == "" {
		typ = ev.Name
	}

	if ev.Name == "error" || typ == "error" {
		if len(env.Error) > 0 && string(env.Error) != "null" {
			return passFailure, failureFromErrorField(env.Error)
		}
		return passFailure, &ir.StreamFailure{Message: "upstream stream failed"}
	}

	switch ev.Name {
	case "message_stop":
		return passTerminal, nil
	case "message_start", "ping", "content_block_stop", "message_delta":
		return passLifecycle, nil
	}
	switch typ {
	case "message_stop":
		return passTerminal, nil
	case "message_start", "ping", "content_block_stop", "message_delta":
		return passLifecycle, nil
	case "content_block_start":
		if env.ContentBlock != nil && env.ContentBlock.Type == "text" && env.ContentBlock.Text == "" {
			return passLifecycle, nil
		}
		return passVisible, nil
	case "content_block_delta":
		if env.Delta != nil && env.Delta.Text == "" && env.Delta.PartialJSON == "" && (env.Delta.Type == "text_delta" || env.Delta.Type == "") {
			return passLifecycle, nil
		}
		return passVisible, nil
	}
	return passVisible, nil
}

func failureFromErrorField(raw json.RawMessage) *ir.StreamFailure {
	if msg := jsonFieldString(raw); msg != "" {
		return &ir.StreamFailure{Message: msg}
	}
	var obj struct {
		Type    json.RawMessage `json:"type"`
		Code    json.RawMessage `json:"code"`
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return &ir.StreamFailure{Message: "upstream stream failed"}
	}
	sf := &ir.StreamFailure{
		Type:    jsonFieldString(obj.Type),
		Code:    jsonFieldString(obj.Code),
		Message: jsonFieldString(obj.Message),
	}
	if sf.Message == "" {
		sf.Message = "upstream stream failed"
	}
	return sf
}

func jsonFieldString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return ""
}

func emptyOrAbsentJSONArray(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return false
	}
	return len(arr) == 0
}

func jsonValuePresent(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	s := string(raw)
	return s != "null" && s != `""` && s != "[]" && s != "{}"
}
