package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"airouter/internal/harlog"
	"airouter/internal/observability"
	"airouter/internal/proxy"
	"airouter/internal/store"
	"airouter/internal/web"
)

// requestIDHeader is returned on every response and exposed via CORS so browser
// clients can correlate with terminal logs / HAR pages.
const requestIDHeader = "X-Airouter-Request-ID"

type Server struct {
	mux    *http.ServeMux
	logger *slog.Logger
	// har records both legs of proxied exchanges when non-nil (enabled by
	// -har-file). Independent of logger level.
	har *harlog.Recorder
}

// New builds the HTTP mux. logger may be nil (falls back to slog.Default).
// harFile, when non-empty, enables the in-memory HAR recorder (creator version
// is creatorVersion) and mounts GET /debug/har. The recorder is also passed to
// the proxy for upstream-leg capture.
func New(s *store.Store, logger *slog.Logger, harFile, creatorVersion string) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	var har *harlog.Recorder
	if harFile != "" {
		har = harlog.New(creatorVersion)
	}
	mux := http.NewServeMux()
	httpLog := logger.With("component", "http")
	web.NewHandler(s, logger.With("component", "web")).Mount(mux)
	proxy.New(s, logger.With("component", "proxy"), har).Mount(mux)
	srv := &Server{mux: mux, logger: httpLog, har: har}
	if har != nil {
		mux.HandleFunc("GET /debug/har", srv.handleHAR)
	}
	return srv
}

// HAR returns the live recorder, or nil when HAR capture is disabled.
func (s *Server) HAR() *harlog.Recorder { return s.har }

func (s *Server) Handler() http.Handler {
	h := cors(s.mux)
	// Always attach TraceInfo + request id (and optionally HAR + debug/trace
	// logging). TraceInfo must be present on every request so prepare/header
	// seams can round-trip session ids even when body tracing is off.
	return requestMiddleware(s.logger, s.har, h)
}

