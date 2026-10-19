package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"airouter/internal/domain"
	"airouter/internal/proxy/ir"
)

func TestEstimateUsageUsesCharacterLength(t *testing.T) {
	req := &ir.Request{
		System: "abcd",
		Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.ContentBlock{{
				Type:       ir.BlockToolResult,
				ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "efghijkl"}},
			}},
		}},
		Tools: []ir.Tool{{Name: "read", Description: "mnop", Parameters: json.RawMessage(`{"q":1}`)}},
	}
	resp := &ir.Response{Content: []ir.ContentBlock{{
		Type:      ir.BlockToolUse,
		ToolName:  "read",
		ToolInput: json.RawMessage(`{"path":"prices.py"}`),
	}}}
	in, out := estimateUsage(req, resp)
	if in != 7 {
		t.Errorf("input = %d, want 7", in)
	}
	if out != 6 {
		t.Errorf("output = %d, want 6", out)
	}
	if got, gotOut := estimateUsage(nil, nil); got != 0 || gotOut != 0 {
		t.Errorf("empty = %d/%d, want 0/0", got, gotOut)
	}
	opaque := &ir.Request{Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
		{Type: ir.BlockReasoning, Text: "abcd", AnthropicSignature: strings.Repeat("s", 400)},
		{Type: ir.BlockRedactedReasoning, RedactedData: strings.Repeat("r", 400)},
	}}}}
	in, out = estimateUsage(opaque, nil)
	if in != 1 || out != 0 {
		t.Fatalf("opaque estimate = %d/%d, want readable text only", in, out)
	}
}

func TestApplyCursorUsageKeepsAuthoritativeTurnEnded(t *testing.T) {
	req := &ir.Request{System: strings.Repeat("a", 400)}
	resp := &ir.Response{
		Content: []ir.ContentBlock{{Type: ir.BlockText, Text: strings.Repeat("b", 40)}},
		Usage: ir.Usage{
			InputTokens: 25231, OutputTokens: 101, CacheReadTokens: 25227, CacheWriteTokens: 0,
			UsageReported: true,
		},
	}
	applyCursorUsage(req, resp, 80)
	if resp.Usage.InputTokens != 25231 || resp.Usage.OutputTokens != 101 {
		t.Fatalf("usage = %+v, want raw 25231/101", resp.Usage)
	}
	if resp.Usage.CacheReadTokens != 25227 || resp.Usage.CacheWriteTokens != 0 || !resp.Usage.UsageReported {
		t.Fatalf("cache marker = %+v, want read 25227 reported", resp.Usage)
	}

	zero := &ir.Response{Usage: ir.Usage{UsageReported: true}}
	applyCursorUsage(req, zero, 80)
	if zero.Usage.InputTokens != 0 || zero.Usage.OutputTokens != 0 || !zero.Usage.UsageReported {
		t.Fatalf("explicit zero = %+v, want reported 0/0", zero.Usage)
	}

	over := &ir.Response{Usage: ir.Usage{InputTokens: 10, CacheReadTokens: 12, CacheWriteTokens: 4, UsageReported: true}}
	applyCursorUsage(nil, over, 0)
	if over.Usage.CacheReadTokens != 10 || over.Usage.CacheWriteTokens != 0 || !over.Usage.UsageReported {
		t.Fatalf("clamped = %+v, want read 10 write 0", over.Usage)
	}
}

func TestApplyCursorUsageEstimatesWhenUnreported(t *testing.T) {
	req := &ir.Request{System: strings.Repeat("a", 400)}
	resp := &ir.Response{
		Content: []ir.ContentBlock{{Type: ir.BlockText, Text: strings.Repeat("b", 40)}},
		Usage:   ir.Usage{InputTokens: 2761154, OutputTokens: 11996, CacheReadTokens: 2372032},
	}
	applyCursorUsage(req, resp, 727415)
	if resp.Usage.InputTokens != 100 || resp.Usage.OutputTokens != 10 || resp.Usage.UsageReported {
		t.Fatalf("estimate = %+v, want 100/10 unreported", resp.Usage)
	}
	if resp.Usage.CacheReadTokens != 0 || resp.Usage.CacheWriteTokens != 0 {
		t.Fatalf("cache = %d/%d, want 0/0", resp.Usage.CacheReadTokens, resp.Usage.CacheWriteTokens)
	}

	empty := &ir.Response{}
	applyCursorUsage(&ir.Request{}, empty, 8)
	if empty.Usage.InputTokens != 2 || empty.Usage.OutputTokens != 0 || empty.Usage.UsageReported {
		t.Fatalf("byte fallback = %+v, want input 2", empty.Usage)
	}
}

func TestUsageInputPlausibleRejectsImpossibleCount(t *testing.T) {
	if !usageInputPlausible(727415, 180000) {
		t.Fatal("180000 tokens fits a 727415 byte request")
	}
	if usageInputPlausible(727415, 2761154) {
		t.Fatal("2761154 tokens cannot fit a 727415 byte request")
	}
	if !usageInputPlausible(0, 0) {
		t.Fatal("missing usage is plausible")
	}
}

