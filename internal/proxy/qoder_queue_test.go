package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"airouter/internal/domain"
)

var errReplay = errors.New("replay read failure")

// trackedCloser records whether the live body was closed. The watchdog
// closes from a timer goroutine, so the flag is mutex-guarded.
type trackedCloser struct {
	r      io.Reader
	mu     sync.Mutex
	closed bool
}

func (c *trackedCloser) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *trackedCloser) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *trackedCloser) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func TestQoderReplayBodyReplaysPrefixThenLive(t *testing.T) {
	live := &trackedCloser{r: strings.NewReader("LIVE")}
	b := &qoderReplayBody{replay: []byte("PEEK"), live: live}
	out, err := io.ReadAll(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "PEEKLIVE" {
		t.Fatalf("out = %q, want the consumed prefix replayed exactly once", out)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if !live.closed {
		t.Fatal("live body not closed")
	}
}

func TestQoderReplayBodyReinjectsPeekError(t *testing.T) {
	// A one-shot (n>0, err) reader does not repeat its error; the replay body
	// must re-inject it after the prefix so the decoder sees the failure.
	live := &trackedCloser{r: strings.NewReader("LIVE")}
	b := &qoderReplayBody{replay: []byte("PEEK"), live: live, peekErr: errReplay}
	out := make([]byte, 16)
	n, err := io.ReadFull(b, out[:4])
	if err != nil || string(out[:n]) != "PEEK" {
		t.Fatalf("prefix = %q, %v", out[:n], err)
	}
	if _, err := b.Read(out); !errors.Is(err, errReplay) {
		t.Fatalf("err = %v, want the peeked read error re-injected", err)
	}
	if live.closed {
		t.Fatal("live body must stay untouched behind a peek error")
	}
}

func TestQoderReplayBodyOnClose(t *testing.T) {
	live := &trackedCloser{r: strings.NewReader("x")}
	called := 0
	b := &qoderReplayBody{live: live, onClose: func() { called++ }}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if called != 1 || !live.closed {
		t.Fatalf("called = %d closed = %v", called, live.closed)
	}
}

func TestCloseAfterDelayClosesAndReportsFired(t *testing.T) {
	live := &trackedCloser{r: strings.NewReader("x")}
	fired := closeAfterDelay(live, 20*time.Millisecond)
	// Full-suite runs load the scheduler; give the callback ample time.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !live.isClosed() {
		time.Sleep(2 * time.Millisecond)
	}
	if !live.isClosed() {
		t.Fatal("watchdog did not close the body")
	}
	// fired() also stops the timer, so it is only consulted after the close
	// was observed.
	if !fired() {
		t.Fatal("fired() must report the close after it ran")
	}
}

func TestCloseAfterDelayStopBeforeFire(t *testing.T) {
	live := &trackedCloser{r: strings.NewReader("x")}
	fired := closeAfterDelay(live, time.Hour)
	if f := fired(); f {
		t.Fatal("stop before fire must report not fired")
	}
	if live.isClosed() {
		t.Fatal("body closed despite the stop")
	}
}

// --- replay send classification and non-2xx body handoff ---

// qoderRecoveryProxy builds a proxy for direct recoverQoderQueue unit tests.
// Without trace info in the context the status poll is unavailable, so the
// recovery loop exercises timed replays only and touches no network.
func qoderRecoveryProxy(t *testing.T, pol *qoderQueuePolicy) *Proxy {
	t.Helper()
	p := New(newTestStore(t), nil)
	if pol != nil {
		p.qoderQueuePolicy = *pol
	}
	return p
}

func qoderRecoveryProvider() *domain.Provider {
	return &domain.Provider{Name: "q", Protocol: domain.ProtocolQoder}
}

func qoderQueuedInitialResponse(t *testing.T) *http.Response {
	t.Helper()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(qoderRefusalSSE(t, 5))),
	}
}

