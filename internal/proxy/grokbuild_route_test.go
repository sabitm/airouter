package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/grokbuild"
	"airouter/internal/proxy/sse"
)

func TestGrokBuildStreamRouteKeepsToolAndReasoningIDs(t *testing.T) {
	var gotHeader http.Header
	var gotBody []byte
	var gotURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotURL = r.URL.String()
		gotBody, _ = io.ReadAll(r.Body)
		if strings.Contains(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"grok-build","capabilities":{"reasoning_effort":["low","high"]}}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, grokBuildSSE())
	}))
	t.Cleanup(upstream.Close)

	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{
		Name: "gb", BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			GrokBuildAuth: true, Preset: grokbuild.Preset, ClientID: grokbuild.ClientID,
			TokenURL: grokbuild.TokenURL, AccessToken: "access-live", RefreshToken: "refresh-live",
			Scopes: grokbuild.Scopes,
		},
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "grok-build", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	px := New(st, nil)
	px.streamClient = &http.Client{Transport: grokBuildRewriteTransport{base: upstream.URL}}
	px.client = px.streamClient
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	body := `{
		"model":"default",
		"input":[
			{"type":"message","role":"user","content":"first"},
			{"type":"reasoning","id":"rs_keep","encrypted_content":"opaque-reasoning","summary":[]},
			{"type":"function_call","id":"fc_keep","call_id":"call_keep","name":"read","arguments":"{\"path\":\"a\"}"},
			{"type":"function_call_output","call_id":"call_keep","output":"file text"},
			{"type":"message","role":"user","content":"next"}
		],
		"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],
		"reasoning":{"effort":"high"},
		"stream":false
	}`
	resp, out := post(t, ts.URL+"/v1/responses", key.Token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, out)
	}
	if !strings.Contains(gotURL, "/responses") {
		t.Fatalf("url = %s", gotURL)
	}
	if gotHeader.Get("Authorization") != "Bearer access-live" {
		t.Fatalf("auth = %s", gotHeader.Get("Authorization"))
	}
	if gotHeader.Get("X-XAI-Token-Auth") != grokbuild.TokenAuthValue || gotHeader.Get("X-Grok-Client-Version") != grokbuild.Version {
		t.Fatalf("headers = %v", gotHeader)
	}
	if gotHeader.Get("X-Grok-Model-Override") != "grok-build" {
		t.Fatalf("override = %s", gotHeader.Get("X-Grok-Model-Override"))
	}
	if gotHeader.Get("X-Email") != "" && gotHeader.Get("X-Email") != "Bearer access-live" {
		t.Fatalf("email header forwarded: %s", gotHeader.Get("X-Email"))
	}
	var doc map[string]any
	if err := json.Unmarshal(gotBody, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["store"] != false || doc["stream"] != true || doc["model"] != "grok-build" {
		t.Fatalf("body flags = %v %v %v", doc["store"], doc["stream"], doc["model"])
	}
	raw := string(gotBody)
	for _, want := range []string{"rs_keep", "opaque-reasoning", "fc_keep", "call_keep", `"effort":"high"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing %s in %s", want, raw)
		}
	}
	if !strings.Contains(string(out), "tool text") {
		t.Fatalf("ingress response = %s", out)
	}
}

func TestResponsesIngressDoesNotPassthroughToGrokBuild(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, grokBuildSSE())
	}))
	t.Cleanup(upstream.Close)
	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{
		Name: "gb", BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			GrokBuildAuth: true, Preset: grokbuild.Preset, ClientID: grokbuild.ClientID,
			TokenURL: grokbuild.TokenURL, AccessToken: "access-live", RefreshToken: "refresh-live", Scopes: grokbuild.Scopes,
		},
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "grok-build", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	px := New(st, nil)
	px.streamClient = &http.Client{Transport: grokBuildRewriteTransport{base: upstream.URL}}
	px.client = px.streamClient
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, out := post(t, ts.URL+"/v1/responses", key.Token, `{"model":"default","input":"hi","store":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(gotBody), `"store":false`) || strings.Contains(string(gotBody), `"store":true`) {
		t.Fatalf("store was passed through: %s", gotBody)
	}
}

func TestGrokBuildReplaysEmittedToolAndReasoning(t *testing.T) {
	var bodies [][]byte
	var urls []string
	turn := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		bodies = append(bodies, got)
		urls = append(urls, r.URL.String())
		w.Header().Set("Content-Type", "text/event-stream")
		if turn == 0 {
			turn++
			_, _ = io.WriteString(w, grokBuildReplaySSE())
			return
		}
		// The unary collector is the third request. Return the same reasoning and
		// function SSE so the collected JSON is asserted, not only the outgoing body.
		if turn == 2 {
			_, _ = io.WriteString(w, grokBuildReplaySSE())
			return
		}
		turn++
		_, _ = io.WriteString(w, grokBuildDoneSSE())
	}))
	t.Cleanup(upstream.Close)
	base, token := grokBuildProxy(t, upstream.URL)

	first := `{"model":"default","input":"read the file","tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}],"reasoning":{"effort":"high"},"stream":true}`
	resp, raw := postStream(t, base+"/v1/responses", token, first)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first status %d body %s", resp.StatusCode, raw)
	}
	itemID, callID, args, reasonID, reasonCipher := collectFunctionReplay(t, raw)
	if itemID != "fc_native" || callID != "call_native" || args != `{"path":"b"}` {
		t.Fatalf("emitted identity = %s %s %s", itemID, callID, args)
	}
	if reasonID != "rs_native" || reasonCipher != "opaque-native" {
		t.Fatalf("emitted reasoning = %s %s", reasonID, reasonCipher)
	}

	second := `{
		"model":"default",
		"stream":true,
		"input":[
			{"type":"message","role":"user","content":"read the file"},
			{"type":"reasoning","id":"` + reasonID + `","encrypted_content":"` + reasonCipher + `","summary":[]},
			{"type":"function_call","id":"` + itemID + `","call_id":"` + callID + `","name":"read","arguments":` + grokJSONString(args) + `},
			{"type":"function_call_output","call_id":"` + callID + `","output":"file text"},
			{"type":"message","role":"user","content":"next"}
		]
	}`
	resp, raw = postStream(t, base+"/v1/responses", token, second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second status %d body %s", resp.StatusCode, raw)
	}
	if len(bodies) != 2 || !strings.Contains(urls[1], "/responses") {
		t.Fatalf("urls = %v", urls)
	}
	replay := string(bodies[1])
	for _, want := range []string{"fc_native", "call_native", "opaque-native", "file text", `"store":false`, `"stream":true`, `"model":"grok-build"`} {
		if !strings.Contains(replay, want) {
			t.Fatalf("replay missing %s in %s", want, replay)
		}
	}

	unary := `{
		"model":"default",
		"stream":false,
		"input":[
			{"type":"message","role":"user","content":"read the file"},
			{"type":"reasoning","id":"` + reasonID + `","encrypted_content":"` + reasonCipher + `","summary":[]},
			{"type":"function_call","id":"` + itemID + `","call_id":"` + callID + `","name":"read","arguments":` + grokJSONString(args) + `},
			{"type":"function_call_output","call_id":"` + callID + `","output":"file text"}
		]
	}`
	resp, out := post(t, base+"/v1/responses", token, unary)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unary status %d body %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(bodies[2]), itemID) || !strings.Contains(string(bodies[2]), callID) {
		t.Fatalf("unary replay dropped ids: %s", bodies[2])
	}
	var collected struct {
		Output []struct {
			Type             string `json:"type"`
			ID               string `json:"id"`
			CallID           string `json:"call_id"`
			Arguments        string `json:"arguments"`
			EncryptedContent string `json:"encrypted_content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &collected); err != nil {
		t.Fatal(err)
	}
	var uItem, uCall, uArgs, uReason, uCipher string
	for _, item := range collected.Output {
		switch item.Type {
		case "function_call":
			uItem, uCall, uArgs = item.ID, item.CallID, item.Arguments
		case "reasoning":
			uReason, uCipher = item.ID, item.EncryptedContent
		}
	}
	if uItem != itemID || uCall != callID || uArgs != args || uReason != reasonID || uCipher != reasonCipher {
		t.Fatalf("collected unary response lost ids: %s %s %s %s %s body %s", uItem, uCall, uArgs, uReason, uCipher, out)
	}
}

func TestGrokBuildLateNativeItemIDIsNotRenamed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, grokBuildLateItemSSE())
	}))
	t.Cleanup(upstream.Close)
	base, token := grokBuildProxy(t, upstream.URL)
	body := `{"model":"default","input":"read the file","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],"stream":true}`
	resp, raw := postStream(t, base+"/v1/responses", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	itemID, callID, args, _, _ := collectFunctionReplay(t, raw)
	if itemID != "fc_late" || callID != "call_late" || args != `{"path":"b"}` {
		t.Fatalf("late identity = %s %s %s body %s", itemID, callID, args, raw)
	}
	if strings.Contains(raw, `"id":"fc_`) && strings.Count(raw, "fc_late") == 0 {
		t.Fatal("synthetic id replaced the late native id")
	}
	if strings.Count(raw, "fc_") != strings.Count(raw, "fc_late") {
		t.Fatalf("published a second function id: %s", raw)
	}
}

func TestGrokBuildParallelLateItemIDsKeepOrder(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, grokBuildParallelLateSSE())
	}))
	t.Cleanup(upstream.Close)
	base, token := grokBuildProxy(t, upstream.URL)
	body := `{"model":"default","input":"read both","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],"stream":true}`
	resp, raw := postStream(t, base+"/v1/responses", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	var ids []string
	reader := sse.NewReader(strings.NewReader(raw))
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var frame struct {
			Type string `json:"type"`
			Item struct {
				Type   string `json:"type"`
				ID     string `json:"id"`
				CallID string `json:"call_id"`
			} `json:"item"`
		}
		if json.Unmarshal(ev.Data, &frame) != nil || frame.Type != "response.output_item.added" || frame.Item.Type != "function_call" {
			continue
		}
		ids = append(ids, frame.Item.ID+"/"+frame.Item.CallID)
	}
	if strings.Join(ids, ",") != "fc_a/call_a,fc_b/call_b" {
		t.Fatalf("SSE order = %v body %s", ids, raw)
	}
}

