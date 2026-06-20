package proxy

import "context"

// TraceInfo carries details discovered while serving a request back to the
// request-logging middleware. The middleware attaches an empty TraceInfo to the
// request context before the handler runs and reads it afterward; the serve path
// records the resolved upstream URL into it once a target is forwarded to.
type TraceInfo struct {
	UpstreamURL string
}

type traceKeyT struct{}

var traceKey traceKeyT

// WithTraceInfo attaches t to ctx so forward/forwardStream can record into it.
func WithTraceInfo(ctx context.Context, t *TraceInfo) context.Context {
	return context.WithValue(ctx, traceKey, t)
}

func traceInfoFrom(ctx context.Context) *TraceInfo {
	t, _ := ctx.Value(traceKey).(*TraceInfo)
	return t
}
