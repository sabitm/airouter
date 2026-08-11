package antigravity

import (
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestDecodeStreamTextAndFinish(t *testing.T) {
	sse := "" +
		"data: {\"response\":{\"responseId\":\"r1\",\"modelVersion\":\"gemini-x\",\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}}\n\n" +
		"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"!\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2}}}\n\n"
	var events []ir.StreamEvent
	err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var finish *ir.StreamEvent
	for i := range events {
		switch events[i].Kind {
		case ir.EventTextDelta:
			text.WriteString(events[i].Text)
		case ir.EventFinish:
			finish = &events[i]
		}
	}
	if text.String() != "hi!" {
		t.Fatalf("text %q", text.String())
	}
	if finish == nil || finish.StopReason != ir.StopEndTurn || finish.InputTokens != 3 || finish.OutputTokens != 2 {
		t.Fatalf("finish %+v", finish)
	}
}

func TestUsageMetadataAccounting(t *testing.T) {
	intPtr := func(v int) *int { return &v }
	cases := []struct {
		name    string
		meta    usageMetadata
		wantIn  int
		wantOut int
	}{
		{
			name:    "prompt and candidates",
			meta:    usageMetadata{PromptTokenCount: 10, CandidatesTokenCount: intPtr(4)},
			wantIn:  10,
			wantOut: 4,
		},
		{
			name:    "candidates plus thoughts",
			meta:    usageMetadata{PromptTokenCount: 10, CandidatesTokenCount: intPtr(4), ThoughtsTokenCount: 6},
			wantIn:  10,
			wantOut: 10,
		},
		{
			name:    "absent candidates derives from total",
			meta:    usageMetadata{PromptTokenCount: 10, ThoughtsTokenCount: 3, TotalTokenCount: intPtr(20)},
			wantIn:  10,
			wantOut: 10, // derived candidates 7 + thoughts 3
		},
		{
			name:    "invalid low total clamps candidates nonnegative",
			meta:    usageMetadata{PromptTokenCount: 10, ThoughtsTokenCount: 5, TotalTokenCount: intPtr(12)},
			wantIn:  10,
			wantOut: 5, // derived candidates max(12-10-5,0)=0 + thoughts 5
		},
		{
			name:    "explicit zero candidates preferred over total derivation",
			meta:    usageMetadata{PromptTokenCount: 10, CandidatesTokenCount: intPtr(0), ThoughtsTokenCount: 5, TotalTokenCount: intPtr(99)},
			wantIn:  10,
			wantOut: 5,
		},
		{
			name:    "present zero total derives zero candidates",
			meta:    usageMetadata{PromptTokenCount: 10, TotalTokenCount: intPtr(0)},
			wantIn:  10,
			wantOut: 0,
		},
		{
			name:    "thoughts without candidates or total",
			meta:    usageMetadata{PromptTokenCount: 10, ThoughtsTokenCount: 7},
			wantIn:  10,
			wantOut: 7,
		},
		{
			name:    "cached content not double-counted",
			meta:    usageMetadata{PromptTokenCount: 100, CachedContentTokenCount: 40, CandidatesTokenCount: intPtr(8)},
			wantIn:  100,
			wantOut: 8,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.meta.inputTokens(); got != tc.wantIn {
				t.Errorf("input = %d, want %d", got, tc.wantIn)
			}
			if got := tc.meta.outputTokens(); got != tc.wantOut {
				t.Errorf("output = %d, want %d", got, tc.wantOut)
			}
		})
	}
}

