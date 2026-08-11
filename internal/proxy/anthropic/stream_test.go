package anthropic

import (
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestDecodeStreamErrorEvent(t *testing.T) {
	body := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	var out []ir.StreamEvent
	err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	sf, ok := ir.AsStreamFailure(err)
	if !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	if sf.Type != "overloaded_error" {
		t.Errorf("type = %q", sf.Type)
	}
	if sf.Message != "Overloaded" {
		t.Errorf("message = %q", sf.Message)
	}
	for _, ev := range out {
		if ev.Kind == ir.EventFinish {
			t.Fatal("must not emit Finish on error")
		}
	}
}
