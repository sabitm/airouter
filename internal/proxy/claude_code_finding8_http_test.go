package proxy

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/claudecode"
)

const finding8LaterText = "later message with enough length for indexes"

func finding8DecodeUseNumber(t *testing.T, data []byte) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode: %v; data=%s", err, data)
	}
	return v
}

func finding8MessagesField(t *testing.T, body string) json.RawMessage {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, body)
	}
	messages, ok := raw["messages"]
	if !ok {
		t.Fatalf("messages missing: %s", body)
	}
	return messages
}

func finding8AssertMessagesUnchanged(t *testing.T, wireBody, wantMessages string) {
	t.Helper()
	want := finding8DecodeUseNumber(t, []byte(wantMessages))
	got := finding8DecodeUseNumber(t, finding8MessagesField(t, wireBody))
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("messages changed through prepare:\n want %s\n got  %s", want, got)
	}
}

func finding8SystemBilling(t *testing.T, wireBody string) string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(wireBody))
	dec.UseNumber()
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		t.Fatalf("decode wire body: %v; body=%s", err, wireBody)
	}
	var sys []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw["system"], &sys); err != nil {
		t.Fatalf("decode system: %v; body=%s", err, wireBody)
	}
	if len(sys) == 0 || !strings.HasPrefix(sys[0].Text, claudecode.BillingHeaderPrefix) {
		t.Fatalf("billing system block missing: %s", wireBody)
	}
	return sys[0].Text
}

func finding8CCVersion(t *testing.T, billing string) string {
	t.Helper()
	i := strings.Index(billing, "cc_version=")
	if i < 0 {
		t.Fatalf("cc_version missing: %s", billing)
	}
	rest := billing[i+len("cc_version="):]
	if j := strings.Index(rest, ";"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func finding8WithStream(body string) string {
	return strings.Replace(body, `"max_tokens":32,`, `"max_tokens":32,"stream":true,`, 1)
}

func finding8IsStream(body string) bool {
	return strings.Contains(body, `"stream":true`)
}

func TestFinding8FirstUserTextSelectionHTTP(t *testing.T) {
	imageOnly := `{"model":"default","max_tokens":32,"messages":[` +
		`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVQI12P4z8AAAAMBAQAY3Y20AAAAAElFTkSuQmCC"}}]},` +
		`{"role":"user","content":[{"type":"text","text":"` + finding8LaterText + `"}]}]}`
	emptySame := `{"model":"default","max_tokens":32,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":""},{"type":"text","text":"second block text"}]},` +
		`{"role":"user","content":[{"type":"text","text":"` + finding8LaterText + `"}]}]}`
	emptyLater := `{"model":"default","max_tokens":32,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":""}]},` +
		`{"role":"user","content":[{"type":"text","text":"` + finding8LaterText + `"}]}]}`
	ordinary := `{"model":"default","max_tokens":32,"messages":[` +
		`{"role":"user","content":"  héllo 😀 世界  "}]}`
	wantImageOnly := `[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVQI12P4z8AAAAMBAQAY3Y20AAAAAElFTkSuQmCC"}}]},` +
		`{"role":"user","content":[{"type":"text","text":"` + finding8LaterText + `"}]}]`
	wantEmptySame := `[{"role":"user","content":[{"type":"text"},{"type":"text","text":"second block text"}]},` +
		`{"role":"user","content":[{"type":"text","text":"` + finding8LaterText + `"}]}]`
	wantEmptyLater := `[{"role":"user","content":[{"type":"text"}]},` +
		`{"role":"user","content":[{"type":"text","text":"` + finding8LaterText + `"}]}]`
	wantOrdinary := `[{"role":"user","content":[{"type":"text","text":"  héllo 😀 世界  "}]}]`
	cases := []struct {
		name         string
		body         string
		wantInput    string
		wantMessages string
	}{
		{name: "image-only-first-unary", body: imageOnly, wantInput: "", wantMessages: wantImageOnly},
		{name: "image-only-first-stream", body: finding8WithStream(imageOnly), wantInput: "", wantMessages: wantImageOnly},
		{name: "empty-first-text-same-message-unary", body: emptySame, wantInput: "", wantMessages: wantEmptySame},
		{name: "empty-first-text-same-message-stream", body: finding8WithStream(emptySame), wantInput: "", wantMessages: wantEmptySame},
		{name: "empty-first-text-later-message-unary", body: emptyLater, wantInput: "", wantMessages: wantEmptyLater},
		{name: "empty-first-text-later-message-stream", body: finding8WithStream(emptyLater), wantInput: "", wantMessages: wantEmptyLater},
		{name: "ordinary-unicode-whitespace-unary", body: ordinary, wantInput: "  héllo 😀 世界  ", wantMessages: wantOrdinary},
		{name: "ordinary-unicode-whitespace-stream", body: finding8WithStream(ordinary), wantInput: "  héllo 😀 世界  ", wantMessages: wantOrdinary},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			captures := newSelectorCapture(2)
			upstream := finding4Upstream(t, captures, finding8IsStream(tc.body))
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  upstream.URL,
				model:    "claude-sonnet-4-6",
			})
			resp, out := postSelector(t, selectorClient(tr), base+"/v1/messages", token, tc.body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			wantText := "hello from anthropic"
			if finding8IsStream(tc.body) {
				wantText = "answer"
			}
			if !strings.Contains(string(out), wantText) {
				t.Fatalf("client response lost content: %s", out)
			}
			wire := captures.next(t)
			captures.assertNoMore(t)
			billing := finding8SystemBilling(t, wire.body)
			cc := finding8CCVersion(t, billing)
			if want := claudecode.CLIVersion + "." + claudecode.Fingerprint(tc.wantInput); cc != want {
				t.Fatalf("cc_version = %q want %q; billing=%s", cc, want, billing)
			}
			if tc.wantInput == "" && strings.Contains(cc, claudecode.Fingerprint(finding8LaterText)) {
				t.Fatalf("later message was fingerprinted: %q", cc)
			}
			if strings.Contains(wire.body, "cch=") {
				t.Fatalf("localhost upstream must omit cch: %s", wire.body)
			}
			finding8AssertMessagesUnchanged(t, wire.body, tc.wantMessages)
			assertSelectorUsage(t, "/v1/messages", finding8IsStream(tc.body), string(out))
		})
	}
}

