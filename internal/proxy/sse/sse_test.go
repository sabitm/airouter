package sse

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReader(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Event
	}{
		{"data only", "data: hello\n\n", Event{Name: "", Data: []byte("hello")}},
		{"named event", "event: ping\ndata: 1\n\n", Event{Name: "ping", Data: []byte("1")}},
		{"multi data joined with newline", "data: line1\ndata: line2\n\n", Event{Name: "", Data: []byte("line1\nline2")}},
		{"comment skipped", ": keepalive\ndata: x\n\n", Event{Name: "", Data: []byte("x")}},
		{"event without data", "event: ping\n\n", Event{Name: "ping", Data: []byte("")}},
		{"crlf line endings", "data: hello\r\n\r\n", Event{Name: "", Data: []byte("hello")}},
		{"single space after colon stripped", "data: hello\n\n", Event{Name: "", Data: []byte("hello")}},
		{"no space after colon", "data:hello\n\n", Event{Name: "", Data: []byte("hello")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewReader(strings.NewReader(tc.in))
			ev, err := r.Next()
			if err != nil && err != io.EOF {
				t.Fatalf("Next: %v", err)
			}
			if ev.Name != tc.want.Name {
				t.Errorf("Name = %q, want %q", ev.Name, tc.want.Name)
			}
			if !bytes.Equal(ev.Data, tc.want.Data) {
				t.Errorf("Data = %q, want %q", ev.Data, tc.want.Data)
			}
		})
	}
}

func TestReaderStrayBlankLineBeforeFields(t *testing.T) {
	// A blank line before any field is not an event terminator; the reader keeps
	// scanning until a real event is closed.
	r := NewReader(strings.NewReader("\n\ndata: late\n\n"))
	ev, err := r.Next()
	if err != nil && err != io.EOF {
		t.Fatalf("Next: %v", err)
	}
	if string(ev.Data) != "late" {
		t.Errorf("Data = %q, want \"late\"", ev.Data)
	}
}

func TestReaderEOFWithoutTrailingBlankReturnsBuffered(t *testing.T) {
	// A stream that ends after a data line with no closing blank line still
	// yields the buffered event.
	r := NewReader(strings.NewReader("data: no-blank-at-end"))
	ev, err := r.Next()
	if err != nil && err != io.EOF {
		t.Fatalf("Next: %v", err)
	}
	if string(ev.Data) != "no-blank-at-end" {
		t.Errorf("Data = %q, want \"no-blank-at-end\"", ev.Data)
	}
}

