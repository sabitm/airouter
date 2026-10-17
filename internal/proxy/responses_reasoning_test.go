package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/ir"
)

const responsesStateRequestBody = `{"model":"default","input":[{"type":"reasoning","id":"rs_client","encrypted_content":"cipher-client","summary":[{"type":"summary_text","text":"prior"}]},{"type":"message","role":"user","content":"next"}]}`

const routingResponsesUpstreamBody = `{"id":"resp_up","model":"m","status":"completed","output":[{"id":"rs_up","type":"reasoning","status":"completed","encrypted_content":"cipher-up","summary":[{"type":"summary_text","text":"up"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`

type routingTarget struct {
	protocol domain.Protocol
	url      string
	model    string
	archived bool
	disabled bool
}

// setupResponsesRouting builds one provider per target and one failover combo.
func setupResponsesRouting(t *testing.T, targets []routingTarget) (string, string, *Proxy, []int64) {
	t.Helper()
	st := newTestStore(t)
	combo := &domain.Combo{Name: "default", Strategy: domain.StrategyFailover}
	var providerIDs []int64
	for i, rt := range targets {
		prov := &domain.Provider{Name: "p" + string(rune('a'+i)), BaseURL: rt.url, APIKey: "k", Protocol: rt.protocol, Archived: rt.archived}
		if err := st.CreateProvider(context.Background(), prov); err != nil {
			t.Fatal(err)
		}
		providerIDs = append(providerIDs, prov.ID)
		combo.Targets = append(combo.Targets, domain.ComboTarget{
			ProviderID: prov.ID, UpstreamModel: rt.model, Enabled: !rt.disabled,
		})
	}
	if err := st.CreateCombo(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "client")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	px := New(st, nil)
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token, px, providerIDs
}

func responsesJSONUpstream(t *testing.T, hits *atomic.Int64, body *[]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if body != nil {
			raw, _ := readAllBody(r)
			*body = raw
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(routingResponsesUpstreamBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func readAllBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

func TestResponsesOwnedStateRoutesToCompatibleCodec(t *testing.T) {
	var oaiHits, respHits atomic.Int64
	var upstreamBody []byte
	oai := responsesJSONUpstream(t, &oaiHits, nil)
	resp := responsesJSONUpstream(t, &respHits, &upstreamBody)
	base, token, px, ids := setupResponsesRouting(t, []routingTarget{
		{protocol: domain.ProtocolOpenAI, url: oai.URL, model: "gpt-4o"},
		{protocol: domain.ProtocolOpenAIResponses, url: resp.URL, model: "m"},
	})
	px.penalizeProvider(ids[0])
	before := backoffSkips(px, ids[0])

	httpResp, out := post(t, base+"/v1/responses", token, responsesStateRequestBody)
	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", httpResp.StatusCode, out)
	}
	if oaiHits.Load() != 0 || respHits.Load() != 1 {
		t.Fatalf("hits openai=%d responses=%d body=%s", oaiHits.Load(), respHits.Load(), out)
	}
	if backoffSkips(px, ids[0]) != before {
		t.Fatalf("skip-credit consumed for excluded provider: %d -> %d", before, backoffSkips(px, ids[0]))
	}
	if !strings.Contains(string(upstreamBody), `"encrypted_content":"cipher-client"`) || !strings.Contains(string(upstreamBody), `"id":"rs_client"`) {
		t.Fatalf("upstream lost provider state: %s", upstreamBody)
	}
	if !strings.Contains(string(out), `"encrypted_content":"cipher-up"`) || !strings.Contains(string(out), `"id":"rs_up"`) {
		t.Fatalf("client response lost provider state: %s", out)
	}
}

func TestResponsesOwnedStateAllIncompatibleIs400(t *testing.T) {
	var hits atomic.Int64
	up := responsesJSONUpstream(t, &hits, nil)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{{protocol: domain.ProtocolOpenAI, url: up.URL, model: "gpt-4o"}})
	resp, out := post(t, base+"/v1/responses", token, responsesStateRequestBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if hits.Load() != 0 {
		t.Fatalf("incompatible target contacted: %d", hits.Load())
	}
	if !strings.Contains(string(out), "provider-owned Responses reasoning") || !strings.Contains(string(out), "invalid_request_error") {
		t.Fatalf("error = %s", out)
	}
}

func TestResponsesOwnedStateAmbiguousIs400ZeroHits(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	a := responsesJSONUpstream(t, &hitsA, nil)
	b := responsesJSONUpstream(t, &hitsB, nil)
	base, token, px, ids := setupResponsesRouting(t, []routingTarget{
		{protocol: domain.ProtocolOpenAIResponses, url: a.URL, model: "model-a"},
		{protocol: domain.ProtocolOpenAIResponses, url: b.URL, model: "model-b"},
	})
	resp, out := post(t, base+"/v1/responses", token, responsesStateRequestBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if hitsA.Load() != 0 || hitsB.Load() != 0 {
		t.Fatalf("upstream contacted: %d/%d", hitsA.Load(), hitsB.Load())
	}
	if !strings.Contains(string(out), "ambiguous across multiple providers") {
		t.Fatalf("ambiguous error missing: %s", out)
	}
	for _, id := range ids {
		if backoffSkips(px, id) != 0 {
			t.Fatalf("ambiguous request consumed backoff for %d", id)
		}
	}
}

func TestAmbiguousResponsesRouteDuplicatesAllowed(t *testing.T) {
	base := []domain.ComboTarget{{ProviderID: 1, UpstreamModel: "m"}, {ProviderID: 1, UpstreamModel: "m"}}
	if msg := ambiguousResponsesRoute(base); msg != "" {
		t.Fatalf("duplicate identical route rejected: %s", msg)
	}
	if msg := ambiguousResponsesRoute([]domain.ComboTarget{{ProviderID: 1, UpstreamModel: "m"}}); msg != "" {
		t.Fatalf("single route rejected: %s", msg)
	}
	if msg := ambiguousResponsesRoute([]domain.ComboTarget{{ProviderID: 1, UpstreamModel: "a"}, {ProviderID: 1, UpstreamModel: "b"}}); msg == "" {
		t.Fatal("same provider different model must be ambiguous")
	}
	if msg := ambiguousResponsesRoute([]domain.ComboTarget{{ProviderID: 1, UpstreamModel: "m"}, {ProviderID: 2, UpstreamModel: "m"}}); msg == "" {
		t.Fatal("different providers must be ambiguous")
	}
}

func TestResponsesOwnedStateIgnoresDisabledAndArchived(t *testing.T) {
	var activeHits atomic.Int64
	active := responsesJSONUpstream(t, &activeHits, nil)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{
		{protocol: domain.ProtocolOpenAIResponses, url: "http://127.0.0.1:1", model: "archived-model", archived: true},
		{protocol: domain.ProtocolOpenAIResponses, url: "http://127.0.0.1:1", model: "disabled-model", disabled: true},
		{protocol: domain.ProtocolOpenAIResponses, url: active.URL, model: "live-model"},
	})
	resp, out := post(t, base+"/v1/responses", token, responsesStateRequestBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if activeHits.Load() != 1 {
		t.Fatalf("active hits=%d", activeHits.Load())
	}
}

func TestResponsesSummaryOnlyStillTranslatesAcrossFormats(t *testing.T) {
	var hits atomic.Int64
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		raw, _ := readAllBody(r)
		got = raw
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(up.Close)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{{protocol: domain.ProtocolOpenAI, url: up.URL, model: "gpt-4o"}})
	body := `{"model":"default","input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"portable chain"}]},{"type":"message","role":"user","content":"next"}]}`
	resp, out := post(t, base+"/v1/responses", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d", hits.Load())
	}
	if !strings.Contains(string(got), "portable chain") {
		t.Fatalf("readable summary lost: %s", got)
	}
	if strings.Contains(string(got), "encrypted_content") {
		t.Fatalf("summary-only request invented encrypted content: %s", got)
	}
}

func TestResponsesOwnedStateCodexTargetPreserves(t *testing.T) {
	var hits atomic.Int64
	var got []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		raw, _ := readAllBody(r)
		got = raw
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[{\"id\":\"rs_up\",\"type\":\"reasoning\",\"status\":\"completed\",\"encrypted_content\":\"cipher-up\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"up\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))
	}))
	t.Cleanup(up.Close)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{{protocol: domain.ProtocolOpenAICodex, url: up.URL, model: "gpt-5-codex"}})
	resp, out := post(t, base+"/v1/responses", token, responsesStateRequestBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d", hits.Load())
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(got, &sent); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"encrypted_content":"cipher-client"`) || !strings.Contains(string(got), `"id":"rs_client"`) {
		t.Fatalf("codex body lost state: %s", got)
	}
	if !strings.Contains(string(out), `"encrypted_content":"cipher-up"`) {
		t.Fatalf("client lost codex state: %s", out)
	}
}

func reasoningEventsCodec(events []ir.StreamEvent) codec {
	return codec{decodeStream: func(_ io.Reader, emit func(ir.StreamEvent) error) error {
		for _, ev := range events {
			if err := emit(ev); err != nil {
				return err
			}
		}
		return nil
	}}
}

func TestCollectStructuredReasoningOrderAndMetadata(t *testing.T) {
	enc := "cipher"
	done := ir.StreamEvent{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{
		ID: "rs_1", Status: "completed", EncryptedContent: &enc,
		Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}, {Type: "summary_text", Text: ""}},
	}}
	events := []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "r1", Model: "m"},
		{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", SummaryIndex: 0, Text: "chain"},
		done,
		done,
		{Kind: ir.EventTextDelta, Text: "answer"},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
	}
	resp, err := collectStreamResponse(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 2 || resp.Content[0].Type != ir.BlockReasoning || resp.Content[1].Type != ir.BlockText {
		t.Fatalf("content order or dedup = %+v", resp.Content)
	}
	block := resp.Content[0]
	if block.Text != "chain" || block.AnthropicSignature != "" || block.RedactedData != "" {
		t.Fatalf("reasoning block = %+v", block)
	}
	if block.ResponsesReasoning == nil || block.ResponsesReasoning.ID != "rs_1" {
		t.Fatalf("metadata = %+v", block.ResponsesReasoning)
	}
	if block.ResponsesReasoning.EncryptedContent == nil || *block.ResponsesReasoning.EncryptedContent != "cipher" {
		t.Fatalf("encrypted = %+v", block.ResponsesReasoning.EncryptedContent)
	}
	if len(block.ResponsesReasoning.Summary) != 2 || block.ResponsesReasoning.Summary[1].Text != "" {
		t.Fatalf("summary = %+v", block.ResponsesReasoning.Summary)
	}
	if resp.Content[1].Text != "answer" {
		t.Fatalf("text = %+v", resp.Content[1])
	}
}

func TestCollectStructuredReasoningInterleavedTextOrder(t *testing.T) {
	events := []ir.StreamEvent{
		{Kind: ir.EventTextDelta, Text: "before"},
		{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", SummaryIndex: 0, Text: "chain"},
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{
			ID: "rs_1", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}},
		}},
		{Kind: ir.EventTextDelta, Text: "after"},
		{Kind: ir.EventFinish},
	}
	resp, err := collectStreamResponse(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 3 {
		t.Fatalf("content = %+v", resp.Content)
	}
	if resp.Content[0].Type != ir.BlockText || resp.Content[0].Text != "before" {
		t.Fatalf("block 0 = %+v", resp.Content[0])
	}
	if resp.Content[1].Type != ir.BlockReasoning || resp.Content[1].Text != "chain" {
		t.Fatalf("block 1 = %+v", resp.Content[1])
	}
	if resp.Content[2].Type != ir.BlockText || resp.Content[2].Text != "after" {
		t.Fatalf("block 2 = %+v", resp.Content[2])
	}
}

func TestCollectOpaqueOnlyReasoningNotAnthropicThinking(t *testing.T) {
	enc := ""
	events := []ir.StreamEvent{
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_opaque", Reasoning: &ir.ResponsesReasoning{
			ID: "rs_opaque", Status: "completed", EncryptedContent: &enc, Summary: []ir.ReasoningSummaryPart{},
		}},
		{Kind: ir.EventFinish},
	}
	resp, err := collectStreamResponse(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != ir.BlockReasoning || resp.Content[0].Text != "" {
		t.Fatalf("content = %+v", resp.Content)
	}
	out, err := anthropicCodec.encodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"type":"thinking"`) || strings.Contains(string(out), "encrypted") {
		t.Fatalf("opaque-only reasoning became Anthropic thinking: %s", out)
	}
	if !responseDropsResponsesOpaque(resp.Content) {
		t.Fatal("opaque-only reasoning not recognized for omission")
	}
}

