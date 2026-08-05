package observability

import (
	"bytes"
	"strings"
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

func TestDescribeBody(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		v := DescribeBody(nil, 0, "application/json", 100)
		if v.Text != "(empty)" || v.Size != 0 || !v.Textual {
			t.Errorf("empty view = %+v", v)
		}
	})
	t.Run("text full", func(t *testing.T) {
		v := DescribeBody([]byte(`{"a":1}`), 7, "application/json", 100)
		if v.Text != `{"a":1}` || v.Truncated || !v.Textual {
			t.Errorf("text = %+v", v)
		}
	})
	t.Run("text truncated", func(t *testing.T) {
		body := []byte("abcdefghij")
		v := DescribeBody(body[:4], 10, "text/plain", 4)
		if !v.Truncated || !strings.Contains(v.Text, "truncated") || !strings.Contains(v.Text, "10 bytes total") {
			t.Errorf("truncated = %+v", v)
		}
		if !strings.HasPrefix(v.Text, "abcd") {
			t.Errorf("prefix = %q", v.Text)
		}
	})
	t.Run("binary summarized", func(t *testing.T) {
		v := DescribeBody([]byte{0x00, 0x01, 0xff}, 3, "application/octet-stream", 100)
		if v.Textual || v.Text != "" || v.Size != 3 || v.Truncated {
			t.Errorf("binary = %+v", v)
		}
	})
	t.Run("truncated binary reports truncation", func(t *testing.T) {
		v := DescribeBody([]byte{0x00, 0x01}, 10, "application/octet-stream", 100)
		if v.Textual || v.Text != "" || v.Size != 10 || !v.Truncated {
			t.Errorf("truncated binary = %+v", v)
		}
	})
	t.Run("event-stream textual", func(t *testing.T) {
		v := DescribeBody([]byte("data: hi\n\n"), 10, "text/event-stream", 100)
		if !v.Textual || v.Text != "data: hi\n\n" {
			t.Errorf("sse = %+v", v)
		}
	})
	t.Run("json with params", func(t *testing.T) {
		if !IsTextual("application/json; charset=utf-8") {
			t.Error("json with params should be textual")
		}
	})
	t.Run("empty content-type textual", func(t *testing.T) {
		if !IsTextual("") {
			t.Error("empty ct should be textual")
		}
	})
}
