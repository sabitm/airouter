// Package sse provides a minimal Server-Sent Events reader and writer used to
// translate streaming responses between provider protocols.
package sse

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Event is one parsed SSE event. Name is empty for OpenAI-style streams that
// only use data lines; Anthropic streams set it (e.g. "content_block_delta").
type Event struct {
	Name string
	Data []byte
}

// Reader parses an SSE byte stream into events. It uses bufio.Reader rather than
// Scanner so individual data lines are not bounded by a token size limit.
type Reader struct {
	br *bufio.Reader
}

func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 64*1024)}
}

// Next returns the next event, or io.EOF when the stream ends. Comment lines
// (starting with ':') are ignored; multiple data lines are joined with '\n'.
func (r *Reader) Next() (Event, error) {
	var ev Event
	var data []string
	hasData := false

	for {
		line, err := r.br.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				if hasData || ev.Name != "" {
					ev.Data = []byte(strings.Join(data, "\n"))
					return ev, nil
				}
				// stray blank line before any field; keep reading
			case strings.HasPrefix(trimmed, ":"):
				// comment, ignore
			case strings.HasPrefix(trimmed, "event:"):
				ev.Name = strings.TrimSpace(trimmed[len("event:"):])
			case strings.HasPrefix(trimmed, "data:"):
				// Per the SSE spec a single optional space after the colon is stripped.
				data = append(data, strings.TrimPrefix(trimmed[len("data:"):], " "))
				hasData = true
			}
		}
		if err != nil {
			if (hasData || ev.Name != "") && err == io.EOF {
				ev.Data = []byte(strings.Join(data, "\n"))
				return ev, nil
			}
			return Event{}, err
		}
	}
}

// Writer emits SSE events to an http.ResponseWriter, flushing after each one so
// clients receive deltas immediately.
type Writer struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// NewWriter sets the SSE response headers and returns a Writer. The bool result
// is false if the ResponseWriter does not support flushing, in which case
// streaming cannot proceed.
func NewWriter(w http.ResponseWriter) (*Writer, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	return &Writer{w: w, flusher: f}, true
}

// WriteEvent writes a named event with a data payload and flushes. An empty name
// produces a data-only event (OpenAI style).
func (w *Writer) WriteEvent(name string, data []byte) error {
	var b strings.Builder
	if name != "" {
		fmt.Fprintf(&b, "event: %s\n", name)
	}
	fmt.Fprintf(&b, "data: %s\n\n", data)
	if _, err := io.WriteString(w.w, b.String()); err != nil {
		return err
	}
	w.flusher.Flush()
	return nil
}

// WriteRaw writes pre-formatted bytes (used for verbatim passthrough relay) and
// flushes.
func (w *Writer) WriteRaw(b []byte) error {
	if _, err := w.w.Write(b); err != nil {
		return err
	}
	w.flusher.Flush()
	return nil
}
