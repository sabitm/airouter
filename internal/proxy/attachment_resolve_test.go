package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/media"
	"airouter/internal/proxy/openai"
)

const testPNGB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVQI12P4z8AAAAMBAQAY3Y20AAAAAElFTkSuQmCC"

func chatImageBody(model string) string {
	b, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "describe"},
					map[string]any{"type": "image_url", "image_url": map[string]any{
						"url": "data:image/png;base64," + testPNGB64,
					}},
				},
			},
		},
	})
	return string(b)
}

func chatPDFBody(model string) string {
	pdf := "JVBERi0xLjQgdGVzdAo=" // %PDF-1.4 test\n
	b, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "file", "file": map[string]any{
						"filename":  "doc.pdf",
						"file_data": "data:application/pdf;base64," + pdf,
					}},
				},
			},
		},
	})
	return string(b)
}

func backoffSkips(p *Proxy, providerID int64) int {
	p.boMu.Lock()
	defer p.boMu.Unlock()
	st := p.bo[providerID]
	if st == nil {
		return 0
	}
	return st.skips
}

// Incompatible primary (Qoder) must not be contacted; Anthropic fallback receives the image.
func TestAttachmentSkipIncompatiblePrimary(t *testing.T) {
	qoder := newScriptedUpstream(t, domain.ProtocolOpenAI) // body unused; hits must stay 0
	anth := newScriptedUpstream(t, domain.ProtocolAnthropic)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{qoder, anth},
		[]domain.Protocol{domain.ProtocolQoder, domain.ProtocolAnthropic})

	resp, out := post(t, base+"/v1/chat/completions", token, chatImageBody("default"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if qoder.hits.Load() != 0 {
		t.Fatalf("qoder hits=%d, want 0 (structural skip)", qoder.hits.Load())
	}
	if anth.hits.Load() != 1 {
		t.Fatalf("anthropic hits=%d, want 1", anth.hits.Load())
	}
	body := anth.requestBody(t)
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), "image") || !strings.Contains(string(raw), testPNGB64) {
		t.Fatalf("anthropic upstream missing image: %s", raw)
	}
}

// Structural skip must not penalize provider backoff or consume existing skip credits.
func TestAttachmentSkipDoesNotPenalize(t *testing.T) {
	qoder := newScriptedUpstream(t, domain.ProtocolOpenAI)
	anth := newScriptedUpstream(t, domain.ProtocolAnthropic)
	base, token, px := setupComboProxy(t, domain.StrategyFailover,
		[]*scriptedUpstream{qoder, anth},
		[]domain.Protocol{domain.ProtocolQoder, domain.ProtocolAnthropic})

	// Discover provider IDs from the live proxy store and seed qoder backoff.
	combo, err := px.store.GetComboByName(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	var qoderID, anthID int64
	for _, tgt := range combo.Targets {
		if tgt.Provider != nil && tgt.Provider.Protocol == domain.ProtocolQoder {
			qoderID = tgt.ProviderID
		}
		if tgt.Provider != nil && tgt.Provider.Protocol == domain.ProtocolAnthropic {
			anthID = tgt.ProviderID
		}
	}
	if qoderID == 0 || anthID == 0 {
		t.Fatalf("ids qoder=%d anth=%d", qoderID, anthID)
	}
	px.penalizeProvider(qoderID)
	before := backoffSkips(px, qoderID)
	if before != backoffBaseSkips {
		t.Fatalf("seeded skips=%d want %d", before, backoffBaseSkips)
	}

	for i := 0; i < 2; i++ {
		resp, out := post(t, base+"/v1/chat/completions", token, chatImageBody("default"))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("img req %d: %d %s", i, resp.StatusCode, out)
		}
	}
	if qoder.hits.Load() != 0 {
		t.Fatalf("qoder hits=%d", qoder.hits.Load())
	}
	if anth.hits.Load() != 2 {
		t.Fatalf("anth hits=%d want 2", anth.hits.Load())
	}
	after := backoffSkips(px, qoderID)
	if after != before {
		t.Fatalf("incompatible provider skip credit changed %d -> %d", before, after)
	}
	// Anthropic must not have been penalized either.
	if backoffSkips(px, anthID) != 0 {
		t.Fatalf("anth backoff skips=%d want 0", backoffSkips(px, anthID))
	}
}

