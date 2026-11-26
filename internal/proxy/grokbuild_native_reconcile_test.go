package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/grokbuild"
)

// grokReplayScript returns the first scripted Grok turn and a clean second
// turn with its own usage so a done-only client replay can be checked end to
// end without borrowing Codex response semantics.
func grokReplayScript(script codexReplayScript) func(int, http.ResponseWriter) {
	return func(n int, w http.ResponseWriter) {
		if n == 1 {
			script.serve(n, w)
			return
		}
		sseFrame(w, "response.created", `"response":{"id":"resp_b","model":"grok-build","created_at":1767000001,"status":"in_progress"}`)
		sseFrame(w, "response.completed", `"response":{"id":"resp_b","model":"grok-build","created_at":1767000001,"status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":7}}`)
	}
}

func completedUsage(t *testing.T, body string) (int, int) {
	t.Helper()
	for _, ev := range collectSSEEvents(t, body) {
		var frame struct {
			Type     string `json:"type"`
			Response struct {
				Usage struct {
					Input  int `json:"input_tokens"`
					Output int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(ev.Data, &frame) != nil || frame.Type != "response.completed" {
			continue
		}
		return frame.Response.Usage.Input, frame.Response.Usage.Output
	}
	t.Fatalf("stream has no response.completed usage: %s", body)
	return 0, 0
}

func replayInput(t *testing.T, body string) []map[string]any {
	t.Helper()
	var doc struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("bad upstream body: %v: %s", err, body)
	}
	return doc.Input
}

func inputItem(items []map[string]any, typ, id string) (int, map[string]any) {
	for i, item := range items {
		itemType, _ := item["type"].(string)
		if itemType != typ {
			continue
		}
		if id != "" {
			itemID, _ := item["id"].(string)
			if itemID != id {
				continue
			}
		}
		return i, item
	}
	return -1, nil
}

func assertIncreasing(t *testing.T, label string, values []int) {
	t.Helper()
	for i := 1; i < len(values); i++ {
		if values[i] <= values[i-1] {
			t.Fatalf("%s order is not increasing: %v", label, values)
		}
	}
}

func summarySemantic(t *testing.T, value any, want string) {
	t.Helper()
	raw, _ := json.Marshal(value)
	var gotValue, wantValue any
	if json.Unmarshal(raw, &gotValue) != nil || json.Unmarshal([]byte(want), &wantValue) != nil ||
		!reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("summary = %s, want %s", raw, want)
	}
}

// TestGrokBuildNativeDoneOnlyReplay keeps only output_item.done items from a
// Grok Build Responses stream, replays them with a function result, and checks
// the second upstream body. It covers late terminal ciphertext, an early
// authoritative completion, explicit empty, absent and provisional-only bytes,
// and silent reasoning.
func TestGrokBuildNativeDoneOnlyReplay(t *testing.T) {
	const summary = `[{"type":"summary_text","text":"readable summary"}]`
	cases := []struct {
		name        string
		script      codexReplayScript
		wantCipher  string
		hasCipher   bool
		wantSummary string
	}{
		{
			name:        "terminal only interleaved",
			script:      codexReplayScript{cipher: "cipher-terminal", terminalCipher: true, interleaved: true},
			wantCipher:  "cipher-terminal",
			hasCipher:   true,
			wantSummary: summary,
		},
		{
			name:        "early done interleaved",
			script:      codexReplayScript{cipher: "cipher-early", doneCipher: true, terminalCipher: true, interleaved: true},
			wantCipher:  "cipher-early",
			hasCipher:   true,
			wantSummary: summary,
		},
		{
			name:        "explicit empty",
			script:      codexReplayScript{cipher: "", terminalCipher: true, interleaved: true},
			wantCipher:  "",
			hasCipher:   true,
			wantSummary: summary,
		},
		{
			name:        "absent",
			script:      codexReplayScript{cipher: "cipher-unused", interleaved: true},
			wantSummary: summary,
		},
		{
			name:        "provisional only",
			script:      codexReplayScript{cipher: "cipher-unused", provisional: true, interleaved: true},
			wantSummary: summary,
		},
		{
			name:        "silent terminal only",
			script:      codexReplayScript{cipher: "cipher-silent", terminalCipher: true, interleaved: true, silent: true, reasoningDoneBeforeTool: true},
			wantCipher:  "cipher-silent",
			hasCipher:   true,
			wantSummary: `[]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &codexReplayUpstream{script: grokReplayScript(tc.script)}
			srv := httptest.NewServer(up)
			t.Cleanup(srv.Close)
			base, token := grokBuildProxy(t, srv.URL)

			body := `{"model":"default","input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"stream":true}`
			resp, out := postStream(t, base+"/v1/responses", token, body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("turn1 status=%d body=%s", resp.StatusCode, out)
			}
			items, terminal, types := codexDoneItems(t, out)
			if len(items) != 2 || strings.Join(types, ",") != "reasoning,function_call" {
				t.Fatalf("done items = %v, types = %v: %s", items, types, out)
			}
			reasoning, tool := items[0], items[1]
			if id, _ := reasoning["id"].(string); id != "rs_a" {
				t.Fatalf("reasoning id = %v", reasoning["id"])
			}
			summarySemantic(t, reasoning["summary"], tc.wantSummary)
			gotCipher, hasCipher := codexEncrypted(reasoning)
			if hasCipher != tc.hasCipher || (tc.hasCipher && gotCipher != tc.wantCipher) {
				t.Fatalf("reasoning done cipher = %q, present %v, want %q/%v: %v", gotCipher, hasCipher, tc.wantCipher, tc.hasCipher, reasoning)
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
			terminalCipher, terminalPresent := codexEncrypted(terminal[0])
			if terminalPresent != tc.hasCipher || (tc.hasCipher && terminalCipher != tc.wantCipher) {
				t.Fatalf("terminal reasoning = %v, want cipher %q/%v", terminal[0], tc.wantCipher, tc.hasCipher)
			}
			if strings.Contains(out, "provisional") {
				t.Fatalf("provisional ciphertext leaked: %s", out)
			}
			if in, outTok := completedUsage(t, out); in != 2 || outTok != 3 {
				t.Fatalf("turn1 usage = %d/%d, want 2/3: %s", in, outTok, out)
			}

			reasoningJSON, _ := json.Marshal(reasoning)
			toolJSON, _ := json.Marshal(tool)
			input := fmt.Sprintf(`"input":[{"type":"message","role":"user","content":"hi"},%s,%s,{"type":"function_call_output","call_id":"call_a","output":"tool result"}]`, reasoningJSON, toolJSON)
			resp, out2 := postStream(t, base+"/v1/responses", token, `{"model":"default","stream":true,"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],`+input+`}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("turn2 status=%d body=%s", resp.StatusCode, out2)
			}
			if up.count() != 2 {
				t.Fatalf("upstream calls = %d, want 2", up.count())
			}
			if in, outTok := completedUsage(t, out2); in != 5 || outTok != 7 {
				t.Fatalf("turn2 usage = %d/%d, want 5/7: %s", in, outTok, out2)
			}
			second := replayInput(t, up.body(1))
			reasonIdx, replayed := inputItem(second, "reasoning", "rs_a")
			toolIdx, replayedTool := inputItem(second, "function_call", "fc_a")
			resultIdx, result := inputItem(second, "function_call_output", "")
			if reasonIdx < 0 || toolIdx < 0 || resultIdx < 0 {
				t.Fatalf("turn2 input lost items: %v", second)
			}
			assertIncreasing(t, "turn2 input", []int{reasonIdx, toolIdx, resultIdx})
			replayedCipher, replayedPresent := codexEncrypted(replayed)
			if replayedPresent != tc.hasCipher || (tc.hasCipher && replayedCipher != tc.wantCipher) {
				t.Fatalf("turn2 reasoning cipher = %q/%v, want %q/%v: %v", replayedCipher, replayedPresent, tc.wantCipher, tc.hasCipher, replayed)
			}
			summarySemantic(t, replayed["summary"], tc.wantSummary)
			if callID, _ := replayedTool["call_id"].(string); callID != "call_a" {
				t.Fatalf("turn2 tool call id = %v", replayedTool["call_id"])
			}
			if args, _ := replayedTool["arguments"].(string); args != `{"x":1}` {
				t.Fatalf("turn2 tool args = %v", replayedTool["arguments"])
			}
			if output, _ := result["output"].(string); output != "tool result" {
				t.Fatalf("turn2 tool result = %v", result["output"])
			}
		})
	}
}

func grokParallelReasoningSSE(cipher string) string {
	const summary = `[{"type":"summary_text","text":"plan"}]`
	return strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_p","model":"grok-build","created_at":1767000000,"status":"in_progress"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_p","status":"in_progress","summary":[]}}`,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs_p","summary_index":0,"delta":"plan"}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_a","name":"f","arguments":""}}`,
		`data: {"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"call_b","name":"f","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\":\"a\""}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"path\":\"b\""}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_p","status":"completed","summary":` + summary + `}}`,
		`data: {"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_b","call_id":"call_b","name":"f","arguments":"{\"path\":\"b\"}","status":"completed"}}`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"f","arguments":"{\"path\":\"a\"}","status":"completed"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_p","status":"completed","output":[{"type":"reasoning","id":"rs_p","status":"completed","summary":` + summary + `,"encrypted_content":"` + cipher + `"},{"type":"function_call","id":"fc_a","call_id":"call_a","name":"f","arguments":"{\"path\":\"a\"}","status":"completed"},{"type":"function_call","id":"fc_b","call_id":"call_b","name":"f","arguments":"{\"path\":\"b\"}","status":"completed"}],"usage":{"input_tokens":4,"output_tokens":5}}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
}

// TestGrokBuildNativeParallelDoneOnlyReplay checks two late tool ids whose
// done frames arrive in reverse order and retained argument fragments.
func TestGrokBuildNativeParallelDoneOnlyReplay(t *testing.T) {
	const cipher = "cipher-parallel"
	up := &codexReplayUpstream{script: func(n int, w http.ResponseWriter) {
		if n == 1 {
			_, _ = io.WriteString(w, grokParallelReasoningSSE(cipher))
			return
		}
		sseFrame(w, "response.created", `"response":{"id":"resp_b","model":"grok-build","created_at":1767000001,"status":"in_progress"}`)
		sseFrame(w, "response.completed", `"response":{"id":"resp_b","model":"grok-build","created_at":1767000001,"status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":7}}`)
	}}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	base, token := grokBuildProxy(t, srv.URL)
	resp, out := postStream(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("turn1 status=%d body=%s", resp.StatusCode, out)
	}
	items, terminal, types := codexDoneItems(t, out)
	if len(items) != 3 || strings.Join(types, ",") != "reasoning,function_call,function_call" {
		t.Fatalf("done items = %v, types = %v: %s", items, types, out)
	}
	reasoning, toolA, toolB := items[0], items[1], items[2]
	if got, present := codexEncrypted(reasoning); !present || got != cipher {
		t.Fatalf("reasoning cipher = %q/%v: %v", got, present, reasoning)
	}
	if summaryJSON, _ := json.Marshal(reasoning["summary"]); !strings.Contains(string(summaryJSON), `"text":"plan"`) {
		t.Fatalf("reasoning summary = %s", summaryJSON)
	}
	if id, _ := toolA["id"].(string); id != "fc_a" {
		t.Fatalf("first tool id = %v", toolA["id"])
	}
	if id, _ := toolB["id"].(string); id != "fc_b" {
		t.Fatalf("second tool id = %v", toolB["id"])
	}
	if callID, _ := toolA["call_id"].(string); callID != "call_a" {
		t.Fatalf("first call id = %v", toolA["call_id"])
	}
	if callID, _ := toolB["call_id"].(string); callID != "call_b" {
		t.Fatalf("second call id = %v", toolB["call_id"])
	}
	if args, _ := toolA["arguments"].(string); args != `{"path":"a"}` {
		t.Fatalf("first args = %v", toolA["arguments"])
	}
	if args, _ := toolB["arguments"].(string); args != `{"path":"b"}` {
		t.Fatalf("second args = %v", toolB["arguments"])
	}
	if len(terminal) != 3 {
		t.Fatalf("terminal output = %v", terminal)
	}
	if got, present := codexEncrypted(terminal[0]); !present || got != cipher {
		t.Fatalf("terminal reasoning = %v", terminal[0])
	}
	if in, outTok := completedUsage(t, out); in != 4 || outTok != 5 {
		t.Fatalf("turn1 usage = %d/%d, want 4/5", in, outTok)
	}

	var input []string
	for _, item := range items {
		raw, _ := json.Marshal(item)
		input = append(input, string(raw))
	}
	body := `{"model":"default","stream":true,"input":[{"type":"message","role":"user","content":"hi"},` +
		strings.Join(input, ",") + `,{"type":"function_call_output","call_id":"call_a","output":"a result"},{"type":"function_call_output","call_id":"call_b","output":"b result"}]}`
	resp, out2 := postStream(t, base+"/v1/responses", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("turn2 status=%d body=%s", resp.StatusCode, out2)
	}
	if up.count() != 2 {
		t.Fatalf("upstream calls = %d, want 2", up.count())
	}
	second := replayInput(t, up.body(1))
	reasonIdx, replayedReason := inputItem(second, "reasoning", "rs_p")
	toolAIdx, replayedA := inputItem(second, "function_call", "fc_a")
	toolBIdx, replayedB := inputItem(second, "function_call", "fc_b")
	resultAIdx, _ := inputItem(second, "function_call_output", "")
	if reasonIdx < 0 || toolAIdx < 0 || toolBIdx < 0 || resultAIdx < 0 {
		t.Fatalf("turn2 input lost items: %v", second)
	}
	assertIncreasing(t, "turn2 input", []int{reasonIdx, toolAIdx, toolBIdx, resultAIdx})
	if got, present := codexEncrypted(replayedReason); !present || got != cipher {
		t.Fatalf("turn2 reasoning cipher = %q/%v: %v", got, present, replayedReason)
	}
	if args, _ := replayedA["arguments"].(string); args != `{"path":"a"}` {
		t.Fatalf("turn2 first args = %v", replayedA["arguments"])
	}
	if args, _ := replayedB["arguments"].(string); args != `{"path":"b"}` {
		t.Fatalf("turn2 second args = %v", replayedB["arguments"])
	}
	if in, outTok := completedUsage(t, out2); in != 5 || outTok != 7 {
		t.Fatalf("turn2 usage = %d/%d, want 5/7", in, outTok)
	}
}

// TestGrokBuildNativeSilentReasoningLaterIndex covers an indexed silent
// placeholder at a later output index than an earlier tool. The placeholder
// must not establish a position on its added frame, so the tool keeps its
// native id and order; the reasoning completion takes the terminal bytes.
func TestGrokBuildNativeSilentReasoningLaterIndex(t *testing.T) {
	const cipher = "cipher-late-index"
	tool := `{"type":"function_call","id":"fc_a","call_id":"call_a","name":"f","arguments":"{\"x\":1}"}`
	up := &codexReplayUpstream{script: func(n int, w http.ResponseWriter) {
		if n > 1 {
			sseFrame(w, "response.created", `"response":{"id":"resp_b","model":"grok-build","created_at":1767000001,"status":"in_progress"}`)
			sseFrame(w, "response.completed", `"response":{"id":"resp_b","model":"grok-build","created_at":1767000001,"status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":7}}`)
			return
		}
		sseFrame(w, "response.created", `"response":{"id":"resp_l","model":"grok-build","created_at":1767000000,"status":"in_progress"}`)
		sseFrame(w, "response.output_item.added", `"output_index":1,"item":{"type":"reasoning","id":"rs_late","status":"in_progress","summary":[]}`)
		sseFrame(w, "response.output_item.added", `"output_index":0,"item":{"type":"function_call","call_id":"call_a","name":"f","arguments":""}`)
		sseFrame(w, "response.function_call_arguments.delta", `"output_index":0,"delta":"{\"x\":"}`)
		sseFrame(w, "response.function_call_arguments.delta", `"output_index":0,"delta":"1}"}`)
		sseFrame(w, "response.output_item.done", `"output_index":0,"item":`+tool)
		sseFrame(w, "response.output_item.done", `"output_index":1,"item":{"type":"reasoning","id":"rs_late","status":"completed","summary":[]}`)
		sseFrame(w, "response.completed", `"response":{"id":"resp_l","status":"completed","output":[`+tool+`,{"type":"reasoning","id":"rs_late","status":"completed","summary":[],"encrypted_content":"`+cipher+`"}],"usage":{"input_tokens":2,"output_tokens":3}}`)
	}}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	base, token := grokBuildProxy(t, srv.URL)

	resp, out := postStream(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("turn1 status=%d body=%s", resp.StatusCode, out)
	}
	items, terminal, types := codexDoneItems(t, out)
	if len(items) != 2 || strings.Join(types, ",") != "function_call,reasoning" {
		t.Fatalf("done items = %v, types = %v: %s", items, types, out)
	}
	toolItem, reasoning := items[0], items[1]
	if id, _ := toolItem["id"].(string); id != "fc_a" {
		t.Fatalf("tool item id = %v", toolItem["id"])
	}
	if callID, _ := toolItem["call_id"].(string); callID != "call_a" {
		t.Fatalf("tool call id = %v", toolItem["call_id"])
	}
	if args, _ := toolItem["arguments"].(string); args != `{"x":1}` {
		t.Fatalf("tool args = %v", toolItem["arguments"])
	}
	if id, _ := reasoning["id"].(string); id != "rs_late" {
		t.Fatalf("reasoning id = %v", reasoning["id"])
	}
	gotCipher, present := codexEncrypted(reasoning)
	if !present || gotCipher != cipher {
		t.Fatalf("reasoning done cipher = %q/%v: %v", gotCipher, present, reasoning)
	}
	if summary, _ := json.Marshal(reasoning["summary"]); string(summary) != `[]` {
		t.Fatalf("reasoning summary = %s, want []", summary)
	}
	if len(terminal) != 2 {
		t.Fatalf("terminal output = %v", terminal)
	}
	if got, present := codexEncrypted(terminal[1]); !present || got != cipher {
		t.Fatalf("terminal reasoning = %v", terminal[1])
	}
	if in, outTok := completedUsage(t, out); in != 2 || outTok != 3 {
		t.Fatalf("turn1 usage = %d/%d, want 2/3: %s", in, outTok, out)
	}

	toolJSON, _ := json.Marshal(toolItem)
	reasoningJSON, _ := json.Marshal(reasoning)
	input := fmt.Sprintf(`"input":[{"type":"message","role":"user","content":"hi"},%s,%s,{"type":"function_call_output","call_id":"call_a","output":"tool result"}]`, toolJSON, reasoningJSON)
	resp, out2 := postStream(t, base+"/v1/responses", token, `{"model":"default","stream":true,`+input+`}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("turn2 status=%d body=%s", resp.StatusCode, out2)
	}
	if up.count() != 2 {
		t.Fatalf("upstream calls = %d, want 2", up.count())
	}
	if in, outTok := completedUsage(t, out2); in != 5 || outTok != 7 {
		t.Fatalf("turn2 usage = %d/%d, want 5/7: %s", in, outTok, out2)
	}
	second := replayInput(t, up.body(1))
	toolIdx, replayedTool := inputItem(second, "function_call", "fc_a")
	reasonIdx, replayedReasoning := inputItem(second, "reasoning", "rs_late")
	resultIdx, result := inputItem(second, "function_call_output", "")
	if toolIdx < 0 || reasonIdx < 0 || resultIdx < 0 {
		t.Fatalf("turn2 input lost items: %v", second)
	}
	assertIncreasing(t, "turn2 input", []int{toolIdx, reasonIdx, resultIdx})
	if got, present := codexEncrypted(replayedReasoning); !present || got != cipher {
		t.Fatalf("turn2 reasoning cipher = %q/%v: %v", got, present, replayedReasoning)
	}
	if args, _ := replayedTool["arguments"].(string); args != `{"x":1}` {
		t.Fatalf("turn2 tool args = %v", replayedTool["arguments"])
	}
	if output, _ := result["output"].(string); output != "tool result" {
		t.Fatalf("turn2 tool result = %v", result["output"])
	}
}

// TestGrokBuildNativeEarlyDoneIncremental proves an authoritative early
// reasoning completion reaches the client before the terminal snapshot.
func TestGrokBuildNativeEarlyDoneIncremental(t *testing.T) {
	const cipher = "cipher-incremental"
	const summary = `[{"type":"summary_text","text":"s"}]`
	release := make(chan struct{})
	var once sync.Once
	releaseTerminal := func() { once.Do(func() { close(release) }) }
	defer releaseTerminal()
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
		sseFrame(w, "response.created", `"response":{"id":"resp_i","model":"grok-build","created_at":1767000000,"status":"in_progress"}`)
		sseFrame(w, "response.output_item.added", `"output_index":0,"item":{"type":"reasoning","id":"rs_i","status":"in_progress","summary":[]}`)
		sseFrame(w, "response.reasoning_summary_text.delta", `"output_index":0,"item_id":"rs_i","summary_index":0,"delta":"s"`)
		sseFrame(w, "response.output_item.done", `"output_index":0,"item":{"type":"reasoning","id":"rs_i","status":"completed","summary":`+summary+`,"encrypted_content":"`+cipher+`"}`)
		flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		sseFrame(w, "response.completed", `"response":{"id":"resp_i","status":"completed","output":[{"type":"reasoning","id":"rs_i","status":"completed","summary":`+summary+`,"encrypted_content":"`+cipher+`"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
		flush()
	}))
	t.Cleanup(srv.Close)
	base, token := grokBuildProxy(t, srv.URL)

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
	if !strings.Contains(seen.String(), `"encrypted_content":"`+cipher+`"`) {
		t.Fatalf("reasoning done lacked cipher before terminal: %s", seen.String())
	}
	releaseTerminal()
	rest, _ := io.ReadAll(resp.Body)
	all := seen.String() + string(rest)
	items, _, types := codexDoneItems(t, all)
	if len(items) != 1 || strings.Join(types, ",") != "reasoning" {
		t.Fatalf("done items = %v, types = %v: %s", items, types, all)
	}
	if !strings.Contains(all, `"type":"response.completed"`) {
		t.Fatalf("stream did not complete: %s", all)
	}
}

type grokRoutingTransport struct {
	bases []string
	mu    sync.Mutex
	next  int
}

func (g *grokRoutingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !grokbuild.IsFixedInferenceURL(r.URL.String()) {
		return nil, io.ErrUnexpectedEOF
	}
	g.mu.Lock()
	i := g.next
	if g.next < len(g.bases)-1 {
		g.next++
	}
	g.mu.Unlock()
	if i >= len(g.bases) {
		i = len(g.bases) - 1
	}
	u, err := url.Parse(g.bases[i] + r.URL.RequestURI())
	if err != nil {
		return nil, err
	}
	clone := r.Clone(r.Context())
	clone.URL = u
	clone.Host = u.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func grokBuildFailoverProxy(t *testing.T, upstreams ...string) (string, string) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()
	combo := &domain.Combo{Name: "default", Strategy: domain.StrategyFailover}
	for i := range upstreams {
		prov := &domain.Provider{
			Name: fmt.Sprintf("gb%d", i), BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
			AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
			OAuthCreds: &domain.OAuthCreds{
				GrokBuildAuth: true, Preset: grokbuild.Preset, ClientID: grokbuild.ClientID,
				TokenURL: grokbuild.TokenURL, AccessToken: fmt.Sprintf("access-%d", i),
				RefreshToken: fmt.Sprintf("refresh-%d", i), Scopes: grokbuild.Scopes,
				GrokBuildConnection: fmt.Sprintf("conn-%d", i),
			},
		}
		if err := st.CreateProvider(ctx, prov); err != nil {
			t.Fatal(err)
		}
		combo.Targets = append(combo.Targets, domain.ComboTarget{ProviderID: prov.ID, UpstreamModel: "grok-build", Enabled: true})
	}
	if err := st.CreateCombo(ctx, combo); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	px := New(st, nil)
	px.streamClient = &http.Client{Transport: &grokRoutingTransport{bases: upstreams}}
	px.client = px.streamClient
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token
}

func malformedReasoningDone(w http.ResponseWriter) {
	sseFrame(w, "response.created", `"response":{"id":"resp_bad","model":"grok-build","created_at":1767000000,"status":"in_progress"}`)
	sseFrame(w, "response.output_item.added", `"output_index":0,"item":{"type":"reasoning","id":"rs_bad","status":"in_progress","summary":[]}`)
	fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_bad\",\"encrypted_content\":123,\"summary\":[]}}\n\n")
}

// TestGrokBuildNativeHoldPendingCapPostCommit checks a confirmed reasoning
// completion without bytes. Once later text passes the shared hold bound, the
// committed stream ends with an error and never reports a successful
// completion. The cap is not raised.
func TestGrokBuildNativeHoldPendingCapPostCommit(t *testing.T) {
	const summary = `[{"type":"summary_text","text":"r"}]`
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseFrame(w, "response.created", `"response":{"id":"resp_cap","model":"grok-build","created_at":1767000000,"status":"in_progress"}`)
		sseFrame(w, "response.output_item.added", `"output_index":0,"item":{"type":"reasoning","id":"rs_cap","status":"in_progress","summary":[]}`)
		sseFrame(w, "response.reasoning_summary_text.delta", `"output_index":0,"item_id":"rs_cap","summary_index":0,"delta":"r"`)
		sseFrame(w, "response.output_item.done", `"output_index":0,"item":{"type":"reasoning","id":"rs_cap","status":"completed","summary":`+summary+`}`)
		for i := 0; i < 1025; i++ { // One event past the 1024-event hold cap.
			sseFrame(w, "response.output_text.delta", `"output_index":1,"item_id":"msg_b","content_index":0,"delta":"q"`)
		}
		sseFrame(w, "response.completed", `"response":{"id":"resp_cap","status":"completed","output":[{"type":"reasoning","id":"rs_cap","status":"completed","summary":`+summary+`,"encrypted_content":"cipher"}],"usage":{"input_tokens":2,"output_tokens":3}}`)
	}))
	t.Cleanup(first.Close)
	base, token := grokBuildProxy(t, first.URL)
	resp, out := postStream(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if !strings.Contains(out, `"type":"error"`) || !strings.Contains(out, "reasoning identity pending limit exceeded") {
		t.Fatalf("expected hold cap error: %s", out)
	}
	if strings.Contains(out, `"type":"response.completed"`) {
		t.Fatalf("successful completion after hold error: %s", out)
	}
}

