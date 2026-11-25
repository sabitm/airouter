package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGrokBuildSilentReasoningNativeUnaryAndStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(silentNativeUpstreamSSE()))
	}))
	t.Cleanup(upstream.Close)
	base, token := grokBuildProxy(t, upstream.URL)
	for _, stream := range []bool{false, true} {
		body := `{"model":"default","input":"hi","stream":false}`
		if stream {
			body = `{"model":"default","input":"hi","stream":true}`
		}
		resp, out := post(t, base+"/v1/responses", token, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream=%v status %d %s", stream, resp.StatusCode, out)
		}
		text := string(out)
		if strings.Contains(text, "conflicting") || strings.Contains(text, "pending stream lifecycle") {
			t.Fatalf("stream=%v native order failed: %s", stream, text)
		}
		if !strings.Contains(text, "fc_a") || !strings.Contains(text, "rs_silent") || !strings.Contains(text, "call_a") {
			t.Fatalf("stream=%v missing native items: %s", stream, text)
		}
	}
}

func silentNativeUpstreamSSE() string {
	return strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_silent"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}","status":"completed"}}`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","id":"rs_silent","status":"completed","encrypted_content":"cipher","summary":[]}}`,
		`data: {"type":"response.completed","response":{"id":"resp_silent","status":"completed","output":[{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}"},{"type":"reasoning","id":"rs_silent","encrypted_content":"cipher","summary":[]}],"usage":{"input_tokens":1,"output_tokens":2}}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
}