func TestDecodeStreamLateUsageMetadata(t *testing.T) {
	// Usage arrives only after finishReason content, including thoughts.
	sse := "" +
		"data: {\"response\":{\"responseId\":\"r1\",\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}]}}\n\n" +
		"data: {\"response\":{\"usageMetadata\":{\"promptTokenCount\":12,\"candidatesTokenCount\":3,\"thoughtsTokenCount\":7,\"cachedContentTokenCount\":4}}}\n\n"
	var finish *ir.StreamEvent
	err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventFinish {
			cp := ev
			finish = &cp
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if finish == nil {
		t.Fatal("missing finish")
	}
	if finish.InputTokens != 12 || finish.OutputTokens != 10 {
		t.Fatalf("usage = %d/%d, want 12/10", finish.InputTokens, finish.OutputTokens)
	}
}

func TestDecodeStreamThoughtOnlyUsageMetadata(t *testing.T) {
	sse := "data: {\"response\":{\"responseId\":\"r-thought\",\"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"thoughtsTokenCount\":7}}}\n\n"
	var finish *ir.StreamEvent
	err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventFinish {
			cp := ev
			finish = &cp
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if finish == nil || finish.InputTokens != 10 || finish.OutputTokens != 7 {
		t.Fatalf("finish = %+v, want usage 10/7", finish)
	}
}

func TestDecodeStreamLateTotalDerivedZeroOverwritesOutput(t *testing.T) {
	// Earlier nonzero candidates must yield to a later authoritative metadata
	// chunk that omits candidates and carries a total that derives output=0.
	sse := "" +
		"data: {\"response\":{\"responseId\":\"r2\",\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":5}}}\n\n" +
		"data: {\"response\":{\"usageMetadata\":{\"promptTokenCount\":10,\"totalTokenCount\":10}}}\n\n"
	var finish *ir.StreamEvent
	err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventFinish {
			cp := ev
			finish = &cp
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if finish == nil {
		t.Fatal("missing finish")
	}
	if finish.InputTokens != 10 || finish.OutputTokens != 0 {
		t.Fatalf("usage = %d/%d, want 10/0", finish.InputTokens, finish.OutputTokens)
	}
}

