package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/sse"
)

const codexCacheUsageSSE = `event: response.created
data: {"type":"response.created","response":{"id":"resp_cache","model":"up","status":"in_progress"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"ok"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_cache","model":"up","status":"completed","usage":{"input_tokens":2600,"output_tokens":300,"total_tokens":2900,"input_tokens_details":{"cached_tokens":2000,"cache_write_tokens":400}}}}

`

const codexCacheUsageUnaryJSON = `{"id":"resp_cache","object":"response","status":"completed","model":"up","output":[{"type":"message","id":"msg_cache","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":2600,"output_tokens":300,"total_tokens":2900,"input_tokens_details":{"cached_tokens":2000,"cache_write_tokens":400}}}`

func TestCodexCacheUsageChatStream(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, codexCacheUsageSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)
	prov := &domain.Provider{Name: "codex", BaseURL: upstream.URL, APIKey: "k", Protocol: domain.ProtocolOpenAICodex}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Targets: []domain.ComboTarget{
		{ProviderID: prov.ID, UpstreamModel: "gpt-5.3-codex", Enabled: true},
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

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	in, out, total, cached, write := collectOpenAIUsageDetails(t, body)
	if in != 2600 || out != 300 || total != 2900 {
		t.Errorf("usage = %d/%d/%d, want 2600/300/2900", in, out, total)
	}
	if cached != 2000 || write != 400 {
		t.Errorf("details = cached %d write %d, want 2000/400", cached, write)
	}
	l := waitForLogs(t, st, 1)[0]
	if l.InputTokens != 2600 || l.OutputTokens != 300 {
		t.Errorf("logged tokens = %d/%d, want inclusive 2600/300", l.InputTokens, l.OutputTokens)
	}
}

func TestCodexCacheUsageChatUnaryCollected(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "stream false", body: `{"model":"default","stream":false,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "stream omitted", body: `{"model":"default","messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			gotStream := make(chan bool, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "read body", http.StatusBadRequest)
					return
				}
				var req struct {
					Stream *bool `json:"stream"`
				}
				if err := json.Unmarshal(body, &req); err != nil {
					http.Error(w, "invalid json", http.StatusBadRequest)
					return
				}
				stream := req.Stream != nil && *req.Stream
				select {
				case gotStream <- stream:
				default:
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, codexCacheUsageSSE)
					w.(http.Flusher).Flush()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, codexCacheUsageUnaryJSON)
			}))
			t.Cleanup(upstream.Close)
			prov := &domain.Provider{Name: "codex", BaseURL: upstream.URL, APIKey: "k", Protocol: domain.ProtocolOpenAICodex}
			if err := st.CreateProvider(ctx, prov); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Targets: []domain.ComboTarget{
				{ProviderID: prov.ID, UpstreamModel: "gpt-5.3-codex", Enabled: true},
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

			resp, out := post(t, ts.URL+"/v1/chat/completions", key.Token, tc.body)
			select {
			case stream := <-gotStream:
				if !stream {
					t.Fatalf("upstream stream = false, want true; client status = %d body = %s", resp.StatusCode, out)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("timed out waiting for upstream stream field; client status = %d body = %s", resp.StatusCode, out)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
			}
			var got struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
					TotalTokens      int `json:"total_tokens"`
					Details          *struct {
						Cached     int `json:"cached_tokens"`
						CacheWrite int `json:"cache_write_tokens"`
					} `json:"prompt_tokens_details"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Choices) == 0 || got.Choices[0].Message.Content != "ok" {
				t.Fatalf("assistant text = %+v body=%s", got.Choices, out)
			}
			if got.Usage == nil || got.Usage.PromptTokens != 2600 || got.Usage.CompletionTokens != 300 || got.Usage.TotalTokens != 2900 {
				t.Fatalf("usage = %+v body=%s", got.Usage, out)
			}
			if got.Usage.Details == nil || got.Usage.Details.Cached != 2000 || got.Usage.Details.CacheWrite != 400 {
				t.Fatalf("details = %+v", got.Usage.Details)
			}
			l := waitForLogs(t, st, 1)[0]
			if l.InputTokens != 2600 || l.OutputTokens != 300 {
				t.Errorf("logged tokens = %d/%d, want inclusive 2600/300", l.InputTokens, l.OutputTokens)
			}
		})
	}
}

func collectOpenAIUsageDetails(t *testing.T, body string) (in, out, total, cached, write int) {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if string(ev.Data) == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct{} `json:"choices"`
			Usage   *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
				Details          *struct {
					Cached     int `json:"cached_tokens"`
					CacheWrite int `json:"cache_write_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal(ev.Data, &chunk) != nil || chunk.Usage == nil || len(chunk.Choices) != 0 {
			continue
		}
		in, out, total = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, chunk.Usage.TotalTokens
		if chunk.Usage.Details != nil {
			cached, write = chunk.Usage.Details.Cached, chunk.Usage.Details.CacheWrite
		}
		return
	}
	return
}
