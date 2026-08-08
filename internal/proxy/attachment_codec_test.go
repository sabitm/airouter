package proxy

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/antigravity"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/openai"
	"airouter/internal/proxy/responses"
)

var (
	testPNG = mustB64("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVQI12P4z8AAAAMBAQAY3Y20AAAAAElFTkSuQmCC")
	testPDF = base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 test\n"))
)

func mustB64(s string) string { return s }

func TestChatImageRoundTripAnthropic(t *testing.T) {
	body := mustJSON(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "see"},
					map[string]any{"type": "image_url", "image_url": map[string]any{
						"url": "data:image/png;base64," + testPNG,
					}},
				},
			},
		},
	})
	req, err := openai.DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Content) != 2 {
		t.Fatalf("blocks=%+v", req.Messages)
	}
	img := req.Messages[0].Content[1]
	if img.Type != ir.BlockImage || img.Image == nil || img.Image.Data != testPNG || img.Image.MediaType != "image/png" {
		t.Fatalf("image=%+v", img)
	}
	up, err := anthropic.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var anth map[string]any
	if err := json.Unmarshal(up, &anth); err != nil {
		t.Fatal(err)
	}
	msgs := anth["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	found := false
	for _, c := range content {
		m := c.(map[string]any)
		if m["type"] == "image" {
			src := m["source"].(map[string]any)
			if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != testPNG {
				t.Fatalf("source=%v", src)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no image in anthropic body: %s", up)
	}
	// Back through Anthropic decode -> OpenAI encode
	back, err := anthropic.DecodeRequest(up)
	if err != nil {
		t.Fatal(err)
	}
	oai, err := openai.EncodeRequest(back)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(oai), "image_url") || !strings.Contains(string(oai), testPNG) {
		t.Fatalf("openai roundtrip missing image: %s", oai)
	}
}

func TestChatPDFRoundTripAnthropic(t *testing.T) {
	body := mustJSON(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "file", "file": map[string]any{
						"filename":  "doc.pdf",
						"file_data": "data:application/pdf;base64," + testPDF,
					}},
				},
			},
		},
	})
	req, err := openai.DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	f := req.Messages[0].Content[0]
	if f.Type != ir.BlockFile || f.File == nil || f.File.Filename != "doc.pdf" || f.File.Data != testPDF {
		t.Fatalf("file=%+v", f)
	}
	up, err := anthropic.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(up), `"type":"document"`) || !strings.Contains(string(up), testPDF) {
		t.Fatalf("anthropic document missing: %s", up)
	}
	back, err := anthropic.DecodeRequest(up)
	if err != nil {
		t.Fatal(err)
	}
	if back.Messages[0].Content[0].File.Filename != "doc.pdf" {
		t.Fatalf("filename lost: %+v", back.Messages[0].Content[0].File)
	}
	oai, err := openai.EncodeRequest(back)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(oai), `"type":"file"`) || !strings.Contains(string(oai), "doc.pdf") {
		t.Fatalf("chat file roundtrip: %s", oai)
	}
}

func TestChatFileRoundTripResponses(t *testing.T) {
	body := mustJSON(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "file", "file": map[string]any{
						"filename":  "note.txt",
						"file_data": "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte("hello")),
					}},
				},
			},
		},
	})
	req, err := openai.DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	up, err := responses.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(up), `"type":"input_file"`) || !strings.Contains(string(up), "note.txt") {
		t.Fatalf("responses input_file missing: %s", up)
	}
	back, err := responses.DecodeRequest(up)
	if err != nil {
		t.Fatal(err)
	}
	oai, err := openai.EncodeRequest(back)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(oai), "note.txt") {
		t.Fatalf("filename not preserved: %s", oai)
	}
}

func TestNonPDFFileNotRelabeledAnthropic(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.ContentBlock{{
				Type: ir.BlockFile,
				File: &ir.File{Filename: "a.txt", MediaType: "text/plain", Data: base64.StdEncoding.EncodeToString([]byte("x"))},
			}},
		}},
	}
	up, err := anthropic.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(up), "document") {
		t.Fatalf("must not emit document for non-pdf: %s", up)
	}
}

// Filename alone must not turn text/plain into an Anthropic document.
func TestPDFFilenameWithTextPlainNotDocument(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.ContentBlock{{
				Type: ir.BlockFile,
				File: &ir.File{
					Filename:  "report.pdf",
					MediaType: "text/plain",
					Data:      base64.StdEncoding.EncodeToString([]byte("not a pdf")),
				},
			}},
		}},
	}
	up, err := anthropic.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(up), "document") {
		t.Fatalf(".pdf filename + text/plain must not encode as document: %s", up)
	}
}

// Anthropic document PDF encodes to OpenAI Chat file_data.
func TestAnthropicPDFToOpenAIChat(t *testing.T) {
	body := mustJSON(map[string]any{
		"model":      "m",
		"max_tokens": 64,
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":  "document",
						"title": "doc.pdf",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "application/pdf",
							"data":       testPDF,
						},
					},
				},
			},
		},
	})
	req, err := anthropic.DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	oai, err := openai.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(oai), `"type":"file"`) || !strings.Contains(string(oai), testPDF) {
		t.Fatalf("chat missing file pdf: %s", oai)
	}
	res, err := responses.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res), `"type":"input_file"`) || !strings.Contains(string(res), testPDF) {
		t.Fatalf("responses missing input_file pdf: %s", res)
	}
}

// Responses input_file PDF encodes through Claude Code (Anthropic document).
func TestResponsesPDFToClaudeCode(t *testing.T) {
	body := mustJSON(map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":      "input_file",
						"filename":  "doc.pdf",
						"file_data": "data:application/pdf;base64," + testPDF,
					},
				},
			},
		},
	})
	req, err := responses.DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Messages[0].Content[0].File == nil || req.Messages[0].Content[0].File.Data != testPDF {
		t.Fatalf("file=%+v", req.Messages[0].Content[0].File)
	}
	up, err := anthropic.EncodeRequestClaudeCode(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(up), `"type":"document"`) || !strings.Contains(string(up), testPDF) {
		t.Fatalf("claude-code missing document: %s", up)
	}
}

func TestAntigravityInlineFile(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.ContentBlock{
				{Type: ir.BlockText, Text: "read"},
				{Type: ir.BlockFile, File: &ir.File{MediaType: "application/pdf", Data: testPDF}},
			},
		}},
	}
	up, err := antigravity.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(up), "inlineData") || !strings.Contains(string(up), testPDF) {
		t.Fatalf("antigravity missing inlineData: %s", up)
	}
	if !strings.Contains(string(up), "application/pdf") {
		t.Fatalf("mime missing: %s", up)
	}
}

func TestAttachmentOnlyUserMessage(t *testing.T) {
	body := mustJSON(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{
						"url": "data:image/png;base64," + testPNG,
					}},
				},
			},
		},
	})
	req, err := openai.DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Content[0].Type != ir.BlockImage {
		t.Fatalf("got %+v", req.Messages)
	}
	up, err := anthropic.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(up), `"type":"image"`) {
		t.Fatalf("expected image-only anthropic message: %s", up)
	}
}

func TestOpaqueFileIDInIR(t *testing.T) {
	body := mustJSON(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "file", "file": map[string]any{"file_id": "file-abc"}},
				},
			},
		},
	})
	req, err := openai.DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Messages[0].Content[0].File.ID != "file-abc" {
		t.Fatalf("id=%+v", req.Messages[0].Content[0].File)
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
