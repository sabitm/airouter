package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// noopHandler returns 200 OK without touching CORS headers, so tests can
// assert what cors() added before delegating.
func noopHandler(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	})
}

func TestCORS_NoOriginPassthrough(t *testing.T) {
	h := cors(noopHandler(http.StatusOK))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO = %q, want none for server-to-server request", got)
	}
	if got := rec.Header().Values("Vary"); len(got) != 0 {
		t.Errorf("Vary = %v, want none", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (delegated to next)", rec.Code)
	}
}

func TestCORS_Preflight(t *testing.T) {
	cases := []struct {
		name     string
		acrh     string
		wantACRH string
		wantACRM string
	}{
		{"default headers", "", "Authorization, Content-Type", "GET, POST, OPTIONS"},
		{"echoed headers", "X-Custom, Authorization", "X-Custom, Authorization", "GET, POST, OPTIONS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := cors(noopHandler(http.StatusOK))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodOptions, "/v1/messages", nil)
			req.Header.Set("Origin", "https://app.example.com")
			req.Header.Set("Access-Control-Request-Method", http.MethodPost)
			if tc.acrh != "" {
				req.Header.Set("Access-Control-Request-Headers", tc.acrh)
			}
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204 (preflight short-circuit)", rec.Code)
			}
			wantOrigin := "https://app.example.com"
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != wantOrigin {
				t.Errorf("ACAO = %q, want %q", got, wantOrigin)
			}
			if got := rec.Header().Get("Access-Control-Allow-Methods"); got != tc.wantACRM {
				t.Errorf("ACA-Methods = %q, want %q", got, tc.wantACRM)
			}
			if got := rec.Header().Get("Access-Control-Allow-Headers"); got != tc.wantACRH {
				t.Errorf("ACA-Headers = %q, want %q", got, tc.wantACRH)
			}
			if got := rec.Header().Get("Access-Control-Max-Age"); got != "86400" {
				t.Errorf("ACA-Max-Age = %q, want 86400", got)
			}
			if !containsHeader(rec.Header().Values("Vary"), "Origin") {
				t.Errorf("Vary = %v, want to contain Origin", rec.Header().Values("Vary"))
			}
			if rec.Body.Len() != 0 {
				t.Errorf("preflight body = %q, want empty", rec.Body.String())
			}
		})
	}
}

func TestCORS_NonPreflightWithOrigin(t *testing.T) {
	h := cors(noopHandler(http.StatusOK))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://app.example.com")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (delegated to next)", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("ACAO = %q, want reflected origin", got)
	}
	if !containsHeader(rec.Header().Values("Vary"), "Origin") {
		t.Errorf("Vary = %v, want to contain Origin", rec.Header().Values("Vary"))
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "" {
		t.Errorf("ACA-Methods = %q, want none on non-preflight", got)
	}
}

func TestCORS_BareOptionsWithoutACRMFallsThrough(t *testing.T) {
	h := cors(noopHandler(http.StatusTeapot))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://app.example.com")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418 (bare OPTIONS delegates, not preflight)", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "" {
		t.Errorf("ACA-Methods = %q, want none (not a preflight)", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("ACAO = %q, want reflected origin", got)
	}
}

func containsHeader(vals []string, want string) bool {
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}

func TestIsProxyPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/messages", true},
		{"/v1/chat/completions", true},
		{"/v1/responses", true},
		{"/v1/models", true},
		{"/messages", true},
		{"/chat/completions", true},
		{"/responses", true},
		{"/models", true},
		{"/dashboard/providers", false},
		{"/dashboard", false},
		{"/v1/unknown", false},
		{"/dashboard/logs/clear", false},
		{"/static/dashboard.js", false},
		{"/", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := isProxyPath(tc.path); got != tc.want {
				t.Errorf("isProxyPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestStatusWriterTruncation(t *testing.T) {
	t.Run("truncated caps capture but counts full bytes", func(t *testing.T) {
		inner := httptest.NewRecorder()
		sw := &statusWriter{ResponseWriter: inner, status: http.StatusOK, capture: &bytes.Buffer{}}
		body := bytes.Repeat([]byte("x"), traceMaxBody+64)
		n, err := sw.Write(body)
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if n != len(body) {
			t.Errorf("wrote %d, want %d", n, len(body))
		}
		if sw.capture.Len() != traceMaxBody {
			t.Errorf("capture = %d bytes, want %d (truncated)", sw.capture.Len(), traceMaxBody)
		}
		if sw.bytesWritten != len(body) {
			t.Errorf("bytesWritten = %d, want %d", sw.bytesWritten, len(body))
		}
		if inner.Body.Len() != len(body) {
			t.Errorf("underlying wrote %d, want %d", inner.Body.Len(), len(body))
		}
	})

	t.Run("captureFull grows unbounded", func(t *testing.T) {
		inner := httptest.NewRecorder()
		sw := &statusWriter{ResponseWriter: inner, status: http.StatusOK, capture: &bytes.Buffer{}, captureFull: true}
		body := bytes.Repeat([]byte("y"), traceMaxBody*3)
		if _, err := sw.Write(body); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if sw.capture.Len() != len(body) {
			t.Errorf("capture = %d, want %d (unbounded)", sw.capture.Len(), len(body))
		}
		if !bytes.Equal(sw.capture.Bytes(), body) {
			t.Errorf("capture content mismatch")
		}
	})

	t.Run("capture nil does not track", func(t *testing.T) {
		inner := httptest.NewRecorder()
		sw := &statusWriter{ResponseWriter: inner, status: http.StatusOK}
		body := []byte("no capture")
		if _, err := sw.Write(body); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if sw.bytesWritten != 0 {
			t.Errorf("bytesWritten = %d, want 0 when capture is nil", sw.bytesWritten)
		}
		if !bytes.Equal(inner.Body.Bytes(), body) {
			t.Errorf("underlying body = %q, want %q", inner.Body.Bytes(), body)
		}
	})

	t.Run("WriteHeader records status", func(t *testing.T) {
		inner := httptest.NewRecorder()
		sw := &statusWriter{ResponseWriter: inner, status: http.StatusOK}
		sw.WriteHeader(http.StatusTeapot)
		if sw.status != http.StatusTeapot {
			t.Errorf("status = %d, want %d", sw.status, http.StatusTeapot)
		}
		if inner.Code != http.StatusTeapot {
			t.Errorf("underlying status = %d, want %d", inner.Code, http.StatusTeapot)
		}
	})

	t.Run("small write under cap captured fully", func(t *testing.T) {
		inner := httptest.NewRecorder()
		sw := &statusWriter{ResponseWriter: inner, status: http.StatusOK, capture: &bytes.Buffer{}}
		body := []byte("hello")
		if _, err := sw.Write(body); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if !bytes.Equal(sw.capture.Bytes(), body) {
			t.Errorf("capture = %q, want %q", sw.capture.Bytes(), body)
		}
		if sw.bytesWritten != len(body) {
			t.Errorf("bytesWritten = %d, want %d", sw.bytesWritten, len(body))
		}
	})

	t.Run("chunked writes accumulate then truncate", func(t *testing.T) {
		inner := httptest.NewRecorder()
		sw := &statusWriter{ResponseWriter: inner, status: http.StatusOK, capture: &bytes.Buffer{}}
		chunk := bytes.Repeat([]byte("z"), traceMaxBody/2+10)
		sw.Write(chunk) // fills past half
		sw.Write(chunk) // second push overflows, should truncate
		if sw.capture.Len() != traceMaxBody {
			t.Errorf("capture = %d, want %d after overflow", sw.capture.Len(), traceMaxBody)
		}
		wantWritten := len(chunk) * 2
		if sw.bytesWritten != wantWritten {
			t.Errorf("bytesWritten = %d, want %d", sw.bytesWritten, wantWritten)
		}
	})
}

// guard against an accidental header-value duplication regression: ACAO must be
// a single value even when both Origin is present and next writes headers.
func TestCORS_NoDuplicateACAO(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "https://app.example.com")
		w.WriteHeader(http.StatusOK)
	})
	h := cors(inner)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://app.example.com")
	h.ServeHTTP(rec, req)

	vals := rec.Header().Values("Access-Control-Allow-Origin")
	if len(vals) != 1 {
		t.Fatalf("ACAO = %v, want exactly one value", vals)
	}
}
