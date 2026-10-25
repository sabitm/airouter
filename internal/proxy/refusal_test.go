package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/responses"
	"airouter/internal/proxy/sse"
)

const responsesFallbackSSE = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"fallback text\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"fallback text\"}]}]}}\n\n"

func TestCollectResponsesRefusalStream(t *testing.T) {
	body := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_r\",\"model\":\"m\",\"status\":\"completed\",\"incomplete_details\":null,\"output\":[{\"id\":\"msg_r\",\"type\":\"message\",\"content\":[{\"type\":\"refusal\",\"refusal\":\"no\"}]}],\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n"
	resp, err := collectStreamResponse(strings.NewReader(body), responsesCodec, nil, "fallback", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != ir.StopRefusal || len(resp.Content) != 1 || !resp.Content[0].Refusal || resp.Content[0].Text != "no" {
		t.Fatalf("collected = %+v", resp)
	}
	if resp.Usage.InputTokens != 5 || resp.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestResponsesRefusalTranslatedToChatAndAnthropic(t *testing.T) {
	const upstream = `{"id":"resp_r","model":"up","status":"completed","incomplete_details":null,"output":[{"type":"message","content":[{"type":"output_text","text":"kept"},{"type":"refusal","refusal":"no"}]}],"usage":{"input_tokens":4,"output_tokens":3}}`
	var cap capturedUpstream
	base, token := setupRefusalUpstream(t, domain.ProtocolOpenAIResponses, upstream, &cap)

	chatResp, chatBody := post(t, base+"/v1/chat/completions", token, `{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
	if chatResp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d body=%s", chatResp.StatusCode, chatBody)
	}
	if !strings.Contains(string(chatBody), `"content":"kept"`) || !strings.Contains(string(chatBody), `"refusal":"no"`) || !strings.Contains(string(chatBody), `"finish_reason":"stop"`) {
		t.Fatalf("chat projection = %s", chatBody)
	}

	anthResp, anthBody := post(t, base+"/v1/messages", token, `{"model":"default","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if anthResp.StatusCode != http.StatusOK {
		t.Fatalf("anthropic status = %d body=%s", anthResp.StatusCode, anthBody)
	}
	if !strings.Contains(string(anthBody), `"text":"kept"`) || !strings.Contains(string(anthBody), `"text":"no"`) || !strings.Contains(string(anthBody), `"stop_reason":"refusal"`) {
		t.Fatalf("anthropic projection = %s", anthBody)
	}
}

func TestResponsesPassthroughPreservesRefusalBytes(t *testing.T) {
	const upstream = `{"id":"resp_r","model":"real-model","status":"completed","incomplete_details":null,"output":[{"type":"message","content":[{"type":"refusal","refusal":"native"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
	var cap capturedUpstream
	base, token := setupRefusalUpstream(t, domain.ProtocolOpenAIResponses, upstream, &cap)
	resp, body := post(t, base+"/v1/responses", token, `{"model":"default","input":[{"type":"message","role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if string(body) != upstream {
		t.Fatalf("response bytes changed: %s", body)
	}
	if !strings.Contains(string(cap.body), `"model":"real-model"`) || strings.Contains(string(cap.body), `"model":"default"`) {
		t.Fatalf("passthrough model rewrite failed: %s", cap.body)
	}
}

func TestClassifyResponsesRefusalDone(t *testing.T) {
	nonempty := sse.Event{Name: "response.refusal.done", Data: []byte(`{"type":"response.refusal.done","item_id":"m","output_index":0,"content_index":0,"refusal":"no"}`)}
	if class, fail := classifyResponsesPassthrough(nonempty); class != passVisible || fail != nil {
		t.Fatalf("nonempty class=%v fail=%v", class, fail)
	}
	empty := sse.Event{Name: "response.refusal.done", Data: []byte(`{"type":"response.refusal.done","item_id":"m","output_index":0,"content_index":0,"refusal":""}`)}
	if class, fail := classifyResponsesPassthrough(empty); class != passLifecycle || fail != nil {
		t.Fatalf("empty class=%v fail=%v", class, fail)
	}
}

func TestResponsesRefusalDoneOnlyDoesNotFailOver(t *testing.T) {
	const doneOnly = "event: response.refusal.done\ndata: {\"type\":\"response.refusal.done\",\"item_id\":\"m\",\"output_index\":0,\"content_index\":0,\"refusal\":\"no\"}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"after visible\"}}\n\n"
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, doneOnly)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)
	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "d1", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolOpenAIResponses}
	p2 := &domain.Provider{Name: "d2", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAIResponses}
	if err := st.CreateProvider(ctx, p1); err != nil || st.CreateProvider(ctx, p2) != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, body := postStream(t, ts.URL+"/v1/responses", key.Token, `{"model":"default","stream":true,"input":"hi"}`)
	if n1 != 1 || n2 != 0 {
		t.Fatalf("hits n1=%d n2=%d", n1, n2)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"refusal":"no"`) || strings.Contains(body, "Hello") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestResponsesEmptyRefusalDoneCanFailOver(t *testing.T) {
	const emptyDone = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_e\",\"status\":\"in_progress\"}}\n\nevent: response.refusal.done\ndata: {\"type\":\"response.refusal.done\",\"item_id\":\"m\",\"output_index\":0,\"content_index\":0,\"refusal\":\"\"}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"before bytes\"}}\n\n"
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, emptyDone)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, responsesFallbackSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)
	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "ed1", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolOpenAIResponses}
	p2 := &domain.Provider{Name: "ed2", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAIResponses}
	if err := st.CreateProvider(ctx, p1); err != nil || st.CreateProvider(ctx, p2) != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, body := postStream(t, ts.URL+"/v1/responses", key.Token, `{"model":"default","stream":true,"input":"hi"}`)
	if n1 != 1 || n2 != 1 || resp.StatusCode != http.StatusOK || !strings.Contains(body, "fallback text") || strings.Contains(body, "before bytes") {
		t.Fatalf("hits=%d/%d status=%d body=%s", n1, n2, resp.StatusCode, body)
	}
}

func TestAnthropicUnaryRefusalProjectsToOpenAIIngresses(t *testing.T) {
	const upstream = `{"id":"msg","type":"message","role":"assistant","model":"up","content":[{"type":"text","text":"no"}],"stop_reason":"refusal","usage":{"input_tokens":4,"output_tokens":1,"cache_read_input_tokens":2,"cache_creation_input_tokens":1}}`
	var cap capturedUpstream
	base, token := setupRefusalUpstream(t, domain.ProtocolAnthropic, upstream, &cap)
	chatResp, chatBody := post(t, base+"/v1/chat/completions", token, `{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
	if chatResp.StatusCode != http.StatusOK || !strings.Contains(string(chatBody), `"refusal":"no"`) || strings.Contains(string(chatBody), `"content":"no"`) {
		t.Fatalf("chat status=%d body=%s", chatResp.StatusCode, chatBody)
	}
	respResp, respBody := post(t, base+"/v1/responses", token, `{"model":"default","input":"hi"}`)
	if respResp.StatusCode != http.StatusOK || !strings.Contains(string(respBody), `"type":"refusal"`) || !strings.Contains(string(respBody), `"refusal":"no"`) || strings.Contains(string(respBody), `"output_text"`) {
		t.Fatalf("responses status=%d body=%s", respResp.StatusCode, respBody)
	}
	if !strings.Contains(string(chatBody), `"cached_tokens":2`) || !strings.Contains(string(respBody), `"cache_write_tokens":1`) {
		t.Fatalf("cache usage dropped chat=%s responses=%s", chatBody, respBody)
	}
}

func TestProjectUnaryRefusalMultipleBlocks(t *testing.T) {
	base := &ir.Response{
		StopReason: ir.StopRefusal,
		Content: []ir.ContentBlock{
			{Type: ir.BlockReasoning, Text: "think"},
			{Type: ir.BlockText, Text: "no "},
			{Type: ir.BlockText, Text: "thanks"},
		},
	}
	got := ir.ProjectUnaryRefusal(base)
	if len(got) != 3 || got[0].Type != ir.BlockReasoning || got[0].Refusal || !got[1].Refusal || got[1].Text != "no " || !got[2].Refusal || got[2].Text != "thanks" {
		t.Fatalf("projected = %+v", got)
	}
	if base.Content[1].Refusal || base.Content[2].Refusal {
		t.Fatal("projection mutated input")
	}
	mixed := &ir.Response{StopReason: ir.StopRefusal, Content: []ir.ContentBlock{
		{Type: ir.BlockText, Text: "kept"},
		{Type: ir.BlockText, Text: "no", Refusal: true},
	}}
	if ir.ProjectUnaryRefusal(mixed)[0].Refusal {
		t.Fatal("mixed sibling was reclassified")
	}
	tool := &ir.Response{StopReason: ir.StopRefusal, Content: []ir.ContentBlock{
		{Type: ir.BlockText, Text: "no"},
		{Type: ir.BlockToolUse, ToolName: "f"},
	}}
	if ir.ProjectUnaryRefusal(tool)[0].Refusal {
		t.Fatal("tool response was reclassified")
	}
	server := &ir.Response{StopReason: ir.StopRefusal, Content: []ir.ContentBlock{
		{Type: ir.BlockText, Text: "no"},
		{Type: ir.BlockServer},
	}}
	if ir.ProjectUnaryRefusal(server)[0].Refusal {
		t.Fatal("server response was reclassified")
	}
	textless := &ir.Response{StopReason: ir.StopRefusal, Content: []ir.ContentBlock{{Type: ir.BlockReasoning, Text: "think"}}}
	empty := ir.ProjectUnaryRefusal(textless)
	if len(empty) != 2 || empty[1].Type != ir.BlockText || !empty[1].Refusal || empty[1].Text != "" || textless.Content[0].Refusal {
		t.Fatalf("textless = %+v", empty)
	}
	other := &ir.Response{StopReason: ir.StopEndTurn, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "kept"}}}
	if ir.ProjectUnaryRefusal(other)[0].Refusal {
		t.Fatal("non-refusal stop was projected")
	}
}

func TestAnthropicMultipleUnmarkedBlocksProject(t *testing.T) {
	const upstream = `{"id":"msg","type":"message","role":"assistant","model":"up","content":[{"type":"thinking","thinking":"think","signature":"sig"},{"type":"text","text":"no "},{"type":"text","text":"thanks"}],"stop_reason":"refusal","usage":{"input_tokens":4,"output_tokens":2}}`
	var cap capturedUpstream
	base, token := setupRefusalUpstream(t, domain.ProtocolAnthropic, upstream, &cap)
	chatResp, chatBody := post(t, base+"/v1/chat/completions", token, `{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
	if chatResp.StatusCode != http.StatusOK || !strings.Contains(string(chatBody), `"refusal":"no thanks"`) || strings.Contains(string(chatBody), `"content":"no`) {
		t.Fatalf("chat status=%d body=%s", chatResp.StatusCode, chatBody)
	}
	respResp, respBody := post(t, base+"/v1/responses", token, `{"model":"default","input":"hi"}`)
	if respResp.StatusCode != http.StatusOK || !strings.Contains(string(respBody), `"refusal":"no "`) || !strings.Contains(string(respBody), `"refusal":"thanks"`) || strings.Contains(string(respBody), `"output_text"`) {
		t.Fatalf("responses status=%d body=%s", respResp.StatusCode, respBody)
	}
}

func TestAnthropicCollectedRefusalProjectsToOpenAIIngresses(t *testing.T) {
	const raw = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg\",\"model\":\"up\",\"usage\":{\"input_tokens\":4,\"cache_read_input_tokens\":2,\"cache_creation_input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"no\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	collected, err := collectStreamResponse(strings.NewReader(raw), anthropicCodec, nil, "up", nil)
	if err != nil {
		t.Fatal(err)
	}
	if collected.StopReason != ir.StopRefusal || len(collected.Content) != 1 || collected.Content[0].Refusal || collected.Content[0].Text != "no" {
		t.Fatalf("collected = %+v", collected)
	}
	chat, err := openaiCodec.encodeResponse(collected)
	if err != nil || !strings.Contains(string(chat), `"refusal":"no"`) || strings.Contains(string(chat), `"content":"no"`) {
		t.Fatalf("chat err=%v body=%s", err, chat)
	}
	resp, err := responsesCodec.encodeResponse(collected)
	if err != nil || !strings.Contains(string(resp), `"refusal":"no"`) || strings.Contains(string(resp), `"output_text"`) {
		t.Fatalf("responses err=%v body=%s", err, resp)
	}
	if collected.Content[0].Refusal {
		t.Fatal("projection mutated collected IR")
	}
	if collected.Usage.CacheReadTokens != 2 || collected.Usage.CacheWriteTokens != 1 {
		t.Fatalf("usage = %+v", collected.Usage)
	}
}

func TestCodexCollectedRefusalAndDistinctResponsesPaths(t *testing.T) {
	const raw = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_r\",\"model\":\"up\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_r\",\"type\":\"message\",\"content\":[{\"type\":\"refusal\",\"refusal\":\"no\"}]}],\"usage\":{\"input_tokens\":7,\"output_tokens\":2,\"input_tokens_details\":{\"cached_tokens\":3,\"cache_write_tokens\":1}}}}\n\n"
	for _, backend := range []codec{codexCodec, opencodeResponsesCodec} {
		collected, err := collectStreamResponse(strings.NewReader(raw), backend, nil, "fallback", nil)
		if err != nil {
			t.Fatal(backend.id, err)
		}
		if collected.StopReason != ir.StopRefusal || len(collected.Content) != 1 || !collected.Content[0].Refusal || collected.Content[0].Text != "no" {
			t.Fatalf("%s collected = %+v", backend.id, collected)
		}
		if collected.Usage.InputTokens != 7 || collected.Usage.CacheReadTokens != 3 || collected.Usage.CacheWriteTokens != 1 {
			t.Fatalf("%s usage = %+v", backend.id, collected.Usage)
		}
	}
	var cap capturedUpstream
	base, token := setupRefusalUpstream(t, domain.ProtocolOpenAICodex, raw, &cap)
	resp, body := postStream(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"hi"}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"refusal":"no"`) || !strings.Contains(body, `"cached_tokens":3`) {
		t.Fatalf("codex ingress status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(cap.body), `"stream":true`) {
		t.Fatalf("codex request = %s", cap.body)
	}

	var oc capturedUpstream
	ocBase, ocToken := setupRefusalUpstreamModel(t, domain.ProtocolOpencode, "muse-spark-1.2-contributor-free", raw, &oc)
	ocResp, ocBody := postStream(t, ocBase+"/v1/responses", ocToken, `{"model":"default","stream":true,"input":"hi"}`)
	if ocResp.StatusCode != http.StatusOK || !strings.Contains(ocBody, `"type":"response.refusal.delta"`) || !strings.Contains(ocBody, `"refusal":"no"`) {
		t.Fatalf("opencode responses status=%d body=%s", ocResp.StatusCode, ocBody)
	}
	if strings.Contains(ocBody, `"status":"incomplete"`) || !strings.Contains(ocBody, `"status":"completed"`) || !strings.Contains(ocBody, `"cached_tokens":3`) {
		t.Fatalf("native status or usage dropped: %s", ocBody)
	}
	if oc.path != "/responses" || !strings.Contains(string(oc.body), `"model":"muse-spark-1.2-contributor-free"`) || strings.Contains(string(oc.body), `"model":"default"`) || !strings.Contains(string(oc.body), `"input"`) {
		t.Fatalf("upstream path=%s body=%s", oc.path, oc.body)
	}
}

func TestCodexBackendReusesRefusalHistory(t *testing.T) {
	req, err := responses.DecodeRequest([]byte(`{"model":"default","input":[{"role":"user","content":"first"},{"role":"assistant","content":[{"type":"refusal","refusal":""}]},{"role":"user","content":"second"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := responses.EncodeCodexRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"refusal":""`) || strings.Count(string(out), `"role":"user"`) < 2 {
		t.Fatalf("codex body = %s", out)
	}
}

func TestRefusalByteDoesNotFailOver(t *testing.T) {
	const partial = "event: response.refusal.delta\ndata: {\"type\":\"response.refusal.delta\",\"item_id\":\"msg_r\",\"output_index\":0,\"content_index\":0,\"delta\":\"no\"}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"message\":\"mid boom\"}}\n\n"
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, partial)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)
	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "r1", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolOpenAIResponses}
	p2 := &domain.Provider{Name: "r2", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if n1 != 1 || n2 != 0 {
		t.Fatalf("hits n1=%d n2=%d", n1, n2)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"refusal":"no"`) || strings.Contains(body, "Hello world") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestEmptyRefusalMetadataCanFailOver(t *testing.T) {
	const createdOnly = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_e\",\"status\":\"in_progress\"}}\n\nevent: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"item_id\":\"msg_e\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"refusal\",\"refusal\":\"\"}}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"before bytes\"}}\n\n"
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, createdOnly)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)
	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "e1", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolOpenAIResponses}
	p2 := &domain.Provider{Name: "e2", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if n1 != 1 || n2 != 1 {
		t.Fatalf("hits n1=%d n2=%d status=%d body=%s", n1, n2, resp.StatusCode, body)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Hello ") || !strings.Contains(body, "world") || strings.Contains(body, "before bytes") || strings.Contains(body, "resp_e") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func setupRefusalUpstream(t *testing.T, protocol domain.Protocol, body string, cap *capturedUpstream) (string, string) {
	return setupRefusalUpstreamModel(t, protocol, "real-model", body, cap)
}

func setupRefusalUpstreamModel(t *testing.T, protocol domain.Protocol, model, body string, cap *capturedUpstream) (string, string) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if cap != nil {
			cap.body = append([]byte(nil), raw...)
			cap.path = r.URL.Path
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)
	prov := &domain.Provider{Name: "p", BaseURL: upstream.URL, APIKey: "k", Protocol: protocol}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: model, Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token
}
