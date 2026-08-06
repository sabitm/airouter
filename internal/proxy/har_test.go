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
	"airouter/internal/harlog"
)

func TestPinnedRecorderUnaryBothLegs(t *testing.T) {
	var cap capturedUpstream
	base, token := setup(t, domain.ProtocolOpenAI, &cap)

	rec := harlog.New("test")
	// Wrap the proxy server with middleware-like pin: TraceInfo.HAR on each request.
	// setup already mounted bare proxy; build a new stack with pin injection.
	st := newTestStore(t)
	ctx := context.Background()
	upstream := newUpstream(t, &cap)
	t.Cleanup(upstream.Close)
	prov := &domain.Provider{Name: "p", BaseURL: upstream.URL, APIKey: "up-key", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "real-model", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tinfo := &TraceInfo{RequestID: "abc123", HAR: rec}
		r = r.WithContext(WithTraceInfo(r.Context(), tinfo))
		mux.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	body := `{"model":"default","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// Upstream leg only (ingress is recorded by server middleware, not proxy).
	pages, entries := rec.Stats()
	if pages != 1 || entries != 1 {
		t.Fatalf("stats pages=%d entries=%d", pages, entries)
	}
	data, err := rec.MarshalHAR()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Log struct {
			Entries []struct {
				PageRef string `json:"pageref"`
				Request struct {
					URL     string `json:"url"`
					Headers []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"headers"`
				} `json:"request"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Log.Entries[0].PageRef != "page_abc123" {
		t.Fatalf("pageref = %q", doc.Log.Entries[0].PageRef)
	}
	foundAuth := false
	for _, h := range doc.Log.Entries[0].Request.Headers {
		if strings.EqualFold(h.Name, "Authorization") && strings.Contains(h.Value, "up-key") {
			foundAuth = true
		}
	}
	if !foundAuth {
		t.Fatalf("upstream auth not in HAR: %+v", doc.Log.Entries[0].Request.Headers)
	}
	_ = base
	_ = token
}

func TestNoPinnedRecorderNoCapture(t *testing.T) {
	var cap capturedUpstream
	st := newTestStore(t)
	ctx := context.Background()
	upstream := newUpstream(t, &cap)
	t.Cleanup(upstream.Close)
	prov := &domain.Provider{Name: "p", BaseURL: upstream.URL, APIKey: "up-key", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "m", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	p := New(st, nil)
	p.Mount(mux)
	// No HAR on TraceInfo.
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(WithTraceInfo(r.Context(), &TraceInfo{RequestID: "x"}))
		mux.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(
		`{"model":"default","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// harRecorder nil path: recordUpstreamHAR no-ops; nothing to assert beyond success.
	if harRecorder(context.Background()) != nil {
		t.Fatal("expected nil without TraceInfo")
	}
}

func TestStreamingHARFinalizeOnClose(t *testing.T) {
	// Direct unit test of harCaptureBody Close recording.
	rec := harlog.New("s")
	ctx := WithTraceInfo(context.Background(), &TraceInfo{RequestID: "stream1", HAR: rec})
	pr, pw := io.Pipe()
	body := &harCaptureBody{
		rc:      pr,
		started: time.Now(),
		method:  "POST",
		url:     "https://up.example/v1/chat/completions",
		reqHdr:  http.Header{"Authorization": []string{"Bearer secret"}},
		reqBody: []byte(`{"stream":true}`),
		status:  200,
		respHdr: http.Header{"Content-Type": []string{"text/event-stream"}},
		mime:    "text/event-stream",
		record:  (&Proxy{}).recordUpstreamHAR,
		ctx:     ctx,
	}
	go func() {
		_, _ = pw.Write([]byte("data: hi\n\n"))
		_ = pw.Close()
	}()
	buf, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != "data: hi\n\n" {
		t.Fatalf("relayed = %q", buf)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	// Second close is safe.
	_ = body.Close()
	pages, entries := rec.Stats()
	if pages != 1 || entries != 1 {
		t.Fatalf("stats pages=%d entries=%d", pages, entries)
	}
	data, _ := rec.MarshalHAR()
	if !strings.Contains(string(data), "data: hi") {
		t.Fatalf("missing stream body in HAR: %s", data)
	}
	if !strings.Contains(string(data), "Bearer secret") {
		t.Fatalf("missing auth in HAR")
	}
}