// Compatible primary fails; trailing incompatible must not hide the real failure.
func TestAttachmentCompatibleFailureNotHidden(t *testing.T) {
	oai := newScriptedUpstream(t, domain.ProtocolOpenAI)
	qoder := newScriptedUpstream(t, domain.ProtocolOpenAI)
	oai.status.Store(http.StatusInternalServerError)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{oai, qoder},
		[]domain.Protocol{domain.ProtocolOpenAI, domain.ProtocolQoder})

	resp, out := post(t, base+"/v1/chat/completions", token, chatImageBody("default"))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500 from compatible primary", resp.StatusCode, out)
	}
	if oai.hits.Load() != 1 {
		t.Fatalf("oai hits=%d", oai.hits.Load())
	}
	if qoder.hits.Load() != 0 {
		t.Fatalf("qoder hits=%d, want 0", qoder.hits.Load())
	}
}

// All targets structurally incompatible => 400 with a concrete reason.
func TestAttachmentAllIncompatible(t *testing.T) {
	a := newScriptedUpstream(t, domain.ProtocolOpenAI)
	b := newScriptedUpstream(t, domain.ProtocolOpenAI)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{a, b},
		[]domain.Protocol{domain.ProtocolQoder, domain.ProtocolCursor})

	resp, out := post(t, base+"/v1/chat/completions", token, chatImageBody("default"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", resp.StatusCode, out)
	}
	if a.hits.Load() != 0 || b.hits.Load() != 0 {
		t.Fatalf("hits a=%d b=%d, want 0/0", a.hits.Load(), b.hits.Load())
	}
	if !strings.Contains(string(out), "does not support") {
		t.Fatalf("error body missing concrete attachment reason: %s", out)
	}
}

// Single incompatible target => 400 with a concrete reason.
func TestAttachmentSingleIncompatible(t *testing.T) {
	a := newScriptedUpstream(t, domain.ProtocolOpenAI)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{a},
		[]domain.Protocol{domain.ProtocolQoder})

	resp, out := post(t, base+"/v1/chat/completions", token, chatImageBody("default"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", resp.StatusCode, out)
	}
	if a.hits.Load() != 0 {
		t.Fatalf("hits=%d", a.hits.Load())
	}
	if !strings.Contains(string(out), "does not support") {
		t.Fatalf("error body missing concrete attachment reason: %s", out)
	}
}

// PDF to Anthropic works; PDF to Codex-incompatible primary is skipped.
func TestAttachmentPDFFailoverToAnthropic(t *testing.T) {
	primary := newScriptedUpstream(t, domain.ProtocolOpenAI)
	anth := newScriptedUpstream(t, domain.ProtocolAnthropic)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{primary, anth},
		[]domain.Protocol{domain.ProtocolQoder, domain.ProtocolAnthropic})

	resp, out := post(t, base+"/v1/chat/completions", token, chatPDFBody("default"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if primary.hits.Load() != 0 || anth.hits.Load() != 1 {
		t.Fatalf("hits primary=%d anth=%d", primary.hits.Load(), anth.hits.Load())
	}
	raw, _ := json.Marshal(anth.requestBody(t))
	if !strings.Contains(string(raw), "document") {
		t.Fatalf("expected document block: %s", raw)
	}
}

// Streaming precommit path also skips incompatible targets.
func TestAttachmentStreamSkipIncompatible(t *testing.T) {
	qoder := newScriptedUpstream(t, domain.ProtocolOpenAI)
	anthHits := 0
	anth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthHits++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"up\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	t.Cleanup(anth.Close)

	st := newTestStore(t)
	ctx := context.Background()
	pq := &domain.Provider{Name: "q", BaseURL: qoder.server.URL, APIKey: "k", Protocol: domain.ProtocolQoder}
	pa := &domain.Provider{Name: "a", BaseURL: anth.URL, APIKey: "k", Protocol: domain.ProtocolAnthropic}
	if err := st.CreateProvider(ctx, pq); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, pa); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: pq.ID, UpstreamModel: "m", Enabled: true},
		{ProviderID: pa.ID, UpstreamModel: "m", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	body := `{"model":"default","stream":true,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + testPNGB64 + `"}}]}]}`
	resp, out := post(t, srv.URL+"/v1/chat/completions", key.Token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if qoder.hits.Load() != 0 {
		t.Fatalf("qoder hits=%d", qoder.hits.Load())
	}
	if anthHits != 1 {
		t.Fatalf("anth hits=%d", anthHits)
	}
}

// Malformed attachment is a client error without upstream contact.
func TestAttachmentMalformedClientError(t *testing.T) {
	up := newScriptedUpstream(t, domain.ProtocolOpenAI)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{up}, []domain.Protocol{domain.ProtocolOpenAI})

	body := `{"model":"default","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,!!!"}}]}]}`
	resp, out := post(t, base+"/v1/chat/completions", token, body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", resp.StatusCode, out)
	}
	if up.hits.Load() != 0 {
		t.Fatalf("upstream contacted on malformed attachment")
	}
}

