package openai

import (
	"encoding/json"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestParseStop(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"null", "null", nil},
		{"empty raw", "", nil},
		{"single string", `"stop"`, []string{"stop"}},
		{"empty string", `""`, nil},
		{"array", `["a","b"]`, []string{"a", "b"}},
		{"invalid json", `[not valid`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseStop(json.RawMessage(tc.raw))
			if len(got) != len(tc.want) {
				t.Fatalf("parseStop(%s) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseStop(%s)[%d] = %q, want %q", tc.raw, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestContentToText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"string", `"hello"`, "hello"},
		{"null", "null", ""},
		{"empty", "", ""},
		{"array text parts", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "ab"},
		{"array non-text only", `[{"type":"image_url","image_url":{"url":"data:x"}}]`, ""},
		{"mixed parts", `[{"type":"image_url"},{"type":"text","text":"keep"}]`, "keep"},
		{"invalid json", `{not valid`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := contentToText(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("contentToText(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestToolResultText(t *testing.T) {
	t.Run("flattens text blocks", func(t *testing.T) {
		b := ir.ContentBlock{ToolResult: []ir.ContentBlock{
			{Type: ir.BlockText, Text: "result "},
			{Type: ir.BlockText, Text: "continued"},
		}}
		if got := toolResultText(b); got != "result continued" {
			t.Errorf("got %q, want %q", got, "result continued")
		}
	})
	t.Run("skips non-text", func(t *testing.T) {
		b := ir.ContentBlock{ToolResult: []ir.ContentBlock{
			{Type: ir.BlockText, Text: "keep"},
			{Type: ir.BlockImage},
		}}
		if got := toolResultText(b); got != "keep" {
			t.Errorf("got %q, want %q", got, "keep")
		}
	})
	t.Run("empty", func(t *testing.T) {
		if got := toolResultText(ir.ContentBlock{}); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestImageFromURL(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		wantMedia  string
		wantData   string
		wantURL    string
		wantIsData bool
	}{
		{"data url with mediaType", "data:image/png;base64,abc", "image/png", "abc", "", true},
		{"data url with jpeg", "data:image/jpeg;base64,ZGVm", "image/jpeg", "ZGVm", "", true},
		{"data without base64 flag", "data:image/gif,raw", "image/gif", "raw", "", true},
		{"plain http url", "https://example.com/img.png", "", "", "https://example.com/img.png", false},
		{"empty", "", "", "", "", false},
		{"malformed data no comma", "data:image/png;base64", "", "", "data:image/png;base64", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img := imageFromURL(tc.url)
			if img == nil {
				t.Fatal("imageFromURL returned nil")
			}
			if tc.wantIsData {
				if img.MediaType != tc.wantMedia {
					t.Errorf("MediaType = %q, want %q", img.MediaType, tc.wantMedia)
				}
				if img.Data != tc.wantData {
					t.Errorf("Data = %q, want %q", img.Data, tc.wantData)
				}
				if img.URL != "" {
					t.Errorf("URL = %q, want empty for data image", img.URL)
				}
			} else {
				if img.URL != tc.wantURL {
					t.Errorf("URL = %q, want %q", img.URL, tc.wantURL)
				}
				if img.Data != "" {
					t.Errorf("Data = %q, want empty for url-only image", img.Data)
				}
			}
		})
	}
}

func TestImageToURL(t *testing.T) {
	t.Run("nil returns empty", func(t *testing.T) {
		if got := imageToURL(nil); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("data image round trips with mediaType", func(t *testing.T) {
		img := &ir.Image{MediaType: "image/png", Data: "abc"}
		want := "data:image/png;base64,abc"
		if got := imageToURL(img); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("data image defaults mediaType to png", func(t *testing.T) {
		img := &ir.Image{Data: "abc"}
		want := "data:image/png;base64,abc"
		if got := imageToURL(img); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("url only passthrough", func(t *testing.T) {
		img := &ir.Image{URL: "https://example.com/x.png"}
		if got := imageToURL(img); got != "https://example.com/x.png" {
			t.Errorf("got %q, want passthrough", got)
		}
	})
}

func TestMustText(t *testing.T) {
	got := mustText("hello")
	var s string
	if err := json.Unmarshal(got, &s); err != nil || s != "hello" {
		t.Errorf("mustText(\"hello\") = %s, want \"hello\"", got)
	}
}

func TestRawOrNull(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "{}"},
		{"whitespace", "   ", "{}"},
		{"json object", `{"a":1}`, `{"a":1}`},
		{"json array", `["x"]`, `["x"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(rawOrNull(tc.in)); got != tc.want {
				t.Errorf("rawOrNull(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDecodeToolChoice(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    *ir.ToolChoice
		wantNil bool
	}{
		{"empty", "", nil, true},
		{"auto", `"auto"`, &ir.ToolChoice{Type: ir.ToolChoiceAuto}, false},
		{"none", `"none"`, &ir.ToolChoice{Type: ir.ToolChoiceNone}, false},
		{"required", `"required"`, &ir.ToolChoice{Type: ir.ToolChoiceAny}, false},
		{"object function", `{"type":"function","function":{"name":"get_weather"}}`, &ir.ToolChoice{Type: ir.ToolChoiceTool, Name: "get_weather"}, false},
		{"unknown string", `"random"`, nil, true},
		{"invalid json string", `"unclosed`, nil, true},
		{"invalid object", `{not valid`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeToolChoice(json.RawMessage(tc.raw))
			if tc.wantNil {
				if got != nil {
					t.Errorf("decodeToolChoice(%s) = %+v, want nil", tc.raw, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("decodeToolChoice(%s) = nil, want %+v", tc.raw, tc.want)
			}
			if got.Type != tc.want.Type || got.Name != tc.want.Name {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestEncodeToolChoice(t *testing.T) {
	cases := []struct {
		name string
		tc   *ir.ToolChoice
		want string
	}{
		{"auto", &ir.ToolChoice{Type: ir.ToolChoiceAuto}, `"auto"`},
		{"none", &ir.ToolChoice{Type: ir.ToolChoiceNone}, `"none"`},
		{"required", &ir.ToolChoice{Type: ir.ToolChoiceAny}, `"required"`},
		{"tool", &ir.ToolChoice{Type: ir.ToolChoiceTool, Name: "get_weather"}, `{"function":{"name":"get_weather"},"type":"function"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(encodeToolChoice(tc.tc))
			var wantAny, gotAny any
			_ = json.Unmarshal([]byte(got), &gotAny)
			_ = json.Unmarshal([]byte(tc.want), &wantAny)
			if gotStr, _ := json.Marshal(gotAny); string(gotStr) != tc.want {
				t.Errorf("encodeToolChoice() = %s, want %s", got, tc.want)
			}
		})
	}
	t.Run("unknown type returns nil", func(t *testing.T) {
		if got := encodeToolChoice(&ir.ToolChoice{Type: "unknown"}); got != nil {
			t.Errorf("got %s, want nil for unknown type", got)
		}
	})
}

func TestStopReasonFromFinish(t *testing.T) {
	cases := []struct {
		finish string
		want   ir.StopReason
	}{
		{"length", ir.StopMaxTokens},
		{"tool_calls", ir.StopToolUse},
		{"function_call", ir.StopToolUse},
		{"stop", ir.StopEndTurn},
		{"", ir.StopEndTurn},
		{"unknown", ir.StopEndTurn},
	}
	for _, tc := range cases {
		if got := stopReasonFromFinish(tc.finish); got != tc.want {
			t.Errorf("stopReasonFromFinish(%q) = %q, want %q", tc.finish, got, tc.want)
		}
	}
}

func TestFinishFromStopReason(t *testing.T) {
	cases := []struct {
		sr   ir.StopReason
		want string
	}{
		{ir.StopMaxTokens, "length"},
		{ir.StopToolUse, "tool_calls"},
		{ir.StopEndTurn, "stop"},
		{ir.StopStopSequence, "stop"},
	}
	for _, tc := range cases {
		if got := finishFromStopReason(tc.sr); got != tc.want {
			t.Errorf("finishFromStopReason(%q) = %q, want %q", tc.sr, got, tc.want)
		}
	}
}

func TestEncodeErrorOpenAI(t *testing.T) {
	t.Run("default type when empty", func(t *testing.T) {
		raw := EncodeError("bad request", "")
		var got struct {
			Error struct {
				Message string          `json:"message"`
				Type    string          `json:"type"`
				Code    json.RawMessage `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, raw)
		}
		if got.Error.Message != "bad request" {
			t.Errorf("message = %q", got.Error.Message)
		}
		if got.Error.Type != "invalid_request_error" {
			t.Errorf("type = %q, want invalid_request_error", got.Error.Type)
		}
		if string(got.Error.Code) != "null" {
			t.Errorf("code = %s, want null", got.Error.Code)
		}
	})

	t.Run("explicit type preserved", func(t *testing.T) {
		raw := EncodeError("overloaded", "overloaded_error")
		var got struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &got)
		if got.Error.Type != "overloaded_error" {
			t.Errorf("type = %q, want overloaded_error", got.Error.Type)
		}
	})
}
