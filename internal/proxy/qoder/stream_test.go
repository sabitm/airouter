package qoder

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"airouter/internal/proxy/ir"
)

// qoderEnv marshals one {statusCodeValue, body} SSE data line. status and body
// are pre-encoded JSON values so tests can use numbers, strings, or objects.
func qoderEnv(t *testing.T, status, body string) string {
	t.Helper()
	return `{"statusCodeValue":` + status + `,"body":` + body + `}`
}

func qoderInnerChunk(t *testing.T, inner string) string {
	t.Helper()
	enc, err := json.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	return string(enc)
}

func decodeQoderEvents(t *testing.T, sse string) []ir.StreamEvent {
	t.Helper()
	var out []ir.StreamEvent
	if err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	}); err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}
	return out
}

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

// --- Q01: envelope type variants ---

func TestDecodeStreamNumericStringStatus(t *testing.T) {
	t.Run("success carries content", func(t *testing.T) {
		inner := `{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`
		sse := "data: " + qoderEnv(t, `"200"`, qoderInnerChunk(t, inner)) + "\n\n"
		out := decodeQoderEvents(t, sse)
		if textOf(out) != "ok" {
			t.Fatalf("events = %+v", out)
		}
	})
	t.Run("error keeps structured status", func(t *testing.T) {
		inner := `{"code":"112","message":"quota exhausted"}`
		sse := "data: " + qoderEnv(t, `"403"`, qoderInnerChunk(t, inner)) + "\n\n"
		var out []ir.StreamEvent
		err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
			out = append(out, ev)
			return nil
		})
		sf, ok := ir.AsStreamFailure(err)
		if !ok {
			t.Fatalf("err = %v, want StreamFailure", err)
		}
		if sf.Status != 403 || sf.Type != "quota_error" {
			t.Fatalf("failure = %+v", sf)
		}
		if len(out) != 0 {
			t.Fatalf("events = %+v, want none", out)
		}
	})
}

func TestDecodeStreamObjectBody(t *testing.T) {
	t.Run("object success body carries content", func(t *testing.T) {
		env := `{"statusCodeValue":200,"body":{"choices":[{"index":0,"delta":{"content":"obj"},"finish_reason":null}]}}`
		out := decodeQoderEvents(t, "data: "+env+"\n\n")
		if textOf(out) != "obj" {
			t.Fatalf("events = %+v", out)
		}
	})
	t.Run("object error body classifies", func(t *testing.T) {
		env := `{"statusCodeValue":429,"body":{"code":"10605","message":"queued"}}`
		_, err := decodeQoderFailing(t, "data: "+env+"\n\n")
		sf, ok := ir.AsStreamFailure(err)
		if !ok {
			t.Fatalf("err = %v, want StreamFailure", err)
		}
		if sf.Status != 429 || sf.Code != "10605" {
			t.Fatalf("failure = %+v", sf)
		}
	})
}

