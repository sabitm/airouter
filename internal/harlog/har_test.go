package harlog

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecorderMarshalHAR(t *testing.T) {
	r := New("test-ver")
	pageID := "page_abc123"
	start := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	r.EnsurePage(pageID, "POST /v1/messages", start)
	r.Record(RecordInput{
		PageID:    pageID,
		StartedAt: start,
		Duration:  50 * time.Millisecond,
		Method:    "POST",
		URL:       "http://localhost:8080/v1/messages",
		ReqHeaders: http.Header{
			"Authorization": []string{"Bearer sk-client"},
			"Content-Type":  []string{"application/json"},
		},
		ReqBody: []byte(`{"model":"default"}`),
		Status:  200,
		RespHeaders: http.Header{
			"Content-Type": []string{"application/json"},
		},
		RespBody: []byte(`{"id":"msg_1"}`),
	})
	r.Record(RecordInput{
		PageID:    pageID,
		StartedAt: start.Add(5 * time.Millisecond),
		Duration:  40 * time.Millisecond,
		Method:    "POST",
		URL:       "https://api.example.com/v1/messages",
		ReqHeaders: http.Header{
			"Authorization": []string{"Bearer sk-provider-secret"},
			"x-api-key":     []string{"secret-key"},
		},
		ReqBody: []byte(`{"model":"claude"}`),
		Status:  200,
		RespHeaders: http.Header{
			"Content-Type": []string{"application/json"},
		},
		RespBody: []byte(`{"id":"up_1"}`),
	})

	data, err := r.MarshalHAR()
	if err != nil {
		t.Fatalf("MarshalHAR: %v", err)
	}

	var doc struct {
		Log struct {
			Version string `json:"version"`
			Creator struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"creator"`
			Pages []struct {
				ID string `json:"id"`
			} `json:"pages"`
			Entries []struct {
				PageRef string `json:"pageref"`
				Request struct {
					Headers []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"headers"`
					PostData *struct {
						Text string `json:"text"`
					} `json:"postData"`
				} `json:"request"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, data)
	}
	if doc.Log.Version != "1.2" {
		t.Fatalf("version = %q, want 1.2", doc.Log.Version)
	}
	if doc.Log.Creator.Name != "airouter" || doc.Log.Creator.Version != "test-ver" {
		t.Fatalf("creator = %+v", doc.Log.Creator)
	}
	if len(doc.Log.Pages) != 1 || doc.Log.Pages[0].ID != pageID {
		t.Fatalf("pages = %+v", doc.Log.Pages)
	}
	if len(doc.Log.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(doc.Log.Entries))
	}
	for i, e := range doc.Log.Entries {
		if e.PageRef != pageID {
			t.Errorf("entry[%d].pageref = %q, want %q", i, e.PageRef, pageID)
		}
	}

	// Verbatim Authorization on the upstream leg.
	found := false
	for _, h := range doc.Log.Entries[1].Request.Headers {
		if strings.EqualFold(h.Name, "Authorization") && h.Value == "Bearer sk-provider-secret" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("upstream Authorization header not preserved: %+v", doc.Log.Entries[1].Request.Headers)
	}
}

func TestBodyTruncationComment(t *testing.T) {
	r := New("v")
	pageID := "page_t"
	big := make([]byte, MaxBody+100)
	for i := range big {
		big[i] = 'a'
	}
	r.EnsurePage(pageID, "big", time.Now())
	r.Record(RecordInput{
		PageID:      pageID,
		StartedAt:   time.Now(),
		Duration:    time.Millisecond,
		Method:      "POST",
		URL:         "http://example/x",
		ReqHeaders:  http.Header{"Content-Type": []string{"text/plain"}},
		ReqBody:     big,
		Status:      200,
		RespHeaders: http.Header{"Content-Type": []string{"text/plain"}},
		RespBody:    big,
	})

	data, err := r.MarshalHAR()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Log struct {
			Entries []struct {
				Request struct {
					PostData *struct {
						Text    string `json:"text"`
						Comment string `json:"comment"`
					} `json:"postData"`
					Comment string `json:"comment"`
				} `json:"request"`
				Response struct {
					Content struct {
						Text    string `json:"text"`
						Comment string `json:"comment"`
						Size    int64  `json:"size"`
					} `json:"content"`
					Comment string `json:"comment"`
				} `json:"response"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	e := doc.Log.Entries[0]
	if e.Request.PostData == nil || e.Request.PostData.Comment == "" {
		t.Fatalf("expected request postData comment, got %+v", e.Request.PostData)
	}
	if !strings.Contains(e.Request.PostData.Comment, itoa(len(big))) {
		t.Fatalf("comment %q missing original length", e.Request.PostData.Comment)
	}
	if len(e.Request.PostData.Text) != MaxBody {
		t.Fatalf("postData text len = %d, want %d", len(e.Request.PostData.Text), MaxBody)
	}
	if e.Response.Content.Comment == "" {
		t.Fatalf("expected response content comment")
	}
	if e.Response.Content.Size != int64(len(big)) {
		t.Fatalf("content.size = %d, want %d", e.Response.Content.Size, len(big))
	}
	if len(e.Response.Content.Text) != MaxBody {
		t.Fatalf("content text len = %d, want %d", len(e.Response.Content.Text), MaxBody)
	}
}

func TestWriteFileRoundTrip(t *testing.T) {
	r := New("rt")
	pageID := "page_w"
	r.EnsurePage(pageID, "t", time.Now())
	r.Record(RecordInput{
		PageID:      pageID,
		StartedAt:   time.Now(),
		Duration:    time.Millisecond,
		Method:      "GET",
		URL:         "http://localhost/v1/models",
		ReqHeaders:  http.Header{},
		Status:      200,
		RespHeaders: http.Header{"Content-Type": []string{"application/json"}},
		RespBody:    []byte(`{"data":[]}`),
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "out.har")
	if err := r.WriteFile(path); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("file not JSON: %v", err)
	}
	logObj, ok := doc["log"].(map[string]any)
	if !ok {
		t.Fatalf("missing log: %v", doc)
	}
	if logObj["version"] != "1.2" {
		t.Fatalf("version = %v", logObj["version"])
	}
}

func TestReqBodySizeWhenAlreadyTruncated(t *testing.T) {
	r := New("v")
	pageID := "page_rb"
	// Caller already truncated to 10 bytes but original wire was 5000.
	prefix := []byte("0123456789")
	oorig := 5000
	r.EnsurePage(pageID, "t", time.Now())
	r.Record(RecordInput{
		PageID:      pageID,
		StartedAt:   time.Now(),
		Duration:    time.Millisecond,
		Method:      "POST",
		URL:         "http://example/x",
		ReqHeaders:  http.Header{"Content-Type": []string{"text/plain"}},
		ReqBody:     prefix,
		ReqBodySize: oorig,
		Status:      200,
		RespHeaders: http.Header{"Content-Type": []string{"text/plain"}},
		RespBody:    []byte("ok"),
	})
	data, err := r.MarshalHAR()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Log struct {
			Entries []struct {
				Request struct {
					BodySize int64 `json:"bodySize"`
					PostData *struct {
						Text    string `json:"text"`
						Comment string `json:"comment"`
					} `json:"postData"`
				} `json:"request"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	e := doc.Log.Entries[0]
	if e.Request.BodySize != int64(oorig) {
		t.Fatalf("bodySize = %d, want %d", e.Request.BodySize, oorig)
	}
	if e.Request.PostData == nil || !strings.Contains(e.Request.PostData.Comment, itoa(oorig)) {
		t.Fatalf("postData comment = %+v", e.Request.PostData)
	}
	if e.Request.PostData.Text != string(prefix) {
		t.Fatalf("text = %q", e.Request.PostData.Text)
	}
}

func TestEnsurePageIdempotent(t *testing.T) {
	r := New("v")
	start := time.Now()
	r.EnsurePage("page_x", "a", start)
	r.EnsurePage("page_x", "b", start)
	data, err := r.MarshalHAR()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Log struct {
			Pages []any `json:"pages"`
		} `json:"log"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Log.Pages) != 1 {
		t.Fatalf("pages = %d, want 1", len(doc.Log.Pages))
	}
}
