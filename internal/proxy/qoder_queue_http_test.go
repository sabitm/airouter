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
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/qoder"
	"airouter/internal/proxy/sse"
)

// qoderPoll records one queue-status poll.
type qoderPoll struct {
	path      string
	requestID string
	sessionID string
}

// qoderChat records one inference send.
type qoderChat struct {
	body string
	auth string
}

type qoderUpstream struct {
	mu sync.Mutex

	chats []qoderChat
	polls []qoderPoll

	// script: canned responses consumed in order; the last repeats forever.
	chatResponses []qoderResponse
	pollStatus    int
	pollBody      string
	pollStall     time.Duration
	// pollScript, when set, overrides the static pollStatus/pollBody pair:
	// entries are consumed in order and the last repeats forever.
	pollScript []qoderPollResponse
}

// qoderPollResponse is one scripted queue-status answer.
type qoderPollResponse struct {
	status int
	body   string
}

type qoderResponse struct {
	status int
	sse    string
	// tail arrives after tailDelay, keeping the stream open past any queue
	// budget that already expired.
	tail      string
	tailDelay time.Duration
	// stallHeaders never writes a response; stallFirstFrame writes headers
	// but no body. Both block until the proxy gives up on the request.
	// bodyDelay pauses between headers and body, so a canceled request
	// context reliably wins the race against the error-body bytes.
	stallHeaders    bool
	stallFirstFrame bool
	bodyDelay       time.Duration
}

func (u *qoderUpstream) recordChat(r *http.Request, body []byte) qoderResponse {
	u.mu.Lock()
	defer u.mu.Unlock()
	idx := len(u.chats)
	u.chats = append(u.chats, qoderChat{body: string(body), auth: r.Header.Get("Authorization")})
	if idx < len(u.chatResponses) {
		return u.chatResponses[idx]
	}
	return u.chatResponses[len(u.chatResponses)-1]
}

func (u *qoderUpstream) recordPoll(r *http.Request) (int, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.polls = append(u.polls, qoderPoll{path: r.URL.String(), requestID: r.Header.Get("X-Request-ID"), sessionID: r.Header.Get("X-Session-ID")})
	if len(u.pollScript) == 0 {
		return u.pollStatus, u.pollBody
	}
	i := len(u.polls) - 1
	if i >= len(u.pollScript) {
		i = len(u.pollScript) - 1
	}
	return u.pollScript[i].status, u.pollScript[i].body
}

func (u *qoderUpstream) chatCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.chats)
}

func (u *qoderUpstream) pollCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.polls)
}

// qoderQueueHarness wires a Qoder provider, combo, and proxy against a
// scripted upstream. It returns the proxy base URL, access token, and the
// upstream handle.
func qoderQueueHarness(t *testing.T, u *qoderUpstream, fallback *capturedUpstream, policy *qoderQueuePolicy) (string, string) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(r.URL.Path, "/sse/agent_chat_generation"):
			resp := u.recordChat(r, body)
			if resp.stallHeaders {
				// Block until the caller abandons the request.
				<-r.Context().Done()
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(resp.status)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if resp.stallFirstFrame {
				<-r.Context().Done()
				return
			}
			if resp.bodyDelay > 0 {
				select {
				case <-time.After(resp.bodyDelay):
				case <-r.Context().Done():
					return
				}
			}
			_, _ = io.WriteString(w, resp.sse)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if resp.tailDelay > 0 {
				select {
				case <-time.After(resp.tailDelay):
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, resp.tail)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		case strings.Contains(r.URL.Path, "/ask/queue/status"):
			status, body := u.recordPoll(r)
			u.mu.Lock()
			stall := u.pollStall
			u.mu.Unlock()
			if stall > 0 {
				select {
				case <-time.After(stall):
				case <-r.Context().Done():
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)

	prov := &domain.Provider{
		Name:       "q",
		BaseURL:    up.URL + "/algo",
		Protocol:   domain.ProtocolQoder,
		AuthMethod: domain.AuthAPIKey,
		APIKey:     "dt-token",
		OAuthCreds: &domain.OAuthCreds{UserID: "queue-user-1", AccessToken: "dt-token", MachineID: "machine-1"},
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	targets := []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "qoder/m1", Enabled: true}}
	if fallback != nil {
		up2 := newUpstream(t, fallback)
		p2 := &domain.Provider{Name: "p2", BaseURL: up2.URL, Protocol: domain.ProtocolOpenAI, APIKey: "up-key"}
		if err := st.CreateProvider(ctx, p2); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, domain.ComboTarget{ProviderID: p2.ID, UpstreamModel: "real-model", Enabled: true})
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: targets}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	// Seed the catalog for the hydrated provider row so prepare succeeds
	// without contacting the real model-list host.
	stored, err := st.GetProvider(ctx, prov.ID)
	if err != nil {
		t.Fatal(err)
	}
	qoder.SeedCatalogForTest(stored, map[string]json.RawMessage{
		"m1": json.RawMessage(`{"key":"m1","is_reasoning":false,"max_output_tokens":32768}`),
	}, []string{"m1"})
	t.Cleanup(qoder.ClearCatalogForTest)

	mux := http.NewServeMux()
	p := New(st, nil)
	if policy != nil {
		p.qoderQueuePolicy = *policy
	}
	tracker := &qoderCloseTracker{inner: p.streamClient.Transport}
	p.streamClient = &http.Client{Transport: tracker}
	trackersMu.Lock()
	trackers[key.Token] = tracker
	trackersMu.Unlock()
	t.Cleanup(func() {
		trackersMu.Lock()
		delete(trackers, key.Token)
		trackersMu.Unlock()
	})
	p.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token
}

var (
	trackersMu sync.Mutex
	trackers   = map[string]*qoderCloseTracker{}
)

func trackerForToken(t *testing.T, token string) *qoderCloseTracker {
	t.Helper()
	trackersMu.Lock()
	defer trackersMu.Unlock()
	tr, ok := trackers[token]
	if !ok {
		t.Fatal("no close tracker for token")
	}
	return tr
}