func TestParseUsage(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		codecID string
		wantIn  int
		wantOut int
	}{
		{
			name:    "openai shape",
			body:    `{"usage":{"prompt_tokens":10,"completion_tokens":20}}`,
			codecID: "oai-chat",
			wantIn:  10,
			wantOut: 20,
		},
		{
			name:    "responses shape",
			body:    `{"usage":{"input_tokens":5,"output_tokens":7}}`,
			codecID: "oai-responses",
			wantIn:  5,
			wantOut: 7,
		},
		{
			name:    "anthropic shape",
			body:    `{"usage":{"input_tokens":5,"output_tokens":7}}`,
			codecID: "anth-msg",
			wantIn:  5,
			wantOut: 7,
		},
		{
			name:    "anthropic with cache folds into input",
			body:    `{"usage":{"input_tokens":5,"output_tokens":7,"cache_creation_input_tokens":3,"cache_read_input_tokens":2}}`,
			codecID: "anth-msg",
			wantIn:  10,
			wantOut: 7,
		},
		{
			name:    "hybrid counted once as openai when codec is oai-chat",
			body:    `{"usage":{"prompt_tokens":10,"completion_tokens":20,"input_tokens":5,"output_tokens":7}}`,
			codecID: "oai-chat",
			wantIn:  10,
			wantOut: 20,
		},
		{
			name:    "hybrid counted once as responses when codec is oai-responses",
			body:    `{"usage":{"prompt_tokens":10,"completion_tokens":20,"input_tokens":5,"output_tokens":7}}`,
			codecID: "oai-responses",
			wantIn:  5,
			wantOut: 7,
		},
		{
			name:    "hybrid fallback prefers complete prompt/completion family",
			body:    `{"usage":{"prompt_tokens":10,"completion_tokens":20,"input_tokens":5,"output_tokens":7}}`,
			codecID: "",
			wantIn:  10,
			wantOut: 20,
		},
		{
			name:    "fallback anthropic caches fold when only io family present",
			body:    `{"usage":{"input_tokens":5,"output_tokens":7,"cache_creation_input_tokens":3,"cache_read_input_tokens":2}}`,
			codecID: "",
			wantIn:  10,
			wantOut: 7,
		},
		{
			name:    "openai codec ignores anthropic cache aliases",
			body:    `{"usage":{"prompt_tokens":10,"completion_tokens":20,"cache_creation_input_tokens":3,"cache_read_input_tokens":2}}`,
			codecID: "oai-chat",
			wantIn:  10,
			wantOut: 20,
		},
		{
			name:    "responses codec ignores anthropic cache aliases",
			body:    `{"usage":{"input_tokens":5,"output_tokens":7,"cache_creation_input_tokens":3,"cache_read_input_tokens":2}}`,
			codecID: "oai-responses",
			wantIn:  5,
			wantOut: 7,
		},
		{"empty body", ``, "", 0, 0},
		{"invalid json", `{not valid`, "", 0, 0},
		{
			name:    "no usage object",
			body:    `{"id":"x"}`,
			codecID: "oai-chat",
			wantIn:  0,
			wantOut: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, out := parseUsage([]byte(tc.body), tc.codecID)
			if in != tc.wantIn {
				t.Errorf("input = %d, want %d", in, tc.wantIn)
			}
			if out != tc.wantOut {
				t.Errorf("output = %d, want %d", out, tc.wantOut)
			}
		})
	}
}

