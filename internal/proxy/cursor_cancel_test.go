package proxy

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/cursor"
)

// cursorCancelFixture wires a proxy in front of a stalling duplex upstream.
type cursorCancelFixture struct {
	ts          *httptest.Server
	upstreamURL string
	token       string
	handlerDone chan struct{}
}

// newCursorCancelFixture starts an h2 upstream that answers with sendFrames
// (optional early deltas so the proxy commits response bytes) and then stalls
// forever, plus a proxy server in front of it.
func newCursorCancelFixture(t *testing.T, sendFrames func(w http.Flusher)) *cursorCancelFixture {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()

	stall := make(chan struct{})
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		go func() {
			buf := make([]byte, 4096)
			for {
				if _, err := r.Body.Read(buf); err != nil {
					return
				}
			}
		}()
		w.Header().Set("Content-Type", cursor.ConnectContentType)
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		if sendFrames != nil {
			sendFrames(fl)
		}
		<-stall
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { close(stall) })

	prov := &domain.Provider{
		Name: "cursor", BaseURL: upstream.URL, Protocol: domain.ProtocolCursor,
		AuthMethod: domain.AuthAPIKey, APIKey: "agent-tok",
		OAuthCreds: &domain.OAuthCreds{CursorAuth: true, MachineID: "m-1"},
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "default", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	var once sync.Once
	handlerDone := make(chan struct{})
	mux := http.NewServeMux()
	p := New(st, nil)
	// The duplex upstream is the httptest TLS server (self-signed); route the
	// proxy's stream client through an h2-capable transport that trusts it.
	p.streamClient = &http.Client{Transport: &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, // self-signed httptest cert
		TLSHandshakeTimeout: 10 * time.Second,
	}}
	p.Mount(mux)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		once.Do(func() { close(handlerDone) })
	}))
	t.Cleanup(ts.Close)
	return &cursorCancelFixture{ts: ts, upstreamURL: upstream.URL, token: key.Token, handlerDone: handlerDone}
}

func (f *cursorCancelFixture) request(ctx context.Context) (*http.Request, error) {
	body := strings.NewReader(`{"model":"default","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.ts.URL+"/v1/chat/completions", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	return req, nil
}

// TestCursorAgentCancelBeforeHeaders cancels the client while the proxy is
// still waiting for the first upstream byte (no response bytes committed).
// The HTTP/2 client cannot cancel a duplex read while the request body is
// open, so without an explicit unblock the handler would hang forever and pin
// any HAR lease.
func TestCursorAgentCancelBeforeHeaders(t *testing.T) {
	f := newCursorCancelFixture(t, nil)

	reqCtx, cancel := context.WithCancel(context.Background())
	req, err := f.request(reqCtx)
	if err != nil {
		t.Fatal(err)
	}
	doDone := make(chan struct{})
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		close(doDone)
	}()

	// Let the proxy reach the stalled upstream read, then disconnect.
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-doDone

	select {
	case <-f.handlerDone:
		t.Log("handler returned after cancel")
	case <-time.After(10 * time.Second):
		t.Fatal("proxy handler did not return after client cancel before headers; HAR lease would stay in-flight")
	}
}

// TestCursorAgentCancelMidStream cancels the client after the proxy committed
// response headers and one delta (the Pi Esc-during-stream case).
func TestCursorAgentCancelMidStream(t *testing.T) {
	f := newCursorCancelFixture(t, func(fl http.Flusher) {
		writeTextDelta(fl.(io.Writer), "partial")
	})

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := f.request(reqCtx)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case <-f.handlerDone:
		t.Log("handler returned after cancel")
	case <-time.After(10 * time.Second):
		t.Fatal("proxy handler did not return after mid-stream client cancel; HAR lease would stay in-flight")
	}
}
