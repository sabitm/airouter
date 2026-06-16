package server

import (
	"net/http"

	"airouter/internal/proxy"
	"airouter/internal/store"
	"airouter/internal/web"
)

type Server struct {
	mux *http.ServeMux
}

func New(s *store.Store, debug bool) *Server {
	mux := http.NewServeMux()
	web.NewHandler(s).Mount(mux)
	proxy.New(s, debug).Mount(mux)
	return &Server{mux: mux}
}

func (s *Server) Handler() http.Handler {
	return s.mux
}
