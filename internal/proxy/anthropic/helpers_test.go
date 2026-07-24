package anthropic

import (
	"encoding/json"
	"testing"

	"airouter/internal/proxy/ir"
)

func textBlocks(texts ...string) json.RawMessage {
	blocks := make([]map[string]string, len(texts))
	for i, t := range texts {
		blocks[i] = map[string]string{"type": "text", "text": t}
	}
	raw, _ := json.Marshal(blocks)
	return raw
}

func jsonString(s string) json.RawMessage {
	raw, _ := json.Marshal(s)
	return raw
}

func TestSystemToTextStripsBillingHeader(t *testing.T) {
	const opener = "You are Claude Code, Anthropic's official CLI for Claude."
	const prompt = "You are an interactive agent that helps users with software engineering tasks."
	const ccBlock = opener + "\n" + prompt
	const billing = "x-anthropic-billing-header: cc_version=2.1.177.01c; cc_entrypoint=cli; cch=256ac;"
	cases := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{
			name: "array block with billing prefix and identity opener",
			raw:  textBlocks(billing + ccBlock),
			want: prompt,
		},
		{
			name: "billing header as its own block",
			raw:  textBlocks(billing, ccBlock),
			want: prompt,
		},
		{
			name: "string system with billing prefix and opener",
			raw:  jsonString("x-anthropic-billing-header: cc_version=2.1.177.01c; cch=256ac;" + ccBlock),
			want: prompt,
		},
		{
			name: "identity opener without billing header",
			raw:  textBlocks(ccBlock),
			want: prompt,
		},
		{
			name: "no markers is untouched",
			raw:  textBlocks(prompt),
			want: prompt,
		},
		{
			name: "incidental mid-prompt Claude Code mention preserved",
			raw:  textBlocks(prompt + "\nClaude Code is available as a CLI."),
			want: prompt + "\nClaude Code is available as a CLI.",
		},
		{
			name: "unrelated leading text untouched",
			raw:  jsonString("x-something-else: keep me;rest"),
			want: "x-something-else: keep me;rest",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := systemToText(tc.raw)
			if got != tc.want {
				t.Fatalf("systemToText:\n got  %q\n want %q", got, tc.want)
			}
		})
	}
}

func TestEncodeError(t *testing.T) {
	t.Run("default errType when empty", func(t *testing.T) {
		raw := EncodeError("bad", "")
		var got struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if got.Type != "error" {
			t.Errorf("outer type = %q, want error", got.Type)
		}
		if got.Error.Type != "invalid_request_error" {
			t.Errorf("error.type = %q, want invalid_request_error", got.Error.Type)
		}
		if got.Error.Message != "bad" {
			t.Errorf("message = %q, want bad", got.Error.Message)
		}
	})
	t.Run("set errType", func(t *testing.T) {
		raw := EncodeError("nope", "overloaded_error")
		var got struct {
			Error struct{ Type string `json:"type"` } `json:"error"`
		}
		_ = json.Unmarshal(raw, &got)
		if got.Error.Type != "overloaded_error" {
			t.Errorf("type = %q, want overloaded_error", got.Error.Type)
		}
	})
}

func TestImageFromSource(t *testing.T) {
	t.Run("nil returns empty image", func(t *testing.T) {
		img := imageFromSource(nil)
		if img == nil || img.MediaType != "" || img.Data != "" || img.URL != "" {
			t.Errorf("got %+v, want empty ir.Image", img)
		}
	})
	t.Run("url type", func(t *testing.T) {
		img := imageFromSource(&anthSource{Type: "url", URL: "https://e.com/x.png"})
		if img.URL != "https://e.com/x.png" || img.Data != "" {
			t.Errorf("got %+v, want URL passthrough", img)
		}
	})
	t.Run("base64 type", func(t *testing.T) {
		img := imageFromSource(&anthSource{Type: "base64", MediaType: "image/png", Data: "abc"})
		if img.MediaType != "image/png" || img.Data != "abc" || img.URL != "" {
			t.Errorf("got %+v, want mediaType+data", img)
		}
	})
	t.Run("unknown type defaults to base64 shape", func(t *testing.T) {
		img := imageFromSource(&anthSource{Type: "weird", MediaType: "image/jpeg", Data: "def"})
		if img.MediaType != "image/jpeg" || img.Data != "def" {
			t.Errorf("got %+v, want mediaType+data for non-url", img)
		}
	})
}

func TestSourceFromImage(t *testing.T) {
	t.Run("nil returns empty source", func(t *testing.T) {
		src := sourceFromImage(nil)
		if src == nil || src.Type != "" {
			t.Errorf("got %+v, want empty anthSource", src)
		}
	})
	t.Run("data image becomes base64", func(t *testing.T) {
		src := sourceFromImage(&ir.Image{MediaType: "image/png", Data: "abc"})
		if src.Type != "base64" || src.MediaType != "image/png" || src.Data != "abc" {
			t.Errorf("got %+v, want base64 source", src)
		}
	})
	t.Run("data image defaults mediaType to png", func(t *testing.T) {
		src := sourceFromImage(&ir.Image{Data: "abc"})
		if src.MediaType != "image/png" {
			t.Errorf("MediaType = %q, want image/png", src.MediaType)
		}
	})
	t.Run("url only image becomes url source", func(t *testing.T) {
		src := sourceFromImage(&ir.Image{URL: "https://e.com/x.png"})
		if src.Type != "url" || src.URL != "https://e.com/x.png" || src.Data != "" {
			t.Errorf("got %+v, want url source", src)
		}
	})
}