func grokBuildParallelLateSSE() string {
	return strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_a","name":"read","arguments":""}}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_b","name":"read","arguments":""}}`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"read","arguments":"{}","status":"completed"}}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}","status":"completed"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_parallel","status":"completed","output":[{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}"},{"type":"function_call","id":"fc_b","call_id":"call_b","name":"read","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
}

func TestGrokBuildRejectsUnsupportedIngressBeforeSend(t *testing.T) {
	sent := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, grokBuildDoneSSE())
	}))
	t.Cleanup(upstream.Close)
	base, token := grokBuildProxy(t, upstream.URL)
	cases := []struct{ unary, stream string }{
		{` {"model":"default","input":"hi","tools":[{"type":"web_search"}]}`, ` {"model":"default","stream":true,"input":"hi","tools":[{"type":"web_search"}]}`},
		{`{"model":"default","input":"hi","previous_response_id":"resp_1"}`, `{"model":"default","stream":true,"input":"hi","previous_response_id":"resp_1"}`},
		{`{"model":"default","input":["rs_stored"]}`, `{"model":"default","stream":true,"input":["rs_stored"]}`},
		{`{"model":"default","input":[{"type":"item_reference","id":"fc_1"}]}`, `{"model":"default","stream":true,"input":[{"type":"item_reference","id":"fc_1"}]}`},
		{`{"model":"default","input":[{"type":"custom_tool_call","call_id":"c","name":"x","input":""}]}`, `{"model":"default","stream":true,"input":[{"type":"custom_tool_call","call_id":"c","name":"x","input":""}]}`},
	}
	for _, body := range cases {
		resp, out := post(t, base+"/v1/responses", token, body.unary)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d body %s for %s", resp.StatusCode, out, body.unary)
		}
		streamResp, streamOut := postStream(t, base+"/v1/responses", token, body.stream)
		if streamResp.StatusCode != http.StatusBadRequest {
			t.Fatalf("stream status %d body %s for %s", streamResp.StatusCode, streamOut, body.stream)
		}
	}
	if sent != 0 {
		t.Fatalf("unsupported ingress reached upstream %d times", sent)
	}
	ok, out := post(t, base+"/v1/responses", token, `{"model":"default","input":"hi","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}`)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("function tool rejected: %d %s", ok.StatusCode, out)
	}
}

