package observability

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestNewLoggerLevelMapping(t *testing.T) {
	cases := []struct {
		debug int
		want  slog.Level
	}{
		{-1, slog.LevelInfo},
		{0, slog.LevelInfo},
		{1, slog.LevelDebug},
		{2, LevelTrace},
		{3, LevelTrace},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		l := NewLogger(tc.debug, &buf)
		// Emit one record at each level; only those >= mapped min should appear.
		l.Log(context.Background(), LevelTrace, "trace")
		l.Debug("debug")
		l.Info("info")
		out := buf.String()
		hasTrace := strings.Contains(out, "msg=trace")
		hasDebug := strings.Contains(out, "msg=debug")
		hasInfo := strings.Contains(out, "msg=info")
		switch tc.want {
		case LevelTrace:
			if !hasTrace || !hasDebug || !hasInfo {
				t.Errorf("debug=%d out=%q, want TRACE+DEBUG+INFO", tc.debug, out)
			}
		case slog.LevelDebug:
			if hasTrace || !hasDebug || !hasInfo {
				t.Errorf("debug=%d out=%q, want DEBUG+INFO only", tc.debug, out)
			}
		case slog.LevelInfo:
			if hasTrace || hasDebug || !hasInfo {
				t.Errorf("debug=%d out=%q, want INFO only", tc.debug, out)
			}
		}
	}
}

func TestTraceLevelLabel(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(2, &buf)
	l.Log(context.Background(), LevelTrace, "body")
	if !strings.Contains(buf.String(), "level=TRACE") {
		t.Fatalf("expected TRACE label, got %q", buf.String())
	}
	// Stock levels keep their default labels.
	buf.Reset()
	l.Info("i")
	l.Warn("w")
	l.Error("e")
	l.Debug("d")
	out := buf.String()
	for _, want := range []string{"level=INFO", "level=WARN", "level=ERROR", "level=DEBUG"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %q", want, out)
		}
	}
}

func TestRequestIDContext(t *testing.T) {
	ctx := context.Background()
	if got := RequestID(ctx); got != "" {
		t.Errorf("empty ctx RequestID = %q", got)
	}
	ctx = WithRequestID(ctx, "abc123")
	if got := RequestID(ctx); got != "abc123" {
		t.Errorf("RequestID = %q, want abc123", got)
	}
	// Empty id is a no-op.
	ctx2 := WithRequestID(context.Background(), "")
	if got := RequestID(ctx2); got != "" {
		t.Errorf("empty id RequestID = %q", got)
	}
}

func TestLoggerAddsRequestID(t *testing.T) {
	var buf bytes.Buffer
	base := NewLogger(1, &buf).With("component", "test")
	ctx := WithRequestID(context.Background(), "req-9")
	Logger(ctx, base).Info("hello")
	out := buf.String()
	if !strings.Contains(out, "request_id=req-9") {
		t.Errorf("missing request_id: %q", out)
	}
	if !strings.Contains(out, "component=test") {
		t.Errorf("missing component: %q", out)
	}

	// Nil base falls back to default without panicking.
	prev := slog.Default()
	slog.SetDefault(NewLogger(1, &buf))
	t.Cleanup(func() { slog.SetDefault(prev) })
	buf.Reset()
	Logger(ctx, nil).Info("via-default")
	if !strings.Contains(buf.String(), "request_id=req-9") {
		t.Errorf("nil base missing request_id: %q", buf.String())
	}
}
