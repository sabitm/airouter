package proxy

import "context"

// TraceInfo carries details discovered while serving a request back to the
// request-logging middleware. The middleware attaches an empty TraceInfo to the
// request context before the handler runs and reads it afterward; the serve path
// records the resolved upstream URL into it once a target is forwarded to.
//
// CodexSessionID is the per-request id the codex backend translate path sets
// for the session_id header and prompt_cache_key (the two must match upstream).
// Empty for non-codex requests.
//
// QoderModelKey/Source are set by prepareUpstreamRequest for the Qoder backend
// so applyUpstreamHeaders can emit X-Model-Key / X-Model-Source after COSY sign.
type TraceInfo struct {
	UpstreamURL      string
	CodexSessionID   string
	QoderModelKey    string
	QoderModelSource string
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