func TestStopReason(t *testing.T) {
	cases := []struct {
		in   string
		want ir.StopReason
	}{
		{"max_tokens", ir.StopMaxTokens},
		{"tool_use", ir.StopToolUse},
		{"stop_sequence", ir.StopStopSequence},
		{"end_turn", ir.StopEndTurn},
		{"", ir.StopEndTurn},
		{"unknown", ir.StopEndTurn},
	}
	for _, tc := range cases {
		if got := stopReason(tc.in); got != tc.want {
			t.Errorf("stopReason(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStopReasonWire(t *testing.T) {
	cases := []struct {
		sr   ir.StopReason
		want string
	}{
		{ir.StopMaxTokens, "max_tokens"},
		{ir.StopToolUse, "tool_use"},
		{ir.StopStopSequence, "stop_sequence"},
		{ir.StopEndTurn, "end_turn"},
		{"unknown", "end_turn"},
	}
	for _, tc := range cases {
		if got := stopReasonWire(tc.sr); got != tc.want {
			t.Errorf("stopReasonWire(%q) = %q, want %q", tc.sr, got, tc.want)
		}
	}
}

func TestStopReason2(t *testing.T) {
	t.Run("empty returns end_turn", func(t *testing.T) {
		if got := stopReason2(""); got != ir.StopEndTurn {
			t.Errorf("got %q, want end_turn", got)
		}
	})
	t.Run("non-empty delegates to stopReason", func(t *testing.T) {
		if got := stopReason2("max_tokens"); got != ir.StopMaxTokens {
			t.Errorf("got %q, want max_tokens", got)
		}
		if got := stopReason2("tool_use"); got != ir.StopToolUse {
			t.Errorf("got %q, want tool_use", got)
		}
	})
}

func TestDecodeBlocks(t *testing.T) {
	t.Run("empty raw nil", func(t *testing.T) {
		if got := decodeBlocks(nil); got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})
	t.Run("string", func(t *testing.T) {
		got := decodeBlocks(jsonString("hello"))
		if len(got) != 1 || got[0].Type != ir.BlockText || got[0].Text != "hello" {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("empty string nil", func(t *testing.T) {
		if got := decodeBlocks(jsonString("")); got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})
	t.Run("text block", func(t *testing.T) {
		raw := mustJSONRaw([]map[string]string{{"type": "text", "text": "hi"}})
		got := decodeBlocks(raw)
		if len(got) != 1 || got[0].Type != ir.BlockText || got[0].Text != "hi" {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("image url block", func(t *testing.T) {
		raw := mustJSONRaw([]map[string]any{{"type": "image", "source": map[string]string{"type": "url", "url": "https://e.com/x.png"}}})
		got := decodeBlocks(raw)
		if len(got) != 1 || got[0].Type != ir.BlockImage || got[0].Image == nil || got[0].Image.URL != "https://e.com/x.png" {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("image base64 block", func(t *testing.T) {
		raw := mustJSONRaw([]map[string]any{{"type": "image", "source": map[string]string{"type": "base64", "media_type": "image/png", "data": "abc"}}})
		got := decodeBlocks(raw)
		if len(got) != 1 || got[0].Type != ir.BlockImage || got[0].Image == nil || got[0].Image.MediaType != "image/png" || got[0].Image.Data != "abc" {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("tool_use block", func(t *testing.T) {
		raw := mustJSONRaw([]map[string]any{{"type": "tool_use", "id": "t1", "name": "get_weather", "input": map[string]string{"city": "sf"}}})
		got := decodeBlocks(raw)
		if len(got) != 1 || got[0].Type != ir.BlockToolUse || got[0].ToolID != "t1" || got[0].ToolName != "get_weather" {
			t.Errorf("got %+v", got)
		}
		if string(got[0].ToolInput) != `{"city":"sf"}` {
			t.Errorf("ToolInput = %s, want {\"city\":\"sf\"}", got[0].ToolInput)
		}
	})
	t.Run("tool_result nested blocks", func(t *testing.T) {
		inner := mustJSONRaw([]map[string]string{{"type": "text", "text": "sunny"}})
		raw := mustJSONRaw([]map[string]any{{"type": "tool_result", "tool_use_id": "t1", "content": inner}})
		got := decodeBlocks(raw)
		if len(got) != 1 || got[0].Type != ir.BlockToolResult || got[0].ToolUseID != "t1" {
			t.Errorf("got %+v", got)
		}
		if len(got[0].ToolResult) != 1 || got[0].ToolResult[0].Text != "sunny" {
			t.Errorf("nested ToolResult = %+v", got[0].ToolResult)
		}
	})
	t.Run("invalid json nil", func(t *testing.T) {
		if got := decodeBlocks(json.RawMessage(`{not valid`)); got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})
}

// mustJSONRaw marshals v to JSON, panicking on error (test data only).
func mustJSONRaw(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
