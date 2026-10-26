package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"airouter/internal/domain"
	"airouter/internal/observability"
	"airouter/internal/proxy/qoder"
)

const (
	// qoderQueueMaxWait is the total recovery budget for one queued request.
	// The queue drains in ~30s windows, so three minutes covers several of
	// them and still returns a retryable error instead of hanging the caller.
	// The caller's own deadline always binds earlier.
	qoderQueueMaxWait = 3 * time.Minute
	// qoderQueuePollTimeout bounds one status poll; the budget bounds the
	// whole recovery, so a hung poll must not eat it in one step.
	qoderQueuePollTimeout = 10 * time.Second
	// qoderQueueDelay* mirror the reference client's retry clamps. The
	// status endpoint has been observed answering isQueued:false while the
	// inference call is still refused, so a status-derived delay never drops
	// below the floor and only one ready hint is trusted.
	qoderQueueDelayDefault = 30 * time.Second
	qoderQueueDelayMin     = 15 * time.Second
	qoderQueueDelayMax     = 60 * time.Second
	qoderQueueStatusFloor  = 30 * time.Second
)

// qoderQueuePolicy holds the queue-recovery timing knobs. The zero value uses
// production defaults; tests inject short budgets and delays so no test sleeps
// for a real retry interval. The struct lives on the Proxy instance, so
// concurrent requests share it read-only and there is no global test state.
type qoderQueuePolicy struct {
	maxWait      time.Duration
	minDelay     time.Duration
	maxDelay     time.Duration
	defaultDelay time.Duration
	statusFloor  time.Duration
	pollTimeout  time.Duration
	now          func() time.Time
}

func (p *Proxy) qoderQueueDefaults() qoderQueuePolicy {
	pol := p.qoderQueuePolicy
	if pol.maxWait <= 0 {
		pol.maxWait = qoderQueueMaxWait
	}
	if pol.minDelay <= 0 {
		pol.minDelay = qoderQueueDelayMin
	}
	if pol.maxDelay <= 0 {
		pol.maxDelay = qoderQueueDelayMax
	}
	if pol.defaultDelay <= 0 {
		pol.defaultDelay = qoderQueueDelayDefault
	}
	if pol.statusFloor <= 0 {
		pol.statusFloor = qoderQueueStatusFloor
	}
	if pol.pollTimeout <= 0 {
		pol.pollTimeout = qoderQueuePollTimeout
	}
	if pol.now == nil {
		pol.now = time.Now
	}
	return pol
}

// delay computes the wait before the next replay. Two hint sources feed this
// and they are not equally trustworthy: the inference refusal describes the
// queue the request actually sits in, so its hint is honored; a status-derived
// delay never drops below the floor. Both are clamped into min..max.
func (pol qoderQueuePolicy) delay(info *qoder.QueueInfo, fromStatus bool) time.Duration {
	d := pol.defaultDelay
	switch {
	case info.RetryAfterSeconds != nil:
		d = time.Duration(*info.RetryAfterSeconds) * time.Second
	case info.WaitTime != nil:
		d = time.Duration(*info.WaitTime) * time.Second
	}
	if fromStatus && d < pol.statusFloor {
		d = pol.statusFloor
	}
	if d < pol.minDelay {
		d = pol.minDelay
	}
	if d > pol.maxDelay {
		d = pol.maxDelay
	}
	return d
}

// qoderReplayBody replays the bytes consumed while peeking the first frame,
// then continues from the live upstream body. The peek must not consume
// inference output: the reconstructed body hands the decoder the exact bytes
// already read, so a successful send is relayed exactly once. peekErr carries
// a real read error observed during the peek; it is re-injected after the
// replay so a one-shot (n>0, err) reader cannot lose the error between the
// reconstruction and the decoder.
type qoderReplayBody struct {
	replay  []byte
	live    io.ReadCloser
	off     int
	peekErr error
	onClose func()
}

func (b *qoderReplayBody) Read(p []byte) (int, error) {
	if b.off < len(b.replay) {
		n := copy(p, b.replay[b.off:])
		b.off += n
		return n, nil
	}
	if b.peekErr != nil {
		err := b.peekErr
		b.peekErr = nil
		return 0, err
	}
	return b.live.Read(p)
}

func (b *qoderReplayBody) Close() error {
	err := b.live.Close()
	if b.onClose != nil {
		b.onClose()
	}
	return err
}

// qoderErrorBody hands a non-2xx replay response to the caller without
// canceling the replay context underneath it, so the upstream error message
// stays readable. Reads stay bound to the replay context: caller cancellation
// still fails them, and the still-armed send timer cuts a stalled body off at
// the budget deadline. The first Close or terminal EOF stops the timer,
// releases the context, and closes the live body exactly once.
type qoderErrorBody struct {
	mu      sync.Mutex
	live    io.ReadCloser
	release func() // stops the send timer and cancels the replay context
	done    bool   // live body closed; later reads answer EOF, later Closes no-op
}