func TestDecodeStreamMalformedEnvelopeFails(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"non-numeric status", `{"statusCodeValue":"abc","body":"{}"}`, "malformed envelope status"},
		{"object status", `{"statusCodeValue":{},"body":"{}"}`, "malformed envelope status"},
		{"number body", `{"statusCodeValue":200,"body":5}`, "malformed envelope body"},
		{"boolean body", `{"statusCodeValue":200,"body":true}`, "malformed envelope body"},
		{"non-envelope json", `{"foo":"bar"}`, "unrecognized stream frame"},
		{"array frame", `[1,2,3]`, "unrecognized stream frame"},
		{"garbage frame", `not json at all`, "unrecognized stream frame"},
		{"non-json inner", `{"statusCodeValue":200,"body":"keepalive"}`, "not a chat chunk"},
		{"array inner", `{"statusCodeValue":200,"body":"[1]"}`, "not a chat chunk"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeQoderFailing(t, "data: "+tc.line+"\n\n")
			if err == nil {
				t.Fatal("expected structured failure")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDecodeStreamMalformedEnvelopeAfterContentStillFails(t *testing.T) {
	content := `{"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`
	sse := "data: " + qoderEnv(t, "200", qoderInnerChunk(t, content)) + "\n\n" +
		"data: " + `{"statusCodeValue":"oops","body":"{}"}` + "\n\n"
	var out []ir.StreamEvent
	err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "malformed envelope status") {
		t.Fatalf("err = %v, want malformed envelope status", err)
	}
	if textOf(out) != "a" {
		t.Fatalf("events = %+v, want the content before the failure", out)
	}
}

func TestDecodeStreamNullStatusIsGenericFailure(t *testing.T) {
	// The reference maps an uninterpretable status to zero and errors; the
	// failure carries no usable embedded status, so the caller default stands.
	_, err := decodeQoderFailing(t, "data: "+`{"statusCodeValue":null,"body":"{}"}`+"\n\n")
	sf, ok := ir.AsStreamFailure(err)
	if !ok || sf.Status != 0 {
		t.Fatalf("err = %v (%+v), want generic failure without embedded status", err, sf)
	}
}

// textOf joins the visible text deltas of a decode.
func textOf(events []ir.StreamEvent) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Kind == ir.EventTextDelta {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

// decodeQoderFailing expects the decode to fail and returns the error.
func decodeQoderFailing(t *testing.T, sse string) ([]ir.StreamEvent, error) {
	t.Helper()
	var out []ir.StreamEvent
	err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	return out, err
}

// --- Q02: finish-reason locations ---

func TestDecodeStreamFinishReasonLocations(t *testing.T) {
	cases := []struct {
		name  string
		inner string
		want  ir.StopReason
	}{
		{
			"choice-level",
			`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
			ir.StopMaxTokens,
		},
		{
			"delta-level with content",
			`{"choices":[{"index":0,"delta":{"content":"partial","finish_reason":"length"}}]}`,
			ir.StopMaxTokens,
		},
		{
			"delta-level tool_calls",
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":""}}],"finish_reason":"tool_calls"}}]}`,
			ir.StopToolUse,
		},
		{
			"top-level only",
			`{"finish_reason":"length","choices":[{"index":0,"delta":{}}]}`,
			ir.StopMaxTokens,
		},
		{
			"top-level with no choices",
			`{"finish_reason":"stop"}`,
			ir.StopEndTurn,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := decodeQoderEvents(t, "data: "+qoderEnv(t, "200", qoderInnerChunk(t, tc.inner))+"\n\n")
			var finish *ir.StreamEvent
			for i := range out {
				if out[i].Kind == ir.EventFinish {
					finish = &out[i]
				}
			}
			if finish == nil {
				t.Fatalf("events = %+v, want finish", out)
			}
			if finish.StopReason != tc.want {
				t.Fatalf("stop reason = %q, want %q", finish.StopReason, tc.want)
			}
		})
	}
}

// --- Q03: usage aliases and cache fields ---

// usageFinishOf returns the last EventFinish event of a decode.
func usageFinishOf(t *testing.T, events []ir.StreamEvent) ir.StreamEvent {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == ir.EventFinish {
			return events[i]
		}
	}
	t.Fatalf("no finish event in %+v", events)
	return ir.StreamEvent{}
}

