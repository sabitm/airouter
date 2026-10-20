package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/claudecode"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func betaList(header string) []string {
	var out []string
	for _, part := range strings.Split(header, ",") {
		if flag := strings.TrimSpace(part); flag != "" {
			out = append(out, flag)
		}
	}
	return out
}

func hasBeta(header, want string) bool {
	for _, flag := range betaList(header) {
		if flag == want {
			return true
		}
	}
	return false
}

func TestTaskBudgetBetaReachesWireAndSchemaIsNotARequest(t *testing.T) {
	type capture struct {
		header http.Header
		body   []byte
	}
	seen := make(chan capture, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- capture{header: r.Header.Clone(), body: append([]byte(nil), body...)}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)

	st := newTestStore(t)
	prov := &domain.Provider{Name: "cc", BaseURL: upstream.URL, Protocol: domain.ProtocolClaudeCode, APIKey: "static-key"}
	if err := st.CreateProvider(context.Background(), prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "claude-sonnet-5", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "client")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	post := func(body, clientBeta string) capture {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key.Token)
		if clientBeta != "" {
			req.Header.Set("anthropic-beta", clientBeta)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.StatusCode, out)
		}
		return <-seen
	}

	withBudget := post(`{"model":"default","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"output_config":{"task_budget":{"type":"tokens","total":1000}}}`, claudecode.BetaLongContext+","+claudecode.BetaOAuth)
	if !hasBeta(withBudget.header.Get("anthropic-beta"), claudecode.BetaTaskBudget) {
		t.Fatalf("wire beta missing task budget: %s", withBudget.header.Get("anthropic-beta"))
	}
	if !hasBeta(withBudget.header.Get("anthropic-beta"), claudecode.BetaLongContext) {
		t.Fatalf("wire beta lost caller flag: %s", withBudget.header.Get("anthropic-beta"))
	}
	if hasBeta(withBudget.header.Get("anthropic-beta"), claudecode.BetaOAuth) {
		t.Fatalf("API-key request borrowed OAuth beta: %s", withBudget.header.Get("anthropic-beta"))
	}
	if !strings.Contains(string(withBudget.body), `"task_budget":{"type":"tokens","total":1000}`) {
		t.Fatalf("task_budget body lost: %s", withBudget.body)
	}

	schemaOnly := post(`{"model":"default","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"task_budget":{"type":"tokens","total":1000}}}}}}`, "")
	if hasBeta(schemaOnly.header.Get("anthropic-beta"), claudecode.BetaTaskBudget) {
		t.Fatalf("schema text became a task-budget request: %s", schemaOnly.header.Get("anthropic-beta"))
	}
}

