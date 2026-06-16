package server

import (
	"log"
	"net/http"
	"time"

	"airouter/internal/store"
	"airouter/internal/web"
)

type Server struct {
	mux *http.ServeMux
}

func New(s *store.Store) *Server {
	mux := http.NewServeMux()
	web.NewHandler(s).Mount(mux)
	// Phase 2 will mount the proxy endpoints (/v1/*) here.
	return &Server{mux: mux}
}

func (s *Server) Handler() http.Handler {
	return logging(s.mux)
}

// logging is a minimal request logger; structured metrics land in a later phase.
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

// Flush exposes the underlying flusher so SSE proxying (phase 3) keeps working
// through this wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
