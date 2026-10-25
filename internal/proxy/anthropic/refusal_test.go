package anthropic

import (
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestDecodeResponseAnthropicRefusalReadable(t *testing.T) {
	body := []byte(`{"id":"msg","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"cannot"}],"stop_reason":"refusal","usage":{"input_tokens":1,"output_tokens":1}}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != ir.StopRefusal || len(resp.Content) != 1 || resp.Content[0].Text != "cannot" || resp.Content[0].Refusal {
		t.Fatalf("got %+v", resp)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"text":"cannot"`) || !strings.Contains(string(out), `"stop_reason":"refusal"`) {
		t.Fatalf("encoded = %s", out)
	}
}

func TestEncodeResponseContentFilterApproximatesRefusal(t *testing.T) {
	resp := &ir.Response{
		ID: "msg", Model: "m", StopReason: ir.StopContentFilter,
		Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "partial"}},
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"text":"partial"`) || !strings.Contains(string(out), `"stop_reason":"refusal"`) {
		t.Fatalf("encoded = %s", out)
	}
}