func TestDecodeStreamUsageNormalizationRegressions(t *testing.T) {
	// Unsupported values are removed (never truncated or overflowed); a
	// response-bearing chunk survives with its remaining counts.
	invalidCases := []struct {
		name            string
		inner           string
		wantIn, wantOut int
	}{
		{"fractional string", `{"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":"stop"}],"usage":{"prompt_tokens":"10.5","completion_tokens":2}}`, 0, 2},
		{"non-finite strings", `{"choices":[{"index":0,"delta":{"content":"b"},"finish_reason":"stop"}],"usage":{"prompt_tokens":"NaN","completion_tokens":"+Inf"}}`, 0, 0},
		{"overflow and negative", `{"choices":[{"index":0,"delta":{"content":"c"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1e30,"completion_tokens":-4}}`, 0, 0},
		{"negative string", `{"choices":[{"index":0,"delta":{"content":"d"},"finish_reason":"stop"}],"usage":{"prompt_tokens":"-1","completion_tokens":5}}`, 0, 5},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			var out []ir.StreamEvent
			err := DecodeStream(strings.NewReader("data: "+qoderEnv(t, "200", qoderInnerChunk(t, tc.inner))+"\n\n"), func(ev ir.StreamEvent) error {
				out = append(out, ev)
				return nil
			})
			if err != nil {
				t.Fatalf("%v", err)
			}
			if len(out) == 0 || out[len(out)-1].Kind != ir.EventFinish {
				t.Fatalf("events = %+v", out)
			}
			if textOf(out) == "" {
				t.Fatal("content dropped")
			}
			f := out[len(out)-1]
			if f.InputTokens != tc.wantIn || f.OutputTokens != tc.wantOut {
				t.Fatalf("finish = %+v, want %d/%d", f, tc.wantIn, tc.wantOut)
			}
		})
	}
	t.Run("null details with cache alias must not panic and keeps alias", func(t *testing.T) {
		inner := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":null,"cache_read_input_tokens":4}}`
		out := decodeQoderEvents(t, "data: "+qoderEnv(t, "200", qoderInnerChunk(t, inner))+"\n\n")
		f := usageFinishOf(t, out)
		if f.InputTokens != 10 || f.OutputTokens != 2 || f.CacheReadTokens != 4 || f.CacheWriteTokens != 0 {
			t.Fatalf("finish = %+v", f)
		}
	})
	t.Run("numeric-string standard counts and detail strings become integers", func(t *testing.T) {
		inner := `{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":"10","completion_tokens":"2","total_tokens":"12","prompt_tokens_details":{"cached_tokens":"3","cache_write_tokens":"1"}}}`
		out := decodeQoderEvents(t, "data: "+qoderEnv(t, "200", qoderInnerChunk(t, inner))+"\n\n")
		if textOf(out) != "hi" {
			t.Fatalf("events = %+v, want content preserved alongside usage", out)
		}
		f := usageFinishOf(t, out)
		if f.InputTokens != 10 || f.OutputTokens != 2 || f.CacheReadTokens != 3 || f.CacheWriteTokens != 1 {
			t.Fatalf("finish = %+v", f)
		}
	})
	t.Run("nested creation count maps to cache write", func(t *testing.T) {
		inner := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cache_creation_tokens":4}}}`
		out := decodeQoderEvents(t, "data: "+qoderEnv(t, "200", qoderInnerChunk(t, inner))+"\n\n")
		f := usageFinishOf(t, out)
		if f.CacheWriteTokens != 4 || f.CacheReadTokens != 0 {
			t.Fatalf("finish = %+v", f)
		}
	})
	t.Run("nested alias for cache read maps to cached tokens", func(t *testing.T) {
		inner := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cache_read_input_tokens":5}}}`
		out := decodeQoderEvents(t, "data: "+qoderEnv(t, "200", qoderInnerChunk(t, inner))+"\n\n")
		f := usageFinishOf(t, out)
		if f.CacheReadTokens != 5 {
			t.Fatalf("finish = %+v", f)
		}
	})
	t.Run("details standard wins over nested alias without double count", func(t *testing.T) {
		inner := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":1,"cache_read_input_tokens":9}}}`
		out := decodeQoderEvents(t, "data: "+qoderEnv(t, "200", qoderInnerChunk(t, inner))+"\n\n")
		f := usageFinishOf(t, out)
		if f.CacheReadTokens != 1 {
			t.Fatalf("finish = %+v", f)
		}
	})
	t.Run("uninterpretable usage object is removed, content survives", func(t *testing.T) {
		inner := `{"choices":[{"index":0,"delta":{"content":"keep"},"finish_reason":"stop"}],"usage":[1,2,3]}`
		out := decodeQoderEvents(t, "data: "+qoderEnv(t, "200", qoderInnerChunk(t, inner))+"\n\n")
		if textOf(out) != "keep" {
			t.Fatalf("events = %+v, want content preserved", out)
		}
	})
}

// --- Q03/Q04: preliminary vs terminal usage evidence ---

// stagedStream feeds frames to the decoder with explicit delays and an
// upstream that never closes, mimicking Qoder's post-terminal keepalive.
type stagedWrite struct {
	after time.Duration
	sse   string
}

type stagedStream struct {
	pr *io.PipeReader
}

func newStagedStream(t *testing.T, writes ...stagedWrite) *stagedStream {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		for _, w := range writes {
			select {
			case <-time.After(w.after):
			case <-time.After(3 * time.Second):
				return
			}
			if _, err := pw.Write([]byte(w.sse)); err != nil {
				return
			}
		}
		// Never close: the upstream socket stays open.
	}()
	return &stagedStream{pr: pr}
}

func decodeWithTimeout(t *testing.T, r io.Reader, timeout time.Duration) ([]ir.StreamEvent, error) {
	t.Helper()
	type result struct {
		events []ir.StreamEvent
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		var out []ir.StreamEvent
		err := DecodeStream(r, func(ev ir.StreamEvent) error {
			out = append(out, ev)
			return nil
		})
		ch <- result{out, err}
	}()
	select {
	case res := <-ch:
		return res.events, res.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("decoder still blocked after %v", timeout)
	}
}

