package cursor

import (
	"bytes"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

// frame builds a Connect-RPC frame (uncompressed) wrapping the given protobuf.
func frame(t *testing.T, proto []byte) []byte {
	t.Helper()
	return wrapConnectFrame(proto, false)
}

// textFrame builds a response frame whose field 2 (StreamUnifiedChatResponse)
// carries the given text in field 1.
func textFrame(t *testing.T, text string) []byte {
	t.Helper()
	resp := encodeField(respText, wireLen, text)
	top := encodeField(respResponse, wireLen, resp)
	return frame(t, top)
}

// thinkingFrame builds a response frame whose field 2 carries thinking text in
// field 25 -> field 1.
func thinkingFrame(t *testing.T, thinking string) []byte {
	t.Helper()
	th := encodeField(thinkingText, wireLen, thinking)
	resp := encodeField(respThinking, wireLen, th)
	top := encodeField(respResponse, wireLen, resp)
	return frame(t, top)
}

// toolCallFrame builds a ClientSideToolV2Call (field 1) with id, name, args, and
// nested mcp_params.tools[0].{name,params}.
func toolCallFrame(t *testing.T, id, name, args string, isLast bool) []byte {
	t.Helper()
	tool := concatBytes(
		encodeField(mcpNestedName, wireLen, name),
		encodeField(mcpNestedParams, wireLen, args),
	)
	params := encodeField(mcpToolsList, wireLen, tool)
	var call []byte
	call = append(call, encodeField(toolID, wireLen, id)...)
	call = append(call, encodeField(toolMCPParams, wireLen, params)...)
	if isLast {
		call = append(call, encodeField(toolIsLast, wireVarint, uint64(1))...)
	}
	top := encodeField(respToolCall, wireLen, call)
	return frame(t, top)
}

func collectEvents(t *testing.T, frames ...[]byte) []ir.StreamEvent {
	t.Helper()
	var out []ir.StreamEvent
	err := DecodeStream(bytes.NewReader(bytes.Join(frames, nil)), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}
	return out
}

func TestDecodeStreamTextDeltas(t *testing.T) {
	evs := collectEvents(t, textFrame(t, "hel"), textFrame(t, "lo"))
	// Expect MessageStart, two TextDeltas, Finish.
	if len(evs) != 4 {
		t.Fatalf("events = %d, want 4: %+v", len(evs), evs)
	}
	if evs[0].Kind != ir.EventMessageStart {
		t.Errorf("ev0 = %v, want MessageStart", evs[0].Kind)
	}
	if evs[1].Kind != ir.EventTextDelta || evs[1].Text != "hel" {
		t.Errorf("ev1 = %+v, want text hel", evs[1])
	}
	if evs[2].Kind != ir.EventTextDelta || evs[2].Text != "lo" {
		t.Errorf("ev2 = %+v, want text lo", evs[2])
	}
	if evs[3].Kind != ir.EventFinish || evs[3].StopReason != ir.StopEndTurn {
		t.Errorf("ev3 = %+v, want Finish/EndTurn", evs[3])
	}
}

func TestDecodeStreamToolCallReassembly(t *testing.T) {
	// Two fragments for the same tool id, then is_last on the second.
	evs := collectEvents(t,
		toolCallFrame(t, "tc1", "mcp_custom_Write", `{"path":"`, false),
		toolCallFrame(t, "tc1", "mcp_custom_Write", `foo.txt"}`, true),
	)
	// MessageStart, ToolCallStart, ToolCallDelta(full first), ToolCallDelta(second), Finish(ToolUse)
	if len(evs) < 2 {
		t.Fatalf("events = %d: %+v", len(evs), evs)
	}
	var starts, deltas int
	var firstName string
	for _, ev := range evs {
		if ev.Kind == ir.EventToolCallStart {
			starts++
			firstName = ev.ToolName
		}
		if ev.Kind == ir.EventToolCallDelta {
			deltas++
		}
	}
	if starts != 1 {
		t.Errorf("tool starts = %d, want 1", starts)
	}
	if firstName != "Write" {
		t.Errorf("tool name = %q, want Write (mcp_custom_ stripped)", firstName)
	}
	if deltas != 2 {
		t.Errorf("tool deltas = %d, want 2", deltas)
	}
	var finish *ir.StreamEvent
	for i := range evs {
		if evs[i].Kind == ir.EventFinish {
			finish = &evs[i]
		}
	}
	if finish == nil || finish.StopReason != ir.StopToolUse {
		t.Errorf("finish = %+v, want ToolUse", finish)
	}
}

func TestDecodeStreamResourceExhaustedBeforeContent(t *testing.T) {
	jsonErr := []byte(`{"error":{"code":"resource_exhausted","message":"too many requests"}}`)
	raw := wrapConnectFrame(jsonErr, false)
	var out []ir.StreamEvent
	err := DecodeStream(bytes.NewReader(raw), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	if err == nil {
		t.Fatal("want rate-limit error before any content")
	}
	if !contains(err.Error(), "rate limited") {
		t.Errorf("err = %q, want rate limited", err.Error())
	}
}

func TestDecodeStreamResourceExhaustedAfterContentFinishes(t *testing.T) {
	jsonErr := []byte(`{"error":{"code":"resource_exhausted","message":"too many requests"}}`)
	raw := bytes.Join([][]byte{textFrame(t, "partial"), wrapConnectFrame(jsonErr, false)}, nil)
	var out []ir.StreamEvent
	err := DecodeStream(bytes.NewReader(raw), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	if err == nil {
		t.Fatal("after content, error frame must still return an error")
	}
	if !contains(err.Error(), "rate limited") {
		t.Errorf("err = %q, want rate limited", err.Error())
	}
	for _, ev := range out {
		if ev.Kind == ir.EventFinish {
			t.Fatalf("must not emit Finish after error frame: %+v", out)
		}
	}
	// Partial content should still have been emitted before the error.
	sawText := false
	for _, ev := range out {
		if ev.Kind == ir.EventTextDelta && ev.Text == "partial" {
			sawText = true
		}
	}
	if !sawText {
		t.Fatalf("want partial text before error, got %+v", out)
	}
}

func TestDecodeStreamComposerThinkingSplit(t *testing.T) {
	// Composer-style: thinking contains reasoning then </think> then visible text.
	evs := collectEvents(t,
		thinkingFrame(t, "reasoning here"),
		thinkingFrame(t, "</think>final answer"),
	)
	var text string
	for _, ev := range evs {
		if ev.Kind == ir.EventTextDelta {
			text += ev.Text
		}
	}
	if text != "final answer" {
		t.Errorf("composer text = %q, want 'final answer'", text)
	}
}

func TestDecodeStreamNonComposerThinkingDropped(t *testing.T) {
	// Thinking without a  tag must not emit any text delta.
	evs := collectEvents(t, thinkingFrame(t, "just reasoning, no close tag"))
	for _, ev := range evs {
		if ev.Kind == ir.EventTextDelta {
			t.Errorf("non-composer thinking leaked as text: %q", ev.Text)
		}
	}
}

func TestDecodeStreamEmptyStream(t *testing.T) {
	evs := collectEvents(t) // no frames
	// Should still emit MessageStart + Finish so unary collect doesn't hang.
	if len(evs) < 2 {
		t.Fatalf("events = %d, want >=2: %+v", len(evs), evs)
	}
	if evs[0].Kind != ir.EventMessageStart {
		t.Errorf("ev0 = %v, want MessageStart", evs[0].Kind)
	}
	if evs[len(evs)-1].Kind != ir.EventFinish {
		t.Errorf("last = %v, want Finish", evs[len(evs)-1].Kind)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && bytesContains(s, sub)
}

func bytesContains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}

func TestParseCursorError(t *testing.T) {
	t.Run("non-json raw fallback", func(t *testing.T) {
		// Raw bodies must not be embedded (terminal logs stay metadata-only).
		err := parseCursorError([]byte("Internal Server Error"))
		if err.Error() != "upstream stream failed" {
			t.Errorf("got %q, want generic failure", err.Error())
		}
		if strings.Contains(err.Error(), "Internal Server Error") {
			t.Errorf("raw body leaked into error: %q", err.Error())
		}
	})

	t.Run("direct message", func(t *testing.T) {
		body := []byte(`{"error":{"code":"internal","message":"boom"}}`)
		err := parseCursorError(body)
		if err.Error() != "cursor: boom" {
			t.Errorf("got %q, want cursor: boom", err.Error())
		}
	})

	t.Run("falls back to detail title when message empty", func(t *testing.T) {
		body := []byte(`{"error":{"code":"x","message":"","details":[{"debug":{"details":{"title":"ttl"}}}]}}`)
		err := parseCursorError(body)
		if err.Error() != "cursor: ttl" {
			t.Errorf("got %q, want cursor: ttl", err.Error())
		}
	})

	t.Run("falls back to detail detail when title empty", func(t *testing.T) {
		body := []byte(`{"error":{"code":"x","message":"","details":[{"debug":{"details":{"title":"","detail":"explanation"}}}]}}`)
		err := parseCursorError(body)
		if err.Error() != "cursor: explanation" {
			t.Errorf("got %q, want cursor: explanation", err.Error())
		}
	})

	t.Run("resource exhausted with message", func(t *testing.T) {
		body := []byte(`{"error":{"code":"resource_exhausted","message":"slow down"}}`)
		err := parseCursorError(body)
		if !strings.Contains(err.Error(), "rate limited") {
			t.Errorf("got %q, want rate limited", err.Error())
		}
		if !strings.Contains(err.Error(), "slow down") {
			t.Errorf("got %q, want message preserved", err.Error())
		}
	})

	t.Run("resource exhausted without message defaults", func(t *testing.T) {
		body := []byte(`{"error":{"code":"resource_exhausted","message":""}}`)
		err := parseCursorError(body)
		if !strings.Contains(err.Error(), "rate limit exceeded") {
			t.Errorf("got %q, want default rate limit message", err.Error())
		}
	})

	t.Run("empty message no details generic fallback", func(t *testing.T) {
		body := []byte(`{"error":{"code":"x","message":""}}`)
		err := parseCursorError(body)
		if err.Error() != "upstream stream failed" {
			t.Errorf("got %q, want generic failure", err.Error())
		}
	})
}

func TestDecloakToolName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"mcp_custom_get_weather", "get_weather"},
		{"mcp_custom_", ""},
		{"plain_tool", "plain_tool"},
		{"", ""},
		{"mcp_custom", "mcp_custom"}, // prefix without trailing underscore stays
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := decloakToolName(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
