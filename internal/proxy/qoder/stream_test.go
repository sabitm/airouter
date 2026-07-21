package qoder

import (
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestDecodeStreamUnwrapsEnvelope(t *testing.T) {
	inner := `{"id":"c1","model":"auto","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`
	env, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": inner})
	sse := "data: " + string(env) + "\n\ndata: {\"statusCodeValue\":200,\"body\":\"[DONE]\"}\n\n"

	var texts []string
	var finished bool
	err := DecodeStream(strings.NewReader(sse), func(ev ir.StreamEvent) error {
		switch ev.Kind {
		case ir.EventTextDelta:
			texts = append(texts, ev.Text)
		case ir.EventFinish:
			finished = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(texts, "") != "hi" {
		t.Fatalf("text=%q", texts)
	}
	if !finished {
		t.Fatal("expected finish")
	}
}