func shortQoderQueuePolicy() *qoderQueuePolicy {
	return &qoderQueuePolicy{
		maxWait:      300 * time.Millisecond,
		minDelay:     time.Millisecond,
		maxDelay:     2 * time.Millisecond,
		defaultDelay: time.Millisecond,
		statusFloor:  time.Millisecond,
		pollTimeout:  200 * time.Millisecond,
	}
}

// qoderSSEFrames wraps OpenAI chunk strings in Qoder envelope data lines.
func qoderSSEFrames(t *testing.T, inners ...string) string {
	t.Helper()
	var b strings.Builder
	for _, inner := range inners {
		enc, err := json.Marshal(inner)
		if err != nil {
			t.Fatal(err)
		}
		env, err := json.Marshal(map[string]any{"statusCodeValue": 200, "body": json.RawMessage(enc)})
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString("data: " + string(env) + "\n\n")
	}
	return b.String()
}

func qoderRefusalSSE(t *testing.T, retryAfter int) string {
	t.Helper()
	inner := fmt.Sprintf(`{"code":"10605","message":"{\"isQueued\":true,\"modelKey\":\"m1\",\"queueCount\":1,\"queueType\":\"p3\",\"retryAfterSeconds\":%d,\"serviceAvailable\":false,\"subCode\":\"RESOURCE_LIMIT\",\"waitTime\":%d}"}`, retryAfter, retryAfter)
	enc, err := json.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	env, err := json.Marshal(map[string]any{"statusCodeValue": 429, "body": json.RawMessage(enc)})
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(env) + "\n\n"
}

const qoderSuccessSSE = "data: " +
	`{"statusCodeValue":200,"body":"{\"id\":\"c1\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"he\"}}]}"}` + "\n\n" +
	"data: " +
	`{"statusCodeValue":200,"body":"{\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\"}}]}"}` + "\n\n" +
	"data: " +
	`{"statusCodeValue":200,"body":"{\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}"}` + "\n\n" +
	"data: {\"statusCodeValue\":200,\"body\":\"[DONE]\"}\n\n"

func qoderStreamRequest() string {
	return `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`
}

func TestQoderQueueReadyThenSuccess(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{
			{status: 200, sse: qoderRefusalSSE(t, 5)},
			{status: 200, sse: qoderSuccessSSE},
		},
		pollStatus: 200,
		pollBody:   `{"isQueued":false,"queueType":"p3"}`,
	}
	base, token := qoderQueueHarness(t, u, nil, shortQoderQueuePolicy())
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	text, finished := collectStreamText(t, "/v1/chat/completions", body)
	if text != "hello" {
		t.Fatalf("text = %q", text)
	}
	if !finished {
		t.Fatal("stream did not finish")
	}
	// Exactly one refused send plus one successful replay: the successful
	// stream is handed back exactly once, never re-sent or duplicated.
	if u.chatCount() != 2 {
		t.Fatalf("chat sends = %d, want 2", u.chatCount())
	}
	if u.pollCount() != 1 {
		t.Fatalf("status polls = %d, want 1", u.pollCount())
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	// The exact encoded request body is stable across queue replays.
	if u.chats[0].body == "" || u.chats[0].body != u.chats[1].body {
		t.Fatal("queue replay did not reuse the exact encoded request body")
	}
	// Fresh COSY signing per send: the Authorization header must differ.
	if u.chats[0].auth == "" || u.chats[0].auth == u.chats[1].auth {
		t.Fatal("queue replay reused the COSY signature")
	}
	// The signed poll correlates with the original inference identity.
	poll := u.polls[0]
	if !strings.Contains(poll.path, "requestSetId=") || !strings.Contains(poll.path, "modelKey=m1") || !strings.Contains(poll.path, "queueType=p3") {
		t.Fatalf("poll url = %q", poll.path)
	}
	plain, derr := qoder.DecodeWireBody([]byte(u.chats[0].body))
	if derr != nil {
		t.Fatalf("decode wire: %v", derr)
	}
	reqID, setID, sessID := qoder.RequestIdentity(plain)
	if poll.requestID != reqID {
		t.Fatalf("poll X-Request-ID = %q, want body request_id %q", poll.requestID, reqID)
	}
	if poll.sessionID != sessID {
		t.Fatalf("poll X-Session-ID = %q, want body session_id %q", poll.sessionID, sessID)
	}
	if !strings.Contains(poll.path, "requestSetId="+setID) {
		t.Fatalf("poll requestSetId missing set id %q: %q", setID, poll.path)
	}
	// The queued refusal response was closed before the replay was sent.
	u2 := trackerForToken(t, token)
	u2.eventsMu.Lock()
	events := append([]string(nil), u2.events...)
	u2.eventsMu.Unlock()
	closeIdx, openIdx := -1, -1
	for i, ev := range events {
		if ev == "chat-0-close" && closeIdx == -1 {
			closeIdx = i
		}
		if ev == "chat-1-open" {
			openIdx = i
		}
	}
	if closeIdx == -1 || openIdx == -1 || closeIdx > openIdx {
		t.Fatalf("queued response not closed before retry; events = %v", events)
	}
}

// trackedBody reports Close into the transport event log.
type trackedBody struct {
	rc io.ReadCloser
	on func()
}

func (b *trackedBody) Read(p []byte) (int, error) { return b.rc.Read(p) }
func (b *trackedBody) Close() error               { b.on(); return b.rc.Close() }

// qoderCloseTracker records when chat response bodies are opened and closed.
type qoderCloseTracker struct {
	inner    http.RoundTripper
	eventsMu sync.Mutex
	events   []string
	n        int
}

func (tr *qoderCloseTracker) RoundTrip(r *http.Request) (*http.Response, error) {
	inner := tr.inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	resp, err := inner.RoundTrip(r)
	if err != nil {
		return resp, err
	}
	if strings.Contains(r.URL.Path, "/sse/agent_chat_generation") && resp.Body != nil {
		tr.eventsMu.Lock()
		idx := tr.n
		tr.n++
		tr.events = append(tr.events, fmt.Sprintf("chat-%d-open", idx))
		tr.eventsMu.Unlock()
		resp.Body = &trackedBody{rc: resp.Body, on: func() {
			tr.eventsMu.Lock()
			tr.events = append(tr.events, fmt.Sprintf("chat-%d-close", idx))
			tr.eventsMu.Unlock()
		}}
	}
	return resp, nil
}

