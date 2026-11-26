package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
)

// codexReplayUpstream is a mock Codex upstream that records each request body
// and streams a scripted Responses SSE sequence.
type codexReplayUpstream struct {
	mu     sync.Mutex
	bodies [][]byte
	script func(request int, w http.ResponseWriter)
}

func (u *codexReplayUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.bodies = append(u.bodies, body)
	n := len(u.bodies)
	u.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if u.script != nil {
		u.script(n, w)
	}
}

func (u *codexReplayUpstream) body(i int) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if i < 0 || i >= len(u.bodies) {
		return ""
	}
	return string(u.bodies[i])
}

func (u *codexReplayUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.bodies)
}

// sseFrame writes one Responses SSE frame with a data-only payload.
func sseFrame(w io.Writer, typ, fields string) {
	fmt.Fprintf(w, "event: %s\ndata: {\"type\":%q,%s}\n\n", typ, typ, fields)
}

// codexReplayScript streams reasoning, a tool call, and a completion. The
// reasoning completion carries cipher only when doneCipher is set; the terminal
// snapshot carries cipher only when terminalCipher is set. When interleaved is
// set, the tool starts before the reasoning completion arrives unless
// reasoningDoneBeforeTool confirms the reasoning position first.
type codexReplayScript struct {
	cipher                  string
	doneCipher              bool
	terminalCipher          bool
	provisional             bool
	interleaved             bool
	silent                  bool
	reasoningDoneBeforeTool bool
}

const codexReplayToolItem = `{"type":"function_call","id":"fc_a","call_id":"call_a","name":"f","arguments":"{\"x\":1}"}`

func (s codexReplayScript) reasoningItem(withCipher bool) string {
	summary := `[{"type":"summary_text","text":"readable summary"}]`
	if s.silent {
		summary = `[]`
	}
	item := `{"type":"reasoning","id":"rs_a","summary":` + summary + `}`
	if !withCipher {
		return item
	}
	return item[:len(item)-1] + `,"encrypted_content":"` + s.cipher + `"}`
}

func (s codexReplayScript) serve(n int, w http.ResponseWriter) {
	sseFrame(w, "response.created", `"response":{"id":"resp_a","model":"gpt-5-codex","created_at":1767000000,"status":"in_progress"}`)
	if n > 1 {
		sseFrame(w, "response.completed", `"response":{"id":"resp_a","status":"completed","output":[]}`)
		return
	}
	added := `{"type":"reasoning","id":"rs_a","summary":[]}`
	if s.provisional {
		added = `{"type":"reasoning","id":"rs_a","summary":[],"encrypted_content":"provisional"}`
	}
	sseFrame(w, "response.output_item.added", `"output_index":0,"item":`+added)
	if !s.silent {
		sseFrame(w, "response.reasoning_summary_text.delta", `"output_index":0,"item_id":"rs_a","summary_index":0,"delta":"readable summary"`)
	}
	if s.reasoningDoneBeforeTool {
		sseFrame(w, "response.output_item.done", `"output_index":0,"item":`+s.reasoningItem(s.doneCipher))
	}
	if s.interleaved {
		sseFrame(w, "response.output_item.added", `"output_index":1,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"f","arguments":""}`)
		sseFrame(w, "response.function_call_arguments.delta", `"output_index":1,"item_id":"fc_a","delta":"{\"x\":"`)
	}
	if !s.reasoningDoneBeforeTool {
		sseFrame(w, "response.output_item.done", `"output_index":0,"item":`+s.reasoningItem(s.doneCipher))
	}
	if s.interleaved {
		sseFrame(w, "response.function_call_arguments.delta", `"output_index":1,"item_id":"fc_a","delta":"1}"`)
	}
	sseFrame(w, "response.output_item.done", `"output_index":1,"item":`+codexReplayToolItem)
	sseFrame(w, "response.completed", `"response":{"id":"resp_a","status":"completed","output":[`+
		s.reasoningItem(s.terminalCipher)+`,`+codexReplayToolItem+`],"usage":{"input_tokens":2,"output_tokens":3}}`)
}