// qoderCtxBody mimics an http response body bound to its request context:
// once the context is canceled, reads fail with the context error.
type qoderCtxBody struct {
	ctx    context.Context
	r      io.Reader
	mu     sync.Mutex
	closes int
}

func (b *qoderCtxBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	return b.r.Read(p)
}

func (b *qoderCtxBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closes++
	return nil
}

func (b *qoderCtxBody) closeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closes
}

// qoderBlockedCtxBody blocks reads until its context is done, mimicking a
// stalled upstream error body.
type qoderBlockedCtxBody struct {
	ctx    context.Context
	mu     sync.Mutex
	closes int
}

func (b *qoderBlockedCtxBody) Read(p []byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *qoderBlockedCtxBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closes++
	return nil
}

func (b *qoderBlockedCtxBody) closeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closes
}

// TestQoderReplayTransportErrorPreserved proves an immediate transport failure
// during a replay send is returned as the original error, not swallowed into
// the queue-budget 503 by the cleanup cancel.
func TestQoderReplayTransportErrorPreserved(t *testing.T) {
	p := qoderRecoveryProxy(t, qoderPolicy(
		500*time.Millisecond, time.Millisecond, 2*time.Millisecond, time.Millisecond, time.Millisecond, 50*time.Millisecond))
	sendErr := errors.New("connection reset before response headers")
	sends := 0
	send := func(ctx context.Context) (*http.Response, error) {
		sends++
		return nil, sendErr
	}
	resp, err := p.recoverQoderQueue(context.Background(), qoderRecoveryProvider(), send, qoderQueuedInitialResponse(t))
	if !errors.Is(err, sendErr) {
		t.Fatalf("err = %v, want the original transport error", err)
	}
	if resp != nil {
		resp.Body.Close()
		t.Fatalf("resp = %+v, want nil alongside the transport error", resp)
	}
	if sends != 1 {
		t.Fatalf("sends = %d, want exactly one failed replay", sends)
	}
}

// TestQoderReplayCallerCancelReportsContextError proves a caller cancellation
// during a blocked replay send surfaces the context error, never the queue
// budget envelope.
func TestQoderReplayCallerCancelReportsContextError(t *testing.T) {
	p := qoderRecoveryProxy(t, qoderPolicy(
		10*time.Second, time.Millisecond, 2*time.Millisecond, time.Millisecond, time.Millisecond, 50*time.Millisecond))
	send := func(ctx context.Context) (*http.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	resp, err := p.recoverQoderQueue(ctx, qoderRecoveryProvider(), send, qoderQueuedInitialResponse(t))
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the caller's cancellation", err)
	}
	if resp != nil {
		t.Fatalf("resp = %+v, want nil", resp)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("elapsed = %v; cancellation did not end the replay", elapsed)
	}
}

// TestQoderReplayBudgetExpiryReturnsQueue503 proves a replay send that is
// still blocked when the recovery budget expires ends in the retryable 503,
// not a transport error.
func TestQoderReplayBudgetExpiryReturnsQueue503(t *testing.T) {
	p := qoderRecoveryProxy(t, qoderPolicy(
		120*time.Millisecond, time.Millisecond, 2*time.Millisecond, time.Millisecond, time.Millisecond, 50*time.Millisecond))
	send := func(ctx context.Context) (*http.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	resp, err := p.recoverQoderQueue(context.Background(), qoderRecoveryProvider(), send, qoderQueuedInitialResponse(t))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("err = %v, want nil budget expiry", err)
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %v, want budget 503", resp)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "qoder_model_queued") {
		t.Fatalf("body = %s, want the queue budget envelope", body)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("elapsed = %v; the blocked send ran past the budget", elapsed)
	}
}