func TestQoderQueueBudgetExpiresWithRetryable503(t *testing.T) {
	refusal := qoderRefusalSSE(t, 30)
	u := &qoderUpstream{
		chatResponses: []qoderResponse{{status: 200, sse: refusal}},
		pollStatus:    200,
		pollBody:      `{"isQueued":true,"queueCount":2,"queueType":"p3"}`,
	}
	base, token := qoderQueueHarness(t, u, nil, shortQoderQueuePolicy())
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("body = %s", body)
	}
	if !strings.Contains(e.Error.Message, "still queued") {
		t.Fatalf("message = %q", e.Error.Message)
	}
	// A usable queued status keeps the request queued: the original inference
	// is never resent, the budget is spent on repeated polls instead.
	if u.chatCount() != 1 {
		t.Fatalf("chat sends = %d, want only the original refused send", u.chatCount())
	}
	if u.pollCount() < 2 {
		t.Fatalf("polls = %d, want repeated polling while queued", u.pollCount())
	}
}

// TestQoderQueueReplayNon2xxMessageReachesClient runs a replay that answers
// HTTP 429 through the full proxy path: the upstream error body must stay
// readable after the queue recovery hands it off, so the client sees the real
// upstream message and status instead of a context-canceled empty body.
func TestQoderQueueReplayNon2xxMessageReachesClient(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{
			{status: 200, sse: qoderRefusalSSE(t, 5)},
			// A delayed body makes the regression deterministic: the buggy
			// cleanup canceled the replay context before handoff, so the
			// error body read failed and the message was lost.
			{status: 429, bodyDelay: 100 * time.Millisecond, sse: `{"error":{"message":"slow down: upstream rejected the replay","code":"rate_limited"}}`},
		},
		pollStatus: 200,
		pollBody:   `{"isQueued":false}`,
	}
	base, token := qoderQueueHarness(t, u, nil, shortQoderQueuePolicy())
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "slow down: upstream rejected the replay") {
		t.Fatalf("body = %s, want the upstream 429 message", body)
	}
	// Exactly one refused send plus the rejected replay; no further retries.
	if u.chatCount() != 2 {
		t.Fatalf("chat sends = %d, want refusal plus one rejected replay", u.chatCount())
	}
}

func TestQoderQueueCallerCancelDuringWait(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{{status: 200, sse: qoderRefusalSSE(t, 30)}},
		pollStatus:    200,
		pollBody:      `{"isQueued":true,"queueCount":2}`,
	}
	base, token := qoderQueueHarness(t, u, nil, shortQoderQueuePolicy())

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", strings.NewReader(qoderStreamRequest()))
	req.Header.Set("Authorization", "Bearer "+token)
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	} else if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("request: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("cancellation took %v; queue wait not context-aware", elapsed)
	}
}

func TestQoderQueueUnknownStatusEndpointFallsBackToTimedReplay(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{
			{status: 200, sse: qoderRefusalSSE(t, 5)},
			{status: 200, sse: qoderSuccessSSE},
		},
		pollStatus: 404,
		pollBody:   "not found",
	}
	base, token := qoderQueueHarness(t, u, nil, shortQoderQueuePolicy())
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if text, _ := collectStreamText(t, "/v1/chat/completions", body); text != "hello" {
		t.Fatalf("text = %q", text)
	}
	if u.chatCount() != 2 {
		t.Fatalf("chat sends = %d, want timed replay after unusable endpoint", u.chatCount())
	}
	if u.pollCount() != 1 {
		t.Fatalf("polls = %d, want polling disabled after one unusable response", u.pollCount())
	}
}

// TestQoderQueueReadyContradictionResumesPolling covers a ready status whose
// replay is refused again: the endpoint is polled immediately anew instead of
// being distrusted after one contradiction, and every ready hint is waited
// (clamped here to 60ms per hint by maxDelay) before each replay.
func TestQoderQueueReadyContradictionResumesPolling(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{
			{status: 200, sse: qoderRefusalSSE(t, 5)},
			{status: 200, sse: qoderRefusalSSE(t, 5)},
			{status: 200, sse: qoderSuccessSSE},
		},
		pollScript: []qoderPollResponse{
			{status: 200, body: `{"isQueued":false,"serviceAvailable":true,"retryAfterSeconds":2,"queueType":"p3"}`},
			{status: 200, body: `{"isQueued":true,"queueCount":2,"queueType":"p3"}`},
			{status: 200, body: `{"isQueued":false,"serviceAvailable":true,"retryAfterSeconds":2,"queueType":"p3"}`},
		},
	}
	base, token := qoderQueueHarness(t, u, nil, qoderPolicy(
		3*time.Second, time.Millisecond, 60*time.Millisecond, time.Millisecond, time.Millisecond, 100*time.Millisecond))
	start := time.Now()
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if text, _ := collectStreamText(t, "/v1/chat/completions", body); text != "hello" {
		t.Fatalf("text = %q", text)
	}
	// Refusal, refused ready replay, then the successful replay after the
	// endpoint was polled again: three sends, three polls.
	if u.chatCount() != 3 {
		t.Fatalf("chat sends = %d, want 3", u.chatCount())
	}
	if u.pollCount() != 3 {
		t.Fatalf("polls = %d, want the endpoint re-polled after the refused replay", u.pollCount())
	}
	// Both ready hints were waited (2s each, clamped to 60ms): the request
	// took at least two clamped waits and far less than the raw hints.
	if elapsed < 110*time.Millisecond {
		t.Fatalf("elapsed = %v; the ready hints were not waited before replay", elapsed)
	}
	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("elapsed = %v; the ready hints ran past the maxDelay clamp", elapsed)
	}
}