// Spoofed image bytes labeled image/png are rejected with zero upstream calls.
func TestAttachmentSpoofedImageClientError(t *testing.T) {
	up := newScriptedUpstream(t, domain.ProtocolOpenAI)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{up}, []domain.Protocol{domain.ProtocolOpenAI})

	fake := base64.StdEncoding.EncodeToString([]byte("not-a-real-png-payload"))
	body := `{"model":"default","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + fake + `"}}]}]}`
	resp, out := post(t, base+"/v1/chat/completions", token, body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", resp.StatusCode, out)
	}
	if up.hits.Load() != 0 {
		t.Fatal("upstream contacted on spoofed image")
	}
}

// Formatted JSON with spaced keys still validates attachments (no marker bypass).
func TestAttachmentFormattedWhitespacePassthrough(t *testing.T) {
	up := newScriptedUpstream(t, domain.ProtocolOpenAI)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{up}, []domain.Protocol{domain.ProtocolOpenAI})

	fake := base64.StdEncoding.EncodeToString([]byte("not-a-real-png-payload"))
	// Spaced JSON that would bypass the old substring marker scan.
	body := `{
  "model" : "default",
  "messages" : [ {
    "role" : "user",
    "content" : [ {
      "type" : "image_url",
      "image_url" : { "url" : "data:image/png;base64,` + fake + `" }
    } ]
  } ]
}`
	resp, out := post(t, base+"/v1/chat/completions", token, body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", resp.StatusCode, out)
	}
	if up.hits.Load() != 0 {
		t.Fatal("upstream contacted; formatted attachment bypassed validation")
	}
}

