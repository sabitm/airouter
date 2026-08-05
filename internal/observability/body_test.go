package observability

import (
	"bytes"
	"testing"
)

func TestCaptureBelowAtAboveLimit(t *testing.T) {
	t.Run("below", func(t *testing.T) {
		c := NewCapture(10)
		n, err := c.Write([]byte("hello"))
		if err != nil || n != 5 {
			t.Fatalf("Write = %d, %v", n, err)
		}
		if !bytes.Equal(c.Bytes(), []byte("hello")) {
			t.Errorf("Bytes = %q", c.Bytes())
		}
		if c.Total() != 5 || c.Truncated() {
			t.Errorf("Total=%d Truncated=%v", c.Total(), c.Truncated())
		}
	})
	t.Run("at", func(t *testing.T) {
		c := NewCapture(5)
		c.Write([]byte("hello"))
		if c.Total() != 5 || c.Truncated() || string(c.Bytes()) != "hello" {
			t.Errorf("at limit: total=%d trunc=%v bytes=%q", c.Total(), c.Truncated(), c.Bytes())
		}
	})
	t.Run("above", func(t *testing.T) {
		c := NewCapture(4)
		n, err := c.Write([]byte("hello world"))
		if err != nil || n != 11 {
			t.Fatalf("Write must report full len: %d %v", n, err)
		}
		if string(c.Bytes()) != "hell" {
			t.Errorf("Bytes = %q, want hell", c.Bytes())
		}
		if c.Total() != 11 || !c.Truncated() {
			t.Errorf("Total=%d Truncated=%v", c.Total(), c.Truncated())
		}
	})
	t.Run("chunked overflow", func(t *testing.T) {
		c := NewCapture(5)
		c.Write([]byte("ab"))
		c.Write([]byte("cdefgh"))
		if string(c.Bytes()) != "abcde" {
			t.Errorf("Bytes = %q", c.Bytes())
		}
		if c.Total() != 8 || !c.Truncated() {
			t.Errorf("Total=%d Truncated=%v", c.Total(), c.Truncated())
		}
	})
	t.Run("zero limit counts only", func(t *testing.T) {
		c := NewCapture(0)
		c.Write([]byte("xyz"))
		if len(c.Bytes()) != 0 || c.Total() != 3 || !c.Truncated() {
			t.Errorf("zero limit: bytes=%q total=%d trunc=%v", c.Bytes(), c.Total(), c.Truncated())
		}
	})
}