func TestFinding8OrdinaryTextCrossIngressHTTP(t *testing.T) {
	cases := []struct {
		name         string
		path         string
		body         string
		wantMessages string
	}{
		{
			name:         "chat-unary",
			path:         "/v1/chat/completions",
			body:         `{"model":"default","max_tokens":32,"messages":[{"role":"user","content":"  héllo 😀 世界  "}]}`,
			wantMessages: `[{"role":"user","content":[{"type":"text","text":"  héllo 😀 世界  "}]}]`,
		},
		{
			name:         "responses-unary",
			path:         "/v1/responses",
			body:         `{"model":"default","max_output_tokens":32,"input":"  héllo 😀 世界  "}`,
			wantMessages: `[{"role":"user","content":[{"type":"text","text":"  héllo 😀 世界  "}]}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			captures := newSelectorCapture(2)
			upstream := finding4Upstream(t, captures, false)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  upstream.URL,
				model:    "claude-sonnet-4-6",
			})
			resp, out := postSelector(t, selectorClient(tr), base+tc.path, token, tc.body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			if !strings.Contains(string(out), "hello from anthropic") {
				t.Fatalf("client response lost content: %s", out)
			}
			wire := captures.next(t)
			captures.assertNoMore(t)
			billing := finding8SystemBilling(t, wire.body)
			cc := finding8CCVersion(t, billing)
			if want := claudecode.CLIVersion + "." + claudecode.Fingerprint("  héllo 😀 世界  "); cc != want {
				t.Fatalf("cc_version = %q want %q; billing=%s", cc, want, billing)
			}
			finding8AssertMessagesUnchanged(t, wire.body, tc.wantMessages)
			assertSelectorUsage(t, tc.path, false, string(out))
		})
	}
}