func TestDecodeStreamEarlyUsageBeforeFinishIsPreliminary(t *testing.T) {
	// Positive input with zero output on a content frame BEFORE the finish is
	// not terminal evidence: the real trailer must still win.
	content := `{"choices":[{"index":0,"delta":{"content":"x"}}],"usage":{"prompt_tokens":10,"completion_tokens":0}}`
	finish := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	trailer := `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4}}`
	sse := "data: " + qoderEnv(t, "200", qoderInnerChunk(t, content)) + "\n\n" +
		"data: " + qoderEnv(t, "200", qoderInnerChunk(t, finish)) + "\n\n" +
		"data: " + qoderEnv(t, "200", qoderInnerChunk(t, trailer)) + "\n\n"
	pipe := newStagedStream(t,
		struct {
			after time.Duration
			sse   string
		}{0, sse},
	)
	t.Cleanup(func() { pipe.pr.Close() })
	out, err := decodeWithTimeout(t, pipe.pr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	f := usageFinishOf(t, out)
	if textOf(out) != "x" || f.InputTokens != 10 || f.OutputTokens != 4 {
		t.Fatalf("events = %+v, want early usage overwritten by the real trailer", out)
	}
}

func TestDecodeStreamZeroFinishUsageIsPlaceholder(t *testing.T) {
	// finish+usage(0/0) may be a placeholder: the decoder must stay open, and
	// a later real trailer must terminate with the real counts.
	finish := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`
	trailer := `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}`
	first := "data: " + qoderEnv(t, "200", qoderInnerChunk(t, finish)) + "\n\n"
	second := "data: " + qoderEnv(t, "200", qoderInnerChunk(t, trailer)) + "\n\n"
	pr, pw := io.Pipe()
	t.Cleanup(func() { pr.Close() })
	go func() {
		_, _ = pw.Write([]byte(first))
		time.Sleep(80 * time.Millisecond)
		_, _ = pw.Write([]byte(second))
	}()
	out, err := decodeWithTimeout(t, pr, 2*time.Second)
	if err != nil {
		t.Fatalf("decoder never completed after the real trailer: %v", err)
	}
	f := usageFinishOf(t, out)
	if f.InputTokens != 10 || f.OutputTokens != 2 {
		t.Fatalf("finish = %+v, want the real trailer counts", f)
	}
}

func TestDecodeStreamZeroFinishUsageAloneDoesNotTerminate(t *testing.T) {
	// Without a real trailer the decoder must keep waiting on the open
	// upstream: a zero-count finish frame is placeholder evidence only.
	finish := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`
	first := "data: " + qoderEnv(t, "200", qoderInnerChunk(t, finish)) + "\n\n"
	trailer := `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}`
	second := "data: " + qoderEnv(t, "200", qoderInnerChunk(t, trailer)) + "\n\n"
	pr, pw := io.Pipe()
	t.Cleanup(func() { pr.Close() })
	go func() {
		_, _ = pw.Write([]byte(first))
		time.Sleep(120 * time.Millisecond)
		_, _ = pw.Write([]byte(second))
	}()
	out, err := decodeWithTimeout(t, pr, 2*time.Second)
	if err != nil {
		t.Fatalf("decoder never completed after the real trailer: %v", err)
	}
	f := usageFinishOf(t, out)
	if f.InputTokens != 3 || f.OutputTokens != 1 {
		t.Fatalf("finish = %+v, want the real trailer counts", f)
	}
}