func TestEstimateUsageIgnoresResponsesOpaqueBytes(t *testing.T) {
	enc := strings.Repeat("z", 4096)
	resp := &ir.Response{Content: []ir.ContentBlock{{Type: ir.BlockReasoning, Text: "chain", ResponsesReasoning: &ir.ResponsesReasoning{
		ID: "rs_1", EncryptedContent: &enc, Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}},
	}}}}
	in, out := estimateUsage(nil, resp)
	if in != 0 || out != charsToTokens(len("chain")) {
		t.Fatalf("estimate = %d,%d", in, out)
	}
}

func TestCollectStructuredReasoningLimits(t *testing.T) {
	summary := []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}}
	done := ir.StreamEvent{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{
		ID: "rs_1", Summary: summary,
	}}
	events := []ir.StreamEvent{
		{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", SummaryIndex: 0, Text: "chain"},
		done,
		{Kind: ir.EventFinish},
	}
	base := int64(len(ir.NewID("resp_")) + len("m"))
	// The delta charges the block overhead, the ID key, one live part, and its
	// text. The done snapshot charges a second, distinct part allocation for
	// the retained clone: each summary part costs one part overhead plus its
	// type and text bytes. The returned projection charges the readable copy
	// once more.
	limit := base + int64(len("chain")) + collectedBlockOverhead + collectedReasoningPartOverhead +
		int64(len("rs_1")+collectedReasoningKeyOverhead) +
		int64(len("rs_1")+collectedReasoningPartOverhead+len("summary_text")+len("chain")) +
		int64(len("chain"))
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil, limit, 10); err != nil {
		t.Fatalf("exact limit failed: %v", err)
	}
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil, limit-1, 10); !errors.Is(err, errCollectedStreamResponseTooLarge) {
		t.Fatalf("over limit error = %v", err)
	}
	repeated := append([]ir.StreamEvent{}, events[:2]...)
	for i := 0; i < 50; i++ {
		repeated = append(repeated, done)
	}
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(repeated), nil, "m", nil, limit, 10); err != nil {
		t.Fatalf("repeated identical snapshots double-charged: %v", err)
	}
}

func TestCollectStructuredReasoningItemCap(t *testing.T) {
	events := make([]ir.StreamEvent, 0, maxCollectedStreamReasoning+1)
	for i := 0; i <= maxCollectedStreamReasoning; i++ {
		id := fmt.Sprintf("rs_%d", i)
		events = append(events, ir.StreamEvent{Kind: ir.EventReasoningItemDone, ReasoningID: id, Reasoning: &ir.ResponsesReasoning{ID: id}})
	}
	_, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil, 1<<30, 10)
	if !errors.Is(err, errCollectedStreamResponseTooLarge) {
		t.Fatalf("error = %v", err)
	}
}

// TestCollectFinalOnlySnapshotPartOverhead proves a final-only structured
// snapshot charges one slice/struct slot per summary part before the clone.
// Many empty parts with empty type and text previously charged nothing beyond
// the item metadata.
func TestCollectFinalOnlySnapshotPartOverhead(t *testing.T) {
	const parts = 1024
	summary := make([]ir.ReasoningSummaryPart, parts)
	events := []ir.StreamEvent{
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{ID: "rs_1", Summary: summary}},
		{Kind: ir.EventFinish},
	}
	base := int64(len(ir.NewID("resp_")) + len("m"))
	// Block overhead, the streamed ID key, and the retained clone: item ID plus
	// one part overhead for every summary slot.
	exact := base + collectedBlockOverhead + int64(len("rs_1")+collectedReasoningKeyOverhead) +
		int64(len("rs_1")+parts*collectedReasoningPartOverhead)
	resp, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil, exact, 10)
	if err != nil {
		t.Fatalf("exact limit failed: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].ResponsesReasoning == nil || len(resp.Content[0].ResponsesReasoning.Summary) != parts {
		t.Fatalf("collected snapshot shape = %+v", resp.Content)
	}
	if resp.Content[0].Text != "" {
		t.Fatalf("empty parts produced readable text: %q", resp.Content[0].Text)
	}
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil, exact-1, 10); !errors.Is(err, errCollectedStreamResponseTooLarge) {
		t.Fatalf("over limit error = %v", err)
	}
}

// TestCollectFinalOnlySnapshotRepeatFree proves an identical repeated
// final-only snapshot is deduplicated and never charges its parts twice.
func TestCollectFinalOnlySnapshotRepeatFree(t *testing.T) {
	done := ir.StreamEvent{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{
		ID: "rs_1", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}},
	}}
	events := []ir.StreamEvent{done, done, done, {Kind: ir.EventFinish}}
	base := int64(len(ir.NewID("resp_")) + len("m"))
	exact := base + collectedBlockOverhead + int64(len("rs_1")+collectedReasoningKeyOverhead) +
		int64(len("rs_1")+collectedReasoningPartOverhead+len("summary_text")+len("chain")) +
		int64(len("chain"))
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil, exact, 10); err != nil {
		t.Fatalf("repeated identical snapshots double-charged: %v", err)
	}
}

// TestCollectFinalOnlySnapshotPartCap rejects one terminal snapshot whose
// summary array exceeds the existing 1024 part cap even when the byte budget
// would otherwise allow it.
func TestCollectFinalOnlySnapshotPartCap(t *testing.T) {
	summary := make([]ir.ReasoningSummaryPart, maxCollectedStreamReasoningParts+1)
	events := []ir.StreamEvent{
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{ID: "rs_1", Summary: summary}},
		{Kind: ir.EventFinish},
	}
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil, 1<<30, 10); !errors.Is(err, errCollectedStreamResponseTooLarge) {
		t.Fatalf("error = %v, want response-too-large", err)
	}
}

