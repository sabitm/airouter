package server

import (
	"log"
	"net/http"
	"time"

	"airouter/internal/proxy"
	"airouter/internal/store"
	"airouter/internal/web"
)

type Server struct {
	mux   *http.ServeMux
	debug bool
}

func New(s *store.Store, debug bool) *Server {
	mux := http.NewServeMux()
	web.NewHandler(s).Mount(mux)
	proxy.New(s, debug).Mount(mux)
	return &Server{mux: mux, debug: debug}
}

func (s *Server) Handler() http.Handler {
	if s.debug {
		return logging(s.mux)
	}
	return s.mux
}

// logging records one line per request (method, path, status, latency). Only
// installed in debug mode; non-debug serves the bare mux silently.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush exposes the underlying flusher so SSE streaming keeps flushing through
// this wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
