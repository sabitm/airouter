package responses

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func TestEncodeResponseCreatedAt(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{
		ID: "resp_1", Model: "m", CreatedAt: 1710000333,
		Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := mustNumericCreatedAt(t, out); got != 1710000333 {
		t.Fatalf("created_at = %d, body = %s", got, out)
	}

	before := time.Now().Unix()
	out, err = EncodeResponse(&ir.Response{ID: "resp_2", Model: "m", CreatedAt: -1})
	after := time.Now().Unix()
	if err != nil {
		t.Fatal(err)
	}
	got := mustNumericCreatedAt(t, out)
	if got < before || got > after {
		t.Fatalf("fallback created_at = %d, want %d..%d, body = %s", got, before, after, out)
	}
}

func TestEncodeResponseErrorOmitsCreatedAt(t *testing.T) {
	raw := EncodeError("nope", "api_error")
	if strings.Contains(string(raw), "created_at") {
		t.Fatalf("error envelope changed: %s", raw)
	}
}

func TestEncodeStreamCreatedAtAgreesAcrossEnvelopes(t *testing.T) {
	body := encodeResponsesStream(t, []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "resp_1", Model: "m", CreatedAt: 1710000111},
		{Kind: ir.EventTextDelta, Text: "ok"},
		{Kind: ir.EventFinish, StopReason: ir.StopMaxTokens},
	}, false)
	got := responseEnvelopeTimes(t, body)
	if got["response.created"] != 1710000111 || got["response.incomplete"] != 1710000111 {
		t.Fatalf("timestamps = %v body=%s", got, body)
	}
	if _, ok := got["response.completed"]; ok {
		t.Fatalf("max-token finish emitted completed: %s", body)
	}
}

func TestEncodeStreamCloseUsesSameCreatedAt(t *testing.T) {
	body := encodeResponsesStream(t, []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "resp_1", Model: "m", CreatedAt: 1710000111},
		{Kind: ir.EventTextDelta, Text: "ok"},
	}, true)
	got := responseEnvelopeTimes(t, body)
	if got["response.created"] != 1710000111 || got["response.completed"] != 1710000111 {
		t.Fatalf("timestamps = %v body=%s", got, body)
	}
}

func TestEncodeStreamFallbackCreatedAtStaysFixed(t *testing.T) {
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("no flusher")
	}
	enc := NewStreamEncoder("m")
	before := time.Now().Unix()
	if err := enc.Encode(ir.StreamEvent{Kind: ir.EventTextDelta, Text: "ok"}, w); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := enc.Encode(ir.StreamEvent{Kind: ir.EventMessageStart, ID: "late", CreatedAt: 42}, w); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(ir.StreamEvent{Kind: ir.EventFinish, StopReason: ir.StopEndTurn}, w); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(w); err != nil {
		t.Fatal(err)
	}
	got := responseEnvelopeTimes(t, rec.Body.String())
	created := got["response.created"]
	if created == 42 || created < before || created > before+5 {
		t.Fatalf("fallback = %d, want clock near %d; envelopes = %v", created, before, got)
	}
	if got["response.completed"] != created {
		t.Fatalf("completed refreshed the clock: %v", got)
	}
}

func TestEncodeStreamErrorOmitsCreatedAt(t *testing.T) {
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("no flusher")
	}
	enc := NewStreamEncoder("m")
	if err := enc.EncodeError(w, "nope", "api_error"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.Body.String(), "created_at") {
		t.Fatalf("error event changed: %s", rec.Body.String())
	}
}

func TestEncodeStreamUsesCreatedAtNotedBeforeQueuedEmit(t *testing.T) {
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("no flusher")
	}
	enc := NewStreamEncoder("m")
	enc.pendingEvents = []ir.StreamEvent{{Kind: ir.EventTextDelta, Text: "ok"}}
	enc.pendingBytes = 64
	if err := enc.Encode(ir.StreamEvent{Kind: ir.EventMessageStart, ID: "resp_q", CreatedAt: 1710000111}, w); err != nil {
		t.Fatal(err)
	}
	got := responseEnvelopeTimes(t, rec.Body.String())
	if got["response.created"] != 1710000111 {
		t.Fatalf("queued emit lost upstream timestamp: %v body=%s", got, rec.Body.String())
	}
}

func encodeResponsesStream(t *testing.T, events []ir.StreamEvent, closeStream bool) string {
	t.Helper()
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("no flusher")
	}
	enc := NewStreamEncoder("m")
	for _, ev := range events {
		if err := enc.Encode(ev, w); err != nil {
			t.Fatalf("encode %v: %v", ev.Kind, err)
		}
	}
	if closeStream {
		if err := enc.Close(w); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Body.String()
}

func responseEnvelopeTimes(t *testing.T, body string) map[string]int64 {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	got := map[string]int64{}
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Name {
		case "response.created", "response.completed", "response.incomplete":
			n := mustNumericCreatedAt(t, ev.Data)
			if prev, ok := got[ev.Name]; ok && prev != n {
				t.Fatalf("%s created_at changed from %d to %d", ev.Name, prev, n)
			}
			got[ev.Name] = n
		}
	}
	if len(got) == 0 {
		t.Fatalf("no response envelopes in %s", body)
	}
	var first int64
	seen := false
	for _, n := range got {
		if !seen {
			first = n
			seen = true
			continue
		}
		if n != first {
			t.Fatalf("envelopes disagree: %v body=%s", got, body)
		}
	}
	return got
}

func mustNumericCreatedAt(t *testing.T, data []byte) int64 {
	t.Helper()
	var env struct {
		CreatedAt json.RawMessage `json:"created_at"`
		Response  struct {
			CreatedAt json.RawMessage `json:"created_at"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("json: %v body=%s", err, data)
	}
	raw := env.Response.CreatedAt
	if len(raw) == 0 {
		raw = env.CreatedAt
	}
	raw = bytesTrim(raw)
	if len(raw) == 0 || raw[0] == '"' {
		t.Fatalf("created_at must be a number, got %s in %s", raw, data)
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			t.Fatalf("created_at is not a plain integer: %s", raw)
		}
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil || n <= 0 {
		t.Fatalf("created_at %s: %v", raw, err)
	}
	return n
}

func bytesTrim(raw []byte) []byte {
	for len(raw) > 0 && (raw[0] == ' ' || raw[0] == '\n' || raw[0] == '\t') {
		raw = raw[1:]
	}
	for len(raw) > 0 && (raw[len(raw)-1] == ' ' || raw[len(raw)-1] == '\n' || raw[len(raw)-1] == '\t') {
		raw = raw[:len(raw)-1]
	}
	return raw
}