func TestDecodeStreamMixedZeroFinishUsageStillTerminates(t *testing.T) {
	// A finish frame with a positive input and zero output is a real count,
	// not an all-zero placeholder: terminal evidence stays sufficient.
	inner := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":0}}`
	pipe := newStagedStream(t,
		struct {
			after time.Duration
			sse   string
		}{0, "data: " + qoderEnv(t, "200", qoderInnerChunk(t, inner)) + "\n\n"},
	)
	t.Cleanup(func() { pipe.pr.Close() })
	out, err := decodeWithTimeout(t, pipe.pr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	f := usageFinishOf(t, out)
	if f.InputTokens != 10 || f.OutputTokens != 0 {
		t.Fatalf("finish = %+v", f)
	}
}

// --- Q01: strict envelope status parsing ---

func TestDecodeStatusValueStrict(t *testing.T) {
	cases := []struct {
		raw    string
		want   int
		wantOK bool
	}{
		{"200", 200, true},
		{"0", 0, true},
		{"599", 599, true},
		{"\"403\"", 403, true},
		{"\" 403 \"", 403, true},
		{"10.9", 0, false},      // fractional number
		{"\"403.5\"", 0, false}, // fractional string
		{"\"10.0\"", 10, true},  // integral value in float form is the integer 10
		{"10.0", 10, true},
		{"\"NaN\"", 0, false},
		{"\"+Inf\"", 0, false},
		{"\"-Inf\"", 0, false},
		{"1e30", 0, false}, // out of the exactly representable range
		{"\"1e30\"", 0, false},
		{"null", 0, false},
		{"\"abc\"", 0, false},
		{"{}", 0, false},
		{"\"429\"", 429, true},
	}
	for _, tc := range cases {
		got, err := decodeStatusValue(json.RawMessage(tc.raw))
		if tc.wantOK && (err != nil || got != tc.want) {
			t.Errorf("decodeStatusValue(%s) = %d, %v; want %d, nil", tc.raw, got, err, tc.want)
		}
		if !tc.wantOK && err == nil {
			t.Errorf("decodeStatusValue(%s) = %d, nil; want error", tc.raw, got)
		}
	}
}

func TestDecodeStreamFractionalStatusIsNotTruncated(t *testing.T) {
	// 200.9 must not collapse to a 200 success and 403.5 must not collapse to
	// a valid 403: unsupported statuses are structured failures.
	for _, status := range []string{"200.9", "403.5", "\"NaN\""} {
		inner := `{"choices":[{"index":0,"delta":{"content":"no"}}]}`
		env := `{"statusCodeValue":` + status + `,"body":` + qoderInnerChunk(t, inner) + `}`
		_, err := decodeQoderFailing(t, "data: "+env+"\n\n")
		if err == nil {
			t.Fatalf("status %s: expected structured failure", status)
		}
		if !strings.Contains(err.Error(), "malformed envelope status") {
			t.Fatalf("status %s: err = %v", status, err)
		}
	}
}

func TestDecodeStreamUsageAliases(t *testing.T) {
	t.Run("aliases fill standard fields", func(t *testing.T) {
		inner := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}`
		out := decodeQoderEvents(t, "data: "+qoderEnv(t, "200", qoderInnerChunk(t, inner))+"\n\n")
		if len(out) == 0 || out[len(out)-1].Kind != ir.EventFinish {
			t.Fatalf("events = %+v", out)
		}
		f := out[len(out)-1]
		if f.InputTokens != 11 || f.OutputTokens != 7 || f.CacheReadTokens != 3 || f.CacheWriteTokens != 4 {
			t.Fatalf("usage = %+v", f)
		}
	})
	t.Run("prompt_cache_hit maps to cache read", func(t *testing.T) {
		inner := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"input_tokens":9,"output_tokens":1,"prompt_cache_hit_tokens":5}}`
		out := decodeQoderEvents(t, "data: "+qoderEnv(t, "200", qoderInnerChunk(t, inner))+"\n\n")
		f := out[len(out)-1]
		if f.CacheReadTokens != 5 || f.CacheWriteTokens != 0 {
			t.Fatalf("usage = %+v", f)
		}
	})
	t.Run("standard fields win over aliases without double counting", func(t *testing.T) {
		inner := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"input_tokens":999,"completion_tokens":2,"output_tokens":999,"prompt_tokens_details":{"cached_tokens":1},"cache_read_input_tokens":999}}`
		out := decodeQoderEvents(t, "data: "+qoderEnv(t, "200", qoderInnerChunk(t, inner))+"\n\n")
		f := out[len(out)-1]
		if f.InputTokens != 10 || f.OutputTokens != 2 || f.CacheReadTokens != 1 {
			t.Fatalf("usage = %+v, want standard fields only", f)
		}
	})
	t.Run("trailing usage-only frame after finish", func(t *testing.T) {
		finish := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
		usage := `{"choices":[],"usage":{"input_tokens":12,"output_tokens":3}}`
		sse := "data: " + qoderEnv(t, "200", qoderInnerChunk(t, finish)) + "\n\n" +
			"data: " + qoderEnv(t, "200", qoderInnerChunk(t, usage)) + "\n\n"
		out := decodeQoderEvents(t, sse)
		if len(out) != 2 || out[1].Kind != ir.EventFinish {
			t.Fatalf("events = %+v", out)
		}
		if out[1].InputTokens != 12 || out[1].OutputTokens != 3 {
			t.Fatalf("finish = %+v", out[1])
		}
	})
	t.Run("empty usage object does not count as terminal usage", func(t *testing.T) {
		inner := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{}}`
		out := decodeQoderEvents(t, "data: "+qoderEnv(t, "200", qoderInnerChunk(t, inner))+"\n\n")
		if len(out) == 0 || out[len(out)-1].InputTokens != 0 || out[len(out)-1].OutputTokens != 0 {
			t.Fatalf("events = %+v", out)
		}
	})
}