// codexDoneItems returns the output_item.done items in wire order and the
// terminal output array, both decoded from the client SSE body.
func codexDoneItems(t *testing.T, body string) ([]map[string]any, []map[string]any, []string) {
	t.Helper()
	var items, terminal []map[string]any
	var types []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var frame struct {
			Type string         `json:"type"`
			Item map[string]any `json:"item"`
			Resp struct {
				Output []map[string]any `json:"output"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatalf("bad frame: %v: %s", err, line)
		}
		switch frame.Type {
		case "response.output_item.done":
			typ, _ := frame.Item["type"].(string)
			types = append(types, typ)
			items = append(items, frame.Item)
		case "response.completed":
			terminal = frame.Resp.Output
		}
	}
	return items, terminal, types
}

func codexEncrypted(item map[string]any) (string, bool) {
	value, ok := item["encrypted_content"]
	if !ok {
		return "", false
	}
	text, _ := value.(string)
	return text, true
}

// TestCodexReplayTwoTurnLoopback runs the official Codex replay shape: the
// client keeps only output_item.done items, then sends them back with a
// matching function_call_output. The second upstream body must carry the exact
// reconciled reasoning item.
func TestCodexReplayTwoTurnLoopback(t *testing.T) {
	const cipher = "cipher-reconciled"
	cases := []struct {
		name string
		codexReplayScript
		wantCipher bool
	}{
		{name: "tool starts before reasoning done", codexReplayScript: codexReplayScript{cipher: cipher, doneCipher: true, terminalCipher: true, interleaved: true}, wantCipher: true},
		{name: "done without cipher then terminal provides it", codexReplayScript: codexReplayScript{cipher: cipher, terminalCipher: true, interleaved: true}, wantCipher: true},
		{name: "silent reasoning before a tool", codexReplayScript: codexReplayScript{cipher: cipher, terminalCipher: true, interleaved: true, silent: true}, wantCipher: true},
		{name: "ordered completion carries cipher", codexReplayScript: codexReplayScript{cipher: cipher, doneCipher: true, terminalCipher: true}, wantCipher: true},
		{name: "no authoritative cipher", codexReplayScript: codexReplayScript{cipher: cipher, interleaved: true}, wantCipher: false},
		{name: "provisional only is not promoted", codexReplayScript: codexReplayScript{cipher: cipher, provisional: true, interleaved: true}, wantCipher: false},
		{name: "provisional then final", codexReplayScript: codexReplayScript{cipher: cipher, provisional: true, doneCipher: true, terminalCipher: true, interleaved: true}, wantCipher: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &codexReplayUpstream{script: tc.codexReplayScript.serve}
			srv := httptest.NewServer(up)
			t.Cleanup(srv.Close)
			base, token, _, _ := setupResponsesRouting(t, []routingTarget{{protocol: domain.ProtocolOpenAICodex, url: srv.URL, model: "gpt-5-codex"}})
			resp, out := post(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"hi"}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("turn1 status=%d body=%s", resp.StatusCode, out)
			}
			items, terminal, types := codexDoneItems(t, string(out))
			if len(items) != 2 {
				t.Fatalf("done items = %d, want reasoning and tool: %s", len(items), out)
			}
			if len(types) != 2 || types[0] != "reasoning" || types[1] != "function_call" {
				t.Fatalf("done order = %v: %s", types, out)
			}
			reasoning := items[0]
			tool := items[1]
			if id, _ := reasoning["id"].(string); id != "rs_a" {
				t.Fatalf("reasoning id = %v", reasoning["id"])
			}
			wantSummary := `[{"text":"readable summary","type":"summary_text"}]`
			if tc.silent {
				wantSummary = `[]`
			}
			if summary, _ := json.Marshal(reasoning["summary"]); string(summary) != wantSummary {
				t.Fatalf("reasoning summary = %s, want %s", summary, wantSummary)
			}
			gotCipher, hasCipher := codexEncrypted(reasoning)
			if hasCipher != tc.wantCipher {
				t.Fatalf("reasoning done cipher present = %v, want %v: %v", hasCipher, tc.wantCipher, reasoning)
			}
			if tc.wantCipher && gotCipher != cipher {
				t.Fatalf("reasoning cipher = %q, want %q", gotCipher, cipher)
			}
			if hasCipher && gotCipher == "provisional" {
				t.Fatalf("provisional cipher was promoted: %v", reasoning)
			}
			if callID, _ := tool["call_id"].(string); callID != "call_a" {
				t.Fatalf("tool call id = %v", tool["call_id"])
			}
			if args, _ := tool["arguments"].(string); args != `{"x":1}` {
				t.Fatalf("tool args = %v", tool["arguments"])
			}
			if len(terminal) != 2 {
				t.Fatalf("terminal output = %v", terminal)
			}
			if got, present := codexEncrypted(terminal[0]); present != tc.wantCipher || (tc.wantCipher && got != cipher) {
				t.Fatalf("terminal reasoning = %v, want cipher %v", terminal[0], tc.wantCipher)
			}

			// Turn 2: replay exactly the collected done items, as the official
			// Codex client does, plus the tool result.
			reasoningJSON, _ := json.Marshal(reasoning)
			toolJSON, _ := json.Marshal(tool)
			input := fmt.Sprintf(`"input":[{"type":"message","role":"user","content":"hi"},%s,%s,{"type":"function_call_output","call_id":"call_a","output":"tool result"}]`, reasoningJSON, toolJSON)
			resp, out = post(t, base+"/v1/responses", token, `{"model":"default","stream":true,`+input+`}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("turn2 status=%d body=%s", resp.StatusCode, out)
			}
			if up.count() != 2 {
				t.Fatalf("upstream calls = %d, want 2", up.count())
			}
			second := up.body(1)
			if !strings.Contains(second, `"id":"rs_a"`) || !strings.Contains(second, `"summary":`+wantSummary) {
				t.Fatalf("turn2 upstream lost the reasoning item: %s", second)
			}
			if tc.wantCipher != strings.Contains(second, `"encrypted_content":"`+cipher+`"`) {
				t.Fatalf("turn2 cipher presence = %v, want %v: %s", !tc.wantCipher, tc.wantCipher, second)
			}
			if strings.Contains(second, "provisional") {
				t.Fatalf("turn2 upstream received provisional cipher: %s", second)
			}
			if !strings.Contains(second, `"call_id":"call_a"`) || !strings.Contains(second, `"output":"tool result"`) {
				t.Fatalf("turn2 upstream lost the tool result: %s", second)
			}
			reasoningAt := strings.Index(second, `"id":"rs_a"`)
			toolAt := strings.Index(second, `"id":"fc_a"`)
			resultAt := strings.Index(second, `"function_call_output"`)
			if reasoningAt < 0 || toolAt < 0 || resultAt < 0 || !(reasoningAt < toolAt && toolAt < resultAt) {
				t.Fatalf("turn2 input order is not reasoning, tool, result: %s", second)
			}
			if !strings.Contains(second, `"include":["reasoning.encrypted_content"]`) {
				t.Fatalf("turn2 lost the Codex include: %s", second)
			}
		})
	}
}

