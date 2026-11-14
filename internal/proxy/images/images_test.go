package images

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

const testPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a7X8AAAAASUVORK5CYII="

func TestDecodeRequestDefaultsAndCodexEnvelope(t *testing.T) {
	req, err := DecodeRequest([]byte(`{"model":"image-combo","prompt":"  draw a lake\n","user":"client-user","stream":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Prompt != "  draw a lake\n" || req.N != 1 || req.Size != "auto" || req.User == nil {
		t.Fatalf("request = %+v", req)
	}
	for _, model := range []string{"gpt-image-2", "gpt-image-2.6-flare", "gpt-image-2.6-sunburst", "gpt-image-high", "custom(high)"} {
		req.Model = model
		body, err := EncodeCodexRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != 7 || got["model"] != model || got["prompt"] != req.Prompt ||
			got["n"] != float64(1) || got["size"] != "auto" || got["quality"] != "auto" ||
			got["background"] != "auto" || got["output_format"] != "png" {
			t.Fatalf("unexpected native envelope: %s", body)
		}
	}
}

func TestDecodeRequestControls(t *testing.T) {
	for _, quality := range []string{"auto", "low", "medium", "high"} {
		body := `{"model":"combo","prompt":"draw","n":1,"size":"auto","quality":"` + quality + `","background":"opaque","output_format":"PNG","response_format":"b64_json"}`
		req, err := DecodeRequest([]byte(body))
		if err != nil || req.Quality != quality || req.OutputFormat != "png" {
			t.Fatalf("request=%+v err=%v", req, err)
		}
	}
}

func TestDecodeRequestRejectsUnsupportedOrMalformed(t *testing.T) {
	cases := []string{
		`null`, `[]`, `{`, `{"prompt":"draw"}`, `{"model":"combo"}`,
		`{"model":" ","prompt":"draw"}`, `{"model":"combo","prompt":" \n"}`,
	}
	for _, field := range []string{
		`"n":0`, `"n":2`, `"n":1.0`, `"n":"1"`, `"n":null`,
		`"size":"1024x1024"`, `"size":""`, `"size":null`,
		`"quality":"max"`, `"quality":false`, `"background":"transparent"`,
		`"output_format":"webp"`, `"response_format":"url"`,
		`"stream":true`, `"stream":"false"`, `"stream":null`,
		`"user":null`, `"user":{}`, `"image":"ref"`, `"images":[]`,
		`"mask":"ref"`, `"partial_images":1`, `"output_compression":80`,
		`"style":"vivid"`, `"moderation":"low"`, `"complexity":"medium"`,
		`"instructions":"sensitive-value"`,
	} {
		cases = append(cases, `{"model":"combo","prompt":"draw",`+field+`}`)
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			_, err := DecodeRequest([]byte(body))
			if _, ok := ir.AsRequestValidation(err); !ok {
				t.Fatalf("expected validation error, got %v", err)
			}
			if strings.Contains(err.Error(), "sensitive-value") {
				t.Fatalf("payload leaked: %v", err)
			}
		})
	}
	for _, field := range []string{"model", "prompt", "size", "quality", "background", "output_format", "response_format"} {
		var fields map[string]any
		for _, value := range []any{nil, 1, false, []any{}, map[string]any{}} {
			fields = map[string]any{"model": "combo", "prompt": "draw", field: value}
			body, _ := json.Marshal(fields)
			if _, err := DecodeRequest(body); err == nil {
				t.Fatalf("accepted %s", body)
			}
		}
	}
}

func TestEncodeCodexRequestRejectsInvalidIR(t *testing.T) {
	if _, err := EncodeCodexRequest(nil); err == nil {
		t.Fatal("nil request accepted")
	}
	if _, err := EncodeCodexRequest(&ir.ImageGenerationRequest{Model: "m", Prompt: "p"}); err == nil {
		t.Fatal("unsupported zero-value controls accepted")
	}
}

func TestCodexResponseRoundTrip(t *testing.T) {
	body := `{"created":123,"data":[{"b64_json":"` + testPNG + `","generation_id":"gen-1"}],"background":"opaque","quality":"high","size":"1024x1536","output_format":"png","usage":{"input_tokens":1474,"output_tokens":1372,"total_tokens":2846,"input_tokens_details":{"image_tokens":1457,"text_tokens":17},"output_tokens_details":{"image_tokens":1372,"text_tokens":0}}}`
	resp, err := DecodeCodexResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage == nil || resp.Usage.InputTokens != 1474 || resp.Usage.OutputTokens != 1372 {
		t.Fatalf("usage=%+v", resp.Usage)
	}
	encoded, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	_ = json.Unmarshal([]byte(body), &want)
	_ = json.Unmarshal(encoded, &got)
	w, _ := json.Marshal(want)
	g, _ := json.Marshal(got)
	if string(w) != string(g) {
		t.Fatalf("response changed: %s", encoded)
	}
}

func TestCodexResponseUsagePresence(t *testing.T) {
	for _, tc := range []struct {
		usage string
		valid bool
	}{
		{"", false}, {`null`, false}, {`{}`, false},
		{`{"input_tokens":0,"output_tokens":0}`, true},
		{`{"input_tokens":1,"output_tokens":2}`, true},
		{`{"input_tokens":-1,"output_tokens":2}`, false},
		{`{"input_tokens":null,"output_tokens":2}`, false},
		{`{"input_tokens":1.5,"output_tokens":2}`, false},
		{`{"input_tokens":"1","output_tokens":2}`, false},
		{`{"input_tokens":1}`, false},
		{`{"input_tokens":1,"output_tokens":2,"total_tokens":null}`, false},
		{`{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"image_tokens":-1}}`, false},
		{`{"input_tokens":1,"output_tokens":2,"output_tokens_details":[]}`, false},
	} {
		t.Run(tc.usage, func(t *testing.T) {
			body := `{"data":[{"b64_json":"` + testPNG + `"}]`
			if tc.usage != "" {
				body += `,"usage":` + tc.usage
			}
			resp, err := DecodeCodexResponse([]byte(body + `}`))
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := EncodeResponse(resp)
			if err != nil {
				t.Fatal(err)
			}
			if (resp.Usage != nil) != tc.valid || strings.Contains(string(encoded), `"usage"`) != tc.valid {
				t.Fatalf("usage presence: %s", encoded)
			}
			if !strings.Contains(string(encoded), `"created":`) {
				t.Fatalf("missing created timestamp: %s", encoded)
			}
		})
	}
}

func TestCodexResponseRejectsInvalidImages(t *testing.T) {
	for _, body := range []string{
		`null`, `[]`, `{`, `{}`, `{"data":[]}`, `{"data":null}`,
		`{"data":[{"url":"https://provider/image.png"}]}`,
		`{"data":[{"b64_json":""}]}`, `{"data":[{"b64_json":42}]}`,
		`{"data":[{"b64_json":"` + testPNG + `"},{"b64_json":"` + testPNG + `"}]}`,
		`{"data":[{"b64_json":"` + testPNG + `"}],"output_format":"jpeg"}`,
		`{"data":[{"b64_json":"` + testPNG + `"}],"created":-1}`,
	} {
		if _, err := DecodeCodexResponse([]byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	for _, data := range []string{
		"invalid-base64", base64.StdEncoding.EncodeToString([]byte("not a PNG")),
		base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8, 0xff, 0, 0}),
		base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}),
		testPNG + "!", testPNG[:len(testPNG)-3],
	} {
		body, _ := json.Marshal(map[string]any{"data": []any{map[string]any{"b64_json": data}}})
		if _, err := DecodeCodexResponse(body); err == nil {
			t.Fatalf("accepted invalid image %q", data)
		}
	}
}

func TestCodexResponseByteLimits(t *testing.T) {
	if _, err := DecodeCodexResponse([]byte(strings.Repeat(" ", maxResponseBytes+1))); err == nil {
		t.Fatal("oversized upstream response accepted")
	}
	if _, err := EncodeResponse(&ir.ImageGenerationResponse{Images: []ir.GeneratedImage{{Data: strings.Repeat("x", maxResponseBytes)}}}); err == nil {
		t.Fatal("oversized encoded response accepted")
	}
}
