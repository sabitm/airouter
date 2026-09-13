package openai

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func TestEncodeResponseCacheDetails(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{
		ID: "chatcmpl-1", Model: "m",
		Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}},
		Usage:   ir.Usage{InputTokens: 2600, OutputTokens: 300, CacheReadTokens: 2000, CacheWriteTokens: 400},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got chatResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Usage == nil || got.Usage.PromptTokens != 2600 || got.Usage.CompletionTokens != 300 || got.Usage.TotalTokens != 2900 {
		t.Fatalf("totals = %+v", got.Usage)
	}
	if got.Usage.PromptTokensDetails == nil || got.Usage.PromptTokensDetails.CachedTokens != 2000 || got.Usage.PromptTokensDetails.CacheWriteTokens != 400 {
		t.Fatalf("details = %+v body=%s", got.Usage.PromptTokensDetails, out)
	}
}

func TestEncodeResponseOmitsZeroDetails(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{
		ID: "chatcmpl-1", Model: "m",
		Usage: ir.Usage{InputTokens: 3, OutputTokens: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "prompt_tokens_details") {
		t.Fatalf("zero details emitted: %s", out)
	}
}

func TestDecodeResponseCacheDetails(t *testing.T) {
	body := `{"id":"chatcmpl-1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2600,"completion_tokens":300,"total_tokens":2900,"prompt_tokens_details":{"cached_tokens":2000,"cache_write_tokens":400}}}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 2600 || resp.Usage.OutputTokens != 300 {
		t.Fatalf("totals = %+v", resp.Usage)
	}
	if resp.Usage.CacheReadTokens != 2000 || resp.Usage.CacheWriteTokens != 400 {
		t.Fatalf("cache = %+v", resp.Usage)
	}
}

func TestDecodeStreamCacheDetails(t *testing.T) {
	body := "data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":2600,\"completion_tokens\":300,\"total_tokens\":2900,\"prompt_tokens_details\":{\"cached_tokens\":2000,\"cache_write_tokens\":400}}}\n\n" +
		"data: [DONE]\n\n"
	var finish ir.StreamEvent
	err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventFinish {
			finish = ev
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if finish.InputTokens != 2600 || finish.OutputTokens != 300 || finish.CacheReadTokens != 2000 || finish.CacheWriteTokens != 400 {
		t.Fatalf("finish = %+v", finish)
	}
}

func TestEncodeStreamUsageTrailerDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("no flusher")
	}
	enc := NewStreamEncoder("m")
	events := []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "chatcmpl-1", Model: "m"},
		{Kind: ir.EventTextDelta, Text: "hi"},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn, InputTokens: 2600, OutputTokens: 300, CacheReadTokens: 2000, CacheWriteTokens: 400},
	}
	for _, ev := range events {
		if err := enc.Encode(ev, w); err != nil {
			t.Fatal(err)
		}
	}
	reader := sse.NewReader(strings.NewReader(rec.Body.String()))
	found := false
	for {
		ev, err := reader.Next()
		if err != nil {
			break
		}
		var chunk chatChunk
		if json.Unmarshal(ev.Data, &chunk) != nil || chunk.Usage == nil || len(chunk.Choices) != 0 {
			continue
		}
		if chunk.Usage.PromptTokens != 2600 || chunk.Usage.CompletionTokens != 300 || chunk.Usage.TotalTokens != 2900 {
			t.Fatalf("totals = %+v", chunk.Usage)
		}
		if chunk.Usage.PromptTokensDetails == nil || chunk.Usage.PromptTokensDetails.CachedTokens != 2000 || chunk.Usage.PromptTokensDetails.CacheWriteTokens != 400 {
			t.Fatalf("details = %+v", chunk.Usage.PromptTokensDetails)
		}
		found = true
	}
	if !found {
		t.Fatal("missing empty-choices usage trailer")
	}
}

func TestEncodeStreamOmitsZeroDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("no flusher")
	}
	enc := NewStreamEncoder("m")
	_ = enc.Encode(ir.StreamEvent{Kind: ir.EventMessageStart, ID: "x", Model: "m"}, w)
	_ = enc.Encode(ir.StreamEvent{Kind: ir.EventFinish, InputTokens: 3, OutputTokens: 2}, w)
	if strings.Contains(rec.Body.String(), "prompt_tokens_details") {
		t.Fatalf("zero details emitted: %s", rec.Body.String())
	}
}

func TestEncodeStreamRetainsStartCacheOnZeroFinish(t *testing.T) {
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("no flusher")
	}
	enc := NewStreamEncoder("m")
	_ = enc.Encode(ir.StreamEvent{Kind: ir.EventMessageStart, ID: "x", Model: "m", InputTokens: 13, CacheReadTokens: 0, CacheWriteTokens: 10}, w)
	_ = enc.Encode(ir.StreamEvent{Kind: ir.EventFinish, InputTokens: 13, OutputTokens: 2}, w)
	reader := sse.NewReader(strings.NewReader(rec.Body.String()))
	for {
		ev, err := reader.Next()
		if err != nil {
			break
		}
		var chunk chatChunk
		if json.Unmarshal(ev.Data, &chunk) != nil || chunk.Usage == nil {
			continue
		}
		if chunk.Usage.PromptTokensDetails == nil || chunk.Usage.PromptTokensDetails.CacheWriteTokens != 10 {
			t.Fatalf("start cache dropped: %+v", chunk.Usage.PromptTokensDetails)
		}
		return
	}
	t.Fatal("missing usage")
}
