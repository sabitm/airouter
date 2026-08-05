package web

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"airouter/internal/observability"
)

func TestExecuteProbeTracesMetadataOnly(t *testing.T) {
	const reqSentinel = "probe-request-sentinel-aaa"
	const respSentinel = "probe-response-sentinel-bbb"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"marker":"` + respSentinel + `"}`))
	}))
	t.Cleanup(up.Close)

	var buf bytes.Buffer
	logger := observability.NewLogger(2, &buf)
	reqBody := `{"metadata":{"marker":"` + reqSentinel + `"}}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, up.URL+"/check", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-token")

	pr, err := executeProbe(context.Background(), logger, upstreamClient, req, "test_probe")
	if err != nil {
		t.Fatalf("executeProbe: %v", err)
	}
	if pr.StatusCode != 200 {
		t.Fatalf("status = %d", pr.StatusCode)
	}
	out := buf.String()
	requestLine := eventLine(out, "probe_request")
	responseLine := eventLine(out, "probe_response")
	if requestLine == "" || responseLine == "" {
		t.Fatalf("missing probe events: %s", out)
	}
	if !strings.Contains(out, "operation=test_probe") {
		t.Fatalf("missing operation: %s", out)
	}
	if !strings.Contains(out, "method=POST") {
		t.Fatalf("missing method: %s", out)
	}
	if !strings.Contains(out, "content_type=application/json") {
		t.Fatalf("missing content_type: %s", out)
	}
	if !strings.Contains(out, "status=200") {
		t.Fatalf("missing status: %s", out)
	}
	if !strings.Contains(out, "duration_ms=") {
		t.Fatalf("missing duration_ms: %s", out)
	}
	if !strings.Contains(requestLine, "size="+strconv.Itoa(len(reqBody))) {
		t.Fatalf("wrong probe request size: %s", requestLine)
	}
	if !strings.Contains(responseLine, "size="+strconv.FormatInt(pr.BodySize, 10)) {
		t.Fatalf("wrong probe response size: %s", responseLine)
	}
	if strings.Contains(out, "secret-token") {
		t.Fatalf("auth header leaked: %s", out)
	}
	if strings.Contains(out, reqSentinel) || strings.Contains(out, respSentinel) {
		t.Fatalf("body sentinel leaked into TRACE: %s", out)
	}
	if strings.Contains(out, " body=") || strings.Contains(out, "textual=") {
		t.Fatalf("body/textual attrs present: %s", out)
	}
	// Callers still receive retained response bytes.
	if !bytes.Contains(pr.Body, []byte(respSentinel)) {
		t.Fatalf("caller body missing retained bytes: %q", pr.Body)
	}
	if pr.BodySize != int64(len(pr.Body)) {
		t.Fatalf("BodySize=%d len(Body)=%d", pr.BodySize, len(pr.Body))
	}
	if pr.Truncated {
		t.Fatalf("unexpected truncation for small response")
	}
}

func eventLine(out, event string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "event="+event) {
			return line
		}
	}
	return ""
}

func TestExecuteProbeResponseTruncationFlag(t *testing.T) {
	// Response larger than probeCaptureMax: retained body truncates, size is full,
	// TRACE truncated=true, and no body bytes in the log.
	const marker = "huge-probe-body-marker"
	payload := []byte(marker + strings.Repeat("x", probeCaptureMax+64))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(up.Close)

	var buf bytes.Buffer
	logger := observability.NewLogger(2, &buf)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, up.URL+"/models", nil)
	if err != nil {
		t.Fatal(err)
	}

	pr, err := executeProbe(context.Background(), logger, upstreamClient, req, "big_probe")
	if err != nil {
		t.Fatalf("executeProbe: %v", err)
	}
	if !pr.Truncated {
		t.Fatal("expected Truncated=true on probeResult")
	}
	if len(pr.Body) != probeCaptureMax {
		t.Fatalf("retained body = %d, want %d", len(pr.Body), probeCaptureMax)
	}
	if pr.BodySize != int64(len(payload)) {
		t.Fatalf("BodySize = %d, want %d", pr.BodySize, len(payload))
	}
	out := buf.String()
	if !strings.Contains(out, "event=probe_response") {
		t.Fatalf("missing probe_response: %s", out)
	}
	if !strings.Contains(out, "truncated=true") {
		t.Fatalf("expected truncated=true: %s", out)
	}
	if !strings.Contains(out, "size="+strconv.FormatInt(int64(len(payload)), 10)) {
		t.Fatalf("expected full size in TRACE: %s", out)
	}
	if strings.Contains(out, marker) {
		t.Fatalf("response body leaked: %s", out)
	}
	// Prefix retained for caller.
	if !bytes.HasPrefix(pr.Body, []byte(marker)) {
		t.Fatalf("caller should still get retained prefix")
	}
}

func TestExecuteProbeRequestContentLengthUnknown(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(up.Close)

	var buf bytes.Buffer
	logger := observability.NewLogger(2, &buf)
	// Chunked body without ContentLength: leave size as -1.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, up.URL, strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1
	req.Body = io.NopCloser(strings.NewReader(`{"a":1}`))
	if _, err := executeProbe(context.Background(), logger, upstreamClient, req, "len_unknown"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "event=probe_request") {
		t.Fatalf("missing probe_request: %s", out)
	}
	if !strings.Contains(out, "size=-1") {
		t.Fatalf("want size=-1 for unknown ContentLength: %s", out)
	}
	if strings.Contains(out, `{"a":1}`) || strings.Contains(out, " body=") {
		t.Fatalf("request body leaked: %s", out)
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
