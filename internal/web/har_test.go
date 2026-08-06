package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"airouter/internal/crypto"
	"airouter/internal/harlog"
	"airouter/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newWebTestStore(t *testing.T) *store.Store {
	t.Helper()
	c, err := crypto.New("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestHARPanelRuntimeStates(t *testing.T) {
	st := newWebTestStore(t)
	ctrl := harlog.NewController(false, "t", discardLogger())
	h := NewHandler(st, nil, ctrl)

	// Idle page contains Start and warning, no Download.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/settings", nil)
	h.settingsPage(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, "HAR capture") {
		t.Fatalf("missing panel: %s", body)
	}
	if !strings.Contains(body, "verbatim prompts") || !strings.Contains(body, "OAuth tokens") {
		t.Fatalf("missing warning: %s", body)
	}
	if !strings.Contains(body, "Start recording") {
		t.Fatalf("missing start: %s", body)
	}
	if strings.Contains(body, "Download HAR") {
		t.Fatalf("download on idle: %s", body)
	}

	// Start via handler.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/dashboard/har/start", nil)
	req.Header.Set("HX-Request", "true")
	h.harStart(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("start status = %d body=%s", rr.Code, rr.Body.String())
	}
	body = rr.Body.String()
	if !strings.Contains(body, "Recording") || !strings.Contains(body, "Stop recording") {
		t.Fatalf("recording panel: %s", body)
	}
	if strings.Contains(body, "Download HAR") {
		t.Fatalf("download while recording: %s", body)
	}

	// Invalid start while recording.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/dashboard/har/start", nil)
	req.Header.Set("HX-Request", "true")
	h.harStart(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid start status = %d", rr.Code)
	}

	// Stop.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/dashboard/har/stop", nil)
	req.Header.Set("HX-Request", "true")
	h.harStop(rr, req)
	body = rr.Body.String()
	if !strings.Contains(body, "Stopped") || !strings.Contains(body, "Download HAR") {
		t.Fatalf("stopped panel: %s", body)
	}
	if !strings.Contains(body, "Start new recording") {
		t.Fatalf("missing start new: %s", body)
	}
	// Invalid stop while stopped.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/dashboard/har/stop", nil)
	req.Header.Set("HX-Request", "true")
	h.harStop(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid stop status = %d", rr.Code)
	}
}

func TestHARPanelFileMode(t *testing.T) {
	st := newWebTestStore(t)
	ctrl := harlog.NewController(true, "t", discardLogger())
	h := NewHandler(st, nil, ctrl)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/settings", nil)
	h.settingsPage(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, "Always recording") {
		t.Fatalf("file mode label missing: %s", body)
	}
	if !strings.Contains(body, "Download HAR") {
		t.Fatalf("missing download: %s", body)
	}
	if strings.Contains(body, "Start recording") || strings.Contains(body, "Stop recording") {
		t.Fatalf("start/stop visible in file mode: %s", body)
	}
	// No configured filesystem path in UI (download filename is fine).
	if strings.Contains(body, "/tmp/") || strings.Contains(body, "/var/") || strings.Contains(body, "AIROUTER_HAR_FILE=") {
		t.Fatalf("possible path leak: %s", body)
	}
	// Mutations rejected.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/dashboard/har/start", nil)
	req.Header.Set("HX-Request", "true")
	h.harStart(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("file start status = %d body=%s", rr.Code, rr.Body.String())
	}
	out, _ := io.ReadAll(rr.Body)
	if !strings.Contains(string(out), "disabled") && !strings.Contains(string(out), "-har-file") {
		t.Fatalf("error body = %s", out)
	}
}