// TestQoderReplayErrorBodyReadableUntilClose proves a non-2xx replay response
// reaches the caller with its body still readable: the replay context stays
// alive through consumption, and EOF releases it plus the budget timer exactly
// once, with a late Close as a no-op.
func TestQoderReplayErrorBodyReadableUntilClose(t *testing.T) {
	p := qoderRecoveryProxy(t, qoderPolicy(
		5*time.Second, time.Millisecond, 2*time.Millisecond, time.Millisecond, time.Millisecond, 50*time.Millisecond))
	msg := `{"error":{"message":"upstream rejected the replay","code":"rate_limited"}}`
	var live *qoderCtxBody
	var sendCtx context.Context
	send := func(ctx context.Context) (*http.Response, error) {
		sendCtx = ctx
		live = &qoderCtxBody{ctx: ctx, r: strings.NewReader(msg)}
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       live,
		}, nil
	}
	resp, err := p.recoverQoderQueue(context.Background(), qoderRecoveryProvider(), send, qoderQueuedInitialResponse(t))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream 429 preserved", resp.StatusCode)
	}
	if sendCtx.Err() != nil {
		t.Fatal("replay context canceled before the error body could be read")
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read error body: %v", err)
	}
	if string(out) != msg {
		t.Fatalf("body = %q, want the upstream error message", out)
	}
	// EOF must release the context and close the live body exactly once.
	if sendCtx.Err() == nil {
		t.Fatal("replay context not released after the error body was consumed")
	}
	if got := live.closeCount(); got != 1 {
		t.Fatalf("live closes = %d after EOF, want exactly 1", got)
	}
	// A late caller Close is a no-op, not a second release.
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("late close: %v", err)
	}
	if got := live.closeCount(); got != 1 {
		t.Fatalf("live closes = %d after late Close, want still 1", got)
	}
}

// TestQoderReplayErrorBodyCallerCancelStopsRead proves caller cancellation
// still fails a blocked error-body read: the handoff must not detach the body
// from the caller's context.
func TestQoderReplayErrorBodyCallerCancelStopsRead(t *testing.T) {
	p := qoderRecoveryProxy(t, qoderPolicy(
		10*time.Second, time.Millisecond, 2*time.Millisecond, time.Millisecond, time.Millisecond, 50*time.Millisecond))
	send := func(ctx context.Context) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       &qoderBlockedCtxBody{ctx: ctx},
		}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	resp, err := p.recoverQoderQueue(ctx, qoderRecoveryProvider(), send, qoderQueuedInitialResponse(t))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	start := time.Now()
	_, readErr := io.ReadAll(resp.Body)
	elapsed := time.Since(start)
	if !errors.Is(readErr, context.Canceled) {
		t.Fatalf("read err = %v, want the caller's cancellation", readErr)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("elapsed = %v; the read ignored caller cancellation", elapsed)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestQoderReplayErrorBodyBoundedByRemainingBudget proves a stalled non-2xx
// body is cut off by the remaining recovery budget, and the later Close
// releases the live body exactly once instead of double-closing.
func TestQoderReplayErrorBodyBoundedByRemainingBudget(t *testing.T) {
	p := qoderRecoveryProxy(t, qoderPolicy(
		120*time.Millisecond, time.Millisecond, 2*time.Millisecond, time.Millisecond, time.Millisecond, 50*time.Millisecond))
	var live *qoderBlockedCtxBody
	send := func(ctx context.Context) (*http.Response, error) {
		live = &qoderBlockedCtxBody{ctx: ctx}
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       live,
		}, nil
	}
	resp, err := p.recoverQoderQueue(context.Background(), qoderRecoveryProvider(), send, qoderQueuedInitialResponse(t))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	start := time.Now()
	_, readErr := io.ReadAll(resp.Body)
	elapsed := time.Since(start)
	if !errors.Is(readErr, context.Canceled) {
		t.Fatalf("read err = %v, want the budget cutoff", readErr)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("elapsed = %v; the stalled error body was unbounded", elapsed)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := live.closeCount(); got != 1 {
		t.Fatalf("live closes = %d, want exactly 1", got)
	}
}