// TestQoderQueueQueuedStatusPollsInsteadOfSending proves a usable queued
// status never triggers an inference resend: the recovery polls until the
// budget expires and answers the retryable 503. queueCount=0 alone does not
// mean ready.
func TestQoderQueueQueuedStatusPollsInsteadOfSending(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{{status: 200, sse: qoderRefusalSSE(t, 5)}},
		pollStatus:    200,
		pollBody:      `{"isQueued":true,"queueCount":0,"queueType":"p3"}`,
	}
	base, token := qoderQueueHarness(t, u, nil, qoderPolicy(
		500*time.Millisecond, time.Millisecond, 2*time.Millisecond, time.Millisecond, time.Millisecond, 200*time.Millisecond))
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "still queued") {
		t.Fatalf("body = %s, want the queue budget envelope", body)
	}
	if u.chatCount() != 1 {
		t.Fatalf("chat sends = %d, want only the original refused send", u.chatCount())
	}
	if u.pollCount() < 3 {
		t.Fatalf("polls = %d, want repeated polling while queued", u.pollCount())
	}
}

// TestQoderQueueQueuedThenReadyHonorsHint runs the full queued-to-ready
// sequence: no inference while queued, and the positive ready hint (2s,
// clamped to 60ms by maxDelay) is waited before the single successful replay.
func TestQoderQueueQueuedThenReadyHonorsHint(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{
			{status: 200, sse: qoderRefusalSSE(t, 5)},
			{status: 200, sse: qoderSuccessSSE},
		},
		pollScript: []qoderPollResponse{
			{status: 200, body: `{"isQueued":true,"queueCount":2,"queueType":"p3"}`},
			{status: 200, body: `{"isQueued":false,"serviceAvailable":true,"retryAfterSeconds":2,"queueType":"p3"}`},
		},
	}
	base, token := qoderQueueHarness(t, u, nil, qoderPolicy(
		2*time.Second, time.Millisecond, 60*time.Millisecond, time.Millisecond, time.Millisecond, 100*time.Millisecond))
	start := time.Now()
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if text, _ := collectStreamText(t, "/v1/chat/completions", body); text != "hello" {
		t.Fatalf("text = %q", text)
	}
	if u.chatCount() != 2 {
		t.Fatalf("chat sends = %d, want refusal plus one replay after ready", u.chatCount())
	}
	if u.pollCount() != 2 {
		t.Fatalf("polls = %d, want one queued poll plus the ready poll", u.pollCount())
	}
	if elapsed < 55*time.Millisecond {
		t.Fatalf("elapsed = %v; the ready hint was not waited", elapsed)
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("elapsed = %v; the ready hint ran past the maxDelay clamp", elapsed)
	}
}

// TestQoderQueueNegativeAdmissionKeepsPolling proves isQueued=false with
// serviceAvailable=false never schedules a replay: the request keeps polling
// until the status truly turns ready, and only then replays once.
func TestQoderQueueNegativeAdmissionKeepsPolling(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{
			{status: 200, sse: qoderRefusalSSE(t, 5)},
			{status: 200, sse: qoderSuccessSSE},
		},
		pollScript: []qoderPollResponse{
			{status: 200, body: `{"isQueued":false,"serviceAvailable":false,"queueCount":0,"queueType":"p3"}`},
			{status: 200, body: `{"isQueued":false,"serviceAvailable":true,"retryAfterSeconds":2,"queueType":"p3"}`},
		},
	}
	base, token := qoderQueueHarness(t, u, nil, qoderPolicy(
		2*time.Second, time.Millisecond, 60*time.Millisecond, time.Millisecond, time.Millisecond, 100*time.Millisecond))
	start := time.Now()
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if text, _ := collectStreamText(t, "/v1/chat/completions", body); text != "hello" {
		t.Fatalf("text = %q", text)
	}
	// The negative-admission poll must not have triggered the replay: two
	// polls happened, and the replay only followed the ready status.
	if u.pollCount() != 2 {
		t.Fatalf("polls = %d, want the negative-admission status polled again", u.pollCount())
	}
	if u.chatCount() != 2 {
		t.Fatalf("chat sends = %d, want refusal plus one replay after ready", u.chatCount())
	}
	if elapsed < 55*time.Millisecond {
		t.Fatalf("elapsed = %v; the ready hint was not waited", elapsed)
	}
}

// TestQoderQueueReadyHintClampedToBudget proves a ready hint larger than the
// remaining budget is bounded by it: the wait is cut to the budget, the
// budget 503 is returned, and no replay happens.
func TestQoderQueueReadyHintClampedToBudget(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{{status: 200, sse: qoderRefusalSSE(t, 5)}},
		pollStatus:    200,
		pollBody:      `{"isQueued":false,"serviceAvailable":true,"retryAfterSeconds":5,"queueType":"p3"}`,
	}
	base, token := qoderQueueHarness(t, u, nil, qoderPolicy(
		80*time.Millisecond, time.Millisecond, 10*time.Second, time.Millisecond, time.Millisecond, 50*time.Millisecond))
	start := time.Now()
	resp, _ := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want budget 503", resp.StatusCode)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("elapsed = %v; the ready hint ran past the budget", elapsed)
	}
	if u.chatCount() != 1 {
		t.Fatalf("chat sends = %d, want no replay after budget expiry", u.chatCount())
	}
	if u.pollCount() != 1 {
		t.Fatalf("polls = %d, want the single ready poll", u.pollCount())
	}
}