func TestGrokBuildWrongHostDoesNotSendToken(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{
		Name: "bad", BaseURL: "https://api.x.ai/v1", Protocol: domain.ProtocolGrokBuild,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			GrokBuildAuth: true, AccessToken: "secret", RefreshToken: "refresh", ClientID: grokbuild.ClientID,
			TokenURL: grokbuild.TokenURL, Preset: grokbuild.Preset, Scopes: grokbuild.Scopes,
		},
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "grok-build", Enabled: true}}}); err != nil {
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
	resp, out := post(t, ts.URL+"/v1/responses", key.Token, `{"model":"default","input":"hi"}`)
	if resp.StatusCode < 400 {
		t.Fatalf("wrong host status %d body %s", resp.StatusCode, out)
	}
	if strings.Contains(string(out), "secret") || strings.Contains(string(out), "refresh") {
		t.Fatalf("token leaked in error: %s", out)
	}
}

type grokBuildRewriteTransport struct {
	base string
}

func (g grokBuildRewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !grokbuild.IsFixedInferenceURL(r.URL.String()) {
		return nil, io.ErrUnexpectedEOF
	}
	u, err := url.Parse(g.base + r.URL.RequestURI())
	if err != nil {
		return nil, err
	}
	clone := r.Clone(r.Context())
	clone.URL = u
	clone.Host = u.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func grokBuildProxy(t *testing.T, upstream string) (string, string) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{
		Name: "gb", BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			GrokBuildAuth: true, Preset: grokbuild.Preset, ClientID: grokbuild.ClientID,
			TokenURL: grokbuild.TokenURL, AccessToken: "access-live", RefreshToken: "refresh-live",
			Scopes: grokbuild.Scopes, GrokBuildConnection: "conn-live",
		},
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "grok-build", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	px := New(st, nil)
	px.streamClient = &http.Client{Transport: grokBuildRewriteTransport{base: upstream}}
	px.client = px.streamClient
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token
}