func (b *qoderErrorBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return 0, io.EOF
	}
	b.mu.Unlock()
	n, err := b.live.Read(p)
	if errors.Is(err, io.EOF) {
		b.finish()
	}
	return n, err
}

func (b *qoderErrorBody) Close() error {
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return nil
	}
	b.done = true
	b.mu.Unlock()
	b.release()
	return b.live.Close()
}

// finish releases the timer and context and closes the live body once; later
// reads answer EOF and later Closes are no-ops.
func (b *qoderErrorBody) finish() {
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return
	}
	b.done = true
	b.mu.Unlock()
	b.release()
	_ = b.live.Close()
}

// closeAfterDelay closes body once after d, breaking a blocked read. The
// returned func stops the timer and reports whether the close already fired;
// callers must check it before handing the body onward, because a fired
// watchdog means the body is dead. No goroutine is allocated: time.AfterFunc
// runs on the runtime timer path.
func closeAfterDelay(body io.Closer, d time.Duration) (fired func() bool) {
	var mu sync.Mutex
	closed := false
	timer := time.AfterFunc(d, func() {
		mu.Lock()
		closed = true
		mu.Unlock()
		_ = body.Close()
	})
	return func() bool {
		timer.Stop()
		mu.Lock()
		defer mu.Unlock()
		return closed
	}
}