// TestQoderQueueCallerCancelDuringReadyDelay proves a caller cancellation
// during a ready-hint wait ends the recovery immediately, before any replay.
func TestQoderQueueCallerCancelDuringReadyDelay(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{{status: 200, sse: qoderRefusalSSE(t, 5)}},
		pollStatus:    200,
		pollBody:      `{"isQueued":false,"serviceAvailable":true,"retryAfterSeconds":2,"queueType":"p3"}`,
	}
	base, token := qoderQueueHarness(t, u, nil, qoderPolicy(
		10*time.Second, time.Millisecond, 10*time.Second, time.Millisecond, time.Millisecond, 100*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", strings.NewReader(qoderStreamRequest()))
	req.Header.Set("Authorization", "Bearer "+token)
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := http.DefaultClient.Do(req)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("err = %v, want the caller's cancellation", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("elapsed = %v; cancellation did not end the ready-hint wait", elapsed)
	}
	if u.chatCount() != 1 {
		t.Fatalf("chat sends = %d, want no replay after cancellation", u.chatCount())
	}
}

// TestQoderQueueMalformedStatusFallsBackToTimedReplay proves a malformed but
// HTTP-successful status body disables polling and preserves the timed
// replay fallback.
func TestQoderQueueMalformedStatusFallsBackToTimedReplay(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{
			{status: 200, sse: qoderRefusalSSE(t, 5)},
			{status: 200, sse: qoderSuccessSSE},
		},
		pollStatus: 200,
		pollBody:   `garbage{{no json`,
	}
	base, token := qoderQueueHarness(t, u, nil, shortQoderQueuePolicy())
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if text, _ := collectStreamText(t, "/v1/chat/completions", body); text != "hello" {
		t.Fatalf("text = %q", text)
	}
	if u.chatCount() != 2 {
		t.Fatalf("chat sends = %d, want timed replay after the unusable endpoint", u.chatCount())
	}
	if u.pollCount() != 1 {
		t.Fatalf("polls = %d, want polling disabled after the malformed answer", u.pollCount())
	}
}

// TestQoderQueueStatusUnavailableAfterQueuedFallsBack proves the transition
// from valid queued polls to an erroring endpoint: polling turns off and the
// recovery finishes through the timed replay fallback.
func TestQoderQueueStatusUnavailableAfterQueuedFallsBack(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{
			{status: 200, sse: qoderRefusalSSE(t, 5)},
			{status: 200, sse: qoderSuccessSSE},
		},
		pollScript: []qoderPollResponse{
			{status: 200, body: `{"isQueued":true,"queueCount":2,"queueType":"p3"}`},
			{status: 503, body: `upstream exploded`},
		},
	}
	base, token := qoderQueueHarness(t, u, nil, shortQoderQueuePolicy())
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if text, _ := collectStreamText(t, "/v1/chat/completions", body); text != "hello" {
		t.Fatalf("text = %q", text)
	}
	// One valid queued poll, then the failed poll disables polling and the
	// timed fallback replay succeeds.
	if u.pollCount() != 2 {
		t.Fatalf("polls = %d, want one queued poll plus the failed one", u.pollCount())
	}
	if u.chatCount() != 2 {
		t.Fatalf("chat sends = %d, want refusal plus the fallback replay", u.chatCount())
	}
}

func TestQoderQueueBillingIsNotQueue(t *testing.T) {
	// A refusal carrying both the queue code and pricingUrl must be treated
	// as queue capacity, and a plain billing block as a 403 quota error.
	billingInner := `{"code":"112","message":"quota exhausted"}`
	enc, err := json.Marshal(billingInner)
	if err != nil {
		t.Fatal(err)
	}
	env, err := json.Marshal(map[string]any{"statusCodeValue": 403, "body": json.RawMessage(enc)})
	if err != nil {
		t.Fatal(err)
	}
	u := &qoderUpstream{
		chatResponses: []qoderResponse{{status: 200, sse: "data: " + string(env) + "\n\n"}},
		pollStatus:    200,
		pollBody:      `{"isQueued":false}`,
	}
	base, token := qoderQueueHarness(t, u, nil, shortQoderQueuePolicy())
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("body = %s", body)
	}
	if !strings.Contains(e.Error.Message, "quota exhausted") {
		t.Fatalf("message = %q, want the billing message", e.Error.Message)
	}
	if u.pollCount() != 0 {
		t.Fatalf("polls = %d, want none for billing", u.pollCount())
	}
}

func TestQoderQueuePrecommitFailureFailsOver(t *testing.T) {
	billingInner := `{"code":"110","message":"daily count exceeded"}`
	enc, _ := json.Marshal(billingInner)
	env, _ := json.Marshal(map[string]any{"statusCodeValue": 403, "body": json.RawMessage(enc)})
	u := &qoderUpstream{
		chatResponses: []qoderResponse{{status: 200, sse: "data: " + string(env) + "\n\n"}},
		pollStatus:    200,
		pollBody:      `{"isQueued":false}`,
	}
	fallback := &capturedUpstream{}
	base, token := qoderQueueHarness(t, u, fallback, shortQoderQueuePolicy())
	// A non-streaming client keeps the fallback target on the unary passthrough
	// path; the Qoder billing failure still surfaces before any commit.
	resp, body := postStream(t, base+"/v1/chat/completions", token,
		`{"model":"default","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "hello from openai") {
		t.Fatalf("body = %s, want the fallback target's completion", body)
	}
	// The pre-commit billing failure failed over after exactly one qoder send.
	if u.chatCount() != 1 {
		t.Fatalf("qoder sends = %d, want single pre-commit attempt", u.chatCount())
	}
}

func TestQoderStreamPostCommitErrorNoRetry(t *testing.T) {
	content := `{"id":"c1","model":"m1","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"}}]}`
	errInner := `{"code":"500","message":"mid-stream failure"}`
	enc, _ := json.Marshal(content)
	env1, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": json.RawMessage(enc)})
	enc2, _ := json.Marshal(errInner)
	env2, _ := json.Marshal(map[string]any{"statusCodeValue": 500, "body": json.RawMessage(enc2)})
	u := &qoderUpstream{
		chatResponses: []qoderResponse{{status: 200, sse: "data: " + string(env1) + "\n\ndata: " + string(env2) + "\n\n"}},
		pollStatus:    200,
		pollBody:      `{"isQueued":false}`,
	}
	fallback := &capturedUpstream{}
	base, token := qoderQueueHarness(t, u, fallback, shortQoderQueuePolicy())
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	// The committed stream terminates with an error frame, not a second target.
	reader := sse.NewReader(strings.NewReader(body))
	var sawError bool
	var text strings.Builder
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
		var probe struct {
			Choices []json.RawMessage `json:"choices"`
			Error   *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &probe)
		if probe.Error != nil && len(probe.Choices) == 0 {
			sawError = true
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		_ = json.Unmarshal(ev.Data, &chunk)
		for _, c := range chunk.Choices {
			text.WriteString(c.Delta.Content)
		}
	}
	if !sawError {
		t.Fatalf("no terminal error frame in %s", body)
	}
	if text.String() != "partial" {
		t.Fatalf("text = %q", text.String())
	}
	if fallback.path != "" {
		t.Fatal("post-commit failure must not fail over to another target")
	}
}

func TestQoderStreamUsageAliasesReachClient(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}},
		pollStatus:    200,
		pollBody:      `{"isQueued":false}`,
	}
	base, token := qoderQueueHarness(t, u, nil, nil)
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	// The final choices-empty chunk carries the aliased usage.
	reader := sse.NewReader(strings.NewReader(body))
	var usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	}
	found := false
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var probe struct {
			Choices []json.RawMessage `json:"choices"`
			Usage   *map[string]any   `json:"usage"`
		}
		_ = json.Unmarshal(ev.Data, &probe)
		if probe.Usage != nil && len(probe.Choices) == 0 {
			b, _ := json.Marshal(*probe.Usage)
			_ = json.Unmarshal(b, &usage)
			found = true
		}
	}
	if !found {
		t.Fatalf("no terminal usage chunk in %s", body)
	}
	if usage.PromptTokens != 7 || usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v, want aliased 7/3", usage)
	}
}

func TestQoderQueueUnaryClientCollectsStream(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}},
		pollStatus:    200,
		pollBody:      `{"isQueued":false}`,
	}
	base, token := qoderQueueHarness(t, u, nil, nil)
	resp, body := postStream(t, base+"/v1/chat/completions", token,
		`{"model":"default","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("body = %s", body)
	}
	if len(out.Choices) == 0 || out.Choices[0].Message.Content != "hello" {
		t.Fatalf("choices = %+v", out.Choices)
	}
	if out.Usage.PromptTokens != 7 || out.Usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

