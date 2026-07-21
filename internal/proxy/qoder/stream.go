package qoder

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"airouter/internal/proxy/openai"
	"airouter/internal/proxy/ir"
)

// DecodeStream unwraps Qoder's {statusCodeValue, body} SSE envelope into plain
// OpenAI chat-completion chunks, then reuses the OpenAI stream decoder.
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	return openai.DecodeStream(newUnwrapReader(r), emit)
}

// unwrapReader converts Qoder envelope SSE into OpenAI-shaped SSE bytes.
type unwrapReader struct {
	sc     *bufio.Scanner
	buf    bytes.Buffer
	err    error
	done   bool
	closed bool
}

func newUnwrapReader(r io.Reader) *unwrapReader {
	sc := bufio.NewScanner(r)
	// Upstream chunks can be large (tool args); raise the default 64K limit.
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	return &unwrapReader{sc: sc}
}

func (u *unwrapReader) Read(p []byte) (int, error) {
	for u.buf.Len() == 0 {
		if u.err != nil {
			return 0, u.err
		}
		if u.done || u.closed {
			return 0, io.EOF
		}
		if !u.sc.Scan() {
			if err := u.sc.Err(); err != nil {
				u.err = err
				return 0, err
			}
			// Ensure a trailing DONE so the OpenAI decoder finishes cleanly.
			u.buf.WriteString("data: [DONE]\n\n")
			u.done = true
			continue
		}
		line := strings.TrimRight(u.sc.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "[DONE]" {
			u.buf.WriteString("data: [DONE]\n\n")
			u.done = true
			continue
		}
		var env struct {
			StatusCodeValue int    `json:"statusCodeValue"`
			Body            string `json:"body"`
		}
		if json.Unmarshal([]byte(data), &env) != nil {
			// Not an envelope — pass through as OpenAI data line.
			u.buf.WriteString("data: ")
			u.buf.WriteString(data)
			u.buf.WriteString("\n\n")
			continue
		}
		status := env.StatusCodeValue
		if status == 0 {
			status = 200
		}
		inner := env.Body
		if status != 200 {
			u.err = fmt.Errorf("qoder: upstream status %d: %s", status, truncate(inner, 200))
			return 0, u.err
		}
		if inner == "" {
			continue
		}
		if inner == "[DONE]" {
			u.buf.WriteString("data: [DONE]\n\n")
			u.done = true
			continue
		}
		// Single-line SSE frame (strip embedded newlines).
		sanitized := strings.ReplaceAll(strings.ReplaceAll(inner, "\r", ""), "\n", "")
		u.buf.WriteString("data: ")
		u.buf.WriteString(sanitized)
		u.buf.WriteString("\n\n")
	}
	return u.buf.Read(p)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
