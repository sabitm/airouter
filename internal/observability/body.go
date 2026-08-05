package observability

import (
	"fmt"
	"strings"
)

// Capture is a bounded body observer. Write always reports len(p) so callers
// never see backpressure; only the first limit bytes are retained.
type Capture struct {
	limit int
	buf   []byte
	total int64
}

// NewCapture returns a Capture that retains at most limit bytes. limit <= 0
// retains nothing while still counting Total.
func NewCapture(limit int) *Capture {
	if limit < 0 {
		limit = 0
	}
	return &Capture{limit: limit}
}

// Write observes p. It always returns len(p), nil.
func (c *Capture) Write(p []byte) (int, error) {
	if c == nil {
		return len(p), nil
	}
	c.total += int64(len(p))
	if c.limit <= 0 || len(c.buf) >= c.limit {
		return len(p), nil
	}
	room := c.limit - len(c.buf)
	if room > len(p) {
		room = len(p)
	}
	c.buf = append(c.buf, p[:room]...)
	return len(p), nil
}

// Bytes returns the retained prefix (not a copy).
func (c *Capture) Bytes() []byte {
	if c == nil {
		return nil
	}
	return c.buf
}

// Total is the number of bytes passed to Write.
func (c *Capture) Total() int64 {
	if c == nil {
		return 0
	}
	return c.total
}

// Truncated reports whether more bytes were observed than retained.
func (c *Capture) Truncated() bool {
	if c == nil {
		return false
	}
	return c.total > int64(len(c.buf))
}

// BodyView is a terminal-safe description of a captured body.
type BodyView struct {
	Text        string
	Size        int64
	Truncated   bool
	Textual     bool
	ContentType string
}

// DescribeBody builds a BodyView from a captured prefix and the original wire
// size. displayLimit caps the rendered Text for textual bodies (<=0 means use
// the full captured prefix). Binary bodies never dump bytes.
func DescribeBody(captured []byte, total int64, contentType string, displayLimit int) BodyView {
	if total < int64(len(captured)) {
		total = int64(len(captured))
	}
	v := BodyView{
		Size:        total,
		Truncated:   total > int64(len(captured)),
		ContentType: contentType,
		Textual:     IsTextual(contentType),
	}
	if total == 0 {
		v.Text = "(empty)"
		return v
	}
	if !v.Textual {
		return v
	}
	shown := captured
	if displayLimit > 0 && len(shown) > displayLimit {
		shown = shown[:displayLimit]
	}
	if total > int64(len(shown)) {
		v.Truncated = true
		v.Text = fmt.Sprintf("%s... (truncated, %d bytes total)", shown, total)
		return v
	}
	v.Text = string(shown)
	return v
}

// IsTextual reports whether a Content-Type is safe to dump as text. Empty type
// is treated as textual since JSON/SSE responses often omit it until the first
// write. application/json (with params), text/*, and event-stream are textual.
func IsTextual(contentType string) bool {
	ct := strings.ToLower(contentType)
	switch {
	case ct == "",
		strings.HasPrefix(ct, "application/json"),
		strings.HasPrefix(ct, "text/"),
		strings.Contains(ct, "event-stream"):
		return true
	default:
		return false
	}
}
