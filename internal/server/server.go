package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"airouter/internal/harlog"
	"airouter/internal/proxy"
	"airouter/internal/store"
	"airouter/internal/web"
)

// traceMaxBody caps how many bytes of a request or response body are logged to
// stderr at trace level, so a long stream or large context cannot flood the
// terminal. A configured -log-file captures the full, untruncated bodies.
const traceMaxBody = 4 << 10

type Server struct {
	mux        *http.ServeMux
	debugLevel int
	// logFile, when non-nil, receives full untruncated trace bodies while stderr
	// keeps a truncated copy. nil means stderr-only (truncated) tracing.
	logFile io.Writer
	// har records both legs of proxied exchanges when non-nil (enabled by
	// -har-file). Independent of debugLevel.
	har *harlog.Recorder
}

// New builds the HTTP mux. harFile, when non-empty, enables the in-memory HAR
// recorder (creator version is creatorVersion) and mounts GET /debug/har.
// The recorder is also passed to the proxy for upstream-leg capture.
func New(s *store.Store, debugLevel int, logFile io.Writer, harFile, creatorVersion string) *Server {
	var har *harlog.Recorder
	if harFile != "" {
		har = harlog.New(creatorVersion)
	}
	mux := http.NewServeMux()
	web.NewHandler(s, debugLevel >= 2, logFile).Mount(mux)
	// The proxy only distinguishes on/off (level >= 1); body trace lives in the
	// middleware below, which sees every path uniformly.
	proxy.New(s, debugLevel >= 1, logFile, har).Mount(mux)
	srv := &Server{mux: mux, debugLevel: debugLevel, logFile: logFile, har: har}
	if har != nil {
		mux.HandleFunc("GET /debug/har", srv.handleHAR)
	}
	return srv
}

// HAR returns the live recorder, or nil when HAR capture is disabled.
func (s *Server) HAR() *harlog.Recorder { return s.har }

func (s *Server) Handler() http.Handler {
	h := cors(s.mux)
	// Always attach TraceInfo (and optionally HAR + debug logging). TraceInfo
	// must be present on every request so prepare/header seams can round-trip
	// session ids even when body tracing is off.
	return requestMiddleware(s.debugLevel, s.logFile, s.har, h)
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

// requestMiddleware attaches TraceInfo to every request, optionally records the
// ingress HAR leg, and (when level >= 1) emits the existing access/trace logs.
// HAR capture is independent of debug level.
func requestMiddleware(level int, logFile io.Writer, har *harlog.Recorder, next http.Handler) http.Handler {
	var fileTrace, stderrTrace *log.Logger
	if logFile != nil {
		fileTrace = log.New(logFile, "", log.LstdFlags)
		stderrTrace = log.New(os.Stderr, "", log.LstdFlags)
	}
	accessLog := level >= 1
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Trace bodies only for provider-facing ingress paths; dashboard asset
		// and HTMX-fragment exchanges would only clutter the log.
		trace := level >= 2 && isProxyPath(r.URL.Path)
		harCap := har != nil && isProxyPath(r.URL.Path)
		var reqBody []byte
		// TraceInfo carries per-request cross-stage state (the resolved upstream
		// URL; the Codex/Claude Code session id shared between the request body and
		// an upstream header). It must be attached regardless of debug level so
		// the prepare and header seams can round-trip the session id even when body
		// tracing is off; only the body capture/logging below stays level-gated.
		tinfo := &proxy.TraceInfo{}
		if har != nil {
			tinfo.RequestID = newRequestID()
		}
		r = r.WithContext(proxy.WithTraceInfo(r.Context(), tinfo))

		if trace || harCap {
			reqBody = drainRequestBody(r)
		}

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		if trace || harCap {
			sw.capture = &bytes.Buffer{}
			// Full capture when a log file wants the whole body, or when HAR is
			// on (harlog applies its own 1 MiB cap). Otherwise cap at traceMaxBody.
			sw.captureFull = (trace && logFile != nil) || harCap
		}
		next.ServeHTTP(sw, r)

		if accessLog {
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
		}
		if trace {
			logTrace(fileTrace, stderrTrace, r, reqBody, sw, tinfo)
		}
		if harCap {
			pageID := "page_" + tinfo.RequestID
			title := r.Method + " " + r.URL.RequestURI()
			har.EnsurePage(pageID, title, start)
			// Prefer the scheme/host the client used so the HAR URL is absolute.
			absURL := absoluteRequestURL(r)
			// Snapshot response headers after the handler finished writing them.
			har.Record(harlog.RecordInput{
				PageID:      pageID,
				StartedAt:   start,
				Duration:    time.Since(start),
				Method:      r.Method,
				URL:         absURL,
				ReqHeaders:  r.Header.Clone(),
				ReqBody:     reqBody,
				Status:      sw.status,
				RespHeaders: sw.Header().Clone(),
				RespBody:    sw.capture.Bytes(),
			})
		}
		if sw.capture != nil && (trace || harCap) {
			// Release the (possibly large) capture buffer now that it has been
			// written. Peak memory is unavoidable: the full body must be buffered
			// before the HAR/trace line can be formatted.
			sw.capture = nil
		}
	})
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
// HAR page suffix.
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

