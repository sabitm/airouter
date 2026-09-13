package responses

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func TestDecodeResponseCacheDetails(t *testing.T) {
	body := `{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":2600,"output_tokens":300,"total_tokens":2900,"input_tokens_details":{"cached_tokens":2000,"cache_write_tokens":400}}}`
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

func TestDecodeResponseMissingDetailsZeros(t *testing.T) {
	body := `{"id":"resp_1","model":"m","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":3}}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage != (ir.Usage{InputTokens: 7, OutputTokens: 3}) {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestDecodeStreamCompletedCacheDetails(t *testing.T) {
	body := `event: response.created
data: {"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"hi"}

event: response.completed
data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","usage":{"input_tokens":2600,"output_tokens":300,"total_tokens":2900,"input_tokens_details":{"cached_tokens":2000,"cache_write_tokens":400}}}}

`
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var finish *ir.StreamEvent
	for i := range evs {
		if evs[i].Kind == ir.EventFinish {
			finish = &evs[i]
		}
	}
	if finish == nil || finish.InputTokens != 2600 || finish.OutputTokens != 300 {
		t.Fatalf("finish = %+v", finish)
	}
	if finish.CacheReadTokens != 2000 || finish.CacheWriteTokens != 400 {
		t.Fatalf("finish cache = %+v", finish)
	}
}

func TestDecodeStreamIncompleteCacheDetails(t *testing.T) {
	body := `event: response.created
data: {"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"}}

event: response.incomplete
data: {"type":"response.incomplete","response":{"id":"r1","status":"incomplete","usage":{"input_tokens":10,"output_tokens":9,"input_tokens_details":{"cached_tokens":6,"cache_write_tokens":1}}}}

`
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var finish *ir.StreamEvent
	for i := range evs {
		if evs[i].Kind == ir.EventFinish {
			finish = &evs[i]
		}
	}
	if finish == nil || finish.CacheReadTokens != 6 || finish.CacheWriteTokens != 1 {
		t.Fatalf("finish = %+v", finish)
	}
}

func TestEncodeResponseCacheDetails(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{
		ID: "resp_1", Model: "m",
		Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}},
		Usage:   ir.Usage{InputTokens: 2600, OutputTokens: 300, CacheReadTokens: 2000, CacheWriteTokens: 400},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
			Details      *struct {
				Cached     int `json:"cached_tokens"`
				CacheWrite int `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Usage.InputTokens != 2600 || got.Usage.OutputTokens != 300 || got.Usage.TotalTokens != 2900 {
		t.Fatalf("totals = %+v", got.Usage)
	}
	if got.Usage.Details == nil || got.Usage.Details.Cached != 2000 || got.Usage.Details.CacheWrite != 400 {
		t.Fatalf("details = %+v body=%s", got.Usage.Details, out)
	}
}

func TestEncodeResponseOmitsZeroDetails(t *testing.T) {
	out, err := EncodeResponse(&ir.Response{
		ID: "resp_1", Model: "m",
		Usage: ir.Usage{InputTokens: 7, OutputTokens: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "input_tokens_details") {
		t.Fatalf("zero details emitted: %s", out)
	}
}

func TestEncodeStreamCompletedCacheDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("no flusher")
	}
	enc := NewStreamEncoder("m")
	events := []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "resp_1", Model: "m"},
		{Kind: ir.EventTextDelta, Text: "hi"},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn, InputTokens: 2600, OutputTokens: 300, CacheReadTokens: 2000, CacheWriteTokens: 400},
	}
	for _, ev := range events {
		if err := enc.Encode(ev, w); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(w); err != nil {
		t.Fatal(err)
	}
	reader := sse.NewReader(strings.NewReader(rec.Body.String()))
	found := false
	for {
		ev, err := reader.Next()
		if err != nil {
			break
		}
		if ev.Name != "response.completed" {
			continue
		}
		var d struct {
			Response struct {
				Usage *struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
					TotalTokens  int `json:"total_tokens"`
					Details      *struct {
						Cached     int `json:"cached_tokens"`
						CacheWrite int `json:"cache_write_tokens"`
					} `json:"input_tokens_details"`
				} `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(ev.Data, &d) != nil || d.Response.Usage == nil {
			t.Fatalf("completed: %s", ev.Data)
		}
		if d.Response.Usage.InputTokens != 2600 || d.Response.Usage.OutputTokens != 300 || d.Response.Usage.TotalTokens != 2900 {
			t.Fatalf("totals = %+v", d.Response.Usage)
		}
		if d.Response.Usage.Details == nil || d.Response.Usage.Details.Cached != 2000 || d.Response.Usage.Details.CacheWrite != 400 {
			t.Fatalf("details = %+v", d.Response.Usage.Details)
		}
		found = true
	}
	if !found {
		t.Fatal("missing response.completed")
	}
}

func TestEncodeStreamOmitsZeroDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("no flusher")
	}
	enc := NewStreamEncoder("m")
	_ = enc.Encode(ir.StreamEvent{Kind: ir.EventFinish, InputTokens: 3, OutputTokens: 2}, w)
	if strings.Contains(rec.Body.String(), "input_tokens_details") {
		t.Fatalf("zero details emitted: %s", rec.Body.String())
	}
}

func TestDecodeResponseClampsOverlargeCache(t *testing.T) {
	body := `{"id":"resp_1","model":"m","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":1,"input_tokens_details":{"cached_tokens":9,"cache_write_tokens":9}}}`
	resp, err := DecodeResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.CacheReadTokens != 9 || resp.Usage.CacheWriteTokens != 1 {
		t.Fatalf("clamped = %+v", resp.Usage)
	}
}
