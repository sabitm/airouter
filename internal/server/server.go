package server

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"airouter/internal/proxy"
	"airouter/internal/store"
	"airouter/internal/web"
)

// traceMaxBody caps how many bytes of a request or response body are logged at
// trace level, so a long stream or large context cannot flood the terminal.
const traceMaxBody = 16 << 10

type Server struct {
	mux        *http.ServeMux
	debugLevel int
}

func New(s *store.Store, debugLevel int) *Server {
	mux := http.NewServeMux()
	web.NewHandler(s, debugLevel >= 2).Mount(mux)
	// The proxy only distinguishes on/off (level >= 1); trace lives in the
	// middleware below, which sees every path uniformly.
	proxy.New(s, debugLevel >= 1).Mount(mux)
	return &Server{mux: mux, debugLevel: debugLevel}
}

func (s *Server) Handler() http.Handler {
	h := cors(s.mux)
	if s.debugLevel >= 1 {
		return logging(s.debugLevel, h)
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
func logging(level int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Trace bodies only for provider-facing ingress paths; dashboard asset
		// and HTMX-fragment exchanges would only clutter the log.
		trace := level >= 2 && isProxyPath(r.URL.Path)
		var reqBody []byte
		if trace {
			reqBody = drainRequestBody(r)
		}

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		if trace {
			sw.capture = &bytes.Buffer{}
		}
		next.ServeHTTP(sw, r)

		log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
		if trace {
			logTrace(r, reqBody, sw)
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

// logTrace emits the captured request and response bodies. Binary responses are
// summarized rather than dumped so the log stays readable.
func logTrace(r *http.Request, reqBody []byte, sw *statusWriter) {
	log.Printf("[trace] >>> %s %s\n%s", r.Method, r.URL.Path, traceBody(reqBody, len(reqBody)))
	if ct := sw.Header().Get("Content-Type"); sw.bytesWritten > 0 && !isTextual(ct) {
		log.Printf("[trace] <<< %d (%s, %d bytes, not logged)", sw.status, ct, sw.bytesWritten)
		return
	}
	log.Printf("[trace] <<< %d\n%s", sw.status, traceBody(sw.capture.Bytes(), sw.bytesWritten))
}

// traceBody renders captured bytes for the log, appending a marker when the
// capture was truncated. total is the full body length; captured may be shorter.
func traceBody(captured []byte, total int) string {
	if total == 0 {
		return "(empty)"
	}
	if len(captured) > traceMaxBody {
		captured = captured[:traceMaxBody]
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
	// capture, when non-nil (trace level), accumulates response bytes up to
	// traceMaxBody; bytesWritten tracks the full length for the truncation marker.
	capture      *bytes.Buffer
	bytesWritten int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.capture != nil {
		if room := traceMaxBody - w.capture.Len(); room > 0 {
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
