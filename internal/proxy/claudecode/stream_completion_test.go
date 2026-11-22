package claudecode

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
)

func claudeCompletionEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

func claudeCompletionStart() string {
	return claudeCompletionEvent("message_start", `{"type":"message_start","message":{"id":"msg_cc","model":"m","usage":{"input_tokens":2}}}`)
}

func claudeClosedText() string {
	return claudeCompletionStart() +
		claudeCompletionEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		claudeCompletionEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`) +
		claudeCompletionEvent("content_block_stop", `{"type":"content_block_stop","index":0}`)
}

func claudeTerminal(reason string) string {
	return claudeCompletionEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+reason+`"},"usage":{"output_tokens":1}}`)
}

func collectClaude(t *testing.T, body string) ([]ir.StreamEvent, error) {
	t.Helper()
	var events []ir.StreamEvent
	err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	return events, err
}

func claudeFinishCount(events []ir.StreamEvent) int {
	n := 0
	for _, ev := range events {
		if ev.Kind == ir.EventFinish {
			n++
		}
	}
	return n
}

func TestClaudeCodeDecodeStreamStrictCompletion(t *testing.T) {
	events, err := collectClaude(t, claudeClosedText()+claudeTerminal("end_turn")+claudeCompletionEvent("message_stop", `{"type":"message_stop"}`))
	if err != nil || claudeFinishCount(events) != 1 {
		t.Fatalf("message_stop err=%v events=%+v", err, events)
	}
	events, err = collectClaude(t, claudeClosedText()+claudeTerminal("max_tokens"))
	if err != nil || claudeFinishCount(events) != 1 || events[len(events)-1].StopReason != ir.StopMaxTokens {
		t.Fatalf("terminal eof err=%v events=%+v", err, events)
	}
	events, err = collectClaude(t, claudeCompletionStart())
	if err == nil || !strings.Contains(err.Error(), "stream ended before completion") || claudeFinishCount(events) != 0 {
		t.Fatalf("start only err=%v events=%+v", err, events)
	}
	events, err = collectClaude(t, claudeClosedText())
	if err == nil || !strings.Contains(err.Error(), "stream ended before completion") || claudeFinishCount(events) != 0 {
		t.Fatalf("text only err=%v events=%+v", err, events)
	}
	openTool := claudeCompletionStart() +
		claudeCompletionEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`) +
		claudeCompletionEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1"}}`) +
		claudeTerminal("tool_use")
	events, err = collectClaude(t, openTool)
	if err == nil || !strings.Contains(err.Error(), "stream ended with an open content block") || claudeFinishCount(events) != 0 {
		t.Fatalf("open tool err=%v events=%+v", err, events)
	}
	events, err = collectClaude(t, "")
	if err != nil || len(events) != 0 {
		t.Fatalf("empty err=%v events=%+v", err, events)
	}
	events, err = collectClaude(t, claudeCompletionEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"PAYLOAD_SECRET_4A"}}`))
	if err == nil || strings.Contains(err.Error(), "PAYLOAD_SECRET_4A") || claudeFinishCount(events) != 0 {
		t.Fatalf("orphan err=%v events=%+v", err, events)
	}
	defaultEvents := []ir.StreamEvent{}
	defaultErr := anthropic.DecodeStream(strings.NewReader(claudeCompletionStart()), func(ev ir.StreamEvent) error {
		defaultEvents = append(defaultEvents, ev)
		return nil
	})
	if defaultErr != nil || claudeFinishCount(defaultEvents) != 1 {
		t.Fatalf("generic decoder changed start-only behavior: %v %+v", defaultErr, defaultEvents)
	}
}

func TestClaudeCodeDecodeStreamStrictBlockLifecycle(t *testing.T) {
	tool := claudeCompletionStart() +
		claudeCompletionEvent("content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`) +
		claudeCompletionEvent("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"SF\"}"}}`)
	redacted := claudeCompletionStart() + claudeCompletionEvent("content_block_start", `{"index":0,"content_block":{"type":"redacted_thinking","data":"opaque"}}`)
	for _, tc := range []struct {
		name, body, wantErr string
	}{
		{"missing-start-index", claudeCompletionStart() + claudeCompletionEvent("content_block_start", `{"content_block":{"type":"text","text":"PAYLOAD_SECRET_4A"}}`) + claudeTerminal("end_turn"), "malformed content block index"},
		{"null-delta-index", tool + claudeCompletionEvent("content_block_delta", `{"index":null,"delta":{"type":"input_json_delta","partial_json":"PAYLOAD_SECRET_4A"}}`) + claudeTerminal("tool_use"), "malformed content block index"},
		{"missing-stop-index", tool + claudeCompletionEvent("content_block_stop", `{"note":"PAYLOAD_SECRET_4A"}`) + claudeTerminal("tool_use"), "malformed content block index"},
		{"null-stop-index", tool + claudeCompletionEvent("content_block_stop", `{"index":null,"note":"PAYLOAD_SECRET_4A"}`) + claudeTerminal("tool_use"), "malformed content block index"},
		{"open-redacted-eof", redacted + claudeTerminal("end_turn"), "stream ended with an open content block"},
		{"open-redacted-stop", redacted + claudeCompletionEvent("message_stop", `{}`), "stream ended with an open content block"},
		{"reused-redacted", redacted + claudeCompletionEvent("content_block_stop", `{"index":0}`) + claudeCompletionEvent("content_block_start", `{"index":0,"content_block":{"type":"redacted_thinking","data":"PAYLOAD_SECRET_4A"}}`), "redacted thinking block restarted after stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events, err := collectClaude(t, tc.body)
			sf, ok := ir.AsStreamFailure(err)
			if !ok || sf.Type != "invalid_request_error" || sf.Message != tc.wantErr || claudeFinishCount(events) != 0 {
				t.Fatalf("err=%v events=%+v", err, events)
			}
			if strings.Contains(err.Error(), "PAYLOAD_SECRET_4A") {
				t.Fatalf("error leaked payload: %v", err)
			}
			for _, ev := range events {
				if strings.Contains(ev.Text+ev.ArgsFrag+ev.Data, "PAYLOAD_SECRET_4A") {
					t.Fatalf("invalid event emitted: %+v", ev)
				}
			}
		})
	}
	for _, ending := range []string{claudeTerminal("max_tokens"), claudeTerminal("max_tokens") + claudeCompletionEvent("message_stop", `{}`)} {
		events, err := collectClaude(t, redacted+claudeCompletionEvent("content_block_stop", `{"index":0}`)+ending)
		if err != nil || claudeFinishCount(events) != 1 || events[len(events)-1].StopReason != ir.StopMaxTokens {
			t.Fatalf("closed redacted: err=%v events=%+v", err, events)
		}
	}
}

func TestClaudeCodeDecodeStreamDoesNotFinishOnFailure(t *testing.T) {
	prefix := claudeClosedText() + claudeTerminal("end_turn")
	var events []ir.StreamEvent
	err := DecodeStream(&claudeSuffixError{r: strings.NewReader(prefix), err: errors.New("upstream read failed")}, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err == nil || err.Error() != "upstream read failed" || claudeFinishCount(events) != 0 {
		t.Fatalf("read failure err=%v events=%+v", err, events)
	}
	events = nil
	err = DecodeStream(strings.NewReader(prefix+claudeCompletionEvent("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	sf, ok := ir.AsStreamFailure(err)
	if !ok || sf.Message != "Overloaded" || claudeFinishCount(events) != 0 {
		t.Fatalf("provider error err=%v events=%+v", err, events)
	}
	events = nil
	err = DecodeStream(&claudeSuffixError{r: strings.NewReader(claudeCompletionStart()), err: context.Canceled}, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if !errors.Is(err, context.Canceled) || claudeFinishCount(events) != 0 {
		t.Fatalf("cancel err=%v events=%+v", err, events)
	}
}

type claudeSuffixError struct {
	r    io.Reader
	err  error
	done bool
}

func (s *claudeSuffixError) Read(p []byte) (int, error) {
	if s.done {
		return 0, s.err
	}
	n, err := s.r.Read(p)
	if err == io.EOF {
		s.done = true
		if n > 0 {
			return n, nil
		}
		return 0, s.err
	}
	return n, err
}