// TestCollectReasoningPartGlobalCap aligns the collector live-part cap with
// the decoder global cap: multiple items that are each under the per-item cap
// must still stop at 1024 retained parts in total.
func TestCollectReasoningPartGlobalCap(t *testing.T) {
	events := make([]ir.StreamEvent, 0, 1201)
	for i := 0; i < 600; i++ {
		events = append(events, ir.StreamEvent{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", SummaryIndex: i, Text: "x"})
	}
	for i := 0; i < 600; i++ {
		events = append(events, ir.StreamEvent{Kind: ir.EventReasoningDelta, ReasoningID: "rs_2", SummaryIndex: i, Text: "x"})
	}
	events = append(events, ir.StreamEvent{Kind: ir.EventFinish})
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil, 1<<30, 10); !errors.Is(err, errCollectedStreamResponseTooLarge) {
		t.Fatalf("error = %v, want response-too-large", err)
	}
}

// TestCollectStructuredSnapshotLateGrowthCharged proves a second snapshot that
// adds an empty part charges that part's overhead and never shrinks the prior
// shape for free.
func TestCollectStructuredSnapshotLateGrowthCharged(t *testing.T) {
	delta := ir.StreamEvent{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", SummaryIndex: 0, Text: "a"}
	done1 := ir.StreamEvent{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{
		ID: "rs_1", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "a"}},
	}}
	done2 := ir.StreamEvent{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{
		ID: "rs_1", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "a"}, {Type: "summary_text", Text: "b"}},
	}}
	events := []ir.StreamEvent{delta, done1, done2, {Kind: ir.EventFinish}}
	base := int64(len(ir.NewID("resp_")) + len("m"))
	exact := base + collectedBlockOverhead +
		int64(len("rs_1")+collectedReasoningKeyOverhead) +
		int64(collectedReasoningPartOverhead+len("a")) +
		int64(len("rs_1")+collectedReasoningPartOverhead+len("summary_text")+len("a")) +
		int64(collectedReasoningPartOverhead+len("summary_text")+len("b")) +
		int64(len("a")+len("b"))
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil, exact, 10); err != nil {
		t.Fatalf("exact limit failed: %v", err)
	}
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil, exact-1, 10); !errors.Is(err, errCollectedStreamResponseTooLarge) {
		t.Fatalf("over limit error = %v", err)
	}
}

