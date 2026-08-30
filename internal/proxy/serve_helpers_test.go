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

	"airouter/internal/proxy/ir"
)

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
		const limit = int64(128)
		fallback := "m"
		base := len(ir.NewID("resp_")) + len(fallback)
		text := strings.Repeat("x", int(limit)-base)
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
