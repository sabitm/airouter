package proxy

import (
	"strings"
	"testing"
)

func TestParseUsage(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantIn  int
		wantOut int
	}{
		{
			name:    "openai shape",
			body:    `{"usage":{"prompt_tokens":10,"completion_tokens":20}}`,
			wantIn:  10,
			wantOut: 20,
		},
		{
			name:    "anthropic shape",
			body:    `{"usage":{"input_tokens":5,"output_tokens":7}}`,
			wantIn:  5,
			wantOut: 7,
		},
		{
			name:    "anthropic with cache folds into input",
			body:    `{"usage":{"input_tokens":5,"output_tokens":7,"cache_creation_input_tokens":3,"cache_read_input_tokens":2}}`,
			wantIn:  10,
			wantOut: 7,
		},
		{
			name:    "both shapes summed",
			body:    `{"usage":{"prompt_tokens":10,"completion_tokens":20,"input_tokens":5,"output_tokens":7}}`,
			wantIn:  15,
			wantOut: 27,
		},
		{"empty body", ``, 0, 0},
		{"invalid json", `{not valid`, 0, 0},
		{
			name:    "no usage object",
			body:    `{"id":"x"}`,
			wantIn:  0,
			wantOut: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, out := parseUsage([]byte(tc.body))
			if in != tc.wantIn {
				t.Errorf("input = %d, want %d", in, tc.wantIn)
			}
			if out != tc.wantOut {
				t.Errorf("output = %d, want %d", out, tc.wantOut)
			}
		})
	}
}

func TestUpstreamErrorMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "openai nested error message",
			body: `{"error":{"message":"rate limit exceeded","type":"rate_limit"}}`,
			want: "rate limit exceeded",
		},
		{
			name: "anthropic nested error message",
			body: `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`,
			want: "overloaded",
		},
		{
			name: "non-json body returns raw body",
			body: `Internal Server Error`,
			want: "Internal Server Error",
		},
		{
			name: "nested but empty message falls back to raw body",
			body: `{"error":{"message":""}}`,
			want: `{"error":{"message":""}}`,
		},
		{
			name: "empty body returns default",
			body: ``,
			want: "upstream error",
		},
		{
			name: "json without error object falls back to raw body",
			body: `{"type":"ok"}`,
			want: `{"type":"ok"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamErrorMessage([]byte(tc.body)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTruncateMsg(t *testing.T) {
	t.Run("under limit unchanged", func(t *testing.T) {
		s := "short message"
		if got := truncateMsg(s, 100); got != s {
			t.Errorf("got %q, want %q", got, s)
		}
	})
	t.Run("exactly at limit unchanged", func(t *testing.T) {
		s := strings.Repeat("a", 50)
		if got := truncateMsg(s, 50); got != s {
			t.Errorf("got %q, want %q", got, s)
		}
	})
	t.Run("over limit truncates with marker", func(t *testing.T) {
		s := strings.Repeat("a", 100)
		got := truncateMsg(s, 10)
		if !strings.HasPrefix(got, "aaaaaaaaaa") {
			t.Errorf("expected first 10 bytes, got %q", got)
		}
		if !strings.Contains(got, "truncated") {
			t.Errorf("expected marker, got %q", got)
		}
		if !strings.Contains(got, "100 bytes total") {
			t.Errorf("expected total byte count, got %q", got)
		}
	})
	t.Run("empty string under limit", func(t *testing.T) {
		if got := truncateMsg("", 10); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("limit zero truncates non-empty", func(t *testing.T) {
		s := "abc"
		got := truncateMsg(s, 0)
		if !strings.Contains(got, "truncated") || !strings.Contains(got, "3 bytes total") {
			t.Errorf("expected truncation marker for 3 bytes, got %q", got)
		}
	})
}
