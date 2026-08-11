package openai

import (
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestDecodeStreamErrorFrame(t *testing.T) {
	body := "data: {\"error\":{\"message\":\"servers overloaded\",\"type\":\"server_error\",\"code\":\"server_is_overloaded\"}}\n\n"
	var out []ir.StreamEvent
	err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	sf, ok := ir.AsStreamFailure(err)
	if !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	if sf.Code != "server_is_overloaded" {
		t.Errorf("code = %q", sf.Code)
	}
	if !strings.Contains(sf.Message, "overloaded") {
		t.Errorf("message = %q", sf.Message)
	}
	for _, ev := range out {
		if ev.Kind == ir.EventFinish || ev.Kind == ir.EventMessageStart {
			t.Fatalf("unexpected event %+v", ev)
		}
	}
}