func TestReaderEOFOnEmptyStream(t *testing.T) {
	r := NewReader(strings.NewReader(""))
	_, err := r.Next()
	if err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

func TestReaderMultipleEvents(t *testing.T) {
	in := "data: one\n\ndata: two\n\n"
	r := NewReader(strings.NewReader(in))
	var got []string
	for {
		ev, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, string(ev.Data))
	}
	want := []string{"one", "two"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestReaderSizeLimits(t *testing.T) {
	t.Run("line at limit", func(t *testing.T) {
		payload := strings.Repeat("x", maxLineBytes-len("data: "))
		ev, err := NewReader(strings.NewReader("data: " + payload + "\n\n")).Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if string(ev.Data) != payload {
			t.Fatalf("data length = %d, want %d", len(ev.Data), len(payload))
		}
	})

	t.Run("line over limit", func(t *testing.T) {
		input := "data: " + strings.Repeat("x", maxLineBytes-len("data: ")+1) + "\n\n"
		_, err := NewReader(strings.NewReader(input)).Next()
		var sizeErr *SizeLimitError
		if !errors.As(err, &sizeErr) || sizeErr.Scope != "line" || sizeErr.Limit != maxLineBytes {
			t.Fatalf("error = %v, want line SizeLimitError", err)
		}
	})

	t.Run("multiline event at limit", func(t *testing.T) {
		first := strings.Repeat("a", maxEventBytes/2)
		second := strings.Repeat("b", maxEventBytes-len(first)-1)
		ev, err := NewReader(strings.NewReader("data: " + first + "\ndata: " + second + "\n\n")).Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if len(ev.Data) != maxEventBytes {
			t.Fatalf("data length = %d, want %d", len(ev.Data), maxEventBytes)
		}
	})

	t.Run("multiline event over limit", func(t *testing.T) {
		first := strings.Repeat("a", maxEventBytes/2)
		second := strings.Repeat("b", maxEventBytes-len(first))
		_, err := NewReader(strings.NewReader("data: " + first + "\ndata: " + second + "\n\n")).Next()
		var sizeErr *SizeLimitError
		if !errors.As(err, &sizeErr) || sizeErr.Scope != "event" || sizeErr.Limit != maxEventBytes {
			t.Fatalf("error = %v, want event SizeLimitError", err)
		}
	})
}

func newWriter(t *testing.T) (*Writer, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	w, ok := NewWriter(rec)
	if !ok {
		t.Fatal("NewWriter: ResponseRecorder should implement Flusher")
	}
	return w, rec
}

func TestWriterDataOnlyEvent(t *testing.T) {
	w, rec := newWriter(t)
	if err := w.WriteEvent("", []byte("hello")); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	want := "data: hello\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if !rec.Flushed {
		t.Error("expected Flush to be called")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}

func TestWriterNamedEvent(t *testing.T) {
	w, rec := newWriter(t)
	if err := w.WriteEvent("ping", []byte("1")); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	want := "event: ping\ndata: 1\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

// A payload containing newlines must be split into multiple data: lines so a
// client reassembles it with the newline preserved. Without the split, the
// embedded newline terminates the data line early and silently corrupts the
// stream.
func TestWriterMultiLineDataRoundTrips(t *testing.T) {
	w, rec := newWriter(t)
	payload := []byte("line1\nline2")
	if err := w.WriteEvent("update", payload); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}

	// Re-parse what the writer emitted: the data must round-trip exactly.
	r := NewReader(bytes.NewReader(rec.Body.Bytes()))
	ev, err := r.Next()
	if err != nil && err != io.EOF {
		t.Fatalf("Next: %v", err)
	}
	if ev.Name != "update" {
		t.Errorf("Name = %q, want update", ev.Name)
	}
	if !bytes.Equal(ev.Data, payload) {
		t.Errorf("round-trip Data = %q, want %q", ev.Data, payload)
	}
}

func TestWriterRawBytes(t *testing.T) {
	w, rec := newWriter(t)
	raw := []byte("data: raw\n\n")
	if err := w.WriteRaw(raw); err != nil {
		t.Fatalf("WriteRaw: %v", err)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, raw) {
		t.Errorf("output = %q, want %q", got, raw)
	}
	if !rec.Flushed {
		t.Error("expected Flush to be called")
	}
}

func TestNewWriterSetsHeaders(t *testing.T) {
	_, rec := newWriter(t)
	h := rec.Header()
	for k, want := range map[string]string{
		"Content-Type":  "text/event-stream",
		"Cache-Control": "no-cache",
		"Connection":    "keep-alive",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// failingWriter is an http.ResponseWriter + http.Flusher that fails all writes.
type failingWriter struct{}

func (failingWriter) Header() http.Header        { return http.Header{} }
func (failingWriter) Write([]byte) (int, error)  { return 0, io.ErrShortWrite }
func (failingWriter) WriteHeader(statusCode int) {}
func (failingWriter) Flush()                     {}

// nonFlusherWriter implements http.ResponseWriter but NOT http.Flusher.
type nonFlusherWriter struct{}

func (nonFlusherWriter) Header() http.Header        { return http.Header{} }
func (nonFlusherWriter) Write([]byte) (int, error)  { return 0, nil }
func (nonFlusherWriter) WriteHeader(statusCode int) {}

func TestNewWriterRejectsNonFlusher(t *testing.T) {
	w, ok := NewWriter(nonFlusherWriter{})
	if ok || w != nil {
		t.Errorf("got (%+v, %v), want (nil, false) for non-Flusher writer", w, ok)
	}
}

func TestWriterWriteEventPropagatesWriteError(t *testing.T) {
	w, ok := NewWriter(failingWriter{})
	if !ok {
		t.Fatal("NewWriter: failingWriter should implement Flusher")
	}
	if err := w.WriteEvent("ping", []byte("data")); err == nil {
		t.Error("got nil error, want write error propagated")
	}
}

func TestWriterWriteRawPropagatesWriteError(t *testing.T) {
	w, ok := NewWriter(failingWriter{})
	if !ok {
		t.Fatal("NewWriter: failingWriter should implement Flusher")
	}
	if err := w.WriteRaw([]byte("data: raw\n\n")); err == nil {
		t.Error("got nil error, want write error propagated")
	}
}
