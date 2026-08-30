// Package sse provides a minimal Server-Sent Events reader and writer used to
// translate streaming responses between provider protocols.
package sse

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	maxLineBytes  = 8 << 20
	maxEventBytes = 8 << 20
)

// SizeLimitError reports an SSE line or assembled event that exceeded its
// parser budget.
type SizeLimitError struct {
	Scope string
	Limit int
}

func (e *SizeLimitError) Error() string {
	return fmt.Sprintf("sse %s exceeds %d bytes", e.Scope, e.Limit)
}

// Event is one parsed SSE event. Name is empty for OpenAI-style streams that
// only use data lines; Anthropic streams set it (e.g. "content_block_delta").
type Event struct {
	Name string
	Data []byte
}

// Reader parses an SSE byte stream into events. ReadSlice keeps allocation
// bounded while still allowing lines larger than the internal 64 KiB buffer.
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
	hasData := false

	for {
		line, err := r.readLine()
		if len(line) > 0 {
			switch {
			case line[0] == ':':
				// Comment, ignore.
			case bytes.HasPrefix(line, []byte("event:")):
				name := strings.TrimSpace(string(line[len("event:"):]))
				if len(name)+len(ev.Data) > maxEventBytes {
					return Event{}, &SizeLimitError{Scope: "event", Limit: maxEventBytes}
				}
				ev.Name = name
			case bytes.HasPrefix(line, []byte("data:")):
				payload := line[len("data:"):]
				if len(payload) > 0 && payload[0] == ' ' {
					payload = payload[1:]
				}
				extra := len(payload)
				if hasData {
					extra++
				}
				if len(ev.Name)+len(ev.Data)+extra > maxEventBytes {
					return Event{}, &SizeLimitError{Scope: "event", Limit: maxEventBytes}
				}
				if hasData {
					ev.Data = append(ev.Data, '\n')
				}
				ev.Data = append(ev.Data, payload...)
				hasData = true
			}
		} else if err == nil {
			if hasData || ev.Name != "" {
				return ev, nil
			}
			// Stray blank line before any field; keep reading.
		}
		if err != nil {
			if (hasData || ev.Name != "") && err == io.EOF {
				return ev, nil
			}
			return Event{}, err
		}
	}
}

func (r *Reader) readLine() ([]byte, error) {
	var line []byte
	for {
		fragment, err := r.br.ReadSlice('\n')
		hasNewline := len(fragment) > 0 && fragment[len(fragment)-1] == '\n'
		if hasNewline {
			fragment = fragment[:len(fragment)-1]
		}
		if len(line)+len(fragment) > maxLineBytes+1 {
			return nil, &SizeLimitError{Scope: "line", Limit: maxLineBytes}
		}
		line = append(line, fragment...)

		if hasNewline || err == io.EOF {
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if len(line) > maxLineBytes {
				return nil, &SizeLimitError{Scope: "line", Limit: maxLineBytes}
			}
			return line, err
		}
		if len(line) > maxLineBytes && line[len(line)-1] != '\r' {
			return nil, &SizeLimitError{Scope: "line", Limit: maxLineBytes}
		}
		if err != nil && err != bufio.ErrBufferFull {
			return line, err
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
// produces a data-only event (OpenAI style). Per the SSE spec, a payload
// containing newlines is split into multiple data: lines, which a reader
// rejoins with a newline; emitting an embedded newline as a single data: line
// would corrupt the stream (a client sees the newline as an event boundary).
func (w *Writer) WriteEvent(name string, data []byte) error {
	var b strings.Builder
	if name != "" {
		fmt.Fprintf(&b, "event: %s\n", name)
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		fmt.Fprintf(&b, "data: %s\n", line)
	}
	b.WriteByte('\n')
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