// --- Q04: safe completion with an open upstream ---

// blockingPipe is a pipe whose writer never closes, mimicking Qoder's
// post-terminal keepalive.
type blockingPipe struct {
	pr *io.PipeReader
}

func newBlockingPipe(t *testing.T, frames ...string) *blockingPipe {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		for _, f := range frames {
			if _, err := pw.Write([]byte(f)); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		// Never close: the upstream socket stays open after terminal frames.
	}()
	return &blockingPipe{pr: pr}
}

func TestDecodeStreamEndsAfterFinishAndUsageWithOpenUpstream(t *testing.T) {
	finish := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	usage := `{"choices":[],"usage":{"input_tokens":8,"output_tokens":2}}`
	sse := "data: " + qoderEnv(t, "200", qoderInnerChunk(t, finish)) + "\n\n" +
		"data: " + qoderEnv(t, "200", qoderInnerChunk(t, usage)) + "\n\n"
	pipe := newBlockingPipe(t, sse)
	t.Cleanup(func() { pipe.pr.Close() })

	done := make(chan []ir.StreamEvent, 1)
	go func() {
		var out []ir.StreamEvent
		_ = DecodeStream(pipe.pr, func(ev ir.StreamEvent) error {
			out = append(out, ev)
			return nil
		})
		done <- out
	}()
	select {
	case out := <-done:
		if len(out) == 0 || out[len(out)-1].Kind != ir.EventFinish {
			t.Fatalf("events = %+v", out)
		}
		if out[len(out)-1].InputTokens != 8 || out[len(out)-1].OutputTokens != 2 {
			t.Fatalf("finish = %+v", out[len(out)-1])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decoder blocked after terminal finish+usage while upstream stayed open")
	}
}

func TestDecodeStreamDoesNotEndOnFinishWithoutUsage(t *testing.T) {
	// Finish alone must not end the stream: the usage trailer may still be in
	// flight. The decoder only completes after usage arrives (or EOF).
	finish := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	usage := `{"choices":[],"usage":{"input_tokens":8,"output_tokens":2}}`
	// The second frame is delayed so the decoder must still be waiting when
	// only the finish has arrived.
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("data: " + qoderEnv(t, "200", qoderInnerChunk(t, finish)) + "\n\n"))
		time.Sleep(80 * time.Millisecond)
		_, _ = pw.Write([]byte("data: " + qoderEnv(t, "200", qoderInnerChunk(t, usage)) + "\n\n"))
		pw.Close()
	}()
	out := make(chan []ir.StreamEvent, 1)
	go func() {
		var events []ir.StreamEvent
		_ = DecodeStream(pr, func(ev ir.StreamEvent) error {
			events = append(events, ev)
			return nil
		})
		out <- events
	}()
	select {
	case events := <-out:
		if len(events) == 0 || events[len(events)-1].Kind != ir.EventFinish {
			t.Fatalf("events = %+v", events)
		}
		if events[len(events)-1].InputTokens != 8 {
			t.Fatalf("finish = %+v, want trailing usage captured", events[len(events)-1])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decoder never finished")
	}
}

