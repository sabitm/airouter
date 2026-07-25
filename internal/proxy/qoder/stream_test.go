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
