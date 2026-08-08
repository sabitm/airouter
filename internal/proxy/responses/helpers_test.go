package responses

import (
	"encoding/json"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestEncodeError(t *testing.T) {
	t.Run("default errType when empty", func(t *testing.T) {
		raw := EncodeError("bad", "")
		var got struct {
			Error struct {
				Message string          `json:"message"`
				Type    string          `json:"type"`
				Code    json.RawMessage `json:"code"`
				Param   json.RawMessage `json:"param"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if got.Error.Message != "bad" {
			t.Errorf("message = %q, want bad", got.Error.Message)
		}
		if got.Error.Type != "invalid_request_error" {
			t.Errorf("type = %q, want invalid_request_error", got.Error.Type)
		}
		if string(got.Error.Code) != "null" || string(got.Error.Param) != "null" {
			t.Errorf("code/param = %s/%s, want null/null", got.Error.Code, got.Error.Param)
		}
	})
	t.Run("set errType", func(t *testing.T) {
		raw := EncodeError("nope", "server_error")
		var got struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &got)
		if got.Error.Type != "server_error" {
			t.Errorf("type = %q, want server_error", got.Error.Type)
		}
	})
}

func TestContentToText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"string", `"hi"`, "hi"},
		{"empty", "", ""},
		{"array text parts", `[{"type":"input_text","text":"a"},{"type":"output_text","text":"b"}]`, "ab"},
		{"invalid json", `{bad`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := contentToText(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestToolResultText(t *testing.T) {
	t.Run("flattens text blocks", func(t *testing.T) {
		b := ir.ContentBlock{ToolResult: []ir.ContentBlock{
			{Type: ir.BlockText, Text: "r "},
			{Type: ir.BlockText, Text: "ok"},
		}}
		if got := toolResultText(b); got != "r ok" {
			t.Errorf("got %q, want r ok", got)
		}
	})
	t.Run("skips non-text", func(t *testing.T) {
		b := ir.ContentBlock{ToolResult: []ir.ContentBlock{
			{Type: ir.BlockText, Text: "keep"},
			{Type: ir.BlockImage},
		}}
		if got := toolResultText(b); got != "keep" {
			t.Errorf("got %q, want keep", got)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if got := toolResultText(ir.ContentBlock{}); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestImageURLString(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"bare string", `"https://e.com/x.png"`, "https://e.com/x.png"},
		{"object", `{"url":"https://e.com/y.png"}`, "https://e.com/y.png"},
		{"empty", "", ""},
		{"invalid object", `{bad`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageURLString(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
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
		// Non-base64 data URLs are rejected at parse; left on URL for InspectRequest.
		{"data without base64 flag", "data:image/gif,raw", "", "", "data:image/gif,raw", false},
		{"plain url", "https://example.com/img.png", "", "", "https://example.com/img.png", false},
		{"empty", "", "", "", "", false},
		{"malformed no comma", "data:image/png;base64", "", "", "data:image/png;base64", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img := imageFromURL(tc.url)
			if img == nil {
				t.Fatal("nil")
			}
			if tc.wantIsData {
				if img.MediaType != tc.wantMedia {
					t.Errorf("MediaType = %q, want %q", img.MediaType, tc.wantMedia)
				}
				if img.Data != tc.wantData {
					t.Errorf("Data = %q, want %q", img.Data, tc.wantData)
				}
			} else if img.URL != tc.wantURL {
				t.Errorf("URL = %q, want %q", img.URL, tc.wantURL)
			}
		})
	}
}

func TestImageToURL(t *testing.T) {
	t.Run("nil empty", func(t *testing.T) {
		if got := imageToURL(nil); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("data round trips", func(t *testing.T) {
		img := &ir.Image{MediaType: "image/png", Data: "abc"}
		if got := imageToURL(img); got != "data:image/png;base64,abc" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("default mediaType", func(t *testing.T) {
		img := &ir.Image{Data: "abc"}
		if got := imageToURL(img); got != "data:image/png;base64,abc" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("url passthrough", func(t *testing.T) {
		img := &ir.Image{URL: "https://e.com/x.png"}
		if got := imageToURL(img); got != "https://e.com/x.png" {
			t.Errorf("got %q", got)
		}
	})
}

func TestMustJSON(t *testing.T) {
	raw := mustJSON(map[string]int{"a": 1})
	var got map[string]int
	if err := json.Unmarshal(raw, &got); err != nil || got["a"] != 1 {
		t.Errorf("mustJSON = %s, err %v", raw, err)
	}
}

func TestRawArgs(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "{}"},
		{"   ", "{}"},
		{`{"x":1}`, `{"x":1}`},
	}
	for _, tc := range cases {
		if got := string(rawArgs(tc.in)); got != tc.want {
			t.Errorf("rawArgs(%q) = %q, want %q", tc.in, got, tc.want)
		}
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
		{"object", `{"type":"function","name":"get_weather"}`, &ir.ToolChoice{Type: ir.ToolChoiceTool, Name: "get_weather"}, false},
		{"unknown string", `"random"`, nil, true},
		{"invalid string", `"bad`, nil, true},
		{"invalid object", `{bad`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeToolChoice(json.RawMessage(tc.raw))
			if tc.wantNil {
				if got != nil {
					t.Errorf("got %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("nil, want %+v", tc.want)
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
		{"tool", &ir.ToolChoice{Type: ir.ToolChoiceTool, Name: "f"}, `{"name":"f","type":"function"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(encodeToolChoice(tc.tc))
			if string(raw) != tc.want {
				t.Errorf("got %s, want %s", raw, tc.want)
			}
		})
	}
	t.Run("unknown nil", func(t *testing.T) {
		if got := encodeToolChoice(&ir.ToolChoice{Type: "x"}); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}

func TestResponsesStopReason(t *testing.T) {
	cases := []struct {
		name    string
		status  string
		sawTool bool
		want    ir.StopReason
	}{
		{"incomplete", "incomplete", false, ir.StopMaxTokens},
		{"completed with tool", "completed", true, ir.StopToolUse},
		{"completed no tool", "completed", false, ir.StopEndTurn},
		{"other with tool", "other", true, ir.StopToolUse},
		{"other no tool", "other", false, ir.StopEndTurn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := responsesStopReason(tc.status, tc.sawTool); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCodexEffortForModel(t *testing.T) {
	cases := []struct {
		model    string
		wantBase string
		wantEff  string
	}{
		{"gpt-5.3-codex-high", "gpt-5.3-codex", "high"},
		{"gpt-5.3-codex-medium", "gpt-5.3-codex", "medium"},
		{"gpt-5.3-codex-none", "gpt-5.3-codex", "none"},
		{"gpt-5.3-codex-xhigh", "gpt-5.3-codex", "xhigh"},
		{"gpt-5.3-codex", "gpt-5.3-codex", "low"},
		{"gpt-5.3-codex-spark", "gpt-5.3-codex-spark", "low"},
		{"single", "single", "low"},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			base, eff := codexEffortForModel(tc.model)
			if base != tc.wantBase {
				t.Errorf("base = %q, want %q", base, tc.wantBase)
			}
			if eff != tc.wantEff {
				t.Errorf("effort = %q, want %q", eff, tc.wantEff)
			}
		})
	}
}

func TestInjectCodexRequestKey(t *testing.T) {
	t.Run("valid body inserts key", func(t *testing.T) {
		body := []byte(`{"model":"x","input":[]}`)
		out := InjectCodexRequestKey(body, "sess-123")
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if m["prompt_cache_key"] != "sess-123" {
			t.Errorf("prompt_cache_key = %v, want sess-123", m["prompt_cache_key"])
		}
		if m["model"] != "x" {
			t.Errorf("model = %v, want x (preserved)", m["model"])
		}
	})
	t.Run("non-json body returned unchanged", func(t *testing.T) {
		body := []byte("not json at all")
		out := InjectCodexRequestKey(body, "k")
		if string(out) != string(body) {
			t.Errorf("got %q, want unchanged body", out)
		}
	})
}

func TestOutputToText(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{"empty", json.RawMessage(``), ""},
		{"null", json.RawMessage(`null`), ""},
		{"string", json.RawMessage(`"hello"`), "hello"},
		{"array of text parts", json.RawMessage(`[{"type":"text","text":"hi "},{"type":"text","text":"there"}]`), "hi there"},
		{"array with non-text parts skipped", json.RawMessage(`[{"type":"image_url"},{"type":"text","text":"keep"}]`), "keep"},
		{"invalid json", json.RawMessage(`{bad`), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := outputToText(tc.raw); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