func TestCallerBetaMergesContextAndForwardedSources(t *testing.T) {
	ctxHeader := http.Header{}
	ctxHeader.Set("anthropic-beta", claudecode.BetaLongContext)
	ctx := withClaudeRequest(context.Background(), ctxHeader, nil)
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	provider := &domain.Provider{Protocol: domain.ProtocolClaudeCode, APIKey: "static-key"}
	forwarded := http.Header{"Anthropic-Beta": []string{claudecode.BetaTaskBudget + ", " + claudecode.BetaLongContext + ", " + claudecode.BetaOAuth}}
	applyUpstreamHeaders(req, provider, forwarded, ctx, []byte(`{"model":"claude-sonnet-5"}`))
	got := req.Header.Get("anthropic-beta")
	for _, want := range []string{claudecode.BetaLongContext, claudecode.BetaTaskBudget} {
		if !hasBeta(got, want) {
			t.Fatalf("merged beta missing %s: %s", want, got)
		}
	}
	if hasBeta(got, claudecode.BetaOAuth) {
		t.Fatalf("API-key request borrowed OAuth beta: %s", got)
	}
	count := 0
	for _, flag := range betaList(got) {
		if flag == claudecode.BetaLongContext {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("caller beta was not deduped: %s", got)
	}

	oauthReq, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	oauthProvider := &domain.Provider{Protocol: domain.ProtocolClaudeCode, APIKey: "sk-ant-oat-test", AuthMethod: domain.AuthOAuth}
	applyUpstreamHeaders(oauthReq, oauthProvider, http.Header{"Anthropic-Beta": []string{claudecode.BetaOAuth}}, context.Background(), []byte(`{"model":"claude-sonnet-5"}`))
	if !hasBeta(oauthReq.Header.Get("anthropic-beta"), claudecode.BetaOAuth) {
		t.Fatalf("OAuth row lost OAuth beta: %s", oauthReq.Header.Get("anthropic-beta"))
	}
}

func TestSyntheticAPIKeyDeviceUsesStableRowIdentity(t *testing.T) {
	base := &domain.Provider{ID: 7, Name: "old", Protocol: domain.ProtocolClaudeCode, BaseURL: "https://One.Example/v1/", APIKey: "key-a"}
	renamed := *base
	renamed.Name = "new"
	renamed.APIKey = "key-b"
	if syntheticAPIKeyDevice(base) != syntheticAPIKeyDevice(&renamed) {
		t.Fatal("rename or token change rotated the synthetic install id")
	}
	sameBase := *base
	sameBase.BaseURL = "https://one.example/v1"
	if syntheticAPIKeyDevice(base) != syntheticAPIKeyDevice(&sameBase) {
		t.Fatal("canonical base URL changed the synthetic install id")
	}
	other := *base
	other.ID = 8
	if syntheticAPIKeyDevice(base) == syntheticAPIKeyDevice(&other) {
		t.Fatal("distinct provider rows shared a synthetic install id")
	}
}

func TestAPIKeyClaudeIdentityDoesNotBorrowOAuthState(t *testing.T) {
	stale := &domain.OAuthCreds{ClaudeAccountUUID: "stale-account", ClaudeDeviceID: strings.Repeat("ab", 32), AccessToken: "stale-token"}
	provider := &domain.Provider{ID: 9, Name: "cc", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode, APIKey: "static-key", OAuthCreds: stale}
	if account, device := claudeIdentity(provider); account != "" || device != "" {
		t.Fatalf("API-key row borrowed OAuth identity %q %q", account, device)
	}
	provider.AuthMethod = domain.AuthOAuth
	account, device := claudeIdentity(provider)
	if account != "stale-account" || device != strings.Repeat("ab", 32) {
		t.Fatalf("OAuth identity was not read from explicit auth: %q %q", account, device)
	}
}

func TestAPIKeyPrepareDropsStaleAccountAndKeepsDeviceAcrossRename(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	provider := &domain.Provider{
		ID: 11, Name: "old", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode,
		APIKey: "static-key", AuthMethod: domain.AuthAPIKey,
		OAuthCreds: &domain.OAuthCreds{ClaudeAccountUUID: "stale-account", ClaudeDeviceID: strings.Repeat("ab", 32)},
	}
	ctx := withClaudeRequest(context.Background(), nil, nil)
	wire, err := prepareClaudeCodeBody(ctx, provider, body)
	if err != nil {
		t.Fatal(err)
	}
	renamed := *provider
	renamed.Name = "new"
	again, err := prepareClaudeCodeBody(ctx, &renamed, body)
	if err != nil {
		t.Fatal(err)
	}
	readID := func(raw []byte) claudecode.UserID {
		var doc struct {
			Metadata struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		var uid claudecode.UserID
		if err := json.Unmarshal([]byte(doc.Metadata.UserID), &uid); err != nil {
			t.Fatal(err)
		}
		return uid
	}
	first, second := readID(wire), readID(again)
	if first.AccountUUID != "" {
		t.Fatalf("stale account reached outbound identity: %+v", first)
	}
	if first.DeviceID != second.DeviceID || first.DeviceID != syntheticAPIKeyDevice(provider) {
		t.Fatalf("synthetic device changed across retry: %q %q", first.DeviceID, second.DeviceID)
	}
}

func collectTextStream(t *testing.T, raw string) *ir.Response {
	t.Helper()
	resp, err := collectStreamResponse(strings.NewReader(raw), anthropicCodec, nil, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func roundTripEventsSSE(t *testing.T, raw string) string {
	t.Helper()
	var events []ir.StreamEvent
	if err := anthropic.DecodeStream(strings.NewReader(raw), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("no SSE writer")
	}
	enc := anthropic.NewStreamEncoder("x")
	for _, ev := range events {
		if err := enc.Encode(ev, w); err != nil {
			t.Fatalf("encode %+v: %v", ev, err)
		}
	}
	return rec.Body.String()
}

func assertTextBlocks(t *testing.T, resp *ir.Response, wantText []string, wantCitations []int) {
	t.Helper()
	var textBlocks []ir.ContentBlock
	for _, block := range resp.Content {
		if block.Type == ir.BlockText {
			textBlocks = append(textBlocks, block)
		}
	}
	if len(textBlocks) != len(wantText) {
		raw, _ := json.Marshal(resp.Content)
		t.Fatalf("text blocks=%d want=%d content=%s", len(textBlocks), len(wantText), raw)
	}
	for i, want := range wantText {
		if textBlocks[i].Text != want {
			t.Fatalf("block %d text=%q want %q", i, textBlocks[i].Text, want)
		}
		if len(textBlocks[i].Citations) != wantCitations[i] {
			t.Fatalf("block %d citations=%d want %d", i, len(textBlocks[i].Citations), wantCitations[i])
		}
	}
}

func TestTextBlockLifecyclePreservesBoundariesAndCitations(t *testing.T) {
	raw := `event: message_start
data: {"message":{"id":"m","model":"x","usage":{"input_tokens":1}}}

event: content_block_start
data: {"index":0,"content_block":{"type":"text","text":"first","citations":[{"type":"char_location","start_char_index":0,"end_char_index":1}]}}

event: content_block_stop
data: {"index":0}

event: content_block_start
data: {"index":1,"content_block":{"type":"text","text":""}}

event: content_block_start
data: {"index":2,"content_block":{"type":"text","text":"second"}}

event: content_block_delta
data: {"index":2,"delta":{"type":"citations_delta","citation":{"type":"char_location","start_char_index":0,"end_char_index":1}}}

event: content_block_stop
data: {"index":2}

event: message_delta
data: {"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {}

`
	resp := collectTextStream(t, raw)
	assertTextBlocks(t, resp, []string{"first", "", "second"}, []int{1, 0, 1})
	citation := string(resp.Content[2].Citations[0].Raw)
	if !strings.Contains(citation, `"start_char_index":0`) || !strings.Contains(citation, `"end_char_index":1`) {
		t.Fatalf("second block citation changed: %s", citation)
	}
	encoded := roundTripEventsSSE(t, raw)
	again := collectTextStream(t, encoded)
	assertTextBlocks(t, again, []string{"first", "", "second"}, []int{1, 0, 1})
}

func TestTextBlockAfterServerKeepsSeparateCitation(t *testing.T) {
	raw := `event: message_start
data: {"message":{"id":"m","model":"x","usage":{"input_tokens":1}}}

event: content_block_start
data: {"index":0,"content_block":{"type":"text","text":"before"}}

event: content_block_stop
data: {"index":0}

event: content_block_start
data: {"index":1,"content_block":{"type":"server_tool_use","id":"s","name":"web_search","input":{}}}

event: content_block_stop
data: {"index":1}

event: content_block_start
data: {"index":2,"content_block":{"type":"text","text":"after"}}

event: content_block_delta
data: {"index":2,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://example.test"}}}

event: content_block_stop
data: {"index":2}

event: message_stop
data: {}

`
	resp := collectTextStream(t, raw)
	assertTextBlocks(t, resp, []string{"before", "after"}, []int{0, 1})
	var server int
	for _, block := range resp.Content {
		if block.Type == ir.BlockServer {
			server++
		}
	}
	if server != 1 {
		t.Fatalf("server blocks=%d content=%+v", server, resp.Content)
	}
	again := collectTextStream(t, roundTripEventsSSE(t, raw))
	assertTextBlocks(t, again, []string{"before", "after"}, []int{0, 1})
}

func TestDanglingCitationFailsClosed(t *testing.T) {
	raw := `event: message_start
data: {"message":{"id":"m","model":"x","usage":{"input_tokens":1}}}

event: content_block_start
data: {"index":0,"content_block":{"type":"text","text":"first"}}

event: content_block_stop
data: {"index":0}

event: content_block_delta
data: {"index":0,"delta":{"type":"citations_delta","citation":{"type":"char_location","start_char_index":0,"end_char_index":1}}}

event: message_stop
data: {}

`
	if _, err := collectStreamResponse(strings.NewReader(raw), anthropicCodec, nil, "x", nil); err == nil {
		t.Fatal("citation after text stop was accepted")
	}
}

func TestMalformedCitationStillFails(t *testing.T) {
	raw := `event: message_start
data: {"message":{"id":"m","model":"x","usage":{"input_tokens":1}}}

event: content_block_start
data: {"index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"index":0,"delta":{"type":"citations_delta","citation":{}}}

event: message_stop
data: {}

`
	if _, err := collectStreamResponse(strings.NewReader(raw), anthropicCodec, nil, "x", nil); err == nil {
		t.Fatal("malformed citation was accepted")
	}
}

func TestTextLifecycleLegacyDeltaStillMerges(t *testing.T) {
	raw := `event: content_block_delta
data: {"index":0,"delta":{"type":"text_delta","text":"a"}}

event: content_block_delta
data: {"index":0,"delta":{"type":"text_delta","text":"b"}}

`
	resp := collectTextStream(t, raw)
	assertTextBlocks(t, resp, []string{"ab"}, []int{0})
	again := collectTextStream(t, roundTripEventsSSE(t, raw))
	assertTextBlocks(t, again, []string{"ab"}, []int{0})
}

func eventStreamCodec(events []ir.StreamEvent) codec {
	return codec{decodeStream: func(_ io.Reader, emit func(ir.StreamEvent) error) error {
		for _, ev := range events {
			if err := emit(ev); err != nil {
				return err
			}
		}
		return nil
	}}
}

func TestCitationAppendChargesStructOverhead(t *testing.T) {
	base := int64(len(ir.NewID("resp_")) + len("m"))
	citation := `{"type":"char_location"}`
	perCitation := int64(len(citation) + collectedCitationOverhead)
	limit := base + collectedBlockOverhead + 2*perCitation
	events := func(n int) []ir.StreamEvent {
		out := make([]ir.StreamEvent, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, ir.StreamEvent{Kind: ir.EventTextCitation, Data: citation})
		}
		return out
	}
	resp, err := collectStreamResponseWithLimits(strings.NewReader(""), eventStreamCodec(events(2)), nil, "m", nil, limit, 10)
	if err != nil {
		t.Fatalf("two citations should fit: %v", err)
	}
	if len(resp.Content) != 1 || len(resp.Content[0].Citations) != 2 {
		t.Fatalf("content = %+v", resp.Content)
	}
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), eventStreamCodec(events(3)), nil, "m", nil, limit, 10); err == nil {
		t.Fatal("citation struct overhead was not billed")
	}
}

func TestCollectedResponseStateChargesBudget(t *testing.T) {
	base := int64(len(ir.NewID("resp_")) + len("m"))
	container := json.RawMessage(`{"id":"c1"}`)
	limit := base + int64(len(container))
	events := []ir.StreamEvent{
		{Kind: ir.EventMessageStart, Container: container},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
	}
	resp, err := collectStreamResponseWithLimits(strings.NewReader(""), eventStreamCodec(events), nil, "m", nil, limit, 10)
	if err != nil {
		t.Fatalf("state at limit failed: %v", err)
	}
	if resp.Anthropic == nil || string(resp.Anthropic.Container) != string(container) {
		t.Fatalf("container = %+v", resp.Anthropic)
	}
	if _, err := collectStreamResponseWithLimits(strings.NewReader(""), eventStreamCodec(events), nil, "m", nil, limit-1, 10); err == nil {
		t.Fatal("response state bytes were not billed")
	}
}
