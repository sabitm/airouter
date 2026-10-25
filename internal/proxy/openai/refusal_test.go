package openai

import (
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func TestDecodeResponseChatRefusal(t *testing.T) {
	body := []byte(`{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"kept","refusal":"no"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != ir.StopRefusal || len(resp.Content) != 2 {
		t.Fatalf("stop=%q content=%+v", resp.StopReason, resp.Content)
	}
	if resp.Content[0].Refusal || resp.Content[0].Text != "kept" || !resp.Content[1].Refusal || resp.Content[1].Text != "no" {
		t.Fatalf("content = %+v", resp.Content)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"content":"kept"`) || !strings.Contains(string(out), `"refusal":"no"`) || !strings.Contains(string(out), `"finish_reason":"stop"`) {
		t.Fatalf("encoded = %s", out)
	}
}

func TestDecodeResponseChatContentFilter(t *testing.T) {
	body := []byte(`{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"partial"},"finish_reason":"content_filter"}]}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != ir.StopContentFilter || len(resp.Content) != 1 || resp.Content[0].Refusal {
		t.Fatalf("got %+v", resp)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"finish_reason":"content_filter"`) || strings.Contains(string(out), `"refusal"`) {
		t.Fatalf("encoded = %s", out)
	}
}

func TestDecodeRequestChatRefusalHistory(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"kept","refusal":""},{"role":"user","content":"second"}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 || !req.Messages[1].Content[1].Refusal || req.Messages[1].Content[1].Text != "" {
		t.Fatalf("messages = %+v", req.Messages)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"refusal":""`) || !strings.Contains(string(out), `"content":"kept"`) {
		t.Fatalf("encoded = %s", out)
	}
}

func TestDecodeRequestMalformedChatRefusal(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"assistant","refusal":1}]}`)
	if _, err := DecodeRequest(body); err == nil {
		t.Fatal("malformed refusal accepted")
	}
}

func TestDecodeStreamChatRefusalDelta(t *testing.T) {
	body := "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"refusal\":\"no\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	var evs []ir.StreamEvent
	err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		evs = append(evs, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var delta *ir.StreamEvent
	var finish *ir.StreamEvent
	for i := range evs {
		if evs[i].Kind == ir.EventTextDelta {
			delta = &evs[i]
		}
		if evs[i].Kind == ir.EventFinish {
			finish = &evs[i]
		}
	}
	if delta == nil || !delta.Refusal || delta.Text != "no" || finish == nil || finish.StopReason != ir.StopRefusal {
		t.Fatalf("events = %+v", evs)
	}
}

func TestDecodeStreamChatRefusalStopPrecedence(t *testing.T) {
	cases := []struct {
		name   string
		finish string
		want   ir.StopReason
	}{
		{name: "empty refusal upgrades stop", finish: "stop", want: ir.StopRefusal},
		{name: "content filter stays stronger", finish: "content_filter", want: ir.StopContentFilter},
		{name: "length stays stronger", finish: "length", want: ir.StopMaxTokens},
		{name: "tool stays stronger", finish: "tool_calls", want: ir.StopToolUse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"refusal\":\"\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"" + tc.finish + "\"}]}\n\n" +
				"data: [DONE]\n\n"
			var finish *ir.StreamEvent
			err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
				if ev.Kind == ir.EventFinish {
					copied := ev
					finish = &copied
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if finish == nil || finish.StopReason != tc.want {
				t.Fatalf("finish = %+v, want %s", finish, tc.want)
			}
		})
	}
}

func TestEncodeStreamChatRefusalStart(t *testing.T) {
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("recorder does not support flushing")
	}
	enc := NewStreamEncoder("m")
	for _, ev := range []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "c"},
		{Kind: ir.EventTextStart, Text: "no", Refusal: true, TextIndexed: true},
		{Kind: ir.EventTextEnd, Index: 0, Refusal: true, TextIndexed: true},
		{Kind: ir.EventFinish, StopReason: ir.StopRefusal},
	} {
		if err := enc.Encode(ev, w); err != nil {
			t.Fatal(err)
		}
	}
	body := rec.Body.String()
	if strings.Contains(body, `"content":"no"`) || !strings.Contains(body, `"refusal":"no"`) || !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("body = %s", body)
	}
}