// qoderIngressText extracts assistant text from a client response body in
// whichever ingress format the path requested. Unary and streaming bodies both
// funnel through the SSE reader (unary bodies are plain JSON, which yields no
// SSE events), so text is parsed per format explicitly.
func qoderIngressText(t *testing.T, path, body string) string {
	t.Helper()
	if !strings.HasSuffix(path, "/messages") {
		// OpenAI-family unary bodies are JSON, not SSE.
		var unary struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
			OutputText string `json:"output_text"`
		}
		if err := json.Unmarshal([]byte(body), &unary); err == nil {
			if len(unary.Choices) > 0 && unary.Choices[0].Message.Content != "" {
				return unary.Choices[0].Message.Content
			}
			var b strings.Builder
			for _, item := range unary.Output {
				if item.Type == "message" {
					for _, part := range item.Content {
						if part.Type == "output_text" {
							b.WriteString(part.Text)
						}
					}
				}
			}
			if b.Len() > 0 {
				return b.String()
			}
		}
	}
	if strings.Contains(path, "/responses") {
		// Responses SSE emits response.output_text.delta events.
		reader := sse.NewReader(strings.NewReader(body))
		var b strings.Builder
		for {
			ev, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			var d struct {
				Delta string `json:"delta"`
			}
			_ = json.Unmarshal(ev.Data, &d)
			if ev.Name == "response.output_text.delta" {
				b.WriteString(d.Delta)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	if strings.HasSuffix(path, "/messages") {
		// Anthropic unary bodies are JSON with content blocks.
		var unary struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(body), &unary); err == nil {
			var b strings.Builder
			for _, c := range unary.Content {
				if c.Type == "text" {
					b.WriteString(c.Text)
				}
			}
			if b.Len() > 0 {
				return b.String()
			}
		}
	}
	text, _ := collectStreamText(t, path, body)
	return text
}

// TestQoderQueueAllIngressesUnaryAndStreaming runs the queue-then-success path
// through every ingress format (OpenAI Chat, OpenAI Responses, Anthropic
// Messages) with both streaming and non-streaming clients.
func TestQoderQueueAllIngressesUnaryAndStreaming(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		request string
	}{
		{
			"openai chat stream",
			"/v1/chat/completions",
			`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			"openai chat unary",
			"/v1/chat/completions",
			`{"model":"default","stream":false,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			"openai responses stream",
			"/v1/responses",
			`{"model":"default","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
		},
		{
			"openai responses unary",
			"/v1/responses",
			`{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
		},
		{
			"anthropic messages stream",
			"/v1/messages",
			`{"model":"default","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			"anthropic messages unary",
			"/v1/messages",
			`{"model":"default","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &qoderUpstream{
				chatResponses: []qoderResponse{
					{status: 200, sse: qoderRefusalSSE(t, 5)},
					{status: 200, sse: qoderSuccessSSE},
				},
				pollStatus: 200,
				pollBody:   `{"isQueued":false,"queueType":"p3"}`,
			}
			base, token := qoderQueueHarness(t, u, nil, shortQoderQueuePolicy())
			resp, body := postStream(t, base+tc.path, token, tc.request)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			if text := qoderIngressText(t, tc.path, body); text != "hello" {
				t.Fatalf("text = %q, want the recovered completion", text)
			}
			if u.chatCount() != 2 {
				t.Fatalf("chat sends = %d, want refusal plus replay", u.chatCount())
			}
			if u.pollCount() != 1 {
				t.Fatalf("polls = %d, want one signed poll", u.pollCount())
			}
		})
	}
}

// TestQoderRequestedLimitReachesPrepare covers Q08 wiring through the proxy
// prepare path: the caller's explicit limit reaches PrepareWireBody and the
// catalog limit raises the default when the caller sets none.
func TestQoderRequestedLimitReachesPrepare(t *testing.T) {
	// Catalog allows 65536; the encoder default is 32768.
	u := &qoderUpstream{
		chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}},
		pollStatus:    200,
		pollBody:      `{"isQueued":false}`,
	}
	base, token := qoderQueueHarness(t, u, nil, nil)
	// Raise the catalog ceiling for this test's provider identity.
	qoder.SeedCatalogForTest(&domain.Provider{
		APIKey:     "dt-token",
		OAuthCreds: &domain.OAuthCreds{UserID: "queue-user-1", AccessToken: "dt-token"},
	}, map[string]json.RawMessage{
		"m1": json.RawMessage(`{"key":"m1","is_reasoning":false,"max_output_tokens":65536}`),
	}, []string{"m1"})

	post := func(body string) []byte {
		t.Helper()
		resp, out := postStream(t, base+"/v1/chat/completions", token, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
		}
		return []byte(out)
	}

	// Caller below the catalog: the caller limit wins on the wire.
	post(`{"model":"default","stream":false,"max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`)
	u.mu.Lock()
	wire := u.chats[len(u.chats)-1].body
	u.mu.Unlock()
	plain, err := qoder.DecodeWireBody([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Parameters struct {
			MaxTokens int `json:"max_tokens"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(plain, &f); err != nil {
		t.Fatal(err)
	}
	if f.Parameters.MaxTokens != 1000 {
		t.Fatalf("wire max_tokens = %d, want caller 1000", f.Parameters.MaxTokens)
	}

	// No caller limit: the catalog limit raises the default above 32768.
	u.mu.Lock()
	u.chats = nil
	u.mu.Unlock()
	post(`{"model":"default","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	u.mu.Lock()
	wire = u.chats[len(u.chats)-1].body
	u.mu.Unlock()
	plain, err = qoder.DecodeWireBody([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(plain, &f); err != nil {
		t.Fatal(err)
	}
	if f.Parameters.MaxTokens != 65536 {
		t.Fatalf("wire max_tokens = %d, want catalog 65536", f.Parameters.MaxTokens)
	}
}

// --- recovery budget enforcement ---

// ms policy helper: every knob explicit so no test waits a real interval.
func qoderPolicy(maxWait, minDelay, maxDelay, defaultDelay, statusFloor, pollTimeout time.Duration) *qoderQueuePolicy {
	return &qoderQueuePolicy{
		maxWait:      maxWait,
		minDelay:     minDelay,
		maxDelay:     maxDelay,
		defaultDelay: defaultDelay,
		statusFloor:  statusFloor,
		pollTimeout:  pollTimeout,
	}
}

// TestQoderQueueBudgetShorterThanDelay makes the retry hint (clamped to a
// 10s delay) exceed the recovery budget: the wait must be bounded to the
// remaining budget and end in the retryable 503 without a replay.
func TestQoderQueueBudgetShorterThanDelay(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{{status: 200, sse: qoderRefusalSSE(t, 30)}},
		pollStatus:    404,
		pollBody:      "not found",
	}
	base, token := qoderQueueHarness(t, u, nil, qoderPolicy(
		80*time.Millisecond, 10*time.Second, 10*time.Second, 10*time.Second, 10*time.Second, 20*time.Millisecond))
	start := time.Now()
	resp, _ := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want budget 503", resp.StatusCode)
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("elapsed = %v; the full retry delay ran past the budget", elapsed)
	}
	if u.chatCount() != 1 {
		t.Fatalf("chat sends = %d, want no replay after budget expiry", u.chatCount())
	}
}

// TestQoderQueueStalledPollConsumesBudget blocks the status endpoint longer
// than the budget with a poll timeout larger than the budget: the poll must
// be bounded by min(remaining, pollTimeout) and no replay may follow.
func TestQoderQueueStalledPollConsumesBudget(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{{status: 200, sse: qoderRefusalSSE(t, 30)}},
		pollStatus:    200,
		pollBody:      `{"isQueued":true,"queueCount":2}`,
		pollStall:     10 * time.Second,
	}
	base, token := qoderQueueHarness(t, u, nil, qoderPolicy(
		80*time.Millisecond, 5*time.Second, 5*time.Second, 5*time.Second, 5*time.Second, 5*time.Second))
	start := time.Now()
	resp, _ := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want budget 503", resp.StatusCode)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("elapsed = %v; the stalled poll ran past the budget", elapsed)
	}
	if u.pollCount() != 1 {
		t.Fatalf("polls = %d, want one bounded poll", u.pollCount())
	}
	if u.chatCount() != 1 {
		t.Fatalf("chat sends = %d, want no replay after budget expiry", u.chatCount())
	}
}

// TestQoderQueueReplayHeadersStalled blocks the replay send before response
// headers: the bounded send must abort at the budget, return the retryable
// 503, and leave nothing to clean up.
func TestQoderQueueReplayHeadersStalled(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{
			{status: 200, sse: qoderRefusalSSE(t, 1)},
			{status: 200, stallHeaders: true},
		},
		pollStatus: 200,
		pollBody:   `{"isQueued":false}`,
	}
	base, token := qoderQueueHarness(t, u, nil, qoderPolicy(
		120*time.Millisecond, time.Millisecond, 2*time.Millisecond, time.Millisecond, time.Millisecond, 30*time.Millisecond))
	start := time.Now()
	resp, _ := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want budget 503", resp.StatusCode)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("elapsed = %v; the stalled replay send ran past the budget", elapsed)
	}
	if u.chatCount() != 2 {
		t.Fatalf("chat sends = %d, want the blocked replay attempt", u.chatCount())
	}
}

