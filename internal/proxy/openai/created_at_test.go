package openai

import (
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestDecodeResponseCreatedAt(t *testing.T) {
	positive, err := DecodeResponse([]byte(`{"id":"chatcmpl-1","created":1710000333,"model":"m","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if positive.CreatedAt != 1710000333 {
		t.Fatalf("created = %d, want 1710000333", positive.CreatedAt)
	}

	absent, err := DecodeResponse([]byte(`{"id":"chatcmpl-2","model":"m","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if absent.CreatedAt != 0 {
		t.Fatalf("absent created = %d, want 0", absent.CreatedAt)
	}

	for _, body := range []string{
		`{"id":"chatcmpl-3","created":0,"model":"m","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl-4","created":-5,"model":"m","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`,
	} {
		resp, err := DecodeResponse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if resp.CreatedAt != 0 {
			t.Fatalf("non-positive created = %d for %s", resp.CreatedAt, body)
		}
	}
}

func TestDecodeResponseNonIntegerCreatedStillRejects(t *testing.T) {
	_, err := DecodeResponse([]byte(`{"id":"c","created":"1710000333","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`))
	if err == nil {
		t.Fatal("string created should still reject the unary body")
	}
}

func TestDecodeStreamCreatedAtFromRolePreamble(t *testing.T) {
	body := sseData(
		`{"id":"chatcmpl-1","model":"grok-4.7","created":1710000111,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"created":99,"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
		`{"created":0,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`not-json`,
		`[DONE]`,
	)
	out, err := decodeStreamEvents(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 1 || out[0].Kind != ir.EventMessageStart {
		t.Fatalf("events = %+v", out)
	}
	if out[0].CreatedAt != 1710000111 || out[0].ID != "chatcmpl-1" || out[0].Model != "grok-4.7" {
		t.Fatalf("start = %+v", out[0])
	}
	if out[1].Kind != ir.EventTextDelta || out[1].Text != "ok" {
		t.Fatalf("text = %+v", out[1])
	}
}

func TestDecodeStreamCreatedAtFromUsagePreambleDoesNotStart(t *testing.T) {
	only := sseData(
		`{"id":"chatcmpl-u","model":"grok-4.7","created":1710000111,"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":0}}`,
		`[DONE]`,
	)
	out, err := decodeStreamEvents(t, only)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("usage preamble started a turn: %+v", out)
	}

	roleOnly := sseData(
		`{"id":"chatcmpl-r","created":1710000111,"choices":[{"delta":{"role":"assistant"}}]}`,
		`[DONE]`,
	)
	out, err = decodeStreamEvents(t, roleOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("role preamble started a turn: %+v", out)
	}

	body := sseData(
		`{"id":"chatcmpl-u","model":"grok-4.7","created":1710000111,"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":0}}`,
		`{"choices":[{"index":0,"delta":{"content":"ok"}}]}`,
		`[DONE]`,
	)
	out, err = decodeStreamEvents(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 2 || out[0].Kind != ir.EventMessageStart || out[0].CreatedAt != 1710000111 || out[1].Text != "ok" {
		t.Fatalf("events = %+v", out)
	}
}

func TestDecodeStreamMalformedCreatedDoesNotDropChunk(t *testing.T) {
	body := sseData(
		`{"id":"chatcmpl-1","created":1710000111,"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"created":"bad","choices":[{"delta":{"content":"ok"}}]}`,
		`{"created":{"n":1},"choices":[{"delta":{"content":"!"}}]}`,
		`{"created":1.5,"choices":[{"delta":{"content":"?"}}]}`,
		`{"created":null,"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	out, err := decodeStreamEvents(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, ev := range out {
		if ev.Kind == ir.EventTextDelta {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "ok!?" {
		t.Fatalf("text = %q, events = %+v", text.String(), out)
	}
	if out[0].Kind != ir.EventMessageStart || out[0].CreatedAt != 1710000111 {
		t.Fatalf("start = %+v", out[0])
	}
}

func TestDecodeStreamStringCreatedIsUnspecified(t *testing.T) {
	body := sseData(`{"id":"c","created":"1710000111","choices":[{"delta":{"content":"ok"}}]}`)
	out, err := decodeStreamEvents(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 2 || out[0].CreatedAt != 0 || out[1].Text != "ok" {
		t.Fatalf("events = %+v", out)
	}
}
