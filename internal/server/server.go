package server

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

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
}

func New(s *store.Store, debugLevel int, logFile io.Writer) *Server {
	mux := http.NewServeMux()
	web.NewHandler(s, debugLevel >= 2).Mount(mux)
	// The proxy only distinguishes on/off (level >= 1); trace lives in the
	// middleware below, which sees every path uniformly.
	proxy.New(s, debugLevel >= 1).Mount(mux)
	return &Server{mux: mux, debugLevel: debugLevel, logFile: logFile}
}

func (s *Server) Handler() http.Handler {
	h := cors(s.mux)
	if s.debugLevel >= 1 {
		return logging(s.debugLevel, s.logFile, h)
	}
	return h
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

// logging records one line per request (method, path, status, latency). At
// level >= 2 it also tees the request and response bodies into the log. Only
// installed in debug mode; non-debug serves the bare mux silently.
//
// When logFile is non-nil the trace is emitted twice: full to the file, and
// truncated to stderr. Otherwise it goes once to the default logger (stderr,
// truncated). Both per-sink loggers carry the default timestamp prefix so their
// lines match the access lines written through the shared default logger.
func logging(level int, logFile io.Writer, next http.Handler) http.Handler {
	var fileTrace, stderrTrace *log.Logger
	if logFile != nil {
		fileTrace = log.New(logFile, "", log.LstdFlags)
		stderrTrace = log.New(os.Stderr, "", log.LstdFlags)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Trace bodies only for provider-facing ingress paths; dashboard asset
		// and HTMX-fragment exchanges would only clutter the log.
		trace := level >= 2 && isProxyPath(r.URL.Path)
		var (
			reqBody []byte
			tinfo   *proxy.TraceInfo
		)
		if trace {
			reqBody = drainRequestBody(r)
			// The serve path records the resolved upstream URL into tinfo so the
			// trace can show the provider hit rather than the inbound path.
			tinfo = &proxy.TraceInfo{}
			r = r.WithContext(proxy.WithTraceInfo(r.Context(), tinfo))
		}

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		if trace {
			sw.capture = &bytes.Buffer{}
			// The file sink logs the whole body, so retain it uncapped; stderr
			// still truncates at format time.
			sw.captureFull = logFile != nil
		}
		next.ServeHTTP(sw, r)

		log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
		if trace {
			logTrace(fileTrace, stderrTrace, r, reqBody, sw, tinfo)
			// Release the (possibly large, uncapped) capture buffer now that it
			// has been written. It is request-scoped and would be collected at
			// handler return regardless; this only trims the lingering window.
			// Peak memory is unavoidable: the full body must be buffered before
			// the trace line can be formatted.
			sw.capture = nil
		}
	})
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
	l.Printf("[trace] <<< %d\n%s", sw.status, traceBody(sw.capture.Bytes(), sw.bytesWritten, limit))
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
	// capture, when non-nil (trace level), accumulates response bytes;
	// bytesWritten tracks the full length for the truncation marker. When
	// captureFull is set (a log file sink wants the whole body) capture grows
	// unbounded; otherwise it stops at traceMaxBody.
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