// TestQoderQueueReplayFirstFrameStalled delivers replay headers but no first
// frame: the bounded first-frame inspection must abort at the budget, close
// the abandoned response, and return the retryable 503.
func TestQoderQueueReplayFirstFrameStalled(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{
			{status: 200, sse: qoderRefusalSSE(t, 1)},
			{status: 200, stallFirstFrame: true},
		},
		pollStatus: 200,
		pollBody:   `{"isQueued":false}`,
	}
	base, token := qoderQueueHarness(t, u, nil, qoderPolicy(
		120*time.Millisecond, time.Millisecond, 2*time.Millisecond, time.Millisecond, time.Millisecond, 30*time.Millisecond))
	start := time.Now()
	resp, _ := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want budget 503", resp.StatusCode)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("elapsed = %v; the stalled first-frame read ran past the budget", elapsed)
	}
	if u.chatCount() != 2 {
		t.Fatalf("chat sends = %d, want the blocked replay attempt", u.chatCount())
	}
	u2 := trackerForToken(t, token)
	u2.eventsMu.Lock()
	events := append([]string(nil), u2.events...)
	u2.eventsMu.Unlock()
	closed := false
	for _, ev := range events {
		if ev == "chat-1-close" {
			closed = true
		}
	}
	if !closed {
		t.Fatalf("abandoned replay response not closed; events = %v", events)
	}
}