func TestSniffStreamUsage(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		codecID string
		wantIn  int
		wantOut int
	}{
		{
			name:    "openai usage chunk",
			data:    `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
			codecID: "oai-chat",
			wantIn:  3,
			wantOut: 2,
		},
		{
			name:    "hybrid openai usage counted once",
			data:    `{"usage":{"prompt_tokens":10,"completion_tokens":20,"input_tokens":5,"output_tokens":7}}`,
			codecID: "oai-chat",
			wantIn:  10,
			wantOut: 20,
		},
		{
			name:    "anthropic message_start nested usage",
			data:    `{"type":"message_start","message":{"usage":{"input_tokens":5,"cache_creation_input_tokens":3,"cache_read_input_tokens":2,"output_tokens":0}}}`,
			codecID: "anth-msg",
			wantIn:  10,
			wantOut: 0,
		},
		{
			name:    "responses completed nested usage",
			data:    `{"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":2}}}`,
			codecID: "oai-responses",
			wantIn:  3,
			wantOut: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := &reqResult{}
			sniffStreamUsage([]byte(tc.data), res, tc.codecID)
			if res.inTok != tc.wantIn || res.outTok != tc.wantOut {
				t.Errorf("got %d/%d, want %d/%d", res.inTok, res.outTok, tc.wantIn, tc.wantOut)
			}
		})
	}
}

func TestSniffStreamUsageAnthropicPartition(t *testing.T) {
	start := `{"type":"message_start","message":{"usage":{"input_tokens":200,"cache_read_input_tokens":2000,"cache_creation_input_tokens":400,"output_tokens":1}}}`
	type snapshot struct {
		in, out, ordinary, cacheRead, cacheWrite int
	}
	type step struct {
		data string
		want snapshot
	}
	seeded := snapshot{in: 2600, out: 1, ordinary: 200, cacheRead: 2000, cacheWrite: 400}
	cases := []struct {
		name  string
		steps []step
	}{
		{
			name: "present zero replaces previous output",
			steps: []step{
				{start, seeded},
				{`{"type":"message_delta","usage":{"output_tokens":0}}`, snapshot{in: 2600, out: 0, ordinary: 200, cacheRead: 2000, cacheWrite: 400}},
			},
		},
		{
			name: "omitted output retains previous",
			steps: []step{
				{start, seeded},
				{`{"type":"message_delta","usage":{"input_tokens":200}}`, seeded},
			},
		},
		{
			name: "null output retains previous",
			steps: []step{
				{start, seeded},
				{`{"type":"message_delta","usage":{"output_tokens":null}}`, seeded},
			},
		},
		{
			name: "later positive replaces previous",
			steps: []step{
				{start, seeded},
				{`{"type":"message_delta","usage":{"output_tokens":7}}`, snapshot{in: 2600, out: 7, ordinary: 200, cacheRead: 2000, cacheWrite: 400}},
			},
		},
		{
			name: "empty usage preserves prior counts",
			steps: []step{
				{start, seeded},
				{`{"type":"message_delta","usage":{}}`, seeded},
				{`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, seeded},
			},
		},
		{
			name: "malformed usage ignored",
			steps: []step{
				{start, seeded},
				{`{"type":"message_delta","usage":{"output_tokens":"bad"}}`, seeded},
				{`{"type":"message_delta","usage":[]}`, seeded},
				{`{"type":"message_delta","usage":{"output_tokens":2}}`, snapshot{in: 2600, out: 2, ordinary: 200, cacheRead: 2000, cacheWrite: 400}},
			},
		},
		{
			name: "present input and cache zero reset",
			steps: []step{
				{start, seeded},
				{`{"type":"message_delta","usage":{"input_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":0}}`, snapshot{}},
			},
		},
		{
			name: "omitted input and cache retain previous",
			steps: []step{
				{start, seeded},
				{`{"type":"message_delta","usage":{"output_tokens":3}}`, snapshot{in: 2600, out: 3, ordinary: 200, cacheRead: 2000, cacheWrite: 400}},
			},
		},
	}
	for _, codecID := range []string{"anth-msg", "claude-code"} {
		for _, tc := range cases {
			t.Run(codecID+"/"+tc.name, func(t *testing.T) {
				res := &reqResult{}
				for i, s := range tc.steps {
					sniffStreamUsage([]byte(s.data), res, codecID)
					got := snapshot{in: res.inTok, out: res.outTok, ordinary: res.anthOrdinary, cacheRead: res.anthCacheRead, cacheWrite: res.anthCacheWrite}
					if got != s.want {
						t.Fatalf("step %d = %+v, want %+v", i, got, s.want)
					}
				}
			})
		}
	}
}

func TestSniffStreamUsageNonAnthropicKeepsPriorOnZero(t *testing.T) {
	cases := []struct {
		name    string
		codecID string
		steps   []string
		wantIn  int
		wantOut int
	}{
		{
			name:    "openai chat",
			codecID: "oai-chat",
			steps: []string{
				`{"usage":{"prompt_tokens":11,"completion_tokens":5}}`,
				`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`,
			},
			wantIn:  11,
			wantOut: 5,
		},
		{
			name:    "responses",
			codecID: "oai-responses",
			steps: []string{
				`{"response":{"usage":{"input_tokens":11,"output_tokens":5}}}`,
				`{"response":{"usage":{"input_tokens":0,"output_tokens":0}}}`,
			},
			wantIn:  11,
			wantOut: 5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := &reqResult{}
			for _, data := range tc.steps {
				sniffStreamUsage([]byte(data), res, tc.codecID)
			}
			if res.inTok != tc.wantIn || res.outTok != tc.wantOut {
				t.Fatalf("got %d/%d, want %d/%d", res.inTok, res.outTok, tc.wantIn, tc.wantOut)
			}
		})
	}
}