func (s *Server) handleHAR(w http.ResponseWriter, r *http.Request) {
	if s.har == nil {
		http.NotFound(w, r)
		return
	}
	data, err := s.har.MarshalHAR()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="airouter.har"`)
	_, _ = w.Write(data)
}

// cors handles browser cross-origin requests. The proxy mounts routes with
// method-specific patterns (POST /messages, ...), so an OPTIONS preflight finds
// the path but no method handler and gets an auto 405 from the mux before any
// handler runs; this middleware answers the preflight itself and adds the
// response headers the browser requires.
//
// It only engages when an Origin header is present, leaving server-to-server
// traffic untouched. The Origin is reflected rather than set to "*" so the
// headers stay valid if a caller ever uses credentialed requests. Authorization
// is still required, so reflecting any origin does not weaken access control.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Add("Vary", "Origin")
		// Expose the correlation header so browser JS can read it.
		exposeRequestID(h)

		// A real preflight carries Access-Control-Request-Method; a bare OPTIONS
		// without it is not a preflight and falls through to the mux.
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			reqHeaders := r.Header.Get("Access-Control-Request-Headers")
			if reqHeaders == "" {
				reqHeaders = "Authorization, Content-Type"
			}
			h.Set("Access-Control-Allow-Headers", reqHeaders)
			h.Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// exposeRequestID merges X-Airouter-Request-ID into Access-Control-Expose-Headers
// without clobbering any value already present.
func exposeRequestID(h http.Header) {
	const expose = "Access-Control-Expose-Headers"
	cur := h.Get(expose)
	if cur == "" {
		h.Set(expose, requestIDHeader)
		return
	}
	for _, p := range strings.Split(cur, ",") {
		if strings.EqualFold(strings.TrimSpace(p), requestIDHeader) {
			return
		}
	}
	h.Set(expose, cur+", "+requestIDHeader)
}

// requestMiddleware attaches TraceInfo and a request id to every request,
// optionally records the ingress HAR leg, and emits access/trace logs gated by
// the logger level. HAR capture is independent of log level.
func requestMiddleware(logger *slog.Logger, har *harlog.Recorder, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := newRequestID()

		// Body observation only for provider-facing ingress paths; dashboard asset
		// and HTMX-fragment exchanges would only clutter the log / HAR.
		proxyPath := isProxyPath(r.URL.Path)
		traceOn := proxyPath && logger.Enabled(r.Context(), observability.LevelTrace)
		harCap := proxyPath && har != nil

		// HAR retains bodies up to harlog.MaxBody. TRACE-only counts bytes the
		// handler actually reads without retaining them (Capture(0)).
		var reqCap *observability.Capture
		switch {
		case harCap:
			reqCap = observability.NewCapture(harlog.MaxBody)
		case traceOn:
			reqCap = observability.NewCapture(0)
		}
		if reqCap != nil && r.Body != nil {
			r.Body = &teeReadCloser{rc: r.Body, cap: reqCap}
		}

		// TraceInfo carries per-request cross-stage state. RequestID is always
		// populated so terminal logs, HAR pages, and the response header share it.
		tinfo := &proxy.TraceInfo{RequestID: reqID}
		ctx := proxy.WithTraceInfo(r.Context(), tinfo)
		ctx = observability.WithRequestID(ctx, reqID)
		r = r.WithContext(ctx)

		// Set the correlation header before the handler runs so even early
		// rejections carry it.
		w.Header().Set(requestIDHeader, reqID)

		var respCap *observability.Capture
		if harCap {
			respCap = observability.NewCapture(harlog.MaxBody)
		}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK, capture: respCap}
		next.ServeHTTP(sw, r)

		log := observability.Logger(r.Context(), logger)
		dur := time.Since(start)
		if log.Enabled(r.Context(), slog.LevelDebug) {
			log.Debug("request_completed",
				"event", "request_completed",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"duration_ms", dur.Milliseconds(),
			)
		}
		if traceOn {
			emitIngressExchange(r.Context(), log, r, tinfo, reqCap, sw, dur)
		}
		if harCap {
			pageID := "page_" + reqID
			title := r.Method + " " + r.URL.RequestURI()
			har.EnsurePage(pageID, title, start)
			var reqBody []byte
			var reqSize int
			if reqCap != nil {
				reqBody = reqCap.Bytes()
				reqSize = int(reqCap.Total())
			}
			var respBody []byte
			var respSize int
			if respCap != nil {
				respBody = respCap.Bytes()
				respSize = int(respCap.Total())
			}
			har.Record(harlog.RecordInput{
				PageID:       pageID,
				StartedAt:    start,
				Duration:     dur,
				Method:       r.Method,
				URL:          absoluteRequestURL(r),
				ReqHeaders:   r.Header.Clone(),
				ReqBody:      reqBody,
				ReqBodySize:  reqSize,
				Status:       sw.status,
				RespHeaders:  sw.Header().Clone(),
				RespBody:     respBody,
				RespBodySize: respSize,
			})
		}
	})
}

// emitIngressExchange writes a metadata-only ingress_exchange TRACE event.
// Request byte counts come from the tee Capture (bytes the handler consumed);
// response byte counts use statusWriter.bytesWritten (no response body retention
// for TRACE alone).
func emitIngressExchange(ctx context.Context, log *slog.Logger, r *http.Request, tinfo *proxy.TraceInfo, reqCap *observability.Capture, sw *statusWriter, dur time.Duration) {
	var reqBytes int64
	if reqCap != nil {
		reqBytes = reqCap.Total()
	}
	attrs := []any{
		"event", "ingress_exchange",
		"method", r.Method,
		"path", r.URL.Path,
		"status", sw.status,
		"duration_ms", dur.Milliseconds(),
		"request_content_type", r.Header.Get("Content-Type"),
		"request_bytes", reqBytes,
		"response_content_type", sw.Header().Get("Content-Type"),
		"response_bytes", sw.bytesWritten,
	}
	if tinfo != nil && tinfo.UpstreamURL != "" {
		attrs = append(attrs, "upstream_url", tinfo.UpstreamURL)
	}
	log.Log(ctx, observability.LevelTrace, "ingress_exchange", attrs...)
}

// absoluteRequestURL builds an absolute URL for the ingress HAR entry.
func absoluteRequestURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host + r.URL.RequestURI()
}

// newRequestID returns a short random hex id used as TraceInfo.RequestID /
// HAR page suffix / X-Airouter-Request-ID.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// isProxyPath reports whether a path is a provider-facing ingress endpoint, the
// only traffic worth tracing. It mirrors the routes mounted in proxy.Mount; a
// new ingress route must be added here to be traced.
func isProxyPath(p string) bool {
	switch strings.TrimPrefix(p, "/v1") {
	case "/messages", "/chat/completions", "/responses", "/models":
		return true
	}
	return false
}

// teeReadCloser observes bytes as the handler reads them without reading ahead.
// This keeps MaxBytesReader and partial-body rejects honest for logging/HAR.
type teeReadCloser struct {
	rc  io.ReadCloser
	cap *observability.Capture
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 && t.cap != nil {
		_, _ = t.cap.Write(p[:n])
	}
	return n, err
}

func (t *teeReadCloser) Close() error { return t.rc.Close() }

type statusWriter struct {
	http.ResponseWriter
	status       int
	wroteHeader  bool
	capture      *observability.Capture
	bytesWritten int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	// Count and capture only bytes actually accepted by the underlying writer.
	if n > 0 {
		w.bytesWritten += int64(n)
		if w.capture != nil {
			_, _ = w.capture.Write(b[:n])
		}
	}
	return n, err
}

// Flush exposes the underlying flusher so SSE streaming keeps flushing through
// this wrapper. net/http commits an implicit 200 on Flush, so mirror that state
// before delegating.
func (w *statusWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