// recoverQoderQueue watches a Qoder stream for the model-queue refusal on its
// first frame and replays the exact same prepared body until the model drains
// or the recovery budget expires. Across replays the body bytes, request id,
// request set id, and session id never change; each send re-resolves the
// effective token through the oauth seam and re-signs the exact wire bytes
// with fresh COSY credentials. A successful stream is returned exactly once —
// the peeked prefix is replayed, never re-sent.
//
// The recovery budget starts before the initial refusal inspection and bounds
// every step of recovery: waits, status polls, replay header sends, and
// first-frame reads. Once a response proves to be a real inference stream it
// is released from the budget and answers only to the caller's context, so a
// normal successful stream is never killed by the queue deadline.
func (p *Proxy) recoverQoderQueue(ctx context.Context, provider *domain.Provider, send func(context.Context) (*http.Response, error), resp *http.Response) (*http.Response, error) {
	pol := p.qoderQueueDefaults()
	logger := observability.Logger(ctx, p.logger)
	startedAt := pol.now()
	deadline := startedAt.Add(pol.maxWait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	remaining := deadline.Sub(pol.now())
	if remaining <= 0 {
		resp.Body.Close()
		return qoderQueueBudgetResponse(nil, pol.now().Sub(startedAt)), nil
	}
	// A stalled first frame must not hold the request past the budget: the
	// watchdog closes the body to break a blocked peek. The body belongs to
	// the original request context, so closing is the only way to unblock it.
	stopInitialWatch := closeAfterDelay(resp.Body, remaining)
	consumed, payload, peekErr := qoder.PeekFirstSSEFrame(resp.Body)
	info := qoder.ParseQueueInfo(payload)
	if info == nil {
		expired := stopInitialWatch()
		if expired {
			// The budget expired while inspecting the first frame; the body
			// was closed under the peek and cannot be handed on.
			_ = resp.Body.Close()
			return qoderQueueBudgetResponse(nil, pol.now().Sub(startedAt)), nil
		}
		if peekErr != nil && ctx.Err() != nil {
			// Caller cancellation must close and surface, never hide behind a
			// half-read body.
			_ = resp.Body.Close()
			return nil, ctx.Err()
		}
		resp.Body = &qoderReplayBody{replay: consumed, live: resp.Body, peekErr: peekErr}
		return resp, nil
	}
	// Queue refusal: this response carries no inference. Close it before
	// waiting so the connection is not held open during recovery.
	if !stopInitialWatch() {
		_ = resp.Body.Close()
	}
	logger.Debug("qoder_queue_detected",
		"event", "qoder_queue_detected",
		"provider", provider.Name,
		"queue_type", info.QueueType,
		"model_key", info.ModelKey,
	)
	statusPollsEnabled := true
	readyReplayUsed := false
	lastInfo := info
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if remaining := deadline.Sub(pol.now()); remaining <= 0 {
			return qoderQueueBudgetResponse(lastInfo, pol.now().Sub(startedAt)), nil
		}
		delay := pol.delay(lastInfo, false)
		if statusPollsEnabled {
			polled, ok := p.pollQoderQueueStatus(ctx, pol, provider, lastInfo, deadline.Sub(pol.now()))
			if !ok {
				// No usable status endpoint: fall back to timed replays.
				statusPollsEnabled = false
				logger.Debug("qoder_queue_status_unavailable",
					"event", "qoder_queue_status_unavailable",
					"provider", provider.Name,
				)
			} else if polled.IsQueued != nil && !*polled.IsQueued {
				// The status endpoint says ready, but it has been observed
				// saying that while the inference call is still refused.
				// Trust one ready hint; after one contradicting replay stop
				// trusting the endpoint for this recovery.
				if readyReplayUsed {
					statusPollsEnabled = false
					logger.Debug("qoder_queue_status_untrusted",
						"event", "qoder_queue_status_untrusted",
						"provider", provider.Name,
					)
				} else {
					readyReplayUsed = true
					delay = 0
				}
			} else {
				delay = pol.delayFromStatus(polled)
			}
		}
		if delay > 0 {
			// No wait may outlive the budget, however large the retry hint.
			if remaining := deadline.Sub(pol.now()); remaining < delay {
				delay = remaining
			}
			if delay > 0 {
				if err := sleepBounded(ctx, delay); err != nil {
					return nil, err
				}
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
		}
		// Budget recheck directly before every replay; the ready path skips
		// the sleep, so this is the only gate for it.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if remaining := deadline.Sub(pol.now()); remaining <= 0 {
			return qoderQueueBudgetResponse(lastInfo, pol.now().Sub(startedAt)), nil
		}
		// Replay sends run on a context bounded by the remaining budget, so a
		// blocked header send or first-frame read cannot exceed it. The
		// context still derives from the caller's, so caller cancellation
		// propagates. A handed-off stream detaches from this bound: the timer
		// is stopped before handoff and the body's Close releases the
		// context, leaving the stream answerable only to the caller.
		sendCtx, cancelSend := context.WithCancel(ctx)
		sendTimer := time.AfterFunc(deadline.Sub(pol.now()), cancelSend)
		// Fresh effective token for every send; the request-local provider
		// copy is mutated in place and generic header construction is unchanged.
		if err := p.resolveToken(sendCtx, provider, false); err != nil {
			logger.Debug("oauth_resolve_failed",
				"event", "oauth_resolve_failed",
				"provider", provider.Name,
				"error", "OAuth token resolution failed",
			)
		}
		next, err := send(sendCtx)
		if err != nil {
			// Classify the failure before any cleanup: cancelSend() below makes
			// sendCtx.Err() non-nil regardless of the cause, so the source
			// must be decided first. A timer that already fired means the
			// budget expired even if its cancelSend has not run yet.
			callerDone := ctx.Err() != nil
			timerFired := !sendTimer.Stop()
			budgetGone := timerFired || sendCtx.Err() != nil
			cancelSend()
			if callerDone {
				return nil, ctx.Err()
			}
			if budgetGone {
				// The budget expired while the replay send was in flight;
				// this is budget expiry, not a transport failure.
				return qoderQueueBudgetResponse(lastInfo, pol.now().Sub(startedAt)), nil
			}
			return next, err
		}
		if next.StatusCode < 200 || next.StatusCode >= 300 {
			// HTTP-level failure: hand the body to the caller with the replay
			// context still live, so the upstream error message stays
			// readable. The send timer stays armed and cuts a stalled error
			// body off at the budget deadline; caller cancellation still
			// propagates through it. Close or terminal EOF releases the
			// timer and the context exactly once.
			next.Body = &qoderErrorBody{
				live: next.Body,
				release: func() {
					sendTimer.Stop()
					cancelSend()
				},
			}
			// The caller maps the status from the body.
			return next, nil
		}
		consumed2, payload2, peekErr2 := qoder.PeekFirstSSEFrame(next.Body)
		info2 := qoder.ParseQueueInfo(payload2)
		if info2 == nil {
			// A real inference (or a broken non-queue) stream: release it from
			// the recovery budget before handoff. The fired timer must win
			// over a not-yet-observed context error, or a stream whose context
			// is about to be canceled would be handed off alive.
			timerFired := !sendTimer.Stop()
			if ctxErr := ctx.Err(); ctxErr != nil {
				_ = next.Body.Close()
				cancelSend()
				return nil, ctxErr
			}
			if timerFired || sendCtx.Err() != nil {
				// The budget expired while the first frame was being read; the
				// body was canceled underneath the peek.
				_ = next.Body.Close()
				cancelSend()
				return qoderQueueBudgetResponse(lastInfo, pol.now().Sub(startedAt)), nil
			}
			next.Body = &qoderReplayBody{replay: consumed2, live: next.Body, peekErr: peekErr2, onClose: cancelSend}
			return next, nil
		}
		// Another refusal: close the queued response before waiting again.
		sendTimer.Stop()
		cancelSend()
		_ = next.Body.Close()
		lastInfo = info2
	}
}

