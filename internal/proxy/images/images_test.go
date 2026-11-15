package images

import (
	"bytes"
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

func editDataURL(mime string, raw []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

func signedImage(sig []byte, n int) []byte {
	raw := bytes.Repeat([]byte{0x11}, n)
	copy(raw, sig)
	return raw
}

var (
	pngSig  = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	jpegSig = []byte{0xff, 0xd8, 0xff}
	webpSig = []byte("RIFF\x00\x00\x00\x00WEBP")
	gifSig  = []byte("GIF89a")
)

func editBody(t *testing.T, refs []string, extra map[string]any) []byte {
	t.Helper()
	fields := map[string]any{"model": "combo", "prompt": "edit the lake", "images": refsToObjects(refs)}
	for key, value := range extra {
		fields[key] = value
	}
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func refsToObjects(refs []string) []any {
	out := make([]any, len(refs))
	for i, ref := range refs {
		out[i] = map[string]string{"image_url": ref}
	}
	return out
}

func TestDecodeEditRequestCanonicalOrderAndEnvelope(t *testing.T) {
	png := editDataURL("image/png", signedImage(pngSig, 8))
	jpeg := editDataURL("image/jpg", signedImage(jpegSig, 4))
	webp := editDataURL("IMAGE/WEBP", signedImage(webpSig, 12))
	req, err := DecodeEditRequest(editBody(t, []string{png, jpeg, webp}, map[string]any{
		"user": "local-user", "stream": false, "output_format": "PNG", "quality": "high", "background": "opaque",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if req.User == nil || *req.User != "local-user" || req.N != 1 || req.OutputFormat != "png" || len(req.Images) != 3 {
		t.Fatalf("request = %+v", req)
	}
	if req.Images[0].MIME != "image/png" || req.Images[1].MIME != "image/jpeg" || req.Images[2].MIME != "image/webp" {
		t.Fatalf("mime order = %+v", req.Images)
	}
	req.Model = "gpt-image-2.6-flare"
	body, err := EncodeCodexEditRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	wantKeys := map[string]bool{"model": true, "prompt": true, "images": true, "n": true, "size": true, "quality": true, "background": true}
	for key := range got {
		if !wantKeys[key] {
			t.Fatalf("unexpected upstream key %s in %s", key, body)
		}
		delete(wantKeys, key)
	}
	if len(wantKeys) != 0 || got["n"] != float64(1) || got["size"] != "auto" || got["quality"] != "high" || got["background"] != "opaque" {
		t.Fatalf("envelope = %s", body)
	}
	images, _ := got["images"].([]any)
	if len(images) != 3 {
		t.Fatal(body)
	}
	for i, want := range []string{"image/png", "image/jpeg", "image/webp"} {
		item, _ := images[i].(map[string]any)
		if len(item) != 1 || item["image_url"] != "data:"+want+";base64,"+req.Images[i].Base64 {
			t.Fatalf("image %d = %+v", i, item)
		}
	}
	if _, err := EncodeCodexEditRequest(nil); err == nil {
		t.Fatal("nil edit accepted")
	}
}

func TestDecodeEditRequestRejectsMalformedSources(t *testing.T) {
	valid := editDataURL("image/png", signedImage(pngSig, 8))
	cases := []string{
		`null`, `[]`, `{`, `{"model":"combo","prompt":"edit"}`,
		`{"model":"combo","prompt":"edit","images":null}`,
		`{"model":"combo","prompt":"edit","images":[]}`,
		`{"model":"combo","prompt":"edit","images":"` + valid + `"}`,
		`{"model":"combo","prompt":"edit","image":"` + valid + `"}`,
		`{"model":"combo","prompt":"edit","images":[{}]}`,
		`{"model":"combo","prompt":"edit","images":[null]}`,
		`{"model":"combo","prompt":"edit","images":[{"image_url":null}]}`,
		`{"model":"combo","prompt":"edit","images":[{"image_url":1}]}`,
		`{"model":"combo","prompt":"edit","images":[{"file_id":"file-secret"}]}`,
		`{"model":"combo","prompt":"edit","images":[{"image_url":"` + valid + `","file_id":"file-secret"}]}`,
		`{"model":"combo","prompt":"edit","images":[{"image_url":"https://secret.example/private-ref.png"}]}`,
		`{"model":"combo","prompt":"edit","images":[{"image_url":"file-secret"}]}`,
		`{"model":"combo","prompt":"edit","images":[{"image_url":"` + valid + `"}],"mask":"mask-secret"}`,
		`{"model":"combo","prompt":"edit","images":[{"image_url":"` + valid + `"}],"stream":true}`,
		`{"model":"combo","prompt":"edit","images":[{"image_url":"` + editDataURL("image/gif", gifSig) + `"}]}`,
		`{"model":"combo","prompt":"edit","images":[{"image_url":"` + editDataURL("image/png", jpegSig) + `"}]}`,
		`{"model":"combo","prompt":"edit","images":[{"image_url":"data:image/png;base64,` + strings.TrimPrefix(valid, "data:image/png;base64,")[:8] + `-abc="}]}`,
		`{"model":"combo","prompt":"edit","images":[{"image_url":"data:image/png;base64,` + strings.TrimRight(strings.TrimPrefix(valid, "data:image/png;base64,"), "=") + `"}]}`,
		`{"model":"combo","prompt":"edit","images":[{"image_url":"data:image/png;base64,` + strings.TrimPrefix(valid, "data:image/png;base64,")[:4] + "\n" + strings.TrimPrefix(valid, "data:image/png;base64,")[4:] + `"}]}`,
	}
	for i := 0; i <= 6; i++ {
		refs := make([]string, i)
		for j := range refs {
			refs[j] = valid
		}
		body, _ := json.Marshal(map[string]any{"model": "combo", "prompt": "edit", "images": refsToObjects(refs)})
		_, err := DecodeEditRequest(body)
		if (err == nil) != (i >= 1 && i <= 5) {
			t.Fatalf("count %d err=%v", i, err)
		}
	}
	for i, body := range cases {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			_, err := DecodeEditRequest([]byte(body))
			if _, ok := ir.AsRequestValidation(err); !ok || IsBudgetExceeded(err) {
				t.Fatalf("expected validation error, got %v", err)
			}
			for _, secret := range []string{"file-secret", "secret.example", "private-ref", "mask-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("payload leaked: %v", err)
				}
			}
		})
	}
}

func TestDecodeEditRequestBounds(t *testing.T) {
	exact := signedImage(pngSig, maxEditImageBytes)
	req, err := DecodeEditRequest(editBody(t, []string{editDataURL("image/png", exact)}, nil))
	if err != nil || len(req.Images) != 1 {
		t.Fatalf("exact per-file limit: %+v %v", req, err)
	}
	exact = nil
	_, err = DecodeEditRequest(editBody(t, []string{editDataURL("image/png", signedImage(pngSig, maxEditImageBytes+1))}, nil))
	if !IsBudgetExceeded(err) {
		t.Fatalf("per-file overflow = %v", err)
	}
	parts := make([]string, 4)
	for i := range parts {
		parts[i] = editDataURL("image/png", signedImage(pngSig, maxEditTotalBytes/4))
	}
	if _, err := DecodeEditRequest(editBody(t, parts, nil)); err != nil {
		t.Fatalf("exact total limit: %v", err)
	}
	parts[3] = editDataURL("image/png", signedImage(pngSig, maxEditTotalBytes/4+1))
	if _, err := DecodeEditRequest(editBody(t, parts, nil)); !IsBudgetExceeded(err) {
		t.Fatalf("total overflow = %v", err)
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