// TestGrokBuildNativeReconcileFailureBoundary checks the existing commit rule
// on the Grok reconcile path: a pre-commit decode failure may fail over, while
// a failure after visible reasoning must terminate the stream without a second
// target or a successful completion.
func TestGrokBuildNativeReconcileFailureBoundary(t *testing.T) {
	cases := []struct {
		name       string
		first      func(http.ResponseWriter)
		wantSecond int64
		wantDone   bool
		wantError  bool
	}{
		{
			name:       "before commit fails over",
			first:      malformedReasoningDone,
			wantSecond: 1,
			wantDone:   true,
		},
		{
			name: "after visible reasoning stays post-commit",
			first: func(w http.ResponseWriter) {
				sseFrame(w, "response.created", `"response":{"id":"resp_bad","model":"grok-build","created_at":1767000000,"status":"in_progress"}`)
				sseFrame(w, "response.output_item.added", `"output_index":0,"item":{"type":"reasoning","id":"rs_bad","status":"in_progress","summary":[]}`)
				sseFrame(w, "response.reasoning_summary_text.delta", `"output_index":0,"item_id":"rs_bad","summary_index":0,"delta":"visible"`)
				fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_bad\",\"encrypted_content\":123,\"summary\":[]}}\n\n")
			},
			wantSecond: 0,
			wantError:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var firstHits, secondHits atomic.Int64
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				firstHits.Add(1)
				_, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				tc.first(w)
			}))
			t.Cleanup(first.Close)
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondHits.Add(1)
				_, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, grokBuildDoneSSE())
			}))
			t.Cleanup(second.Close)
			base, token := grokBuildFailoverProxy(t, first.URL, second.URL)
			resp, out := postStream(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"hi"}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			if firstHits.Load() != 1 || secondHits.Load() != tc.wantSecond {
				t.Fatalf("hits first=%d second=%d, want 1/%d", firstHits.Load(), secondHits.Load(), tc.wantSecond)
			}
			if !tc.wantError && strings.Contains(out, "rs_bad") {
				t.Fatalf("pre-commit failure leaked the malformed item: %s", out)
			}
			if tc.wantError && !strings.Contains(out, `"type":"reasoning"`) {
				t.Fatalf("post-commit failure lost the visible reasoning prefix: %s", out)
			}
			if got := strings.Contains(out, `"type":"error"`); got != tc.wantError {
				t.Fatalf("error frame = %v, want %v: %s", got, tc.wantError, out)
			}
			if got := strings.Contains(out, `"type":"response.completed"`); got == tc.wantError {
				t.Fatalf("completed frame = %v, want %v: %s", got, !tc.wantError, out)
			}
			if got := strings.Contains(out, "done"); got != tc.wantDone {
				t.Fatalf("fallback text = %v, want %v: %s", got, tc.wantDone, out)
			}
		})
	}
}