// TestCollectFinalOnlySnapshotPartOverheadRealDecoder runs the real Responses
// decoder through the collector so the clone part charge is enforced on the
// production route, not only a synthetic codec.
func TestCollectFinalOnlySnapshotPartOverheadRealDecoder(t *testing.T) {
	const parts = 100
	summary := make([]string, parts)
	for i := range summary {
		summary[i] = `{"type":"","text":""}`
	}
	body := responsesSSEData(
		`{"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","output":[{"id":"rs_f","type":"reasoning","status":"completed","summary":[`+strings.Join(summary, ",")+`]}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	)
	base := int64(len("r1") + len("m"))
	// The decoder fills the default summary_text type and keeps the completed
	// status, so the clone charges those bytes on every part. base uses the
	// replacement response id because replace subtracts a shorter id.
	exact := base + collectedBlockOverhead + int64(len("rs_f")+collectedReasoningKeyOverhead) +
		int64(len("rs_f")+len("completed")+parts*(collectedReasoningPartOverhead+len("summary_text")))
	if _, err := collectStreamResponseWithLimits(strings.NewReader(body), responsesCodec, nil, "m", nil, exact, 10); err != nil {
		t.Fatalf("exact limit failed: %v", err)
	}
	if _, err := collectStreamResponseWithLimits(strings.NewReader(body), responsesCodec, nil, "m", nil, exact-1, 10); !errors.Is(err, errCollectedStreamResponseTooLarge) {
		t.Fatalf("over limit error = %v", err)
	}
}

func codexReasoningSSEUpstream(t *testing.T, body *[]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body != nil {
			raw, _ := readAllBody(r)
			*body = raw
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n"))
		_, _ = w.Write([]byte("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"rs_up\",\"type\":\"reasoning\",\"status\":\"in_progress\",\"summary\":[]}}\n\n"))
		_, _ = w.Write([]byte("event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_up\",\"output_index\":0,\"summary_index\":0,\"delta\":\"up\"}\n\n"))
		_, _ = w.Write([]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_up\",\"type\":\"reasoning\",\"status\":\"completed\",\"encrypted_content\":\"cipher-up\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"up\"}]}}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[{\"id\":\"rs_up\",\"type\":\"reasoning\",\"status\":\"completed\",\"encrypted_content\":\"cipher-up\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"up\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResponsesOwnedStateStreamingPreserved(t *testing.T) {
	var got []byte
	logs := captureProxyDebugLogs(t)
	up := codexReasoningSSEUpstream(t, &got)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{{protocol: domain.ProtocolOpenAICodex, url: up.URL, model: "gpt-5-codex"}})
	streamReq := `{"model":"default","stream":true,"input":[{"type":"reasoning","id":"rs_client","encrypted_content":"cipher-client","summary":[{"type":"summary_text","text":"prior"}]},{"type":"message","role":"user","content":"next"}]}`
	resp, out := post(t, base+"/v1/responses", token, streamReq)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if !strings.Contains(string(got), `"encrypted_content":"cipher-client"`) {
		t.Fatalf("upstream lost state: %s", got)
	}
	for _, want := range []string{
		`"type":"response.reasoning_summary_text.delta"`,
		`"type":"response.output_item.done"`,
		`"encrypted_content":"cipher-up"`,
		`"type":"response.completed"`,
		`"status":"completed"`,
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("client stream missing %s: %s", want, out)
		}
	}
	if strings.Contains(logs.String(), "msg=thinking_opaque_omitted") {
		t.Fatalf("preserving ingress noted an omission: %s", logs.String())
	}
}

func TestResponsesIngressStreamPassthroughPreservesState(t *testing.T) {
	upstream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_up\",\"type\":\"reasoning\",\"status\":\"completed\",\"encrypted_content\":\"cipher-up\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"up\"}]}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstream))
	}))
	t.Cleanup(up.Close)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{{protocol: domain.ProtocolOpenAIResponses, url: up.URL, model: "m"}})
	resp, out := post(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if string(out) != upstream {
		t.Fatalf("passthrough changed bytes:\n%s", out)
	}
}

func captureProxyDebugLogs(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestResponsesOpaqueOmissionDiagnosticOnceUnary(t *testing.T) {
	upstreamBody := `{"id":"resp_up","model":"m","status":"completed","output":[` +
		`{"id":"rs_1","type":"reasoning","status":"completed","encrypted_content":"c1","summary":[{"type":"summary_text","text":"a"}]},` +
		`{"id":"rs_2","type":"reasoning","status":"completed","encrypted_content":"c2","summary":[{"type":"summary_text","text":"b"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	t.Cleanup(up.Close)
	logs := captureProxyDebugLogs(t)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{{protocol: domain.ProtocolOpenAIResponses, url: up.URL, model: "m"}})
	resp, out := post(t, base+"/v1/chat/completions", token, `{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if strings.Contains(string(out), "encrypted") || strings.Contains(string(out), "rs_1") {
		t.Fatalf("opaque state leaked into chat response: %s", out)
	}
	if got := strings.Count(logs.String(), "msg=thinking_opaque_omitted"); got != 1 {
		t.Fatalf("diagnostics = %d logs=%s", got, logs.String())
	}
	if strings.Contains(logs.String(), "c1") || strings.Contains(logs.String(), "c2") {
		t.Fatalf("log leaked opaque bytes: %s", logs.String())
	}
}

func TestResponsesOpaqueOmissionDiagnosticOnceStream(t *testing.T) {
	upstream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"encrypted_content\":\"c1\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"a\"}]}}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"id\":\"rs_2\",\"type\":\"reasoning\",\"status\":\"completed\",\"encrypted_content\":\"c2\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"b\"}]}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstream))
	}))
	t.Cleanup(up.Close)
	logs := captureProxyDebugLogs(t)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{{protocol: domain.ProtocolOpenAIResponses, url: up.URL, model: "m"}})
	resp, out := post(t, base+"/v1/chat/completions", token, `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if strings.Contains(string(out), "encrypted") || strings.Contains(string(out), "rs_1") {
		t.Fatalf("opaque state leaked into chat stream: %s", out)
	}
	if got := strings.Count(logs.String(), "msg=thinking_opaque_omitted"); got != 1 {
		t.Fatalf("diagnostics = %d logs=%s", got, logs.String())
	}
	if strings.Contains(logs.String(), "c1") || strings.Contains(logs.String(), "c2") {
		t.Fatalf("log leaked opaque bytes: %s", logs.String())
	}
}

func TestNoteOpaqueOmissionOnceAcrossFamilies(t *testing.T) {
	logs := captureProxyDebugLogs(t)
	res := &reqResult{}
	logger := slog.Default()
	noteAnthropicOmission(res, logger, "oai-chat")
	noteResponsesOmission(res, logger, "oai-chat")
	noteAnthropicOmission(res, logger, "anth-msg")
	noteResponsesOmission(res, logger, "oai-chat")
	if got := strings.Count(logs.String(), "msg=thinking_opaque_omitted"); got != 1 {
		t.Fatalf("diagnostics = %d logs=%s", got, logs.String())
	}
}

func TestPreservesResponsesReasoningCodecIDs(t *testing.T) {
	for _, id := range []string{"oai-responses", "oai-codex", "opencode-responses"} {
		if !preservesResponsesReasoning(id) {
			t.Fatalf("codec %q must preserve provider-owned Responses reasoning", id)
		}
	}
	for _, id := range []string{"", "oai-chat", "opencode-chat", "anth-msg", "opencode-messages", "claude-code"} {
		if preservesResponsesReasoning(id) {
			t.Fatalf("codec %q must not preserve provider-owned Responses reasoning", id)
		}
	}
}

func TestCollectStructuredReasoningIndexAliasBinding(t *testing.T) {
	events := []ir.StreamEvent{
		{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 3, SummaryIndex: 0, Text: "chain"},
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", ReasoningIndexSet: true, ReasoningIndex: 3, Reasoning: &ir.ResponsesReasoning{
			ID: "rs_1", Status: "completed", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}},
		}},
		{Kind: ir.EventFinish},
	}
	resp, err := collectStreamResponse(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("index-only delta and id done duplicated: %+v", resp.Content)
	}
	block := resp.Content[0]
	if block.Text != "chain" || block.ResponsesReasoning == nil || block.ResponsesReasoning.ID != "rs_1" {
		t.Fatalf("block = %+v", block)
	}
}

func TestCollectStructuredReasoningIdentityConflicts(t *testing.T) {
	summary := []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "a"}}
	cases := []struct {
		name   string
		events []ir.StreamEvent
	}{
		{
			name: "index conflict",
			events: []ir.StreamEvent{
				{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", ReasoningIndexSet: true, ReasoningIndex: 3, Text: "a"},
				{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", ReasoningIndexSet: true, ReasoningIndex: 4, Reasoning: &ir.ResponsesReasoning{ID: "rs_1", Summary: summary}},
			},
		},
		{
			name: "id conflict",
			events: []ir.StreamEvent{
				{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", ReasoningIndexSet: true, ReasoningIndex: 3, Text: "a"},
				{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_2", ReasoningIndexSet: true, ReasoningIndex: 3, Reasoning: &ir.ResponsesReasoning{ID: "rs_2", Summary: summary}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := collectStreamResponse(strings.NewReader(""), reasoningEventsCodec(tc.events), nil, "m", nil)
			sf, ok := ir.AsStreamFailure(err)
			if !ok || !strings.HasPrefix(sf.Message, "conflicting reasoning") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestCollectStructuredReasoningUnidentifiedItemsDistinct(t *testing.T) {
	done := func(text string) ir.StreamEvent {
		return ir.StreamEvent{Kind: ir.EventReasoningItemDone, Reasoning: &ir.ResponsesReasoning{
			Status:  "completed",
			Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: text}},
		}}
	}
	events := []ir.StreamEvent{done("first"), done("second"), {Kind: ir.EventFinish}}
	resp, err := collectStreamResponse(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("unidentified items collapsed: %+v", resp.Content)
	}
	if resp.Content[0].Text != "first" || resp.Content[1].Text != "second" {
		t.Fatalf("texts = %+v", resp.Content)
	}
}

func TestCollectTextAndReasoningBlocksCharged(t *testing.T) {
	base := int64(len(ir.NewID("resp_")) + len("m"))
	events := []ir.StreamEvent{
		{Kind: ir.EventTextDelta, Text: "x"},
		{Kind: ir.EventReasoningDelta, Text: "y"},
		{Kind: ir.EventTextDelta, Text: "x"},
		{Kind: ir.EventReasoningDelta, Text: "y"},
		{Kind: ir.EventFinish},
	}
	// Two legacy reasoning blocks each retain one projected byte in addition to
	// the byte charged while the fragment was appended.
	limit := base + 4*(collectedBlockOverhead+1) + 2
	resp, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil, limit, 10)
	if err != nil {
		t.Fatalf("exact block limit failed: %v", err)
	}
	if len(resp.Content) != 4 {
		t.Fatalf("alternating blocks collapsed: %+v", resp.Content)
	}
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil, limit-1, 10); !errors.Is(err, errCollectedStreamResponseTooLarge) {
		t.Fatalf("over limit error = %v", err)
	}
}

func TestCollectTextAndReasoningBlockCap(t *testing.T) {
	events := make([]ir.StreamEvent, 0, 2*maxCollectedStreamBlocks+1)
	for i := 0; i <= maxCollectedStreamBlocks; i++ {
		events = append(events,
			ir.StreamEvent{Kind: ir.EventTextDelta, Text: "x"},
			ir.StreamEvent{Kind: ir.EventReasoningDelta, Text: "y"},
		)
	}
	_, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil, 1<<30, 10)
	if !errors.Is(err, errCollectedStreamResponseTooLarge) {
		t.Fatalf("error = %v, want response-too-large", err)
	}
}

func TestCollectFragmentAccumulationBounded(t *testing.T) {
	const n = 5000
	var events []ir.StreamEvent
	for i := 0; i < n; i++ {
		events = append(events, ir.StreamEvent{Kind: ir.EventTextDelta, Text: "z"})
	}
	for i := 0; i < n; i++ {
		events = append(events, ir.StreamEvent{Kind: ir.EventReasoningDelta, Text: "q"})
	}
	for i := 0; i < n; i++ {
		events = append(events, ir.StreamEvent{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", Text: "r"})
	}
	events = append(events, ir.StreamEvent{Kind: ir.EventFinish})
	// Three blocks: one text, one legacy reasoning, one structured reasoning.
	// The structured item allocates one summary part, charged once before it
	// grows with the fragment bytes. Both reasoning blocks retain a separate
	// readable projection.
	base := int64(len(ir.NewID("resp_")) + len("m"))
	limit := base + 3*collectedBlockOverhead + collectedReasoningPartOverhead +
		int64(len("rs_1")+collectedReasoningKeyOverhead) + int64(3*n) + int64(2*n)
	resp, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil, limit, 10)
	if err != nil {
		t.Fatalf("exact fragment limit failed: %v", err)
	}
	if len(resp.Content) != 3 || len(resp.Content[0].Text) != n || len(resp.Content[1].Text) != n || len(resp.Content[2].Text) != n {
		t.Fatalf("fragment content = %d blocks", len(resp.Content))
	}
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil, limit-1, 10); !errors.Is(err, errCollectedStreamResponseTooLarge) {
		t.Fatalf("over limit error = %v", err)
	}
}

func TestCollectEmptyTextAndLegacyReasoningCreateNoBlocks(t *testing.T) {
	events := []ir.StreamEvent{
		{Kind: ir.EventTextDelta},
		{Kind: ir.EventReasoningDelta},
		{Kind: ir.EventTextDelta},
		{Kind: ir.EventFinish},
	}
	resp, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil, 1<<20, 10)
	if err != nil {
		t.Fatalf("empty events charged a block: %v", err)
	}
	if len(resp.Content) != 0 {
		t.Fatalf("empty events created blocks: %+v", resp.Content)
	}
}

func TestTranslatedSinkProjectsResponsesItemDoneIndependently(t *testing.T) {
	enc := "cipher"
	empty := ""
	cases := []struct {
		name      string
		ingressID string
		reasoning *ir.ResponsesReasoning
		project   bool
		visible   bool
	}{
		{"anthropic encrypted", "anth-msg", &ir.ResponsesReasoning{ID: "rs_1", EncryptedContent: &enc, Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "read"}}}, true, false},
		{"anthropic id only", "anth-msg", &ir.ResponsesReasoning{ID: "rs_1"}, true, false},
		{"anthropic empty encrypted", "anth-msg", &ir.ResponsesReasoning{EncryptedContent: &empty}, true, false},
		{"chat id only", "oai-chat", &ir.ResponsesReasoning{ID: "rs_1"}, true, false},
		{"responses preserved", "oai-responses", &ir.ResponsesReasoning{ID: "rs_1", EncryptedContent: &enc}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &translatedSink{w: httptest.NewRecorder(), res: &reqResult{}, ingressID: tc.ingressID}
			ev := ir.StreamEvent{Kind: ir.EventReasoningItemDone, Reasoning: tc.reasoning}
			if got := sink.projectAway(ev); got != tc.project {
				t.Fatalf("projectAway = %v want %v", got, tc.project)
			}
			if got := sink.visible(ev); got != tc.visible {
				t.Fatalf("visible = %v want %v", got, tc.visible)
			}
		})
	}
}