func TestForceOpenAIStreamIncludeUsage(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		want  string // substring or exact checks below
		check func(t *testing.T, out map[string]any)
	}{
		{
			name: "missing stream_options",
			body: `{"model":"m","stream":true,"messages":[]}`,
			check: func(t *testing.T, out map[string]any) {
				opts, ok := out["stream_options"].(map[string]any)
				if !ok || opts["include_usage"] != true {
					t.Fatalf("stream_options = %#v", out["stream_options"])
				}
			},
		},
		{
			name: "preserve unrelated stream_options",
			body: `{"model":"m","stream":true,"stream_options":{"include_usage":false,"foo":1}}`,
			check: func(t *testing.T, out map[string]any) {
				opts := out["stream_options"].(map[string]any)
				if opts["include_usage"] != true {
					t.Fatalf("include_usage = %#v", opts["include_usage"])
				}
				if opts["foo"] != float64(1) {
					t.Fatalf("foo = %#v, want 1", opts["foo"])
				}
			},
		},
		{
			name: "override include_usage false",
			body: `{"model":"m","stream":true,"stream_options":{"include_usage":false}}`,
			check: func(t *testing.T, out map[string]any) {
				opts := out["stream_options"].(map[string]any)
				if opts["include_usage"] != true {
					t.Fatalf("include_usage = %#v", opts["include_usage"])
				}
			},
		},
		{
			name: "unary unchanged",
			body: `{"model":"m","messages":[],"stream_options":{"include_usage":false}}`,
			check: func(t *testing.T, out map[string]any) {
				if _, ok := out["stream_options"]; !ok {
					// original had stream_options; must remain untouched
					t.Fatal("stream_options missing")
				}
				opts := out["stream_options"].(map[string]any)
				if opts["include_usage"] != false {
					t.Fatalf("unary include_usage mutated: %#v", opts["include_usage"])
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := forceOpenAIStreamIncludeUsage([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatal(err)
			}
			tc.check(t, m)
		})
	}
}

func TestCollectStreamResponseLimits(t *testing.T) {
	stream := func(events []ir.StreamEvent) codec {
		return codec{decodeStream: func(_ io.Reader, emit func(ir.StreamEvent) error) error {
			for _, ev := range events {
				if err := emit(ev); err != nil {
					return err
				}
			}
			return nil
		}}
	}

	t.Run("text at limit", func(t *testing.T) {
		fallback := "m"
		base := len(ir.NewID("resp_")) + len(fallback)
		text := strings.Repeat("x", 16)
		limit := int64(base) + collectedBlockOverhead + int64(len(text))
		resp, err := collectStreamResponseWithLimits(strings.NewReader(""), stream([]ir.StreamEvent{
			{Kind: ir.EventTextDelta, Text: text},
			{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
		}), nil, fallback, nil, limit, 10)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if len(resp.Content) != 1 || resp.Content[0].Text != text {
			t.Fatalf("content = %+v", resp.Content)
		}
	})

	t.Run("text over limit", func(t *testing.T) {
		_, err := collectStreamResponseWithLimits(strings.NewReader(""), stream([]ir.StreamEvent{
			{Kind: ir.EventTextDelta, Text: strings.Repeat("x", 128)},
		}), nil, "m", nil, 64, 10)
		if !errors.Is(err, errCollectedStreamResponseTooLarge) {
			t.Fatalf("error = %v, want response-too-large", err)
		}
	})

	t.Run("reasoning-only stream", func(t *testing.T) {
		resp, err := collectStreamResponseWithLimits(strings.NewReader(""), stream([]ir.StreamEvent{
			{Kind: ir.EventMessageStart, ID: "r1", Model: "m"},
			{Kind: ir.EventReasoningDelta, Text: "chain"},
			{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
		}), nil, "m", nil, 1024, 10)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if len(resp.Content) != 1 || resp.Content[0].Type != ir.BlockReasoning || resp.Content[0].Text != "chain" {
			t.Fatalf("content = %+v", resp.Content)
		}
	})

	t.Run("reasoning over limit", func(t *testing.T) {
		_, err := collectStreamResponseWithLimits(strings.NewReader(""), stream([]ir.StreamEvent{
			{Kind: ir.EventReasoningDelta, Text: strings.Repeat("x", 128)},
		}), nil, "m", nil, 64, 10)
		if !errors.Is(err, errCollectedStreamResponseTooLarge) {
			t.Fatalf("error = %v, want response-too-large", err)
		}
	})

	t.Run("tool arguments over limit", func(t *testing.T) {
		_, err := collectStreamResponseWithLimits(strings.NewReader(""), stream([]ir.StreamEvent{
			{Kind: ir.EventToolCallStart, Index: 0, ToolID: "call", ToolName: "fn"},
			{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: strings.Repeat("x", 128)},
		}), nil, "m", nil, 64, 10)
		if !errors.Is(err, errCollectedStreamResponseTooLarge) {
			t.Fatalf("error = %v, want response-too-large", err)
		}
	})

	t.Run("too many tools", func(t *testing.T) {
		_, err := collectStreamResponseWithLimits(strings.NewReader(""), stream([]ir.StreamEvent{
			{Kind: ir.EventToolCallStart, Index: 0, ToolID: "a", ToolName: "fa"},
			{Kind: ir.EventToolCallStart, Index: 1, ToolID: "b", ToolName: "fb"},
		}), nil, "m", nil, 1024, 1)
		if !errors.Is(err, errCollectedStreamTooManyTools) {
			t.Fatalf("error = %v, want too-many-tools", err)
		}
	})

	t.Run("tool delta before start keeps args", func(t *testing.T) {
		resp, err := collectStreamResponseWithLimits(strings.NewReader(""), stream([]ir.StreamEvent{
			{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `{"ci`},
			{Kind: ir.EventToolCallStart, Index: 0, ToolID: "call_1", ToolName: "get_weather"},
			{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `ty":"SF"}`},
			{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
		}), nil, "m", nil, 1024, 10)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if len(resp.Content) != 1 {
			t.Fatalf("content = %+v", resp.Content)
		}
		b := resp.Content[0]
		if b.Type != ir.BlockToolUse || b.ToolID != "call_1" || b.ToolName != "get_weather" {
			t.Fatalf("tool identity = %+v", b)
		}
		if string(b.ToolInput) != `{"city":"SF"}` {
			t.Fatalf("tool input = %s, want {\"city\":\"SF\"}", b.ToolInput)
		}
	})

	t.Run("split identity across starts keeps args", func(t *testing.T) {
		resp, err := collectStreamResponseWithLimits(strings.NewReader(""), stream([]ir.StreamEvent{
			{Kind: ir.EventToolCallStart, Index: 0, ToolID: "call_1"},
			{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `{"ci`},
			{Kind: ir.EventToolCallStart, Index: 0, ToolName: "get_weather"},
			{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `ty":"SF"}`},
			{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
		}), nil, "m", nil, 1024, 10)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if len(resp.Content) != 1 {
			t.Fatalf("content = %+v", resp.Content)
		}
		b := resp.Content[0]
		if b.Type != ir.BlockToolUse || b.ToolID != "call_1" || b.ToolName != "get_weather" {
			t.Fatalf("tool identity = %+v", b)
		}
		if string(b.ToolInput) != `{"city":"SF"}` {
			t.Fatalf("tool input = %s, want {\"city\":\"SF\"}", b.ToolInput)
		}
	})

	t.Run("cache details from start and finish", func(t *testing.T) {
		resp, err := collectStreamResponseWithLimits(strings.NewReader(""), stream([]ir.StreamEvent{
			{Kind: ir.EventMessageStart, ID: "r1", Model: "m", InputTokens: 13, CacheWriteTokens: 10},
			{Kind: ir.EventTextDelta, Text: "hi"},
			{Kind: ir.EventFinish, StopReason: ir.StopEndTurn, InputTokens: 2600, OutputTokens: 300, CacheReadTokens: 2000, CacheWriteTokens: 400},
		}), nil, "m", nil, 1024, 10)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if resp.Usage.InputTokens != 2600 || resp.Usage.OutputTokens != 300 {
			t.Fatalf("totals = %+v", resp.Usage)
		}
		if resp.Usage.CacheReadTokens != 2000 || resp.Usage.CacheWriteTokens != 400 {
			t.Fatalf("cache = %+v", resp.Usage)
		}
	})

	t.Run("reported zero finish replaces start usage", func(t *testing.T) {
		resp, err := collectStreamResponseWithLimits(strings.NewReader(""), stream([]ir.StreamEvent{
			{Kind: ir.EventMessageStart, ID: "r1", Model: "m", InputTokens: 13, CacheWriteTokens: 10},
			{Kind: ir.EventFinish, StopReason: ir.StopEndTurn, UsageReported: true},
		}), nil, "m", nil, 1024, 10)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if !resp.Usage.UsageReported || resp.Usage.InputTokens != 0 || resp.Usage.OutputTokens != 0 {
			t.Fatalf("usage = %+v, want reported zeros", resp.Usage)
		}
		if resp.Usage.CacheReadTokens != 0 || resp.Usage.CacheWriteTokens != 0 {
			t.Fatalf("cache = %+v, want cleared", resp.Usage)
		}
	})

	t.Run("finish without cache keeps start cache", func(t *testing.T) {
		resp, err := collectStreamResponseWithLimits(strings.NewReader(""), stream([]ir.StreamEvent{
			{Kind: ir.EventMessageStart, ID: "r1", Model: "m", InputTokens: 13, CacheWriteTokens: 10},
			{Kind: ir.EventFinish, StopReason: ir.StopEndTurn, OutputTokens: 2},
		}), nil, "m", nil, 1024, 10)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if resp.Usage.InputTokens != 13 || resp.Usage.CacheWriteTokens != 10 || resp.Usage.OutputTokens != 2 {
			t.Fatalf("usage = %+v", resp.Usage)
		}
	})

	t.Run("late start does not refund retained args", func(t *testing.T) {
		const limit = int64(64)
		fallback := "m"
		base := len(ir.NewID("resp_")) + len(fallback)
		// Leave 4 bytes after args; identity "call"+"fn" is 6, so a late
		// Start that refunded args would succeed and this must not.
		args := strings.Repeat("x", int(limit)-base-4)
		_, err := collectStreamResponseWithLimits(strings.NewReader(""), stream([]ir.StreamEvent{
			{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: args},
			{Kind: ir.EventToolCallStart, Index: 0, ToolID: "call", ToolName: "fn"},
		}), nil, fallback, nil, limit, 10)
		if !errors.Is(err, errCollectedStreamResponseTooLarge) {
			t.Fatalf("error = %v, want response-too-large", err)
		}
	})

	// Empty redacted events each bill the flat block overhead, so a flood
	// stops at the byte budget instead of growing the extras slice unbounded.
	t.Run("empty redacted flood bounded by overhead", func(t *testing.T) {
		base := int64(len(ir.NewID("resp_")) + len("m"))
		limit := base + 4*collectedBlockOverhead
		events := make([]ir.StreamEvent, 0, 100)
		for i := 0; i < 100; i++ {
			events = append(events, ir.StreamEvent{Kind: ir.EventRedactedReasoning})
		}
		resp, err := collectStreamResponseWithLimits(strings.NewReader(""), stream(events[:4]), nil, "m", nil, limit, 10)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if len(resp.Content) != 4 {
			t.Fatalf("blocks = %d, want 4", len(resp.Content))
		}
		for _, b := range resp.Content {
			if b.Type != ir.BlockRedactedReasoning {
				t.Fatalf("content = %+v", resp.Content)
			}
		}
		_, err = collectStreamResponseWithLimits(strings.NewReader(""), stream(events[:5]), nil, "m", nil, limit, 10)
		if !errors.Is(err, errCollectedStreamResponseTooLarge) {
			t.Fatalf("error = %v, want response-too-large", err)
		}
	})

	// Distinct thinking indices bill one overhead each on top of payload, so
	// tiny signatures across many indices hit the budget proportionally.
	t.Run("distinct thinking blocks charged overhead", func(t *testing.T) {
		base := int64(len(ir.NewID("resp_")) + len("m"))
		limit := base + 3*collectedBlockOverhead + 3
		events := make([]ir.StreamEvent, 0, 4)
		for i := 0; i < 4; i++ {
			events = append(events, ir.StreamEvent{Kind: ir.EventReasoningSignature, Index: i, Signature: "s"})
		}
		resp, err := collectStreamResponseWithLimits(strings.NewReader(""), stream(events[:3]), nil, "m", nil, limit, 10)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if len(resp.Content) != 3 {
			t.Fatalf("blocks = %+v", resp.Content)
		}
		for i, b := range resp.Content {
			if b.Type != ir.BlockReasoning || b.Text != "" || b.AnthropicSignature != "s" {
				t.Fatalf("block %d = %+v", i, b)
			}
		}
		_, err = collectStreamResponseWithLimits(strings.NewReader(""), stream(events), nil, "m", nil, limit, 10)
		if !errors.Is(err, errCollectedStreamResponseTooLarge) {
			t.Fatalf("error = %v, want response-too-large", err)
		}
	})

	// Repeated updates to one block bill payload only; the overhead is charged
	// exactly once and the threshold is byte-exact.
	t.Run("repeated thinking updates charge once", func(t *testing.T) {
		base := int64(len(ir.NewID("resp_")) + len("m"))
		events := make([]ir.StreamEvent, 0, 20)
		for i := 0; i < 10; i++ {
			events = append(events,
				ir.StreamEvent{Kind: ir.EventReasoningDelta, Indexed: true, Index: 7, Text: "ab"},
				ir.StreamEvent{Kind: ir.EventReasoningSignature, Index: 7, Signature: "s"},
			)
		}
		limit := base + collectedBlockOverhead + 30
		resp, err := collectStreamResponseWithLimits(strings.NewReader(""), stream(events), nil, "m", nil, limit, 10)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if len(resp.Content) != 1 || resp.Content[0].Text != strings.Repeat("ab", 10) || resp.Content[0].AnthropicSignature != strings.Repeat("s", 10) {
			t.Fatalf("content = %+v", resp.Content)
		}
		_, err = collectStreamResponseWithLimits(strings.NewReader(""), stream(events), nil, "m", nil, limit-1, 10)
		if !errors.Is(err, errCollectedStreamResponseTooLarge) {
			t.Fatalf("error = %v, want response-too-large", err)
		}
	})

	// The dedicated block cap holds even with a generous byte budget.
	t.Run("thinking and redacted block count bounded", func(t *testing.T) {
		thinking := make([]ir.StreamEvent, 0, maxCollectedStreamBlocks+1)
		for i := 0; i <= maxCollectedStreamBlocks; i++ {
			thinking = append(thinking, ir.StreamEvent{Kind: ir.EventReasoningSignature, Index: i, Signature: "s"})
		}
		_, err := collectStreamResponseWithLimits(strings.NewReader(""), stream(thinking), nil, "m", nil, 1<<30, 10)
		if !errors.Is(err, errCollectedStreamResponseTooLarge) {
			t.Fatalf("thinking error = %v, want response-too-large", err)
		}
		redacted := make([]ir.StreamEvent, 0, maxCollectedStreamBlocks+1)
		for i := 0; i <= maxCollectedStreamBlocks; i++ {
			redacted = append(redacted, ir.StreamEvent{Kind: ir.EventRedactedReasoning, Data: "d"})
		}
		_, err = collectStreamResponseWithLimits(strings.NewReader(""), stream(redacted), nil, "m", nil, 1<<30, 10)
		if !errors.Is(err, errCollectedStreamResponseTooLarge) {
			t.Fatalf("redacted error = %v, want response-too-large", err)
		}
	})
}

func TestCollectStreamResponseQoderMetadata(t *testing.T) {
	envelope := func(inner string) string {
		raw, err := json.Marshal(map[string]any{"statusCodeValue": 200, "body": inner})
		if err != nil {
			t.Fatal(err)
		}
		return "data: " + string(raw) + "\n\n"
	}
	qoder := backendCodec(domain.ProtocolQoder, "")

	t.Run("role only is empty stream", func(t *testing.T) {
		body := envelope(`{"id":"chatcmpl-role","model":"auto","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)
		resp, err := collectStreamResponse(strings.NewReader(body), qoder, nil, "fallback-model", nil)
		if resp != nil {
			t.Fatalf("response = %+v, want nil", resp)
		}
		sf, ok := ir.AsStreamFailure(err)
		if !ok {
			t.Fatalf("error = %v, want StreamFailure", err)
		}
		if sf.Message != "upstream returned an empty stream" {
			t.Fatalf("message = %q", sf.Message)
		}
	})

	t.Run("explicit finish is empty completion", func(t *testing.T) {
		body := envelope(`{"id":"chatcmpl-empty","model":"auto","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`) +
			envelope(`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":6,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":2,"cache_write_tokens":1}}}`)
		resp, err := collectStreamResponse(strings.NewReader(body), qoder, nil, "fallback-model", nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.ID != "chatcmpl-empty" || resp.Model != "auto" {
			t.Fatalf("identity = %q/%q", resp.ID, resp.Model)
		}
		if resp.StopReason != ir.StopMaxTokens {
			t.Fatalf("stop = %q", resp.StopReason)
		}
		if len(resp.Content) != 0 {
			t.Fatalf("content = %+v, want empty", resp.Content)
		}
		if resp.Usage.InputTokens != 6 || resp.Usage.OutputTokens != 0 || resp.Usage.CacheReadTokens != 2 || resp.Usage.CacheWriteTokens != 1 {
			t.Fatalf("usage = %+v", resp.Usage)
		}
	})
}

func TestClampErrorMessage(t *testing.T) {
	message := strings.Repeat("x", upstreamErrorMax-1) + "€tail"
	got := clampErrorMessage(message)
	if len(got) > upstreamErrorMax {
		t.Fatalf("length = %d, want <= %d", len(got), upstreamErrorMax)
	}
	if !utf8.ValidString(got) {
		t.Fatal("clamped message is not valid UTF-8")
	}
	if !strings.HasSuffix(got, "x") {
		t.Fatalf("message ended inside multibyte rune: %q", got[len(got)-4:])
	}
}

func TestWriteErrClampsMessage(t *testing.T) {
	message := strings.Repeat("x", upstreamErrorMax+1)
	rec := httptest.NewRecorder()
	writeErr(rec, openaiCodec, http.StatusBadGateway, message, "api_error")
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Error.Message) != upstreamErrorMax {
		t.Fatalf("client error length = %d, want %d", len(envelope.Error.Message), upstreamErrorMax)
	}
}

func TestUpstreamErrorMessage(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name:   "openai nested error message",
			status: http.StatusTooManyRequests,
			body:   `{"error":{"message":"rate limit exceeded","type":"rate_limit"}}`,
			want:   "rate limit exceeded",
		},
		{
			name:   "anthropic nested error message",
			status: http.StatusServiceUnavailable,
			body:   `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`,
			want:   "overloaded",
		},
		{
			name:   "top-level message",
			status: http.StatusBadRequest,
			body:   `{"message":"invalid request"}`,
			want:   "invalid request",
		},
		{
			name:   "top-level detail",
			status: http.StatusBadRequest,
			body:   `{"detail":"invalid parameter"}`,
			want:   "invalid parameter",
		},
		{
			name:   "string error field",
			status: http.StatusBadRequest,
			body:   `{"error":"bad input"}`,
			want:   "bad input",
		},
		{
			name:   "non-json body uses status fallback",
			status: http.StatusBadGateway,
			body:   `Internal Server Error`,
			want:   "upstream returned 502 Bad Gateway",
		},
		{
			name:   "empty structured message uses status fallback",
			status: http.StatusUnauthorized,
			body:   `{"error":{"message":""}}`,
			want:   "upstream returned 401 Unauthorized",
		},
		{
			name:   "empty body uses status fallback",
			status: 599,
			body:   ``,
			want:   "upstream returned HTTP status 599",
		},
		{
			name:   "unrecognized json uses status fallback",
			status: http.StatusInternalServerError,
			body:   `{"type":"ok"}`,
			want:   "upstream returned 500 Internal Server Error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamErrorMessage(tc.status, []byte(tc.body)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// sinkToolWant is the expected identity and raw argument payload of one
// translated tool call in first-seen order.
type sinkToolWant struct {
	id   string
	name string
	args string
}

// TestTranslatedSinkGroupsToolFragmentsByIndex covers the estimate collector:
// fragments must attach to their IR tool index, not to an ordinal slot shared
// with the independent name list. The expected output count is derived from the
// expected names and arguments so misattribution cannot hide behind an equal
// aggregate length.
func TestTranslatedSinkGroupsToolFragmentsByIndex(t *testing.T) {
	cases := []struct {
		name     string
		events   []ir.StreamEvent
		wantText string
		wantStop ir.StopReason
		want     []sinkToolWant
	}{
		{
			name: "single multi-fragment",
			events: []ir.StreamEvent{
				{Kind: ir.EventTextDelta, Text: "hi "},
				{Kind: ir.EventToolCallStart, Index: 0, ToolID: "call_1", ToolName: "get_weather"},
				{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `{"city":`},
				{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `"paris"}`},
				{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
			},
			wantText: "hi ",
			wantStop: ir.StopToolUse,
			want:     []sinkToolWant{{id: "call_1", name: "get_weather", args: `{"city":"paris"}`}},
		},
		{
			name: "two interleaved tools",
			events: []ir.StreamEvent{
				{Kind: ir.EventToolCallStart, Index: 0, ToolID: "a0", ToolName: "alpha"},
				{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: "AAA"},
				{Kind: ir.EventToolCallStart, Index: 1, ToolID: "b1", ToolName: "beta"},
				{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: "B"},
				{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: "CCC"},
				{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: "BB"},
			},
			want: []sinkToolWant{
				{id: "a0", name: "alpha", args: "AAACCC"},
				{id: "b1", name: "beta", args: "BBB"},
			},
		},
		{
			name: "sparse out of order first seen",
			events: []ir.StreamEvent{
				{Kind: ir.EventToolCallStart, Index: 5, ToolID: "i5", ToolName: "five"},
				{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: "zero"},
				{Kind: ir.EventToolCallStart, Index: 0, ToolID: "i0", ToolName: "zero-name"},
				{Kind: ir.EventToolCallDelta, Index: -3, ArgsFrag: "neg"},
				{Kind: ir.EventToolCallDelta, Index: 5, ArgsFrag: "five-args"},
			},
			want: []sinkToolWant{
				{id: "i5", name: "five", args: "five-args"},
				{id: "i0", name: "zero-name", args: "zero"},
				{name: "", args: "neg"},
			},
		},
		{
			name: "args before identity",
			events: []ir.StreamEvent{
				{Kind: ir.EventToolCallDelta, Index: 3, ArgsFrag: `{"a":`},
				{Kind: ir.EventToolCallStart, Index: 3, ToolID: "l3", ToolName: "late"},
				{Kind: ir.EventToolCallDelta, Index: 3, ArgsFrag: `1}`},
			},
			want: []sinkToolWant{{id: "l3", name: "late", args: `{"a":1}`}},
		},
		{
			name: "repeated split start preserves args and identity",
			events: []ir.StreamEvent{
				{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: "X"},
				{Kind: ir.EventToolCallStart, Index: 1, ToolID: "only-id"},
				{Kind: ir.EventToolCallStart, Index: 1},
				{Kind: ir.EventToolCallStart, Index: 1, ToolName: "final-name"},
				{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: "Y"},
			},
			want: []sinkToolWant{{id: "only-id", name: "final-name", args: "XY"}},
		},
		{
			name: "empty fragment",
			events: []ir.StreamEvent{
				{Kind: ir.EventToolCallStart, Index: 0, ToolName: "e"},
				{Kind: ir.EventToolCallDelta, Index: 0},
				{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: ""},
			},
			want: []sinkToolWant{{name: "e", args: ""}},
		},
		{
			name: "start args ignored",
			events: []ir.StreamEvent{
				{Kind: ir.EventToolCallStart, Index: 0, ToolName: "n", ArgsFrag: "SHOULD_NOT_COUNT"},
				{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: "real"},
			},
			want: []sinkToolWant{{name: "n", args: "real"}},
		},
		{
			name: "malformed partial json retained",
			events: []ir.StreamEvent{
				{Kind: ir.EventToolCallStart, Index: 0, ToolName: "m"},
				{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `{"a":`},
			},
			want: []sinkToolWant{{name: "m", args: `{"a":`}},
		},
		{
			name: "text and readable reasoning preserved",
			events: []ir.StreamEvent{
				{Kind: ir.EventTextDelta, Text: "t"},
				{Kind: ir.EventReasoningDelta, Text: "r"},
				{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
			},
			wantText: "tr",
			wantStop: ir.StopEndTurn,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &translatedSink{}
			for _, ev := range tc.events {
				sink.note(ev)
			}
			resp := sink.response()
			if resp.StopReason != tc.wantStop {
				t.Fatalf("stop reason = %q, want %q", resp.StopReason, tc.wantStop)
			}
			var gotText string
			var gotTools []sinkToolWant
			for _, block := range resp.Content {
				switch block.Type {
				case ir.BlockText:
					gotText += block.Text
				case ir.BlockToolUse:
					gotTools = append(gotTools, sinkToolWant{id: block.ToolID, name: block.ToolName, args: string(block.ToolInput)})
				}
			}
			if gotText != tc.wantText {
				t.Errorf("text = %q, want %q", gotText, tc.wantText)
			}
			if len(gotTools) != len(tc.want) {
				t.Fatalf("tools = %+v, want %+v", gotTools, tc.want)
			}
			for i := range tc.want {
				if gotTools[i] != tc.want[i] {
					t.Errorf("tool[%d] = %+v, want %+v", i, gotTools[i], tc.want[i])
				}
			}
			wantOut := charsToTokens(len(tc.wantText) + toolArgChars(tc.want))
			if _, out := estimateUsage(nil, resp); out != wantOut {
				t.Errorf("estimate output = %d, want %d", out, wantOut)
			}
		})
	}
}

func toolArgChars(tools []sinkToolWant) int {
	n := 0
	for _, tool := range tools {
		n += len(tool.name) + len(tool.args)
	}
	return n
}

// TestTranslatedSinkResponseSnapshotIsRepeatable verifies response() does not
// consume accumulated state: repeated calls return equal snapshots and a later
// fragment appends without mutating an earlier snapshot's byte slice.
func TestTranslatedSinkResponseSnapshotIsRepeatable(t *testing.T) {
	sink := &translatedSink{}
	sink.note(ir.StreamEvent{Kind: ir.EventToolCallStart, Index: 0, ToolID: "id", ToolName: "n"})
	sink.note(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: "A"})
	first := sink.response()
	second := sink.response()
	if len(first.Content) != 1 || string(first.Content[0].ToolInput) != "A" {
		t.Fatalf("first = %+v, want one tool with args A", first.Content)
	}
	if len(second.Content) != 1 || string(second.Content[0].ToolInput) != "A" {
		t.Fatalf("second = %+v, want one tool with args A", second.Content)
	}
	sink.note(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: "B"})
	if string(first.Content[0].ToolInput) != "A" {
		t.Fatalf("first snapshot mutated after append: %q", first.Content[0].ToolInput)
	}
	third := sink.response()
	if len(third.Content) != 1 || string(third.Content[0].ToolInput) != "AB" {
		t.Fatalf("third = %+v, want one tool with args AB", third.Content)
	}
}

func TestTranslatedSinkNilAndEmptyResponse(t *testing.T) {
	var nilSink *translatedSink
	if got := nilSink.response(); got != nil {
		t.Fatalf("nil sink response = %+v, want nil", got)
	}
	empty := (&translatedSink{}).response()
	if empty == nil || len(empty.Content) != 0 {
		t.Fatalf("empty sink response = %+v, want no content", empty)
	}
}