// drainRequestBody reads the full body so it can be logged, then restores it
// from the buffer so the handler still sees the complete request. Trace mode is
// operator-enabled, so buffering the body in memory is acceptable here.
func drainRequestBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(nil))
		return b
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b
}

// logTrace emits the captured request and response bodies. When fileTrace is
// non-nil the bodies are written in full to the file and truncated to stderr;
// otherwise a single truncated copy goes to the default logger (stderr).
func logTrace(fileTrace, stderrTrace *log.Logger, r *http.Request, reqBody []byte, sw *statusWriter, tinfo *proxy.TraceInfo) {
	target := r.URL.Path
	if tinfo != nil && tinfo.UpstreamURL != "" {
		target = tinfo.UpstreamURL
	}
	if fileTrace != nil {
		emitTrace(fileTrace, r, reqBody, sw, target, 0)
		emitTrace(stderrTrace, r, reqBody, sw, target, traceMaxBody)
		return
	}
	emitTrace(log.Default(), r, reqBody, sw, target, traceMaxBody)
}

// emitTrace writes one request/response trace pair to l. limit caps each body's
// logged length (<= 0 means unlimited). The request line shows the resolved
// upstream provider URL when one was reached; otherwise (a local /models
// response or a pre-upstream rejection) it falls back to the inbound path.
// Binary responses are summarized rather than dumped.
func emitTrace(l *log.Logger, r *http.Request, reqBody []byte, sw *statusWriter, target string, limit int) {
	l.Printf("[trace] >>> %s %s\n%s", r.Method, target, traceBody(reqBody, len(reqBody), limit))
	if ct := sw.Header().Get("Content-Type"); sw.bytesWritten > 0 && !isTextual(ct) {
		l.Printf("[trace] <<< %d (%s, %d bytes, not logged)", sw.status, ct, sw.bytesWritten)
		return
	}
	var captured []byte
	if sw.capture != nil {
		captured = sw.capture.Bytes()
	}
	l.Printf("[trace] <<< %d\n%s", sw.status, traceBody(captured, sw.bytesWritten, limit))
}

// traceBody renders captured bytes for the log, appending a marker when the
// capture was truncated. total is the full body length; captured may be shorter.
// limit caps the logged length; limit <= 0 logs everything captured.
func traceBody(captured []byte, total, limit int) string {
	if total == 0 {
		return "(empty)"
	}
	if limit > 0 && len(captured) > limit {
		captured = captured[:limit]
	}
	if total > len(captured) {
		return fmt.Sprintf("%s... (truncated, %d bytes total)", captured, total)
	}
	return string(captured)
}

// isTextual reports whether a Content-Type is safe to dump as text. Empty type
// is treated as textual since the proxy's JSON/SSE responses often omit it until
// the first write.
func isTextual(contentType string) bool {
	ct := strings.ToLower(contentType)
	switch {
	case ct == "",
		strings.HasPrefix(ct, "application/json"),
		strings.HasPrefix(ct, "text/"),
		strings.Contains(ct, "event-stream"):
		return true
	default:
		return false
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
	// capture, when non-nil (trace level or HAR), accumulates response bytes;
	// bytesWritten tracks the full length for the truncation marker. When
	// captureFull is set (log-file sink or HAR) capture grows unbounded;
	// otherwise it stops at traceMaxBody.
	capture      *bytes.Buffer
	captureFull  bool
	bytesWritten int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.capture != nil {
		if w.captureFull {
			w.capture.Write(b)
		} else if room := traceMaxBody - w.capture.Len(); room > 0 {
			if room >= len(b) {
				w.capture.Write(b)
			} else {
				w.capture.Write(b[:room])
			}
		}
		w.bytesWritten += len(b)
	}
	return w.ResponseWriter.Write(b)
}

// Flush exposes the underlying flusher so SSE streaming keeps flushing through
// this wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