func TestTranslatedSinkAnthropicDropsOpaqueDoneWithoutRetention(t *testing.T) {
	logs := captureProxyDebugLogs(t)
	enc := "cipher-secret"
	sink := &translatedSink{w: httptest.NewRecorder(), res: &reqResult{}, ingressID: "anth-msg", logger: slog.Default()}
	err := sink.handle(ir.StreamEvent{Kind: ir.EventReasoningItemDone, Reasoning: &ir.ResponsesReasoning{
		ID: "rs_1", EncryptedContent: &enc, Summary: []ir.ReasoningSummaryPart{},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.pending) != 0 {
		t.Fatalf("opaque Responses snapshot retained in pending: %+v", sink.pending)
	}
	if !sink.res.opaqueOmitted {
		t.Fatal("omission not recorded")
	}
	if !strings.Contains(logs.String(), "msg=thinking_opaque_omitted") {
		t.Fatalf("omission missing: %s", logs.String())
	}
	if strings.Contains(logs.String(), "cipher-secret") {
		t.Fatalf("log leaked opaque bytes: %s", logs.String())
	}
}

const anthropicReasoningSSE = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n" +
	"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"encrypted_content\":\"c1\",\"summary\":[]}}\n\n" +
	"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"id\":\"rs_2\",\"type\":\"reasoning\",\"status\":\"completed\",\"encrypted_content\":\"\",\"summary\":[]}}\n\n" +
	"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":2,\"item\":{\"id\":\"rs_3\",\"type\":\"reasoning\",\"status\":\"completed\",\"summary\":[]}}\n\n" +
	"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":3,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}}\n\n" +
	"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":3,\"delta\":\"ok\"}\n\n" +
	"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"

func TestAnthropicIngressOpaqueResponsesReasoningUnaryNoThinking(t *testing.T) {
	upstreamBody := `{"id":"resp_up","model":"m","status":"completed","output":[` +
		`{"id":"rs_1","type":"reasoning","status":"completed","encrypted_content":"c1","summary":[]},` +
		`{"id":"rs_2","type":"reasoning","status":"completed","encrypted_content":"","summary":[]},` +
		`{"id":"rs_3","type":"reasoning","status":"completed","summary":[]},` +
		`{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],` +
		`"usage":{"input_tokens":1,"output_tokens":1}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	t.Cleanup(up.Close)
	logs := captureProxyDebugLogs(t)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{{protocol: domain.ProtocolOpenAIResponses, url: up.URL, model: "m"}})
	resp, out := post(t, base+"/v1/messages", token, `{"model":"default","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if strings.Contains(string(out), "thinking") || strings.Contains(string(out), "c1") || strings.Contains(string(out), "rs_1") {
		t.Fatalf("opaque-only Responses state surfaced in Messages response: %s", out)
	}
	if !strings.Contains(string(out), `"text":"ok"`) {
		t.Fatalf("message text lost: %s", out)
	}
	if got := strings.Count(logs.String(), "msg=thinking_opaque_omitted"); got != 1 {
		t.Fatalf("diagnostics = %d logs=%s", got, logs.String())
	}
}

func TestAnthropicIngressOpaqueResponsesReasoningStreamNoThinking(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicReasoningSSE))
	}))
	t.Cleanup(up.Close)
	logs := captureProxyDebugLogs(t)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{{protocol: domain.ProtocolOpenAIResponses, url: up.URL, model: "m"}})
	resp, out := post(t, base+"/v1/messages", token, `{"model":"default","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if strings.Contains(string(out), `"type":"thinking"`) {
		t.Fatalf("opaque-only Responses state became empty Anthropic thinking: %s", out)
	}
	if strings.Contains(string(out), "c1") || strings.Contains(string(out), "rs_1") {
		t.Fatalf("opaque bytes leaked: %s", out)
	}
	if !strings.Contains(string(out), `"text":"ok"`) {
		t.Fatalf("message text lost: %s", out)
	}
	if got := strings.Count(logs.String(), "msg=thinking_opaque_omitted"); got != 1 {
		t.Fatalf("diagnostics = %d logs=%s", got, logs.String())
	}
}

const responsesReasoningSuccessSSE = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
	"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_up\",\"type\":\"reasoning\",\"status\":\"completed\",\"encrypted_content\":\"cipher-up\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"up\"}]}}\n\n" +
	"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":1,\"delta\":\"ok\"}\n\n" +
	"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"

// codexPrecommitSSEUpstream replies with a reasoning stream whose second item
// carries a malformed supported field. The first item streams a full opaque
// snapshot, so a preserved reply commits before the failure.
func codexPrecommitSSEUpstream(t *testing.T, body *[]byte, visibleFirst bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body != nil {
			raw, _ := readAllBody(r)
			*body = raw
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n"))
		if visibleFirst {
			_, _ = w.Write([]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_vis\",\"type\":\"reasoning\",\"status\":\"completed\",\"encrypted_content\":\"cipher-visible\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"up\"}]}}\n\n"))
		}
		_, _ = w.Write([]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"id\":\"rs_bad\",\"type\":\"reasoning\",\"encrypted_content\":123,\"summary\":[]}}\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCodexTranslatedPrecommitFailureFailsOver replaces the earlier passthrough
// proof with a real translated decoder failure. The ingress is Responses and the
// backend is oai-codex, so the codec IDs differ and the SSE decode runs. A
// request without incoming owned history may fail over before any byte is
// visible; the offending frames must never reach the client.
func TestCodexTranslatedPrecommitFailureFailsOver(t *testing.T) {
	var badBody []byte
	badUpstream := codexPrecommitSSEUpstream(t, &badBody, false)
	var badHits atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits.Add(1)
		badUpstream.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(bad.Close)
	var goodHits atomic.Int64
	healthy := codexReasoningSSEUpstream(t, nil)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHits.Add(1)
		healthy.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(good.Close)
	// The ingress is oai-responses and the backend is oai-codex, so the IDs
	// differ and the translated SSE decode actually runs.
	if responsesCodec.id != "oai-responses" {
		t.Fatalf("ingress codec = %q, want oai-responses", responsesCodec.id)
	}
	if backend := backendCodecFor(domain.ProtocolOpenAICodex, bad.URL, "gpt-5-codex"); backend.id != "oai-codex" || backend.id == responsesCodec.id {
		t.Fatalf("backend codec = %q, want oai-codex distinct from %q", backend.id, responsesCodec.id)
	}
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{
		{protocol: domain.ProtocolOpenAICodex, url: bad.URL, model: "gpt-5-codex"},
		{protocol: domain.ProtocolOpenAICodex, url: good.URL, model: "gpt-5-codex"},
	})
	resp, out := post(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if badHits.Load() != 1 {
		t.Fatalf("bad target hits = %d, want 1", badHits.Load())
	}
	if goodHits.Load() != 1 {
		t.Fatalf("failover did not reach the healthy target: %d", goodHits.Load())
	}
	if !strings.Contains(string(badBody), `"model":"gpt-5-codex"`) || !strings.Contains(string(badBody), `"stream":true`) {
		t.Fatalf("bad target did not receive the translated codex body: %s", badBody)
	}
	// No opaque Done and no response bytes from the bad target may reach the
	// client before the failover.
	if strings.Contains(string(out), "rs_bad") || strings.Contains(string(out), "123") {
		t.Fatalf("malformed frame leaked to the client: %s", out)
	}
	if got := strings.Count(string(out), "event: response.created"); got != 1 {
		t.Fatalf("client saw %d response.created events, want only the healthy one: %s", got, out)
	}
	if !strings.Contains(string(out), `"encrypted_content":"cipher-up"`) {
		t.Fatalf("healthy translated response lost state: %s", out)
	}
}

// TestCodexTranslatedPostcommitFailureStopsWithStreamError keeps the visible
// preserved opaque prefix, terminates with an ingress error frame, never emits a
// successful completed event, and never contacts the second target.
func TestCodexTranslatedPostcommitFailureStopsWithStreamError(t *testing.T) {
	logs := captureProxyDebugLogs(t)
	var badBody []byte
	badUpstream := codexPrecommitSSEUpstream(t, &badBody, true)
	var badHits atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits.Add(1)
		badUpstream.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(bad.Close)
	var goodHits atomic.Int64
	good := responsesJSONUpstream(t, &goodHits, nil)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{
		{protocol: domain.ProtocolOpenAICodex, url: bad.URL, model: "gpt-5-codex"},
		{protocol: domain.ProtocolOpenAICodex, url: good.URL, model: "gpt-5-codex"},
	})
	resp, out := post(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if badHits.Load() != 1 {
		t.Fatalf("bad target hits = %d, want 1", badHits.Load())
	}
	if !strings.Contains(string(out), `"encrypted_content":"cipher-visible"`) {
		t.Fatalf("visible opaque prefix lost: %s", out)
	}
	if goodHits.Load() != 0 {
		t.Fatalf("failover after commitment: %d", goodHits.Load())
	}
	if strings.Contains(string(out), "response.completed") {
		t.Fatalf("broken translated stream ended as completed: %s", out)
	}
	if strings.Contains(string(out), "rs_bad") || strings.Contains(string(out), "123") {
		t.Fatalf("malformed frame reached the client: %s", out)
	}
	if strings.Contains(logs.String(), "cipher-visible") || strings.Contains(logs.String(), "123") {
		t.Fatalf("log leaked opaque or malformed bytes: %s", logs.String())
	}
}

// codexRetentionSSEUpstream streams one reasoning item as `chunks` summary
// deltas of chunkBytes each, then the matching opaque snapshot and a completion.
// Each delta is a valid SSE event under the 8 MiB per-event parser cap; the
// snapshot text repeats the accumulated delta text so the stream is valid when
// it fits the retention budget.
func codexRetentionSSEUpstream(t *testing.T, chunkBytes, chunks int) *httptest.Server {
	t.Helper()
	filler := strings.Repeat("r", chunkBytes)
	accumulated := strings.Repeat(filler, chunks)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		write := func(s string) bool {
			if _, err := io.WriteString(w, s); err != nil {
				return false
			}
			if flusher != nil {
				flusher.Flush()
			}
			return true
		}
		if !write("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n") {
			return
		}
		for i := 0; i < chunks; i++ {
			payload := fmt.Sprintf("event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_big\",\"output_index\":0,\"summary_index\":0,\"delta\":%q}\n\n", filler)
			if !write(payload) {
				return
			}
		}
		item := fmt.Sprintf(`{"id":"rs_big","type":"reasoning","status":"completed","encrypted_content":"cipher-big","summary":[{"type":"summary_text","text":%q}]}`, accumulated)
		if !write("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + item + "}\n\n") {
			return
		}
		_ = write("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCodexTranslatedRetentionLimitBelowBudgetSucceeds is the control for the
// retention-limit failover test: the same stream shape below the 64 MiB budget
// completes at the first target, so the failover in the sibling test is caused
// by the retention limit alone and not by an invalid stream.
func TestCodexTranslatedRetentionLimitBelowBudgetSucceeds(t *testing.T) {
	var badHits atomic.Int64
	badUpstream := codexRetentionSSEUpstream(t, 1<<20, 2)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits.Add(1)
		badUpstream.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(bad.Close)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{{protocol: domain.ProtocolOpenAICodex, url: bad.URL, model: "gpt-5-codex"}})
	resp, out := post(t, base+"/v1/responses", token, `{"model":"default","input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if badHits.Load() != 1 {
		t.Fatalf("target hits = %d, want 1", badHits.Load())
	}
	if !strings.Contains(string(out), `"encrypted_content":"cipher-big"`) {
		t.Fatalf("below-budget stream lost its opaque snapshot: %s", out)
	}
}