// TestQoderQueueInitialInspectionBounded stalls the very first response
// before its first frame: once in recovery, the initial refusal inspection
// must also be bounded by the budget.
func TestQoderQueueInitialInspectionBounded(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{{status: 200, stallFirstFrame: true}},
		pollStatus:    200,
		pollBody:      `{"isQueued":false}`,
	}
	base, token := qoderQueueHarness(t, u, nil, qoderPolicy(
		80*time.Millisecond, time.Millisecond, 2*time.Millisecond, time.Millisecond, time.Millisecond, 30*time.Millisecond))
	start := time.Now()
	resp, _ := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want budget 503", resp.StatusCode)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("elapsed = %v; the stalled initial inspection ran past the budget", elapsed)
	}
	u2 := trackerForToken(t, token)
	u2.eventsMu.Lock()
	events := append([]string(nil), u2.events...)
	u2.eventsMu.Unlock()
	closed := false
	for _, ev := range events {
		if ev == "chat-0-close" {
			closed = true
		}
	}
	if !closed {
		t.Fatalf("stalled initial response not closed; events = %v", events)
	}
}

// TestQoderQueueImmediateReadyConsumesBudget spins the ready path with zero
// delays until the budget runs out: no replay may start once the budget is
// gone, and no background send may happen after the 503 returns.
func TestQoderQueueImmediateReadyConsumesBudget(t *testing.T) {
	refusal := qoderRefusalSSE(t, 1)
	u := &qoderUpstream{
		chatResponses: []qoderResponse{
			{status: 200, sse: refusal},
			{status: 200, sse: refusal},
			{status: 200, sse: refusal},
		},
		pollStatus: 200,
		pollBody:   `{"isQueued":false}`,
	}
	base, token := qoderQueueHarness(t, u, nil, qoderPolicy(
		60*time.Millisecond, time.Millisecond, 2*time.Millisecond, time.Millisecond, time.Millisecond, 20*time.Millisecond))
	resp, _ := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want budget 503", resp.StatusCode)
	}
	if u.chatCount() < 2 {
		t.Fatalf("chat sends = %d, want replays before budget expiry", u.chatCount())
	}
	// No replay may start after the budget expired.
	before := u.chatCount()
	time.Sleep(60 * time.Millisecond)
	if after := u.chatCount(); after != before {
		t.Fatalf("chat sends grew from %d to %d after the budget 503", before, after)
	}
}

// TestQoderQueueCallerDeadlineBindsEarlier gives the caller a deadline
// shorter than the recovery budget: the caller's deadline, not the budget,
// must end the wait, and the wait may not outlive it.
func TestQoderQueueCallerDeadlineBindsEarlier(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{{status: 200, sse: qoderRefusalSSE(t, 30)}},
		pollStatus:    200,
		pollBody:      `{"isQueued":true,"queueCount":2}`,
	}
	base, token := qoderQueueHarness(t, u, nil, qoderPolicy(
		10*time.Minute, 30*time.Second, 60*time.Second, 30*time.Second, 30*time.Second, 10*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", strings.NewReader(qoderStreamRequest()))
	req.Header.Set("Authorization", "Bearer "+token)
	start := time.Now()
	_, err := http.DefaultClient.Do(req)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("err = %v, want the caller's deadline error", err)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("elapsed = %v; the wait outlived the caller deadline", elapsed)
	}
}

// qoderSuccessHead is the first half of qoderSuccessSSE (role and content);
// qoderSuccessTail carries the finish, usage, and [DONE] frames.
const qoderSuccessHead = "data: " +
	`{"statusCodeValue":200,"body":"{\"id\":\"c1\",\"model\":\"m1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"he\"}}]}"}` + "\n\n" +
	"data: " +
	`{"statusCodeValue":200,"body":"{\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\"}}]}"}` + "\n\n"

const qoderSuccessTail = "data: " +
	`{"statusCodeValue":200,"body":"{\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}"}` + "\n\n" +
	"data: {\"statusCodeValue\":200,\"body\":\"[DONE]\"}\n\n"

// TestQoderQueueSuccessOutlivesRecoveryDeadline proves the handed-off stream
// is bound only to the caller's context: the queue recovers within the
// budget, then the rest of the successful stream arrives after the budget
// would have expired and must still be delivered, exactly once.
func TestQoderQueueSuccessOutlivesRecoveryDeadline(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{
			{status: 200, sse: qoderRefusalSSE(t, 1)},
			{status: 200, sse: qoderSuccessHead, tail: qoderSuccessTail, tailDelay: 700 * time.Millisecond},
		},
		pollStatus: 200,
		pollBody:   `{"isQueued":false}`,
	}
	base, token := qoderQueueHarness(t, u, nil, qoderPolicy(
		200*time.Millisecond, time.Millisecond, 2*time.Millisecond, time.Millisecond, time.Millisecond, 50*time.Millisecond))
	start := time.Now()
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderStreamRequest())
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	text, finished := collectStreamText(t, "/v1/chat/completions", body)
	if text != "hello" || !finished {
		t.Fatalf("text = %q finished = %v; the stream did not outlive the recovery deadline", text, finished)
	}
	if elapsed < 500*time.Millisecond {
		t.Fatalf("elapsed = %v; the stream ended before the delayed tail could arrive", elapsed)
	}
	if u.chatCount() != 2 {
		t.Fatalf("chat sends = %d, want refusal plus exactly one successful replay", u.chatCount())
	}
}
