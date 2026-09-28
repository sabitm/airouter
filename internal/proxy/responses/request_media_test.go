package responses

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestDecodeRequestFunctionCallOutputMedia(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1Pe"
	body := `{
  "model": "default",
  "input": [
    {"type": "message", "role": "user",
     "content": [{"type": "input_text", "text": "look at the shot"}]},
    {"type": "function_call", "call_id": "call_1",
     "name": "screenshot", "arguments": "{}"},
    {"type": "function_call_output", "call_id": "call_1", "output": [
      {"type": "input_text", "text": "here is the screenshot"},
      {"type": "input_image", "image_url": "data:image/png;base64,` + png + `"}
    ]}
  ]
}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(req.Messages))
	}
	user := req.Messages[2]
	if user.Role != ir.RoleUser || len(user.Content) != 1 || user.Content[0].Type != ir.BlockToolResult {
		t.Fatalf("tool result message = %+v", user)
	}
	blocks := user.Content[0].ToolResult
	if len(blocks) != 2 {
		t.Fatalf("tool result blocks = %+v, want text + image", blocks)
	}
	if blocks[0].Type != ir.BlockText || blocks[0].Text != "here is the screenshot" {
		t.Fatalf("text block = %+v", blocks[0])
	}
	if blocks[1].Type != ir.BlockImage || blocks[1].Image == nil {
		t.Fatalf("image block = %+v", blocks[1])
	}
	if blocks[1].Image.MediaType != "image/png" || blocks[1].Image.Data != png {
		t.Fatalf("image = %+v", blocks[1].Image)
	}
	if user.Content[0].ToolUseID != "call_1" {
		t.Fatalf("tool use id = %q", user.Content[0].ToolUseID)
	}
}

func TestDecodeRequestInputImageFileID(t *testing.T) {
	body := `{
  "model": "default",
  "input": [{"type": "message", "role": "user", "content": [
    {"type": "input_image", "file_id": "file-img-1"}
  ]}]
}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	img := req.Messages[0].Content[0].Image
	if img == nil || img.ID != "file-img-1" || img.URL != "" || img.Data != "" {
		t.Fatalf("image = %+v", img)
	}
}

func TestEncodeRequestToolResultMedia(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1Pe"
	req := &ir.Request{
		Model: "default",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
				{Type: ir.BlockToolUse, ToolID: "call_1", ToolName: "screenshot", ToolInput: json.RawMessage(`{}`)},
			}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockToolResult, ToolUseID: "call_1", ToolResult: []ir.ContentBlock{
					{Type: ir.BlockText, Text: "caption"},
					{Type: ir.BlockImage, Image: &ir.Image{ID: "file-img-1"}},
					{Type: ir.BlockFile, File: &ir.File{Filename: "note.txt", MediaType: "text/plain", Data: "aGVsbG8="}},
					{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: png}},
				}},
			}},
		},
	}
	raw, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Input []struct {
			Type   string          `json:"type"`
			Output json.RawMessage `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	var output json.RawMessage
	for _, item := range got.Input {
		if item.Type == "function_call_output" {
			output = item.Output
		}
	}
	var parts []map[string]any
	if err := json.Unmarshal(output, &parts); err != nil {
		t.Fatalf("output = %s: %v", output, err)
	}
	if len(parts) != 4 {
		t.Fatalf("parts = %#v", parts)
	}
	if parts[0]["type"] != "input_text" || parts[0]["text"] != "caption" {
		t.Fatalf("text part = %#v", parts[0])
	}
	if parts[1]["type"] != "input_image" || parts[1]["file_id"] != "file-img-1" {
		t.Fatalf("image id part = %#v", parts[1])
	}
	if _, ok := parts[1]["image_url"]; ok {
		t.Fatalf("id-only image gained image_url: %#v", parts[1])
	}
	if parts[2]["type"] != "input_file" || parts[2]["filename"] != "note.txt" {
		t.Fatalf("file part = %#v", parts[2])
	}
	if parts[3]["type"] != "input_image" || !strings.Contains(fmt.Sprint(parts[3]["image_url"]), png) {
		t.Fatalf("inline image part = %#v", parts[3])
	}
}

func TestEncodeRequestToolResultTextOnly(t *testing.T) {
	req := &ir.Request{
		Model: "default",
		Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.ContentBlock{{
				Type: ir.BlockToolResult, ToolUseID: "call_1",
				ToolResult: []ir.ContentBlock{
					{Type: ir.BlockText, Text: "sun"},
					{Type: ir.BlockText, Text: "ny"},
				},
			}},
		}},
	}
	raw, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Input []struct {
			Type   string          `json:"type"`
			Output json.RawMessage `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, item := range got.Input {
		if item.Type != "function_call_output" {
			continue
		}
		var s string
		if err := json.Unmarshal(item.Output, &s); err != nil {
			t.Fatalf("text-only output = %s", item.Output)
		}
		if s != "sunny" {
			t.Fatalf("output = %q", s)
		}
		return
	}
	t.Fatal("function_call_output missing")
}