// TestCodexReplayIncrementalEmission proves an ordered healthy stream still
// writes frames to the client before the terminal snapshot. The upstream blocks
// its completion until the test observes the reasoning done frame.
func TestCodexReplayIncrementalEmission(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseTerminal := func() { once.Do(func() { close(release) }) }
	defer releaseTerminal()
	script := codexReplayScript{cipher: "cipher-inc", doneCipher: true, terminalCipher: true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flush := func() {
			if flusher != nil {
				flusher.Flush()
			}
		}
		sseFrame(w, "response.created", `"response":{"id":"resp_a","model":"gpt-5-codex","created_at":1767000000,"status":"in_progress"}`)
		sseFrame(w, "response.output_item.added", `"output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[]}`)
		sseFrame(w, "response.reasoning_summary_text.delta", `"output_index":0,"item_id":"rs_a","summary_index":0,"delta":"readable summary"`)
		sseFrame(w, "response.output_item.done", `"output_index":0,"item":`+script.reasoningItem(true))
		flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		sseFrame(w, "response.completed", `"response":{"id":"resp_a","status":"completed","output":[`+script.reasoningItem(true)+`]}`)
		flush()
	}))
	t.Cleanup(srv.Close)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{{protocol: domain.ProtocolOpenAICodex, url: srv.URL, model: "gpt-5-codex"}})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/responses", strings.NewReader(`{"model":"default","stream":true,"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Read until the reasoning done frame arrives, before the upstream is
	// allowed to send its completion.
	var seen strings.Builder
	buf := make([]byte, 512)
	for !strings.Contains(seen.String(), `"type":"response.output_item.done"`) {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			seen.Write(buf[:n])
		}
		if err != nil {
			t.Fatalf("read before terminal: %v: %s", err, seen.String())
		}
	}
	if !strings.Contains(seen.String(), `"encrypted_content":"cipher-inc"`) {
		t.Fatalf("reasoning done lacked cipher before terminal: %s", seen.String())
	}
	releaseTerminal()
	rest, _ := io.ReadAll(resp.Body)
	all := seen.String() + string(rest)
	items, _, _ := codexDoneItems(t, all)
	if len(items) != 1 {
		t.Fatalf("reasoning done frames = %d: %s", len(items), all)
	}
	if !strings.Contains(all, `"type":"response.completed"`) {
		t.Fatalf("stream did not complete: %s", all)
	}
}

