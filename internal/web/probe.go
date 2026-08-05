package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"airouter/internal/observability"
)

// probeCaptureMax is the in-memory bound for a probe response body. Bytes beyond
// this are drained to io.Discard so the connection can be reused while BodySize
// still reflects the full wire length.
const probeCaptureMax = 1 << 20 // 1 MiB

// probeResult is the outcome of one outbound dashboard probe.
type probeResult struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	BodySize   int64
	Truncated  bool
	Duration   time.Duration
}

// executeProbe runs req, captures a bounded body, and emits metadata-only TRACE
// probe_request / probe_response events. Transport and body-read errors are
// logged once here (DEBUG) so callers must not re-log them. Auth headers and
// body bytes are never logged.
func executeProbe(ctx context.Context, logger *slog.Logger, client *http.Client, req *http.Request, operation string) (probeResult, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if client == nil {
		client = upstreamClient
	}
	log := observability.Logger(ctx, logger)
	start := time.Now()

	if log.Enabled(ctx, observability.LevelTrace) {
		// ContentLength is -1 when unknown; do not clone/read GetBody just for logging.
		size := req.ContentLength
		log.Log(ctx, observability.LevelTrace, "probe_request",
			"event", "probe_request",
			"operation", operation,
			"method", req.Method,
			"url", req.URL.String(),
			"content_type", req.Header.Get("Content-Type"),
			"size", size,
		)
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Debug("probe_transport_failed",
			"event", "probe_transport_failed",
			"operation", operation,
			"method", req.Method,
			"url", req.URL.String(),
			"error", err,
		)
		return probeResult{Duration: time.Since(start)}, err
	}
	defer resp.Body.Close()

	body, total, truncated, rerr := readBounded(resp.Body, probeCaptureMax)
	pr := probeResult{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header.Clone(),
		Body:       body,
		BodySize:   total,
		Truncated:  truncated,
		Duration:   time.Since(start),
	}
	if rerr != nil {
		log.Debug("probe_read_failed",
			"event", "probe_read_failed",
			"operation", operation,
			"method", req.Method,
			"url", req.URL.String(),
			"status", resp.StatusCode,
			"error", rerr,
		)
		return pr, rerr
	}

	if log.Enabled(ctx, observability.LevelTrace) {
		log.Log(ctx, observability.LevelTrace, "probe_response",
			"event", "probe_response",
			"operation", operation,
			"method", req.Method,
			"url", req.URL.String(),
			"status", resp.StatusCode,
			"duration_ms", pr.Duration.Milliseconds(),
			"content_type", resp.Header.Get("Content-Type"),
			"size", total,
			"truncated", truncated,
		)
	}
	return pr, nil
}

// readBounded retains at most limit bytes, drains the remainder to count the
// full size, and reports whether truncation occurred.
func readBounded(r io.Reader, limit int) (body []byte, total int64, truncated bool, err error) {
	cap := observability.NewCapture(limit)
	// Tee through Capture while also retaining the prefix for callers that need
	// the bytes (json.Unmarshal). Use a limited reader for the retained part,
	// then drain.
	buf := make([]byte, 32*1024)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			total += int64(n)
			_, _ = cap.Write(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return append([]byte(nil), cap.Bytes()...), total, total > int64(len(cap.Bytes())), rerr
		}
	}
	out := append([]byte(nil), cap.Bytes()...)
	return out, total, total > int64(len(out)), nil
}
