package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"airouter/internal/harlog"
	"airouter/internal/observability"
	"airouter/internal/proxy"
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
			if got := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, requestIDHeader) {
				t.Errorf("ACEH = %q, want to contain %s", got, requestIDHeader)
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
	if got := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, requestIDHeader) {
		t.Errorf("ACEH = %q, want to contain %s", got, requestIDHeader)
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

func TestStatusWriter(t *testing.T) {
	const captureLimit = 64

	t.Run("truncated caps capture but counts full bytes", func(t *testing.T) {
		inner := httptest.NewRecorder()
		cap := observability.NewCapture(captureLimit)
		sw := &statusWriter{ResponseWriter: inner, status: http.StatusOK, capture: cap}
		body := bytes.Repeat([]byte("x"), captureLimit+64)
		n, err := sw.Write(body)
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if n != len(body) {
			t.Errorf("wrote %d, want %d", n, len(body))
		}
		if len(cap.Bytes()) != captureLimit {
			t.Errorf("capture = %d bytes, want %d (truncated)", len(cap.Bytes()), captureLimit)
		}
		if sw.bytesWritten != int64(len(body)) {
			t.Errorf("bytesWritten = %d, want %d", sw.bytesWritten, len(body))
		}
		if inner.Body.Len() != len(body) {
			t.Errorf("underlying wrote %d, want %d", inner.Body.Len(), len(body))
		}
	})

	t.Run("nil capture still counts written bytes", func(t *testing.T) {
		inner := httptest.NewRecorder()
		sw := &statusWriter{ResponseWriter: inner, status: http.StatusOK}
		body := []byte("no capture")
		if _, err := sw.Write(body); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if !bytes.Equal(inner.Body.Bytes(), body) {
			t.Errorf("underlying body = %q, want %q", inner.Body.Bytes(), body)
		}
		if sw.bytesWritten != int64(len(body)) {
			t.Errorf("bytesWritten = %d, want %d", sw.bytesWritten, len(body))
		}
	})

	t.Run("first WriteHeader wins", func(t *testing.T) {
		inner := httptest.NewRecorder()
		sw := &statusWriter{ResponseWriter: inner, status: http.StatusOK}
		sw.WriteHeader(http.StatusTeapot)
		sw.WriteHeader(http.StatusAccepted)
		if sw.status != http.StatusTeapot {
			t.Errorf("status = %d, want %d", sw.status, http.StatusTeapot)
		}
		if inner.Code != http.StatusTeapot {
			t.Errorf("underlying status = %d, want %d", inner.Code, http.StatusTeapot)
		}
	})

	t.Run("implicit 200 on first Write", func(t *testing.T) {
		inner := httptest.NewRecorder()
		sw := &statusWriter{ResponseWriter: inner, status: http.StatusOK}
		sw.Write([]byte("hi"))
		if sw.status != http.StatusOK || !sw.wroteHeader {
			t.Errorf("status=%d wroteHeader=%v", sw.status, sw.wroteHeader)
		}
		if inner.Code != http.StatusOK {
			t.Errorf("underlying = %d", inner.Code)
		}
	})

	t.Run("small write under cap captured fully", func(t *testing.T) {
		inner := httptest.NewRecorder()
		cap := observability.NewCapture(captureLimit)
		sw := &statusWriter{ResponseWriter: inner, status: http.StatusOK, capture: cap}
		body := []byte("hello")
		if _, err := sw.Write(body); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if !bytes.Equal(cap.Bytes(), body) {
			t.Errorf("capture = %q, want %q", cap.Bytes(), body)
		}
		if sw.bytesWritten != int64(len(body)) {
			t.Errorf("bytesWritten = %d, want %d", sw.bytesWritten, len(body))
		}
	})

	t.Run("chunked writes accumulate then truncate", func(t *testing.T) {
		inner := httptest.NewRecorder()
		cap := observability.NewCapture(captureLimit)
		sw := &statusWriter{ResponseWriter: inner, status: http.StatusOK, capture: cap}
		chunk := bytes.Repeat([]byte("z"), captureLimit/2+10)
		sw.Write(chunk)
		sw.Write(chunk)
		if len(cap.Bytes()) != captureLimit {
			t.Errorf("capture = %d, want %d after overflow", len(cap.Bytes()), captureLimit)
		}
		wantWritten := int64(len(chunk) * 2)
		if sw.bytesWritten != wantWritten {
			t.Errorf("bytesWritten = %d, want %d", sw.bytesWritten, wantWritten)
		}
	})

	t.Run("Flush commits implicit 200", func(t *testing.T) {
		inner := httptest.NewRecorder()
		sw := &statusWriter{ResponseWriter: inner, status: http.StatusOK}
		sw.Flush()
		sw.WriteHeader(http.StatusInternalServerError)
		if !sw.wroteHeader || sw.status != http.StatusOK {
			t.Errorf("status=%d wroteHeader=%v, want committed 200", sw.status, sw.wroteHeader)
		}
		if inner.Code != http.StatusOK {
			t.Errorf("underlying status=%d, want 200", inner.Code)
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

func TestRequestIDAlwaysSet(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(0, &buf) // info only
	var gotID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = observability.RequestID(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	h := requestMiddleware(logger, nil, inner)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	h.ServeHTTP(rec, req)

	hdr := rec.Header().Get(requestIDHeader)
	if hdr == "" {
		t.Fatal("missing X-Airouter-Request-ID response header")
	}
	if gotID == "" || gotID != hdr {
		t.Errorf("ctx id=%q header=%q", gotID, hdr)
	}
	// debug=0: no request_completed
	if strings.Contains(buf.String(), "request_completed") {
		t.Errorf("debug=0 should not log request_completed: %s", buf.String())
	}
}

func TestDebugLevels(t *testing.T) {
	body := `{"model":"x","messages":[]}`
	const secretSentinel = "unique-prompt-sentinel-xyz"
	bodyWithSentinel := `{"model":"x","messages":[{"role":"user","content":"` + secretSentinel + `"}]}`

	run := func(level int, path string, reqBody string, handler http.HandlerFunc) string {
		var buf bytes.Buffer
		logger := observability.NewLogger(level, &buf)
		h := requestMiddleware(logger, nil, handler)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		return buf.String()
	}

	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	})

	t.Run("debug0 no access no trace", func(t *testing.T) {
		out := run(0, "/v1/chat/completions", body, echo)
		if strings.Contains(out, "request_completed") || strings.Contains(out, "ingress_exchange") {
			t.Fatalf("unexpected logs: %s", out)
		}
	})

	t.Run("debug1 completion no exchange", func(t *testing.T) {
		out := run(1, "/v1/chat/completions", bodyWithSentinel, echo)
		if !strings.Contains(out, "request_completed") {
			t.Fatalf("missing request_completed: %s", out)
		}
		if strings.Contains(out, "ingress_exchange") {
			t.Fatalf("exchange event at debug=1: %s", out)
		}
		if strings.Contains(out, secretSentinel) || strings.Contains(out, bodyWithSentinel) {
			t.Fatalf("raw body leaked at debug=1: %s", out)
		}
	})

	t.Run("debug2 metadata exchange no body", func(t *testing.T) {
		out := run(2, "/v1/chat/completions", bodyWithSentinel, echo)
		if !strings.Contains(out, "ingress_exchange") {
			t.Fatalf("missing ingress_exchange: %s", out)
		}
		if strings.Contains(out, "ingress_request_body") || strings.Contains(out, "ingress_response_body") {
			t.Fatalf("legacy body events present: %s", out)
		}
		if !strings.Contains(out, "request_bytes=") || !strings.Contains(out, "response_bytes=") {
			t.Fatalf("missing size fields: %s", out)
		}
		if !strings.Contains(out, "request_content_type=application/json") {
			t.Fatalf("missing request content type: %s", out)
		}
		if !strings.Contains(out, "response_content_type=application/json") {
			t.Fatalf("missing response content type: %s", out)
		}
		wantSize := strconv.Itoa(len(bodyWithSentinel))
		if !strings.Contains(out, "request_bytes="+wantSize) {
			t.Fatalf("wrong request_bytes: %s", out)
		}
		if !strings.Contains(out, "response_bytes="+wantSize) {
			t.Fatalf("wrong response_bytes: %s", out)
		}
		if strings.Contains(out, secretSentinel) || strings.Contains(out, " body=") || strings.Contains(out, "textual=") {
			t.Fatalf("body payload leaked at TRACE: %s", out)
		}
	})

	t.Run("non-proxy path no exchange at debug2", func(t *testing.T) {
		out := run(2, "/dashboard", body, echo)
		if strings.Contains(out, "ingress_exchange") {
			t.Fatalf("dashboard should not exchange-trace: %s", out)
		}
		if !strings.Contains(out, "request_completed") {
			t.Fatalf("still want access log: %s", out)
		}
	})
}

func TestIngressExchangeUpstreamURL(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(2, &buf)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Content-Type", "application/json")
	sw := &statusWriter{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK, bytesWritten: 12}
	sw.Header().Set("Content-Type", "text/event-stream")
	reqCap := observability.NewCapture(0)
	_, _ = reqCap.Write([]byte("abcdefghij"))
	tinfo := &proxy.TraceInfo{UpstreamURL: "https://provider.example/v1/messages"}
	emitIngressExchange(req.Context(), logger, req, tinfo, reqCap, sw, 0)
	out := buf.String()
	if !strings.Contains(out, "event=ingress_exchange") {
		t.Fatalf("missing event: %s", out)
	}
	if !strings.Contains(out, "upstream_url=https://provider.example/v1/messages") {
		t.Fatalf("missing upstream_url: %s", out)
	}
	if !strings.Contains(out, "request_bytes=10") || !strings.Contains(out, "response_bytes=12") {
		t.Fatalf("wrong sizes: %s", out)
	}
	if strings.Contains(out, " body=") {
		t.Fatalf("body attr present: %s", out)
	}
}

func TestRequestBodyNotEagerlyDrained(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(2, &buf)
	var readN int
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read only 4 bytes; the rest must not have been pre-consumed.
		p := make([]byte, 4)
		n, _ := r.Body.Read(p)
		readN = n
		w.WriteHeader(http.StatusOK)
	})
	h := requestMiddleware(logger, nil, inner)
	rec := httptest.NewRecorder()
	payload := []byte(`{"model":"default","messages":[{"role":"user","content":"hello world"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if readN != 4 {
		t.Fatalf("handler read %d, want 4", readN)
	}
	// Capture should only have observed 4 bytes.
	if !strings.Contains(buf.String(), "request_bytes=4") {
		t.Fatalf("expected observed request_bytes=4 in logs: %s", buf.String())
	}
	if strings.Contains(buf.String(), string(payload)) {
		t.Fatalf("request body leaked into TRACE: %s", buf.String())
	}
}

func TestHARBoundedBodySizes(t *testing.T) {
	rec := harlog.New("test")
	logger := observability.NewLogger(0, io.Discard)
	payload := bytes.Repeat([]byte("p"), 100)
	respBody := bytes.Repeat([]byte("r"), 80)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBody)
	})
	h := requestMiddleware(logger, rec, inner)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "text/plain")
	req.Host = "localhost:31415"
	h.ServeHTTP(rr, req)

	data, err := rec.MarshalHAR()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Log struct {
			Entries []struct {
				Request struct {
					BodySize int64 `json:"bodySize"`
					PostData *struct {
						Text string `json:"text"`
					} `json:"postData"`
				} `json:"request"`
				Response struct {
					Content struct {
						Size int64  `json:"size"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"response"`
				PageRef string `json:"pageref"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Log.Entries) != 1 {
		t.Fatalf("entries = %d", len(doc.Log.Entries))
	}
	e := doc.Log.Entries[0]
	if e.Request.BodySize != int64(len(payload)) {
		t.Errorf("req bodySize = %d, want %d", e.Request.BodySize, len(payload))
	}
	if e.Response.Content.Size != int64(len(respBody)) {
		t.Errorf("resp size = %d, want %d", e.Response.Content.Size, len(respBody))
	}
	if e.PageRef == "" || !strings.HasPrefix(e.PageRef, "page_") {
		t.Errorf("pageref = %q", e.PageRef)
	}
	// Request id header matches page suffix.
	rid := rr.Header().Get(requestIDHeader)
	if e.PageRef != "page_"+rid {
		t.Errorf("pageref %q != page_%s", e.PageRef, rid)
	}
}

func TestMiddlewareSSEFlush(t *testing.T) {
	logger := observability.NewLogger(1, io.Discard)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: hi\n\n"))
		fl.Flush()
	})
	h := requestMiddleware(logger, nil, inner)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "data: hi") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestPartialWriteCountsActual(t *testing.T) {
	cap := observability.NewCapture(100)
	inner := &partialRW{n: 3}
	sw := &statusWriter{ResponseWriter: inner, status: http.StatusOK, capture: cap}
	body := []byte("abcdef")
	n, err := sw.Write(body)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 3 {
		t.Fatalf("n = %d, want 3", n)
	}
	if sw.bytesWritten != 3 {
		t.Errorf("bytesWritten = %d, want 3", sw.bytesWritten)
	}
	if string(cap.Bytes()) != "abc" {
		t.Errorf("capture = %q, want abc", cap.Bytes())
	}
}

type partialRW struct {
	n   int
	hdr http.Header
}

func (p *partialRW) Header() http.Header {
	if p.hdr == nil {
		p.hdr = http.Header{}
	}
	return p.hdr
}
func (p *partialRW) WriteHeader(int) {}
func (p *partialRW) Write(b []byte) (int, error) {
	if len(b) > p.n {
		return p.n, nil
	}
	return len(b), nil
}