// delayFromStatus computes a status-poll-derived delay, which never drops
// below the floor because the endpoint's ready hints are unreliable.
func (pol qoderQueuePolicy) delayFromStatus(st *qoder.QueueStatus) time.Duration {
	d := pol.defaultDelay
	switch {
	case st.RetryAfterSeconds != nil:
		d = time.Duration(*st.RetryAfterSeconds) * time.Second
	case st.WaitTime != nil:
		d = time.Duration(*st.WaitTime) * time.Second
	}
	if d < pol.statusFloor {
		d = pol.statusFloor
	}
	if d < pol.minDelay {
		d = pol.minDelay
	}
	if d > pol.maxDelay {
		d = pol.maxDelay
	}
	return d
}

// pollQoderQueueStatus queries the signed queue-status endpoint keyed by the
// original request set id. One poll is bounded by min(remaining, pollTimeout):
// a hung poll must consume the budget, not extend it. ok=false means the
// endpoint is missing, erroring, or answering an unknown shape; the caller
// falls back to timed replays.
func (p *Proxy) pollQoderQueueStatus(ctx context.Context, pol qoderQueuePolicy, provider *domain.Provider, info *qoder.QueueInfo, remaining time.Duration) (*qoder.QueueStatus, bool) {
	reqID, setID, sessID := "", "", ""
	modelKey := info.ModelKey
	if t := traceInfoFrom(ctx); t != nil {
		reqID, setID, sessID = t.QoderRequestID, t.QoderRequestSetID, t.QoderSessionID
		if modelKey == "" {
			modelKey = t.QoderModelKey
		}
	}
	if setID == "" {
		return nil, false
	}
	statusURL := qoder.QueueStatusURL(effectiveProviderBaseURL(provider), setID, modelKey, info.QueueType)
	if err := p.resolveToken(ctx, provider, false); err != nil {
		observability.Logger(ctx, p.logger).Debug("oauth_resolve_failed",
			"event", "oauth_resolve_failed",
			"provider", provider.Name,
			"error", "OAuth token resolution failed",
		)
	}
	headers, err := qoder.BuildCosyHeaders([]byte{}, statusURL, qoder.CredsFromProvider(provider))
	if err != nil {
		return nil, false
	}
	if remaining > pol.pollTimeout {
		remaining = pol.pollTimeout
	}
	if remaining <= 0 {
		return nil, false
	}
	hctx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// Set after the COSY copy: BuildCosyHeaders emits its own X-Request-Id,
	// and the poll must carry the original inference's correlation id.
	req.Header.Set("X-Request-ID", reqID)
	req.Header.Set("X-Session-ID", sessID)
	started := pol.now()
	resp, err := p.streamClient.Do(req)
	if err != nil {
		if harRecorder(ctx) != nil {
			p.recordUpstreamHAR(ctx, started, pol.now().Sub(started), req.Method, statusURL, req.Header.Clone(), nil, 0, nil, nil, "", 0, err.Error())
		}
		return nil, false
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	mime := resp.Header.Get("Content-Type")
	if harRecorder(ctx) != nil {
		failure := ""
		if readErr != nil {
			failure = readErr.Error()
		}
		p.recordUpstreamHAR(ctx, started, pol.now().Sub(started), req.Method, statusURL, req.Header.Clone(), nil, resp.StatusCode, resp.Header.Clone(), body, mime, len(body), failure)
	}
	if readErr != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false
	}
	st := qoder.ParseQueueStatus(string(body))
	return st, st != nil
}

// qoderQueueBudgetResponse returns the structured retryable envelope for a
// request whose queue budget expired. It is an HTTP failure the caller maps
// to a 503, never fabricated assistant content.
func qoderQueueBudgetResponse(info *qoder.QueueInfo, waited time.Duration) *http.Response {
	queueType := "?"
	if info != nil && info.QueueType != "" {
		queueType = info.QueueType
	}
	body := fmt.Sprintf(`{"error":{"message":%q,"code":"qoder_model_queued","type":"rate_limit_error"}}`,
		fmt.Sprintf("qoder model is still queued after %s (queueType=%s); retry shortly", formatQueueDuration(waited), queueType))
	return &http.Response{
		StatusCode:    http.StatusServiceUnavailable,
		Body:          io.NopCloser(strings.NewReader(body)),
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		ContentLength: int64(len(body)),
	}
}

func formatQueueDuration(d time.Duration) string {
	secs := int(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	return fmt.Sprintf("%dm%ds", secs/60, secs%60)
}

// sleepBounded waits for d or until the caller cancels. Recovery deadlines are
// enforced by the loop's budget checks, so no queue wait outlives the caller.
func sleepBounded(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