func TestDecodeStreamToolDecloakAndThoughtDrop(t *testing.T) {
	sse := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[" +
		"{\"text\":\"think\",\"thought\":true}," +
		"{\"functionCall\":{\"name\":\"Shell_ide\",\"args\":{\"cmd\":\"ls\"}}}" +
		"]},\"finishReason\":\"STOP\"}]}}\n\n"
	var events []ir.StreamEvent
	err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawText bool
	var toolName string
	var stop ir.StopReason
	for _, ev := range events {
		if ev.Kind == ir.EventTextDelta {
			sawText = true
		}
		if ev.Kind == ir.EventToolCallStart {
			toolName = ev.ToolName
		}
		if ev.Kind == ir.EventFinish {
			stop = ev.StopReason
		}
	}
	if sawText {
		t.Fatal("thought text should be dropped")
	}
	if toolName != "Shell" {
		t.Fatalf("decloak got %q", toolName)
	}
	if stop != ir.StopToolUse {
		t.Fatalf("stop %q", stop)
	}
}

func TestUnwrapChunk(t *testing.T) {
	t.Run("invalid json returns nil false", func(t *testing.T) {
		got, ok := unwrapChunk([]byte(`{bad`))
		if ok || got != nil {
			t.Errorf("got (%+v, %v), want (nil, false)", got, ok)
		}
	})

	t.Run("empty object returns nil false", func(t *testing.T) {
		got, ok := unwrapChunk([]byte(`{}`))
		if ok || got != nil {
			t.Errorf("got (%+v, %v), want (nil, false)", got, ok)
		}
	})

	t.Run("wrapped response shape", func(t *testing.T) {
		data := []byte(`{"response":{"responseId":"r1","modelVersion":"gemini-x","candidates":[{"content":{"parts":[{"text":"hi"}]}}]}}`)
		got, ok := unwrapChunk(data)
		if !ok || got == nil {
			t.Fatalf("got (%+v, %v), want (non-nil, true)", got, ok)
		}
		if got.ResponseID != "r1" || got.ModelVersion != "gemini-x" {
			t.Errorf("fields = %+v", got)
		}
		if len(got.Candidates) != 1 {
			t.Errorf("candidates len = %d", len(got.Candidates))
		}
	})

	t.Run("bare candidates shape", func(t *testing.T) {
		data := []byte(`{"candidates":[{"finishReason":"STOP"}]}`)
		got, ok := unwrapChunk(data)
		if !ok || got == nil {
			t.Fatalf("got (%+v, %v), want (non-nil, true)", got, ok)
		}
		if len(got.Candidates) != 1 || got.Candidates[0].FinishReason != "STOP" {
			t.Errorf("candidates = %+v", got.Candidates)
		}
		// Bare path should leave wrapper-only fields zero.
		if got.ResponseID != "" || got.ModelVersion != "" {
			t.Errorf("wrapper fields should be empty: %+v", got)
		}
	})

	t.Run("bare usageMetadata shape", func(t *testing.T) {
		data := []byte(`{"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}`)
		got, ok := unwrapChunk(data)
		if !ok || got == nil {
			t.Fatalf("got (%+v, %v), want (non-nil, true)", got, ok)
		}
		if got.UsageMetadata == nil || got.UsageMetadata.PromptTokenCount != 3 {
			t.Errorf("usage = %+v", got.UsageMetadata)
		}
	})

	t.Run("bare responseId shape", func(t *testing.T) {
		data := []byte(`{"responseId":"r2"}`)
		got, ok := unwrapChunk(data)
		if !ok || got == nil {
			t.Fatalf("got (%+v, %v), want (non-nil, true)", got, ok)
		}
		if got.ResponseID != "r2" {
			t.Errorf("ResponseID = %q", got.ResponseID)
		}
	})

	t.Run("bare modelVersion alone is not recognized", func(t *testing.T) {
		// unwrapChunk's bare-field detection checks Candidates/UsageMetadata/ResponseID
		// but not ModelVersion alone — a bare modelVersion returns no-match.
		data := []byte(`{"modelVersion":"gemini-y"}`)
		got, ok := unwrapChunk(data)
		if ok || got != nil {
			t.Errorf("got (%+v, %v), want (nil, false)", got, ok)
		}
	})

	t.Run("bare responseId carries modelVersion through", func(t *testing.T) {
		// When a bare recognized field (responseId) is present, modelVersion is
		// also carried into the synthesized geminiResponse.
		data := []byte(`{"responseId":"r2","modelVersion":"gemini-y"}`)
		got, ok := unwrapChunk(data)
		if !ok || got == nil {
			t.Fatalf("got (%+v, %v), want (non-nil, true)", got, ok)
		}
		if got.ResponseID != "r2" || got.ModelVersion != "gemini-y" {
			t.Errorf("fields = %+v", got)
		}
	})

	t.Run("wrapped response takes precedence over bare fields", func(t *testing.T) {
		// When both `response` and bare `candidates` are present, the wrapped
		// response wins and bare fields are ignored.
		data := []byte(`{"response":{"responseId":"wrapped"},"candidates":[{"finishReason":"BARE"}]}`)
		got, ok := unwrapChunk(data)
		if !ok || got == nil {
			t.Fatalf("got (%+v, %v), want (non-nil, true)", got, ok)
		}
		if got.ResponseID != "wrapped" {
			t.Errorf("ResponseID = %q, want wrapped", got.ResponseID)
		}
		// Bare candidates should be ignored since wrapped response is used.
		if len(got.Candidates) != 0 {
			t.Errorf("bare candidates leaked into wrapped response: %+v", got.Candidates)
		}
	})
}

func TestMapFinish(t *testing.T) {
	cases := []struct {
		name     string
		reason   string
		sawTools bool
		want     ir.StopReason
	}{
		{"max_tokens no tools", "MAX_TOKENS", false, ir.StopMaxTokens},
		{"max_tokens with tools", "MAX_TOKENS", true, ir.StopMaxTokens},
		{"stop no tools", "STOP", false, ir.StopEndTurn},
		{"stop with tools", "STOP", true, ir.StopToolUse},
		{"unknown with tools", "SAFETY", true, ir.StopToolUse},
		{"unknown no tools", "SAFETY", false, ir.StopEndTurn},
		{"lowercase stop (case-insensitive)", "stop", false, ir.StopEndTurn},
		{"mixed case max_tokens", "max_tokens", false, ir.StopMaxTokens},
		{"empty reason no tools", "", false, ir.StopEndTurn},
		{"empty reason with tools", "", true, ir.StopToolUse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapFinish(tc.reason, tc.sawTools); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDecodeStreamErrorEnvelope(t *testing.T) {
	sse := "data: {\"error\":{\"code\":503,\"message\":\"backend unavailable\",\"status\":\"UNAVAILABLE\"}}\n\n"
	var events []ir.StreamEvent
	err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	sf, ok := ir.AsStreamFailure(err)
	if !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	if !strings.Contains(sf.Message, "unavailable") {
		t.Errorf("message = %q", sf.Message)
	}
	if len(events) != 0 {
		t.Fatalf("want no events, got %+v", events)
	}
}
