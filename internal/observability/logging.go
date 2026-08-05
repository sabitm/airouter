// Package observability provides structured terminal logging and bounded body
// capture shared by the server, proxy, and dashboard.
package observability

import (
	"context"
	"io"
	"log/slog"
)

// LevelTrace is one step below Debug. It is used for truncated request/response
// body dumps at -debug=2. Mapped from NewLogger when debugLevel >= 2.
const LevelTrace slog.Level = slog.LevelDebug - 4

type requestIDKeyT struct{}

var requestIDKey requestIDKeyT

// NewLogger builds a text slog.Logger whose minimum level is derived from the
// historical -debug flag: <=0 Info, ==1 Debug, >=2 Trace.
func NewLogger(debugLevel int, out io.Writer) *slog.Logger {
	level := slog.LevelInfo
	switch {
	case debugLevel >= 2:
		level = LevelTrace
	case debugLevel == 1:
		level = slog.LevelDebug
	}
	h := slog.NewTextHandler(out, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceLevel,
	})
	return slog.New(h)
}

// replaceLevel renders LevelTrace as TRACE without disturbing the stock labels.
func replaceLevel(_ []string, a slog.Attr) slog.Attr {
	if a.Key != slog.LevelKey {
		return a
	}
	if lvl, ok := a.Value.Any().(slog.Level); ok && lvl == LevelTrace {
		return slog.Attr{Key: slog.LevelKey, Value: slog.StringValue("TRACE")}
	}
	return a
}

// WithRequestID stores a request correlation id on ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the correlation id previously stored on ctx, or "".
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// Logger returns base (or slog.Default when base is nil) enriched with
// request_id when one is present on ctx.
func Logger(ctx context.Context, base *slog.Logger) *slog.Logger {
	if base == nil {
		base = slog.Default()
	}
	if id := RequestID(ctx); id != "" {
		return base.With("request_id", id)
	}
	return base
}
