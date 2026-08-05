package observability

// Capture is a bounded body observer. Write always reports len(p) so callers
// never see backpressure; only the first limit bytes are retained. Used by HAR
// capture and bounded probe response reads — not for terminal body dumps.
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