// TestCodexReconcileGateIsNarrow proves the reconcile policy applies only to
// the Responses ingress with a Codex backend. A different Responses-family
// backend with the same late-cipher shape keeps its existing frames: the
// reasoning completion still closes at its stream position.
func TestCodexReconcileGateIsNarrow(t *testing.T) {
	const cipher = "cipher-late"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseFrame(w, "response.created", `"response":{"id":"resp_a","model":"m","created_at":1767000000,"status":"in_progress"}`)
		sseFrame(w, "response.output_item.added", `"output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[]}`)
		sseFrame(w, "response.reasoning_summary_text.delta", `"output_index":0,"item_id":"rs_a","summary_index":0,"delta":"s"`)
		sseFrame(w, "response.output_item.done", `"output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[{"type":"summary_text","text":"s"}]}`)
		sseFrame(w, "response.output_item.added", `"output_index":1,"item":`+codexReplayToolItem)
		sseFrame(w, "response.output_item.done", `"output_index":1,"item":`+codexReplayToolItem)
		sseFrame(w, "response.completed", `"response":{"id":"resp_a","status":"completed","output":[{"type":"reasoning","id":"rs_a","summary":[{"type":"summary_text","text":"s"}],"encrypted_content":"`+cipher+`"},`+codexReplayToolItem+`]}`)
	}))
	t.Cleanup(srv.Close)
	base, token, _, _ := setupResponsesRouting(t, []routingTarget{{protocol: domain.ProtocolOpenAIResponses, url: srv.URL, model: "m"}})
	resp, out := post(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	items, _, types := codexDoneItems(t, string(out))
	if len(items) != 2 {
		t.Fatalf("done items = %d: %s", len(items), out)
	}
	if types[0] != "reasoning" {
		t.Fatalf("plain Responses reasoning moved behind the tool: %v", types)
	}
	if _, present := codexEncrypted(items[0]); present {
		t.Fatalf("plain Responses promoted late cipher: %v", items[0])
	}
}

// TestCodexReconcileCapAndFailureStayPostCommit proves the new hold obeys the
// existing commit rules: once reasoning bytes are visible the stream is
// committed, so an encoder cap or a decode failure terminates with an ingress
// error frame and never contacts the next target.
func TestCodexReconcileCapAndFailureStayPostCommit(t *testing.T) {
	overflow := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseFrame(w, "response.created", `"response":{"id":"resp_a","model":"m","created_at":1767000000,"status":"in_progress"}`)
		sseFrame(w, "response.output_item.added", `"output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[]}`)
		sseFrame(w, "response.reasoning_summary_text.delta", `"output_index":0,"item_id":"rs_a","summary_index":0,"delta":"visible"`)
		for i := 0; i < 1100; i++ {
			sseFrame(w, "response.output_text.delta", fmt.Sprintf(`"output_index":1,"item_id":"msg_a","content_index":0,"delta":"x"`))
		}
	}
	broken := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseFrame(w, "response.created", `"response":{"id":"resp_a","model":"m","created_at":1767000000,"status":"in_progress"}`)
		sseFrame(w, "response.output_item.added", `"output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[]}`)
		sseFrame(w, "response.reasoning_summary_text.delta", `"output_index":0,"item_id":"rs_a","summary_index":0,"delta":"visible"`)
		sseFrame(w, "response.output_item.done", `"output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[{"type":"summary_text","text":"visible"}]}`)
		sseFrame(w, "response.output_item.added", `"output_index":1,"item":`+codexReplayToolItem)
		// Malformed reasoning field after commitment: a protocol failure, not a
		// successful partial replay.
		fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":2,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_bad\",\"encrypted_content\":123,\"summary\":[]}}\n\n")
	}
	for _, tc := range []struct {
		name   string
		script func(http.ResponseWriter)
	}{
		{name: "pending cap", script: overflow},
		{name: "decode failure while holding", script: broken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := tc.script
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				script(w)
			}))
			t.Cleanup(first.Close)
			var secondHits atomic.Int64
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondHits.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				sseFrame(w, "response.completed", `"response":{"id":"resp_b","status":"completed","output":[]}`)
			}))
			t.Cleanup(second.Close)
			base, token, _, _ := setupResponsesRouting(t, []routingTarget{
				{protocol: domain.ProtocolOpenAICodex, url: first.URL, model: "gpt-5-codex"},
				{protocol: domain.ProtocolOpenAICodex, url: second.URL, model: "gpt-5-codex"},
			})
			resp, out := post(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"hi"}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			if hits := secondHits.Load(); hits != 0 {
				t.Fatalf("post-commit failure failed over: %d", hits)
			}
			text := string(out)
			if !strings.Contains(text, `"type":"reasoning"`) {
				t.Fatalf("visible reasoning prefix lost: %s", text)
			}
			if !strings.Contains(text, `"type":"error"`) {
				t.Fatalf("stream did not terminate with an error frame: %s", text)
			}
			if strings.Contains(text, `"type":"response.completed"`) {
				t.Fatalf("failed stream ended as completed: %s", text)
			}
			if strings.Contains(text, "rs_bad") {
				t.Fatalf("malformed item leaked: %s", text)
			}
		})
	}
}