func TestDecodeChatRefusalAllocGrowth(t *testing.T) {
	measureResponse := func(parts int) int64 {
		resp := &ir.Response{ID: "c", Model: "m", StopReason: ir.StopRefusal}
		for i := 0; i < parts; i++ {
			resp.Content = append(resp.Content, ir.ContentBlock{Type: ir.BlockText, Text: "12345678", Refusal: true})
		}
		result := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := EncodeResponse(resp); err != nil {
					b.Fatal(err)
				}
			}
		})
		return result.AllocedBytesPerOp()
	}
	measureHistory := func(parts int) int64 {
		req := &ir.Request{Model: "m", Messages: []ir.Message{{Role: ir.RoleAssistant}}}
		for i := 0; i < parts; i++ {
			req.Messages[0].Content = append(req.Messages[0].Content, ir.ContentBlock{Type: ir.BlockText, Text: "12345678", Refusal: true})
		}
		result := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := EncodeRequest(req); err != nil {
					b.Fatal(err)
				}
			}
		})
		return result.AllocedBytesPerOp()
	}
	respSmall, respLarge := measureResponse(4000), measureResponse(8000)
	histSmall, histLarge := measureHistory(4000), measureHistory(8000)
	if respSmall <= 0 || respLarge > respSmall*3 || histSmall <= 0 || histLarge > histSmall*3 {
		t.Fatalf("response %d/%d history %d/%d", respSmall, respLarge, histSmall, histLarge)
	}
}

func TestDecodeStreamMalformedChatRefusal(t *testing.T) {
	t.Run("before commitment", func(t *testing.T) {
		body := "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"refusal\":17,\"content\":\"keep\"},\"finish_reason\":\"content_filter\"}]}\n\n" +
			"data: [DONE]\n\n"
		var n int
		err := DecodeStream(strings.NewReader(body), func(ir.StreamEvent) error {
			n++
			return nil
		})
		if err == nil || n != 0 {
			t.Fatalf("err=%v events=%d", err, n)
		}
	})
	t.Run("after valid content", func(t *testing.T) {
		body := "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"refusal\":\"no\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"refusal\":17,\"content\":\"keep\"},\"finish_reason\":\"content_filter\"}]}\n\n" +
			"data: [DONE]\n\n"
		var saw bool
		err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
			if ev.Kind == ir.EventTextDelta && ev.Refusal && ev.Text == "no" {
				saw = true
			}
			if ev.Kind == ir.EventFinish {
				t.Fatal("malformed chunk still finished")
			}
			return nil
		})
		if err == nil || !saw {
			t.Fatalf("err=%v saw=%v", err, saw)
		}
	})
	t.Run("unrelated malformed chunk stays skipped", func(t *testing.T) {
		body := "data: {\"choices\":[{\"delta\":{\"content\":1}}]}\n\n" +
			"data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"kept\"},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n"
		var got string
		err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
			if ev.Kind == ir.EventTextDelta {
				got += ev.Text
			}
			return nil
		})
		if err != nil || got != "kept" {
			t.Fatalf("err=%v text=%q", err, got)
		}
	})
	t.Run("null and empty remain valid", func(t *testing.T) {
		body := "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"refusal\":null}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"refusal\":\"\"},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n"
		var finish *ir.StreamEvent
		err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
			if ev.Kind == ir.EventFinish {
				copied := ev
				finish = &copied
			}
			return nil
		})
		if err != nil || finish == nil || finish.StopReason != ir.StopRefusal {
			t.Fatalf("err=%v finish=%+v", err, finish)
		}
	})
}

func TestEncodeStreamChatRefusalAndFilter(t *testing.T) {
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("recorder does not support flushing")
	}
	enc := NewStreamEncoder("m")
	for _, ev := range []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "c1"},
		{Kind: ir.EventTextDelta, Text: "kept"},
		{Kind: ir.EventTextDelta, Text: "", Refusal: true},
		{Kind: ir.EventFinish, StopReason: ir.StopContentFilter},
	} {
		if err := enc.Encode(ev, w); err != nil {
			t.Fatal(err)
		}
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"kept"`) || !strings.Contains(body, `"refusal":""`) || !strings.Contains(body, `"finish_reason":"content_filter"`) {
		t.Fatalf("body = %s", body)
	}
}
