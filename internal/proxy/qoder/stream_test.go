package qoder

import (
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestDecodeStreamUnwrapsEnvelope(t *testing.T) {
	inner := `{"id":"c1","model":"auto","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`
	env, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": inner})
	sse := "data: " + string(env) + "\n\ndata: {\"statusCodeValue\":200,\"body\":\"[DONE]\"}\n\n"

	var texts []string
	var finished bool
	err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
		switch ev.Kind {
		case ir.EventTextDelta:
			texts = append(texts, ev.Text)
		case ir.EventFinish:
			finished = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(texts, "") != "hi" {
		t.Fatalf("text=%q", texts)
	}
	if !finished {
		t.Fatal("expected finish")
	}
}

func TestDecodeStreamRoleOnlyEnvelopeEmitsNothing(t *testing.T) {
	inner := `{"id":"chatcmpl-role","model":"auto","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`
	env, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": inner})
	// EOF without an explicit DONE still reaches the shared decoder as [DONE].
	sse := "data: " + string(env) + "\n\n"
	var out []ir.StreamEvent
	err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("events = %+v, want none", out)
	}
}

func TestDecodeStreamExplicitEmptyFinishKeepsMetadata(t *testing.T) {
	role := `{"id":"chatcmpl-empty","model":"auto","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`
	finish := `{"choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":6,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":2,"cache_write_tokens":1}}}`
	var frames []string
	for _, inner := range []string{role, finish} {
		env, err := json.Marshal(map[string]any{"statusCodeValue": 200, "body": inner})
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, "data: "+string(env))
	}
	sse := strings.Join(frames, "\n\n") + "\n\n"
	var out []ir.StreamEvent
	err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Kind != ir.EventMessageStart || out[1].Kind != ir.EventFinish {
		t.Fatalf("events = %+v", out)
	}
	if out[0].ID != "chatcmpl-empty" || out[0].Model != "auto" {
		t.Fatalf("metadata = %q/%q", out[0].ID, out[0].Model)
	}
	if out[1].StopReason != ir.StopMaxTokens || out[1].InputTokens != 6 || out[1].CacheReadTokens != 2 || out[1].CacheWriteTokens != 1 {
		t.Fatalf("finish = %+v", out[1])
	}
}

func TestTruncate(t *testing.T) {
	t.Run("short unchanged", func(t *testing.T) {
		if got := truncate("short", 100); got != "short" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("over limit truncates with marker", func(t *testing.T) {
		s := strings.Repeat("x", 50)
		got := truncate(s, 10)
		if !strings.HasPrefix(got, "xxxxxxxxxx") {
			t.Errorf("expected first 10 bytes, got %q", got)
		}
		if !strings.HasSuffix(got, "...") {
			t.Errorf("expected ... suffix, got %q", got)
		}
	})
	t.Run("empty unchanged", func(t *testing.T) {
		if got := truncate("", 10); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("exactly at limit unchanged", func(t *testing.T) {
		s := strings.Repeat("x", 10)
		if got := truncate(s, 10); got != s {
			t.Errorf("got %q, want %q", got, s)
		}
	})
}