// Same-format passthrough preserves unknown nested fields while rewriting model.
func TestAttachmentPassthroughPreservesUnknownFields(t *testing.T) {
	up := newScriptedUpstream(t, domain.ProtocolOpenAI)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{up}, []domain.Protocol{domain.ProtocolOpenAI})

	body := mustJSON(map[string]any{
		"model": "default",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "hi", "cache_control": map[string]any{"type": "ephemeral"}},
					map[string]any{
						"type": "image_url",
						"image_url": map[string]any{
							"url":    "data:image/png;base64," + testPNGB64,
							"detail": "high",
						},
					},
				},
			},
		},
		"custom_extension": map[string]any{"foo": "bar"},
	})
	resp, out := post(t, base+"/v1/chat/completions", token, string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	got := up.requestBody(t)
	if got["model"] != "real-model" {
		t.Fatalf("model=%v want real-model", got["model"])
	}
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"custom_extension"`) || !strings.Contains(string(raw), `"foo":"bar"`) {
		t.Fatalf("unknown top-level field lost: %s", raw)
	}
	if !strings.Contains(string(raw), `"detail":"high"`) {
		t.Fatalf("unknown nested image field lost: %s", raw)
	}
	if !strings.Contains(string(raw), `"cache_control"`) {
		t.Fatalf("unknown nested content field lost: %s", raw)
	}
}

// Provider file ID is accepted on same-format passthrough and rejected when translated.
func TestAttachmentFileIDPassthroughVsTranslated(t *testing.T) {
	// Passthrough OpenAI->OpenAI with file_id only.
	up := newScriptedUpstream(t, domain.ProtocolOpenAI)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{up}, []domain.Protocol{domain.ProtocolOpenAI})
	body := mustJSON(map[string]any{
		"model": "default",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "file", "file": map[string]any{"file_id": "file-abc"}},
				},
			},
		},
	})
	resp, out := post(t, base+"/v1/chat/completions", token, string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("passthrough status=%d body=%s", resp.StatusCode, out)
	}
	raw, _ := json.Marshal(up.requestBody(t))
	if !strings.Contains(string(raw), "file-abc") {
		t.Fatalf("file id not forwarded: %s", raw)
	}

	// Translated OpenAI->Anthropic with file_id only must 400 (not portable).
	anth := newScriptedUpstream(t, domain.ProtocolAnthropic)
	base2, token2 := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{anth}, []domain.Protocol{domain.ProtocolAnthropic})
	resp2, out2 := post(t, base2+"/v1/chat/completions", token2, string(body))
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("translated status=%d body=%s, want 400", resp2.StatusCode, out2)
	}
	if anth.hits.Load() != 0 {
		t.Fatal("anthropic contacted for non-portable file id")
	}
	if !strings.Contains(string(out2), "file ID") && !strings.Contains(string(out2), "file IDs") {
		t.Fatalf("error body missing file-id reason: %s", out2)
	}
}

// Oversized JSON body maps to exactly 413.
func TestRequestBodyTooLarge413(t *testing.T) {
	up := newScriptedUpstream(t, domain.ProtocolOpenAI)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{up}, []domain.Protocol{domain.ProtocolOpenAI})

	big := make([]byte, maxBodyBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	body := `{"model":"default","messages":[{"role":"user","content":"` + string(big) + `"}]}`
	resp, _ := post(t, base+"/v1/chat/completions", token, body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want 413", resp.StatusCode)
	}
	if up.hits.Load() != 0 {
		t.Fatal("upstream should not be contacted")
	}
}

// materialize converts URL images to inline data and dedupes fetches per request.
func TestAttachmentMaterializeDedupe(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		raw, _ := base64.StdEncoding.DecodeString(testPNGB64)
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)

	u := strings.Replace(srv.URL, "127.0.0.1", "example.com", 1)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial(network, strings.TrimPrefix(srv.URL, "http://"))
		},
	}
	fetcher := &media.Fetcher{
		Resolver: staticIPResolver{ip: net.ParseIP("8.8.8.8")},
		Client:   &http.Client{Transport: tr, Timeout: 5 * time.Second},
	}
	prep := &attachmentPrep{fetcher: fetcher}
	req := &ir.Request{Messages: []ir.Message{{
		Role: ir.RoleUser,
		Content: []ir.ContentBlock{
			{Type: ir.BlockImage, Image: &ir.Image{URL: u + "/a.png"}},
			{Type: ir.BlockImage, Image: &ir.Image{URL: u + "/a.png"}},
		},
	}}}
	// kiro requires materialization.
	if err := prep.materialize(context.Background(), req, kiroCodec); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d want 1", hits.Load())
	}
	for _, b := range req.Messages[0].Content {
		if b.Image == nil || b.Image.Data == "" || b.Image.URL != "" || b.Image.MediaType != "image/png" {
			t.Fatalf("not inlined: %+v", b.Image)
		}
		if b.Image.Data != testPNGB64 {
			t.Fatalf("data mismatch")
		}
	}
}

type staticIPResolver struct{ ip net.IP }

func (s staticIPResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: s.ip}}, nil
}

// Anthropic PDF ingress can translate to OpenAI Chat file_data.
func TestAttachmentAnthropicPDFToChatProxy(t *testing.T) {
	up := newScriptedUpstream(t, domain.ProtocolOpenAI)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{up}, []domain.Protocol{domain.ProtocolOpenAI})

	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 test\n"))
	body := mustJSON(map[string]any{
		"model":      "default",
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
							"data":       pdf,
						},
					},
				},
			},
		},
	})
	resp, out := post(t, base+"/v1/messages", token, string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	raw, _ := json.Marshal(up.requestBody(t))
	if !strings.Contains(string(raw), `"type":"file"`) || !strings.Contains(string(raw), pdf) {
		t.Fatalf("chat upstream missing file: %s", raw)
	}
}

// Conflicting multi-source file is a client error.
func TestAttachmentMultipleSourcesClientError(t *testing.T) {
	up := newScriptedUpstream(t, domain.ProtocolOpenAI)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{up}, []domain.Protocol{domain.ProtocolOpenAI})

	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 test\n"))
	// Craft IR via OpenAI decode is hard for multi-source; use translated path with
	// a hand-built body that openai decode maps... Chat wire has single source fields.
	// Drive InspectRequest path through a custom decode by using serveTranslated after
	// building via openai helpers is not possible for multi-source. Unit coverage is
	// in media.Inspect; here verify OpenAI decode of file_id+file_data if both set.
	body := mustJSON(map[string]any{
		"model": "default",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "file", "file": map[string]any{
						"file_id":   "file-abc",
						"file_data": "data:application/pdf;base64," + pdf,
					}},
				},
			},
		},
	})
	// Ensure openai decode produces both ID and Data.
	req, err := openai.DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Messages[0].Content[0].File.ID == "" || req.Messages[0].Content[0].File.Data == "" {
		t.Fatalf("expected both sources in IR: %+v", req.Messages[0].Content[0].File)
	}
	resp, out := post(t, base+"/v1/chat/completions", token, string(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", resp.StatusCode, out)
	}
	if up.hits.Load() != 0 {
		t.Fatal("upstream contacted")
	}
}

func anthToolResultImageBody(model string) string {
	b, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 64,
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type":  "tool_use",
						"id":    "toolu_1",
						"name":  "screenshot",
						"input": map[string]any{},
					},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "toolu_1",
						"content": []any{
							map[string]any{"type": "text", "text": "captured"},
							map[string]any{
								"type": "image",
								"source": map[string]any{
									"type":       "base64",
									"media_type": "image/png",
									"data":       testPNGB64,
								},
							},
						},
					},
				},
			},
		},
	})
	return string(b)
}

func anthToolResultPDFBody(model string) string {
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 test\n"))
	b, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 64,
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type":  "tool_use",
						"id":    "toolu_pdf",
						"name":  "read_doc",
						"input": map[string]any{},
					},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "toolu_pdf",
						"content": []any{
							map[string]any{
								"type":  "document",
								"title": "nested.pdf",
								"source": map[string]any{
									"type":       "base64",
									"media_type": "application/pdf",
									"data":       pdf,
								},
							},
						},
					},
				},
			},
		},
	})
	return string(b)
}

// Nested tool_result media must not silently drop on OpenAI-style backends.
func TestAttachmentNestedToolResultMedia(t *testing.T) {
	oai := newScriptedUpstream(t, domain.ProtocolOpenAI)
	anth := newScriptedUpstream(t, domain.ProtocolAnthropic)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{oai, anth},
		[]domain.Protocol{domain.ProtocolOpenAI, domain.ProtocolAnthropic})

	resp, out := post(t, base+"/v1/messages", token, anthToolResultImageBody("default"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if oai.hits.Load() != 0 {
		t.Fatalf("openai hits=%d want 0 (nested tool_result media incompatible)", oai.hits.Load())
	}
	if anth.hits.Load() != 1 {
		t.Fatalf("anthropic hits=%d want 1", anth.hits.Load())
	}
	raw, _ := json.Marshal(anth.requestBody(t))
	if !strings.Contains(string(raw), "tool_result") || !strings.Contains(string(raw), testPNGB64) {
		t.Fatalf("anthropic lost nested tool_result image: %s", raw)
	}
	if !strings.Contains(string(raw), `"type":"image"`) {
		t.Fatalf("anthropic nested image block missing: %s", raw)
	}

	// Nested PDF path: OpenAI primary skipped, Anthropic preserves document.
	oai2 := newScriptedUpstream(t, domain.ProtocolOpenAI)
	anth2 := newScriptedUpstream(t, domain.ProtocolAnthropic)
	base2, token2 := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{oai2, anth2},
		[]domain.Protocol{domain.ProtocolOpenAI, domain.ProtocolAnthropic})
	resp2, out2 := post(t, base2+"/v1/messages", token2, anthToolResultPDFBody("default"))
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("pdf status=%d body=%s", resp2.StatusCode, out2)
	}
	if oai2.hits.Load() != 0 || anth2.hits.Load() != 1 {
		t.Fatalf("pdf hits oai=%d anth=%d", oai2.hits.Load(), anth2.hits.Load())
	}
	raw2, _ := json.Marshal(anth2.requestBody(t))
	if !strings.Contains(string(raw2), "document") || !strings.Contains(string(raw2), "nested.pdf") {
		t.Fatalf("anthropic lost nested tool_result pdf: %s", raw2)
	}

	// Single incompatible translated target returns 400 with a concrete reason.
	oaiOnly := newScriptedUpstream(t, domain.ProtocolOpenAI)
	base3, token3 := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{oaiOnly}, []domain.Protocol{domain.ProtocolOpenAI})
	resp3, out3 := post(t, base3+"/v1/messages", token3, anthToolResultImageBody("default"))
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("single incompatible status=%d body=%s, want 400", resp3.StatusCode, out3)
	}
	if oaiOnly.hits.Load() != 0 {
		t.Fatal("openai contacted for nested tool_result media")
	}
	if !strings.Contains(string(out3), "tool_result") {
		t.Fatalf("error body missing tool_result reason: %s", out3)
	}
}

// Missing-source recognized image blocks must 400 with zero upstream calls.
func TestAttachmentMissingSourceImageClientError(t *testing.T) {
	// OpenAI chat: {"type":"image_url"} with no nested object.
	up := newScriptedUpstream(t, domain.ProtocolOpenAI)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{up}, []domain.Protocol{domain.ProtocolOpenAI})
	body := `{"model":"default","messages":[{"role":"user","content":[{"type":"image_url"}]}]}`
	resp, out := post(t, base+"/v1/chat/completions", token, body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("openai status=%d body=%s, want 400", resp.StatusCode, out)
	}
	if up.hits.Load() != 0 {
		t.Fatal("openai upstream contacted for empty image_url")
	}

	// Responses: {"type":"input_image"} with empty/missing image_url.
	up2 := newScriptedUpstream(t, domain.ProtocolOpenAIResponses)
	base2, token2 := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{up2}, []domain.Protocol{domain.ProtocolOpenAIResponses})
	body2 := mustJSON(map[string]any{
		"model": "default",
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_image"},
				},
			},
		},
	})
	resp2, out2 := post(t, base2+"/v1/responses", token2, string(body2))
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("responses status=%d body=%s, want 400", resp2.StatusCode, out2)
	}
	if up2.hits.Load() != 0 {
		t.Fatal("responses upstream contacted for empty input_image")
	}
}

// Non-HTTP(S) / credential-bearing / missing-host attachment URLs fail closed.
func TestAttachmentUnsafeRemoteURLClientError(t *testing.T) {
	up := newScriptedUpstream(t, domain.ProtocolOpenAI)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{up}, []domain.Protocol{domain.ProtocolOpenAI})

	cases := []string{
		"file:///etc/passwd",
		"ftp://example.com/a.png",
		"http://user:pass@example.com/a.png",
		"http:///no-host",
	}
	for _, u := range cases {
		body := mustJSON(map[string]any{
			"model": "default",
			"messages": []any{
				map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{
							"type":      "image_url",
							"image_url": map[string]any{"url": u},
						},
					},
				},
			},
		})
		resp, out := post(t, base+"/v1/chat/completions", token, string(body))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("url %q status=%d body=%s, want 400", u, resp.StatusCode, out)
		}
	}
	if up.hits.Load() != 0 {
		t.Fatalf("upstream contacted on unsafe URLs: hits=%d", up.hits.Load())
	}

	// Valid https still reaches a URL-native backend.
	good := mustJSON(map[string]any{
		"model": "default",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "https://example.com/a.png"},
					},
				},
			},
		},
	})
	resp, out := post(t, base+"/v1/chat/completions", token, string(good))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid https status=%d body=%s", resp.StatusCode, out)
	}
	if up.hits.Load() != 1 {
		t.Fatalf("valid https hits=%d want 1", up.hits.Load())
	}
}