// TestCodexTranslatedRetentionLimitPrecommitFailsOver exercises the real
// production reasoning retention limit (64 MiB) with multiple SSE events that
// each stay under the 8 MiB per-event parser cap. The client is unary to a
// stream-only Codex target, so the whole stream is collected before any byte is
// committed; the limit error is therefore pre-commit and the request may fail
// over. The bad stream is otherwise valid (see the below-budget control), so the
// only failure source is the retention limit, and the opaque Done that follows
// the oversized deltas is never emitted.
func TestCodexTranslatedRetentionLimitPrecommitFailsOver(t *testing.T) {
	const chunkBytes = 6 << 20 // valid 6 MiB summary delta, under the 8 MiB event cap
	const chunks = 12          // 72 MiB total, past the 64 MiB retention budget
	var badHits atomic.Int64
	badUpstream := codexRetentionSSEUpstream(t, chunkBytes, chunks)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits.Add(1)
		badUpstream.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(bad.Close)
	var goodHits atomic.Int64
	healthy := codexReasoningSSEUpstream(t, nil)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHits.Add(1)
		healthy.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(good.Close)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{
		{protocol: domain.ProtocolOpenAICodex, url: bad.URL, model: "gpt-5-codex"},
		{protocol: domain.ProtocolOpenAICodex, url: good.URL, model: "gpt-5-codex"},
	})
	resp, out := post(t, base+"/v1/responses", token, `{"model":"default","input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if badHits.Load() != 1 {
		t.Fatalf("bad target hits = %d, want 1", badHits.Load())
	}
	if goodHits.Load() != 1 {
		t.Fatalf("retention-limit failure did not fail over: %d", goodHits.Load())
	}
	if strings.Contains(string(out), "cipher-big") || strings.Contains(string(out), "rs_big") {
		t.Fatalf("opaque snapshot after the oversized deltas reached the client: %s", out)
	}
	if strings.Contains(string(out), strings.Repeat("r", 64)) {
		t.Fatal("oversized reasoning bytes leaked into the unary response")
	}
	if !strings.Contains(string(out), `"encrypted_content":"cipher-up"`) {
		t.Fatalf("healthy collected response lost state: %s", out)
	}
}

func TestResponsesStreamCommittedNoFailover(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"m\",\"status\":\"in_progress\",\"output\":[]}}\n\n"))
		_, _ = w.Write([]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"status\":\"completed\",\"encrypted_content\":\"cipher-visible\",\"summary\":[]}}\n\n"))
		_, _ = w.Write([]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"id\":\"rs_2\",\"type\":\"reasoning\",\"encrypted_content\":123,\"summary\":[]}}\n\n"))
	}))
	t.Cleanup(broken.Close)
	var goodHits atomic.Int64
	good := responsesJSONUpstream(t, &goodHits, nil)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{
		{protocol: domain.ProtocolOpenAIResponses, url: broken.URL, model: "m"},
		{protocol: domain.ProtocolOpenAIResponses, url: good.URL, model: "m"},
	})
	logs := captureProxyDebugLogs(t)
	resp, out := post(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), `"encrypted_content":"cipher-visible"`) {
		t.Fatalf("visible opaque prefix lost: %s", out)
	}
	if goodHits.Load() != 0 {
		t.Fatalf("failover after commitment: %d", goodHits.Load())
	}
	if strings.Contains(string(out), "response.completed") {
		t.Fatalf("broken stream ended as completed: %s", out)
	}
	if strings.Contains(logs.String(), "cipher-visible") {
		t.Fatalf("log leaked opaque bytes: %s", logs.String())
	}
}

func TestResponsesMalformedMetadataIngress400ZeroHitsPassthroughAndTranslated(t *testing.T) {
	malformedInputs := []string{
		`{"model":"default","input":[{"type":"reasoning","encrypted_content":123}]}`,
		`{"model":"default","input":[{"type":"reasoning","summary":[null]}]}`,
		`{"model":"default","input":[{"type":"reasoning","id":7}]}`,
		`{"model":"default","stream":true,"input":[{"type":"reasoning","encrypted_content":{"x":1}}]}`,
	}
	for _, targetProto := range []domain.Protocol{domain.ProtocolOpenAIResponses, domain.ProtocolOpenAI} {
		var hits atomic.Int64
		up := responsesJSONUpstream(t, &hits, nil)
		base, token, _, _ := setupResponsesRouting(t, []routingTarget{{protocol: targetProto, url: up.URL, model: "m"}})
		for _, body := range malformedInputs {
			resp, out := post(t, base+"/v1/responses", token, body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("target %v status=%d body=%s", targetProto, resp.StatusCode, out)
			}
			if hits.Load() != 0 {
				t.Fatalf("malformed ingress contacted upstream: %d", hits.Load())
			}
			if strings.Contains(string(out), "123") || strings.Contains(string(out), "[null]") || strings.Contains(string(out), `{"x":1}`) {
				t.Fatalf("error echoed payload: %s", out)
			}
		}
	}
}

func TestResponsesAmbiguityPreExistingPenaltyCreditUnchanged(t *testing.T) {
	a := responsesJSONUpstream(t, &atomic.Int64{}, nil)
	b := responsesJSONUpstream(t, &atomic.Int64{}, nil)
	base, token, px, ids := setupResponsesRouting(t, []routingTarget{
		{protocol: domain.ProtocolOpenAIResponses, url: a.URL, model: "model-a"},
		{protocol: domain.ProtocolOpenAIResponses, url: b.URL, model: "model-b"},
	})
	px.penalizeProvider(ids[0])
	before := backoffSkips(px, ids[0])
	resp, out := post(t, base+"/v1/responses", token, responsesStateRequestBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if got := backoffSkips(px, ids[0]); got != before {
		t.Fatalf("ambiguity changed penalized credit: %d -> %d", before, got)
	}
	if got := backoffSkips(px, ids[1]); got != 0 {
		t.Fatalf("ambiguity penalized compatible target: %d", got)
	}
}

func TestResponsesFailoverUsesTargetModelNotPrevious(t *testing.T) {
	var bodyA, bodyB []byte
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyA, _ = readAllBody(r)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	t.Cleanup(failing.Close)
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyB, _ = readAllBody(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(routingResponsesUpstreamBody))
	}))
	t.Cleanup(healthy.Close)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{
		{protocol: domain.ProtocolOpenAIResponses, url: failing.URL, model: "model-a"},
		{protocol: domain.ProtocolOpenAIResponses, url: healthy.URL, model: "model-b"},
	})
	resp, out := post(t, base+"/v1/responses", token, `{"model":"default","input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if !strings.Contains(string(bodyA), `"model":"model-a"`) {
		t.Fatalf("first attempt model = %s", bodyA)
	}
	if !strings.Contains(string(bodyB), `"model":"model-b"`) {
		t.Fatalf("retry reused previous model: %s", bodyB)
	}
	if strings.Contains(string(bodyB), "model-a") {
		t.Fatalf("retry body bled previous target: %s", bodyB)
	}
}

// responsesSSEData frames JSON events as data-only SSE records for the Responses
// decoder. The proxy collector tests use the real decoder, not a synthetic
// event source, so the regression covers the production path.
func responsesSSEData(events ...string) string {
	var b strings.Builder
	for _, ev := range events {
		b.WriteString("data: ")
		b.WriteString(ev)
		b.WriteString("\n\n")
	}
	return b.String()
}