func TestDecodeStreamFinishAndUsageOneFrameOpenUpstream(t *testing.T) {
	inner := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"input_tokens":5,"output_tokens":6}}`
	pipe := newBlockingPipe(t, "data: "+qoderEnv(t, "200", qoderInnerChunk(t, inner))+"\n\n")
	t.Cleanup(func() { pipe.pr.Close() })
	done := make(chan []ir.StreamEvent, 1)
	go func() {
		var out []ir.StreamEvent
		_ = DecodeStream(pipe.pr, func(ev ir.StreamEvent) error {
			out = append(out, ev)
			return nil
		})
		done <- out
	}()
	select {
	case out := <-done:
		if len(out) != 2 || out[1].InputTokens != 5 || out[1].OutputTokens != 6 {
			t.Fatalf("events = %+v", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decoder blocked after a single terminal chunk while upstream stayed open")
	}
}

// --- tool fragments and errors ---

func TestDecodeStreamToolCallFragments(t *testing.T) {
	start := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`
	frag1 := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"par"}}]}}]}`
	frag2 := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"is\"}"}}],"finish_reason":"tool_calls"}}]}`
	var frames []string
	for _, inner := range []string{start, frag1, frag2} {
		frames = append(frames, "data: "+qoderEnv(t, "200", qoderInnerChunk(t, inner)))
	}
	var starts, deltas int
	var id, name, args string
	var finish *ir.StreamEvent
	err := DecodeStream(strings.NewReader(strings.Join(frames, "\n\n")+"\n\n"), func(ev ir.StreamEvent) error {
		switch ev.Kind {
		case ir.EventToolCallStart:
			starts++
			id, name = ev.ToolID, ev.ToolName
		case ir.EventToolCallDelta:
			deltas++
			args += ev.ArgsFrag
		case ir.EventFinish:
			f := ev
			finish = &f
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if starts != 1 || deltas != 2 || id != "call_1" || name != "get_weather" || args != `{"city":"paris"}` {
		t.Fatalf("starts=%d deltas=%d id=%q name=%q args=%q", starts, deltas, id, name, args)
	}
	if finish == nil || finish.StopReason != ir.StopToolUse {
		t.Fatalf("finish = %+v", finish)
	}
}

func TestDecodeStreamErrorBeforeAndAfterContent(t *testing.T) {
	t.Run("before content", func(t *testing.T) {
		inner := `{"code":"500","message":"boom"}`
		out, err := decodeQoderFailing(t, "data: "+qoderEnv(t, "500", qoderInnerChunk(t, inner))+"\n\n")
		if err == nil {
			t.Fatal("expected failure")
		}
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Status != 500 || sf.Message != inner {
			t.Fatalf("err = %v (%+v), want status 500 failure", err, sf)
		}
		if len(out) != 0 {
			t.Fatalf("events = %+v", out)
		}
	})
	t.Run("after content fails the stream", func(t *testing.T) {
		content := `{"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`
		errInner := `{"code":"500","message":"boom"}`
		sse := "data: " + qoderEnv(t, "200", qoderInnerChunk(t, content)) + "\n\n" +
			"data: " + qoderEnv(t, "500", qoderInnerChunk(t, errInner)) + "\n\n"
		out, err := decodeQoderFailing(t, sse)
		if err == nil {
			t.Fatal("expected failure")
		}
		if sf, ok := ir.AsStreamFailure(err); !ok || sf.Status != 500 {
			t.Fatalf("err = %v, want structured 500 failure", err)
		}
		var texts []string
		for _, ev := range out {
			if ev.Kind == ir.EventTextDelta {
				texts = append(texts, ev.Text)
			}
		}
		if strings.Join(texts, "") != "a" {
			t.Fatalf("events = %+v", out)
		}
	})
	t.Run("billing becomes quota error with 403", func(t *testing.T) {
		inner := `{"code":110,"message":"daily count exceeded"}`
		_, err := decodeQoderFailing(t, "data: "+qoderEnv(t, "500", qoderInnerChunk(t, inner))+"\n\n")
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Type != "quota_error" || sf.Status != 403 {
			t.Fatalf("err = %v (%+v), want quota_error 403", err, sf)
		}
	})
}

func TestDecodeStreamQueueRefusalWithoutRecoveryKeepsStatus(t *testing.T) {
	// A 10605 envelope that reaches the decoder (queue recovery bypassed)
	// must stay a structured failure with the embedded status, not content.
	inner := `{"code":"10605","message":"{\"isQueued\":true,\"queueType\":\"p3\",\"retryAfterSeconds\":30}"}`
	_, err := decodeQoderFailing(t, "data: "+qoderEnv(t, "429", qoderInnerChunk(t, inner))+"\n\n")
	sf, ok := ir.AsStreamFailure(err)
	if !ok || sf.Status != 429 || sf.Code != "10605" {
		t.Fatalf("err = %v (%+v), want 429 queue failure", err, sf)
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
