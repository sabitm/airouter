package web

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/observability"
)

func TestExecuteProbeTracesAndTruncates(t *testing.T) {
	long := strings.Repeat("x", probeTraceDisplay+64)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"` + long + `"}]}`))
	}))
	t.Cleanup(up.Close)

	var buf bytes.Buffer
	logger := observability.NewLogger(2, &buf)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, up.URL+"/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret-token")

	pr, err := executeProbe(context.Background(), logger, upstreamClient, req, "test_probe")
	if err != nil {
		t.Fatalf("executeProbe: %v", err)
	}
	if pr.StatusCode != 200 {
		t.Fatalf("status = %d", pr.StatusCode)
	}
	out := buf.String()
	if !strings.Contains(out, "probe_request") || !strings.Contains(out, "probe_response") {
		t.Fatalf("missing probe events: %s", out)
	}
	if strings.Contains(out, "secret-token") {
		t.Fatalf("auth header leaked: %s", out)
	}
	if strings.Contains(out, long) || !strings.Contains(out, "truncated") {
		t.Fatalf("body not truncated in TRACE: %s", out)
	}
	if pr.BodySize <= int64(probeTraceDisplay) {
		t.Fatalf("BodySize = %d, want > display cap", pr.BodySize)
	}
}

func TestExecuteProbeRequestBodyTrace(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(up.Close)

	var buf bytes.Buffer
	logger := observability.NewLogger(2, &buf)
	requestBody := `{"metadata":{"ideType":9}}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, up.URL, strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if _, err := executeProbe(context.Background(), logger, upstreamClient, req, "body_probe"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "event=probe_request") || !strings.Contains(out, "body=") || !strings.Contains(out, "ideType") {
		t.Fatalf("request body missing from TRACE: %s", out)
	}
}

func TestExecuteProbeTransportErrorOnce(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(1, &buf)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = executeProbe(context.Background(), logger, upstreamClient, req, "transport_probe")
	if err == nil {
		t.Fatal("expected transport error")
	}
	out := buf.String()
	if strings.Count(out, "event=probe_transport_failed") != 1 {
		t.Fatalf("want exactly one transport error event, got: %s", out)
	}
	if strings.Contains(out, "probe_response") {
		t.Fatalf("unexpected probe_response on transport error: %s", out)
	}
}

func TestExecuteProbeBinarySummary(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0x00, 0x01, 0xff, 0xfe})
	}))
	t.Cleanup(up.Close)

	var buf bytes.Buffer
	logger := observability.NewLogger(2, &buf)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, up.URL, nil)
	pr, err := executeProbe(context.Background(), logger, upstreamClient, req, "bin")
	if err != nil {
		t.Fatal(err)
	}
	if pr.BodySize != 4 {
		t.Fatalf("BodySize = %d", pr.BodySize)
	}
	out := buf.String()
	if !strings.Contains(out, "textual=false") {
		t.Fatalf("expected textual=false: %s", out)
	}
	// Binary payload must not appear as a dumped body attr value of raw bytes
	// in a way that includes the 0xff sequence as text; DescribeBody leaves body empty.
	if strings.Contains(out, "body=") && strings.Contains(out, "\xff") {
		t.Fatalf("binary dumped: %q", out)
	}
}

func TestReadBoundedTruncation(t *testing.T) {
	// Larger than limit: retain prefix, report full size.
	payload := bytes.Repeat([]byte("a"), probeCaptureMax+100)
	body, total, trunc, err := readBounded(bytes.NewReader(payload), 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 64 || total != int64(len(payload)) || !trunc {
		t.Fatalf("body=%d total=%d trunc=%v", len(body), total, trunc)
	}
}