func collectResponsesSSE(t *testing.T, body string) *ir.Response {
	t.Helper()
	resp, err := collectStreamResponse(strings.NewReader(body), responsesCodec, nil, "m", nil)
	if err != nil {
		t.Fatalf("collect real Responses stream: %v", err)
	}
	return resp
}

func responsesReasoningSummary(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, text := range parts {
		out = append(out, fmt.Sprintf(`{"type":"summary_text","text":%q}`, text))
	}
	return "[" + strings.Join(out, ",") + "]"
}

func responsesReasoningAdded(id string) string {
	return fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"item":{"id":%q,"type":"reasoning","status":"in_progress","summary":[]}}`, id)
}

func responsesReasoningDelta(id string, summaryIndex int, delta string) string {
	return fmt.Sprintf(`{"type":"response.reasoning_summary_text.delta","item_id":%q,"output_index":0,"summary_index":%d,"delta":%q}`, id, summaryIndex, delta)
}

func responsesReasoningDone(id string, summary string) string {
	return fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":{"id":%q,"type":"reasoning","status":"completed","summary":%s}}`, id, summary)
}

// TestCollectResponsesSummaryIndexOrderRealDecoder is the true regression for
// the forced stream-to-unary collector. A valid gap stream delivers a higher
// summary index before a recovered lower one; the retained readable projection
// must follow canonical part order, not arrival order.
func TestCollectResponsesSummaryIndexOrderRealDecoder(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string
		summary []string
		wantID  string
	}{
		{
			name: "high index then recovered zero",
			body: responsesSSEData(
				responsesReasoningAdded("rs_g"),
				responsesReasoningDelta("rs_g", 1, " tail"),
				responsesReasoningDone("rs_g", responsesReasoningSummary("head", " tail")),
			),
			want:    "head tail",
			summary: []string{"head", " tail"},
			wantID:  "rs_g",
		},
		{
			name: "mid gap zero to two then recovered one",
			body: responsesSSEData(
				responsesReasoningAdded("rs_mid"),
				responsesReasoningDelta("rs_mid", 0, "p0"),
				responsesReasoningDelta("rs_mid", 2, "p2"),
				responsesReasoningDone("rs_mid", responsesReasoningSummary("p0", "p1", "p2")),
			),
			want:    "p0p1p2",
			summary: []string{"p0", "p1", "p2"},
			wantID:  "rs_mid",
		},
		{
			name: "suffix fragments fill multiple parts",
			body: responsesSSEData(
				responsesReasoningAdded("rs_multi"),
				responsesReasoningDelta("rs_multi", 1, "tw"),
				responsesReasoningDone("rs_multi", responsesReasoningSummary("one", "two", "three")),
			),
			want:    "onetwothree",
			summary: []string{"one", "two", "three"},
			wantID:  "rs_multi",
		},
		{
			name: "final only snapshot",
			body: responsesSSEData(
				`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","output":[{"id":"rs_f","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"alpha"},{"type":"summary_text","text":"beta"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
			),
			want:    "alphabeta",
			summary: []string{"alpha", "beta"},
			wantID:  "rs_f",
		},
		{
			name: "index alias then id done",
			body: responsesSSEData(
				responsesReasoningDelta("", 0, "chain"),
				responsesReasoningDone("rs_alias", responsesReasoningSummary("chain")),
			),
			want:    "chain",
			summary: []string{"chain"},
			wantID:  "rs_alias",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := collectResponsesSSE(t, tc.body)
			if len(resp.Content) != 1 || resp.Content[0].Type != ir.BlockReasoning {
				t.Fatalf("content = %+v", resp.Content)
			}
			block := resp.Content[0]
			if block.Text != tc.want {
				t.Fatalf("text = %q want %q", block.Text, tc.want)
			}
			if block.ResponsesReasoning == nil {
				t.Fatal("canonical reasoning snapshot missing")
			}
			if block.ResponsesReasoning.ID != tc.wantID {
				t.Fatalf("id = %q want %q", block.ResponsesReasoning.ID, tc.wantID)
			}
			got := make([]string, 0, len(block.ResponsesReasoning.Summary))
			for _, part := range block.ResponsesReasoning.Summary {
				got = append(got, part.Text)
			}
			if strings.Join(got, "\x1f") != strings.Join(tc.summary, "\x1f") {
				t.Fatalf("summary = %q want %q", got, tc.summary)
			}
		})
	}
}

// TestCollectStructuredReasoningDoneByIndexRejects proves the collector
// validates a complete snapshot against per-index parts instead of trusting
// arrival order: a changed part, a shrink, and a shorter array all fail.
func TestCollectStructuredReasoningDoneByIndexRejects(t *testing.T) {
	summary := []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}}
	cases := []struct {
		name   string
		events []ir.StreamEvent
	}{
		{
			name: "changed part text",
			events: []ir.StreamEvent{
				{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", SummaryIndex: 0, Text: "hello"},
				{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{ID: "rs_1", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "world"}}}},
			},
		},
		{
			name: "shorter snapshot array",
			events: []ir.StreamEvent{
				{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", SummaryIndex: 0, Text: "a"},
				{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", SummaryIndex: 1, Text: "b"},
				{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{ID: "rs_1", Summary: summary}},
			},
		},
		{
			name: "changed retained part type",
			events: []ir.StreamEvent{
				{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", SummaryIndex: 0, Text: "a"},
				{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{ID: "rs_1", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "a"}}}},
				{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{ID: "rs_1", Summary: []ir.ReasoningSummaryPart{{Type: "custom", Text: "a"}}}},
			},
		},
		{
			name: "snapshot shrink without deltas",
			events: []ir.StreamEvent{
				{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{ID: "rs_1", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "a"}, {Type: "summary_text", Text: "b"}}}},
				{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{ID: "rs_1", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "a"}}}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := collectStreamResponse(strings.NewReader(""), reasoningEventsCodec(tc.events), nil, "m", nil)
			sf, ok := ir.AsStreamFailure(err)
			if !ok || sf.Message != "conflicting reasoning summary" {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// TestCollectResponsesGapUnaryProjections checks the complete collected IR and
// both unary renderings. Responses keeps the ordered summary; Anthropic keeps
// only readable text.
func TestCollectResponsesGapUnaryProjections(t *testing.T) {
	body := responsesSSEData(
		responsesReasoningAdded("rs_g"),
		responsesReasoningDelta("rs_g", 1, " tail"),
		responsesReasoningDone("rs_g", responsesReasoningSummary("head", " tail")),
	)
	resp := collectResponsesSSE(t, body)

	out, err := responsesCodec.encodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Output []struct {
			Type    string `json:"type"`
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Output) != 1 || decoded.Output[0].Type != "reasoning" {
		t.Fatalf("responses output = %+v body=%s", decoded.Output, out)
	}
	if len(decoded.Output[0].Summary) != 2 || decoded.Output[0].Summary[0].Text != "head" || decoded.Output[0].Summary[1].Text != " tail" {
		t.Fatalf("responses summary = %+v body=%s", decoded.Output[0].Summary, out)
	}

	anth, err := anthropicCodec.encodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(anth), "head tail") {
		t.Fatalf("anthropic projection lost readable summary: %s", anth)
	}
	if strings.Contains(string(anth), "encrypted") {
		t.Fatalf("anthropic projection invented opaque content: %s", anth)
	}
}

// TestCollectStructuredReasoningGapInterleavedOrder keeps the gap item in
// place when text and a tool surround it and never merges an unrelated text
// block into the reasoning projection.
func TestCollectStructuredReasoningGapInterleavedOrder(t *testing.T) {
	events := []ir.StreamEvent{
		{Kind: ir.EventTextDelta, Text: "before"},
		{Kind: ir.EventReasoningDelta, ReasoningID: "rs_g", SummaryIndex: 1, Text: " tail"},
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_g", Reasoning: &ir.ResponsesReasoning{
			ID: "rs_g", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "head"}, {Type: "summary_text", Text: " tail"}},
		}},
		{Kind: ir.EventToolCallStart, Index: 0, ToolID: "call_1", ToolName: "fn"},
		{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `{}`},
		{Kind: ir.EventTextDelta, Text: "after"},
		{Kind: ir.EventFinish},
	}
	resp, err := collectStreamResponse(strings.NewReader(""), reasoningEventsCodec(events), nil, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 4 {
		t.Fatalf("content = %+v", resp.Content)
	}
	if resp.Content[0].Type != ir.BlockText || resp.Content[0].Text != "before" {
		t.Fatalf("block 0 = %+v", resp.Content[0])
	}
	if resp.Content[1].Type != ir.BlockReasoning || resp.Content[1].Text != "head tail" {
		t.Fatalf("block 1 = %+v", resp.Content[1])
	}
	if resp.Content[2].Type != ir.BlockToolUse || resp.Content[2].ToolName != "fn" {
		t.Fatalf("block 2 = %+v", resp.Content[2])
	}
	if resp.Content[3].Type != ir.BlockText || resp.Content[3].Text != "after" {
		t.Fatalf("block 3 = %+v", resp.Content[3])
	}
}

// TestResponsesCodexForcedUnaryGapSummaryOrder runs the free/Codex forced
// stream-to-unary HTTP shape against a local httptest upstream. No network and
// no live inference is used.
func TestResponsesCodexForcedUnaryGapSummaryOrder(t *testing.T) {
	upstream := responsesSSEData(
		`{"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_up","type":"reasoning","status":"in_progress","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_up","output_index":0,"summary_index":1,"delta":" tail"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_up","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"head"},{"type":"summary_text","text":" tail"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","output":[{"id":"rs_up","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"head"},{"type":"summary_text","text":" tail"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstream))
	}))
	t.Cleanup(up.Close)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{{protocol: domain.ProtocolOpenAICodex, url: up.URL, model: "gpt-5-codex"}})
	resp, out := post(t, base+"/v1/responses", token, responsesStateRequestBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	var decoded struct {
		Output []struct {
			Type    string `json:"type"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Output) != 1 || decoded.Output[0].Type != "reasoning" {
		t.Fatalf("output = %+v body=%s", decoded.Output, out)
	}
	if len(decoded.Output[0].Summary) != 2 || decoded.Output[0].Summary[0].Text != "head" || decoded.Output[0].Summary[1].Text != " tail" {
		t.Fatalf("summary = %+v body=%s", decoded.Output[0].Summary, out)
	}
}

func projectionReadableLen(parts []ir.ReasoningSummaryPart) int {
	n := 0
	for _, part := range parts {
		n += len(part.Text)
	}
	return n
}

func assertCollectedProjection(t *testing.T, block ir.ContentBlock, want string, enc *string) {
	t.Helper()
	if block.Type != ir.BlockReasoning || block.Text != want {
		t.Fatalf("projection = %+v, want %q", block, want)
	}
	if block.ResponsesReasoning == nil {
		t.Fatal("canonical snapshot missing")
	}
	if got := block.ResponsesReasoning.ReadableText(); got != want {
		t.Fatalf("canonical readable = %q, want %q", got, want)
	}
	switch {
	case enc == nil && block.ResponsesReasoning.EncryptedContent != nil:
		t.Fatalf("opaque invented: %+v", block.ResponsesReasoning.EncryptedContent)
	case enc != nil && (block.ResponsesReasoning.EncryptedContent == nil || *block.ResponsesReasoning.EncryptedContent != *enc):
		t.Fatalf("opaque = %+v, want %q", block.ResponsesReasoning.EncryptedContent, *enc)
	}
}

// TestCollectReasoningProjectionCharged reserves the returned readable copy
// before it is built. Live parts and the snapshot clone stay charged; the
// projection is one additional copy and is not a usage-estimator artifact.
func TestCollectReasoningProjectionCharged(t *testing.T) {
	base := int64(len(ir.NewID("resp_")) + len("m"))
	multi := []ir.ReasoningSummaryPart{
		{Type: "summary_text", Text: "one"},
		{Type: "summary_text", Text: ""},
		{Type: "summary_text", Text: "three"},
	}
	deltaDone := []ir.StreamEvent{
		{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", SummaryIndex: 0, Text: "one"},
		{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", SummaryIndex: 2, Text: "three"},
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{ID: "rs_1", Summary: multi}},
		{Kind: ir.EventFinish},
	}
	// Index 2 allocates the empty index 1 placeholder too. The snapshot clone
	// then charges three parts, and the projection charges the nonempty texts.
	deltaExact := base + collectedBlockOverhead +
		int64(len("rs_1")+collectedReasoningKeyOverhead) +
		int64(3*collectedReasoningPartOverhead+len("one")+len("three")) +
		int64(len("rs_1")+3*collectedReasoningPartOverhead+3*len("summary_text")+len("one")+len("three")) +
		int64(projectionReadableLen(multi))
	resp, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(deltaDone), nil, "m", nil, deltaExact, 10)
	if err != nil {
		t.Fatalf("delta plus done exact budget failed: %v", err)
	}
	assertCollectedProjection(t, resp.Content[0], "onethree", nil)
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(deltaDone), nil, "m", nil, deltaExact-1, 10); !errors.Is(err, errCollectedStreamResponseTooLarge) {
		t.Fatalf("one byte under delta projection = %v", err)
	}

	snapshotOnly := []ir.StreamEvent{
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{ID: "rs_1", Summary: multi}},
		{Kind: ir.EventFinish},
	}
	snapExact := base + collectedBlockOverhead +
		int64(len("rs_1")+collectedReasoningKeyOverhead) +
		int64(len("rs_1")+3*collectedReasoningPartOverhead+3*len("summary_text")+len("one")+len("three")) +
		int64(projectionReadableLen(multi))
	resp, err = collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(snapshotOnly), nil, "m", nil, snapExact, 10)
	if err != nil {
		t.Fatalf("snapshot-only exact budget failed: %v", err)
	}
	assertCollectedProjection(t, resp.Content[0], "onethree", nil)
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(snapshotOnly), nil, "m", nil, snapExact-1, 10); !errors.Is(err, errCollectedStreamResponseTooLarge) {
		t.Fatalf("one byte under snapshot projection = %v", err)
	}

	repeated := append([]ir.StreamEvent{}, snapshotOnly[:1]...)
	for i := 0; i < 20; i++ {
		repeated = append(repeated, snapshotOnly[0])
	}
	repeated = append(repeated, ir.StreamEvent{Kind: ir.EventFinish})
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(repeated), nil, "m", nil, snapExact, 10); err != nil {
		t.Fatalf("repeated snapshots charged projection again: %v", err)
	}
}

func TestCollectReasoningProjectionOpaqueAndEmptyFree(t *testing.T) {
	enc := "cipher-bytes"
	empty := ""
	base := int64(len(ir.NewID("resp_")) + len("m"))
	opaque := []ir.StreamEvent{
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_opaque", Reasoning: &ir.ResponsesReasoning{
			ID: "rs_opaque", EncryptedContent: &enc, Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: ""}},
		}},
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_empty", Reasoning: &ir.ResponsesReasoning{
			ID: "rs_empty", EncryptedContent: &empty,
		}},
		{Kind: ir.EventFinish},
	}
	exact := base + 2*collectedBlockOverhead +
		int64(len("rs_opaque")+collectedReasoningKeyOverhead) +
		int64(len("rs_empty")+collectedReasoningKeyOverhead) +
		int64(len("rs_opaque")+len(enc)+collectedReasoningPartOverhead+len("summary_text")) +
		int64(len("rs_empty")+len(empty))
	resp, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(opaque), nil, "m", nil, exact, 10)
	if err != nil {
		t.Fatalf("opaque and empty projection charged readable bytes: %v", err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("content = %+v", resp.Content)
	}
	assertCollectedProjection(t, resp.Content[0], "", &enc)
	assertCollectedProjection(t, resp.Content[1], "", &empty)
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), reasoningEventsCodec(opaque), nil, "m", nil, exact-1, 10); !errors.Is(err, errCollectedStreamResponseTooLarge) {
		t.Fatalf("shared budget was loose: %v", err)
	}
}

func TestCollectReasoningProjectionRealDecoder(t *testing.T) {
	enc := "cipher"
	body := responsesSSEData(
		`{"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_real","type":"reasoning","status":"in_progress","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_real","output_index":0,"summary_index":0,"delta":"one"}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_real","output_index":0,"summary_index":1,"delta":"two"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_real","type":"reasoning","status":"completed","encrypted_content":"cipher","summary":[{"type":"summary_text","text":"one"},{"type":"summary_text","text":"two"}]}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","output":[{"id":"rs_real","type":"reasoning","status":"completed","encrypted_content":"cipher","summary":[{"type":"summary_text","text":"one"},{"type":"summary_text","text":"two"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	)
	base := int64(len("r1") + len("m"))
	// Decoder emits two summary deltas and one done. The collector charges the
	// live parts, the cloned snapshot including the empty middle part and opaque
	// bytes, and one readable projection. The completed status is on the clone.
	exact := base + collectedBlockOverhead +
		int64(len("rs_real")+2*collectedReasoningKeyOverhead) +
		int64(2*collectedReasoningPartOverhead+len("one")+len("two")) +
		int64(len("rs_real")+len("completed")+len(enc)+2*collectedReasoningPartOverhead+2*len("summary_text")+len("one")+len("two")) +
		int64(len("one")+len("two"))
	resp, err := collectStreamResponseWithLimits(strings.NewReader(body), responsesCodec, nil, "m", nil, exact, 10)
	if err != nil {
		t.Fatalf("real decoder exact budget failed: %v", err)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("content = %+v", resp.Content)
	}
	assertCollectedProjection(t, resp.Content[0], "onetwo", &enc)
	if len(resp.Content[0].ResponsesReasoning.Summary) != 2 || resp.Content[0].ResponsesReasoning.Summary[1].Text != "two" {
		t.Fatalf("empty summary part lost: %+v", resp.Content[0].ResponsesReasoning.Summary)
	}
	if _, err := collectStreamResponseWithLimits(strings.NewReader(body), responsesCodec, nil, "m", nil, exact-1, 10); !errors.Is(err, errCollectedStreamResponseTooLarge) {
		t.Fatalf("real decoder one byte under = %v", err)
	}
}