func collectFunctionReplay(t *testing.T, body string) (itemID, callID, args, reasonID, reasonCipher string) {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	var buf strings.Builder
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var frame struct {
			Type     string `json:"type"`
			Response struct {
				Output []struct {
					Type             string `json:"type"`
					ID               string `json:"id"`
					CallID           string `json:"call_id"`
					Arguments        string `json:"arguments"`
					EncryptedContent string `json:"encrypted_content"`
				} `json:"output"`
			} `json:"response"`
			Item struct {
				ID               string `json:"id"`
				CallID           string `json:"call_id"`
				Type             string `json:"type"`
				EncryptedContent string `json:"encrypted_content"`
			} `json:"item"`
			Delta string `json:"delta"`
		}
		_ = json.Unmarshal(ev.Data, &frame)
		switch ev.Name {
		case "response.output_item.added", "response.output_item.done":
			switch frame.Item.Type {
			case "function_call":
				itemID = frame.Item.ID
				callID = frame.Item.CallID
			case "reasoning":
				reasonID = frame.Item.ID
				reasonCipher = frame.Item.EncryptedContent
			}
		case "response.function_call_arguments.delta":
			buf.WriteString(frame.Delta)
		case "response.completed":
			for _, item := range frame.Response.Output {
				switch item.Type {
				case "function_call":
					if item.ID != "" {
						itemID = item.ID
					}
					if item.CallID != "" {
						callID = item.CallID
					}
					if item.Arguments != "" {
						args = item.Arguments
					}
				case "reasoning":
					if item.ID != "" {
						reasonID = item.ID
					}
					if item.EncryptedContent != "" {
						reasonCipher = item.EncryptedContent
					}
				}
			}
		}
	}
	if args == "" {
		args = buf.String()
	}
	return itemID, callID, args, reasonID, reasonCipher
}

func grokBuildLateItemSSE() string {
	return strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_late","name":"read","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\":"}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"b\"}"}`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_late","call_id":"call_late","name":"read","arguments":"{\"path\":\"b\"}","status":"completed"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_late","status":"completed","output":[{"type":"function_call","id":"fc_late","call_id":"call_late","name":"read","arguments":"{\"path\":\"b\"}"}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
}

func grokJSONString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func grokBuildReplaySSE() string {
	return strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_native","encrypted_content":"opaque-native","summary":[]}}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_native","encrypted_content":"opaque-native","summary":[],"status":"completed"}}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_native","call_id":"call_native","name":"read","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_native","delta":"{\"path\":"}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_native","delta":"\"b\"}"}`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_native","call_id":"call_native","name":"read","arguments":"{\"path\":\"b\"}","status":"completed"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_native","status":"completed","output":[{"type":"reasoning","id":"rs_native","encrypted_content":"opaque-native","summary":[]},{"type":"function_call","id":"fc_native","call_id":"call_native","name":"read","arguments":"{\"path\":\"b\"}"}],"usage":{"input_tokens":2,"output_tokens":3}}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
}

func grokBuildDoneSSE() string {
	return strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"done"}`,
		`data: {"type":"response.completed","response":{"id":"resp_done","status":"completed","usage":{"input_tokens":4,"output_tokens":1}}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
}

func grokBuildSSE() string {
	return strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_out","call_id":"call_out","name":"read","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_out","delta":"{\"path\":"}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_out","delta":"\"b\"}"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_out","call_id":"call_out","name":"read","arguments":"{\"path\":\"b\"}","status":"completed"}}`,
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"tool text"}`,
		`data: {"type":"response.completed","response":{"id":"resp_x","status":"completed","usage":{"input_tokens":2,"output_tokens":3}}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
}
