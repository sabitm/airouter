package kirocatalog

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"airouter/internal/domain"
	"airouter/internal/observability"
	"airouter/internal/proxy/kiro"
)

const (
	legacyTarget     = "AmazonCodeWhispererService.ListAvailableModels"
	managementTarget = "KiroControlPlaneBearerService.ListAvailableModels"
	catalogOrigin    = "AI_EDITOR"
	maxPages         = 10
	// Deadline is the shared catalog budget for pages, refresh, and fallback.
	Deadline   = 15 * time.Second
	jsonType   = "application/x-amz-json-1.0"
	jsonAccept = "application/json"
	captureMax = 1 << 20
)

// Client has no timeout. Callers bind one context deadline before the first
// management or legacy request. That deadline covers every page, refresh, and
// fallback together.
type Client struct {
	HTTP *http.Client
}

// NewClient returns a catalog client. A nil HTTP client uses the package
// default, which tests may replace. An explicit client is not affected later.
func NewClient(httpClient *http.Client) *Client {
	if httpClient != nil {
		return &Client{HTTP: httpClient}
	}
	defaultClientMu.Lock()
	defer defaultClientMu.Unlock()
	if defaultHTTP == nil {
		defaultHTTP = &http.Client{}
	}
	return &Client{HTTP: defaultHTTP}
}

var (
	defaultClientMu sync.Mutex
	defaultHTTP     *http.Client
)

// SetDefaultHTTPClient replaces the client used when Client.HTTP is nil.
// Tests use it to keep dashboard and proxy discovery on a fake transport.
func SetDefaultHTTPClient(c *http.Client) {
	defaultClientMu.Lock()
	if c == nil {
		c = &http.Client{}
	}
	defaultHTTP = c
	defaultClientMu.Unlock()
}

// CurrentDefaultHTTPClient returns the replaceable client.
func CurrentDefaultHTTPClient() *http.Client {
	defaultClientMu.Lock()
	defer defaultClientMu.Unlock()
	if defaultHTTP == nil {
		defaultHTTP = &http.Client{}
	}
	return defaultHTTP
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	defaultClientMu.Lock()
	defer defaultClientMu.Unlock()
	if defaultHTTP == nil {
		defaultHTTP = &http.Client{}
	}
	return defaultHTTP
}

// List loads the live catalog for p. Explicit management discovery tries the
// regional control plane first. Auth rejection stays an auth rejection. A
// bounded legacy fallback is used only for transport or body-read failure,
// HTTP 404, and HTTP 5xx. The default remains legacy. p is not mutated.
// The caller context must already carry the shared deadline.
func (c *Client) List(ctx context.Context, logger *slog.Logger, p *domain.Provider) ([]Model, error) {
	if p == nil || strings.TrimSpace(p.BaseURL) == "" {
		return nil, ErrInvalidBaseURL
	}
	id := kiro.IdentityFromProvider(p)
	if kiro.UseManagementDiscovery(id) {
		models, err := c.listManagement(ctx, logger, p, id)
		if err == nil {
			return models, nil
		}
		if !managementFallback(err) || ctx.Err() != nil {
			return nil, err
		}
	}
	return c.listLegacy(ctx, logger, p)
}

// ListWithRefresh loads the catalog and retries once after a 401 or 403 when
// refresh is non-nil. One deadline covers pages, refresh, fallback, and retry.
// A result never mixes pages from different tokens. A nil refresh disables
// retry. The parent context is not replaced when it is already canceled.
// Existing shorter deadlines are preserved and longer deadlines are capped.
func (c *Client) ListWithRefresh(ctx context.Context, logger *slog.Logger, p *domain.Provider, refresh func(context.Context) (string, error)) ([]Model, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, Deadline)
	defer cancel()
	models, err := c.List(ctx, logger, p)
	if refresh == nil || ctx.Err() != nil || AuthStatus(err) == 0 {
		return models, err
	}
	tok, rerr := refresh(ctx)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if rerr != nil {
		return nil, &RefreshError{Err: rerr}
	}
	if tok == "" {
		return nil, &RefreshError{Err: errors.New("empty access token")}
	}
	retry := *p
	retry.APIKey = tok
	return c.List(ctx, logger, &retry)
}

func (c *Client) listLegacy(ctx context.Context, logger *slog.Logger, p *domain.Provider) ([]Model, error) {
	var models []Model
	seen := map[string]struct{}{}
	seenTokens := map[string]struct{}{}
	nextToken := ""
	for page := 1; page <= maxPages; page++ {
		body, err := catalogBody(p, nextToken)
		if err != nil {
			return nil, err
		}
		req, err := legacyRequest(ctx, p, body)
		if err != nil {
			return nil, err
		}
		pr, err := c.execute(ctx, logger, req, "kiro_models")
		// Match management precedence: cancellation, then a received non-success
		// status, then truncation, then an ordinary read or transport failure.
		if classified, cerr := classifyReceived(pr, err); classified {
			return nil, cerr
		}
		pageModels, token, err := Parse(pr.Body)
		if err != nil {
			return nil, err
		}
		models = appendModels(models, seen, pageModels)
		if token == "" {
			if len(models) == 0 {
				return nil, ErrEmpty
			}
			return models, nil
		}
		if _, ok := seenTokens[token]; ok {
			return nil, ErrTokenRepeat
		}
		seenTokens[token] = struct{}{}
		nextToken = token
	}
	return nil, ErrPageLimit
}

func (c *Client) listManagement(ctx context.Context, logger *slog.Logger, p *domain.Provider, id kiro.Identity) ([]Model, error) {
	endpoint := kiro.ManagementURL(kiro.Region(id))
	var models []Model
	seen := map[string]struct{}{}
	seenTokens := map[string]struct{}{}
	nextToken := ""
	for page := 1; page <= maxPages; page++ {
		req, err := managementRequest(ctx, p, id, nextToken, endpoint)
		if err != nil {
			return nil, err
		}
		pr, err := c.execute(ctx, logger, req, "kiro_management_models")
		// A received status or truncation result is authoritative even when the
		// body read fails. Only a genuine transport or body-read failure, with
		// neither result, may fall back.
		if classified, cerr := classifyReceived(pr, err); classified {
			return nil, cerr
		}
		pageModels, token, perr := Parse(pr.Body)
		if perr != nil {
			return nil, perr
		}
		models = appendModels(models, seen, pageModels)
		if token == "" {
			if len(models) == 0 {
				return nil, ErrEmpty
			}
			return models, nil
		}
		if _, ok := seenTokens[token]; ok {
			return nil, ErrTokenRepeat
		}
		seenTokens[token] = struct{}{}
		nextToken = token
	}
	return nil, ErrPageLimit
}

func appendModels(dst []Model, seen map[string]struct{}, page []Model) []Model {
	for _, model := range page {
		if _, ok := seen[model.ID]; ok {
			continue
		}
		seen[model.ID] = struct{}{}
		dst = append(dst, model)
	}
	return dst
}

func catalogBody(p *domain.Provider, nextToken string) ([]byte, error) {
	body := map[string]string{"origin": catalogOrigin}
	if arn := kiro.ProfileArnForBody(kiro.IdentityFromProvider(p)); arn != "" {
		body["profileArn"] = arn
	}
	if nextToken != "" {
		body["nextToken"] = nextToken
	}
	return json.Marshal(body)
}

func legacyRequest(ctx context.Context, p *domain.Provider, body []byte) (*http.Request, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/") + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, ErrInvalidBaseURL
	}
	applyHeaders(req, p, legacyTarget, jsonAccept)
	return req, nil
}

// managementRequest follows the active KiroControlPlaneBearer RPC serializer.
// The HTTP GET /List-Available-Models trait is unused. The live client posts
// AWS JSON 1.0 to the regional root with origin, profileArn, and nextToken.
func managementRequest(ctx context.Context, p *domain.Provider, id kiro.Identity, nextToken, endpoint string) (*http.Request, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, ErrInvalidBaseURL
	}
	if u.Path == "" {
		u.Path = "/"
	}
	u.RawQuery = ""
	u.Fragment = ""
	body := map[string]string{"origin": catalogOrigin}
	if arn := kiro.ProfileArnForBody(id); arn != "" {
		body["profileArn"] = arn
	}
	if nextToken != "" {
		body["nextToken"] = nextToken
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	applyHeaders(req, p, managementTarget, jsonAccept)
	return req, nil
}

func applyHeaders(req *http.Request, p *domain.Provider, target, accept string) {
	req.Header.Set("Content-Type", jsonType)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-Amz-Target", target)
	req.Header.Set("User-Agent", kiro.UserAgent)
	req.Header.Set("X-Amz-User-Agent", kiro.XAmzUserAgent)
	req.Header.Set("Amz-Sdk-Request", kiro.AmzSdkRequest)
	req.Header.Set("Amz-Sdk-Invocation-Id", newUUID())
	kiro.ApplyAuthHeaders(req.Header, kiro.IdentityFromProvider(p))
}

// classifyReceived reports whether the probe result is already a catalog
// outcome. Context cancellation and deadline win. A received non-2xx status
// wins over truncation and a later body-read error. Truncation then wins over
// that read error. A remaining transport or body-read error is returned
// unchanged so the caller can apply the bounded fallback policy.
func classifyReceived(pr result, err error) (bool, error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true, err
	}
	if pr.StatusCode != 0 && (pr.StatusCode < 200 || pr.StatusCode >= 300) {
		return true, &StatusError{Status: pr.StatusCode}
	}
	if pr.Truncated {
		return true, ErrTruncated
	}
	if err != nil {
		return true, err
	}
	return false, nil
}

// managementFallback is true only for a transport or body-read failure, HTTP
// 404, or an HTTP 5xx. Catalog shape, empty results, pagination limits, request
// construction, cancellation, and other 4xx responses stay on management.
func managementFallback(err error) bool {
	if err == nil || AuthStatus(err) != 0 {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var status *StatusError
	if errors.As(err, &status) {
		return status.Status == http.StatusNotFound || status.Status >= 500
	}
	var transport *TransportError
	return errors.As(err, &transport)
}

type result struct {
	StatusCode int
	Body       []byte
	Truncated  bool
}

func (c *Client) execute(ctx context.Context, logger *slog.Logger, req *http.Request, operation string) (result, error) {
	if logger == nil {
		logger = slog.Default()
	}
	log := observability.Logger(ctx, logger)
	start := time.Now()
	endpoint := safeEndpoint(req.URL)
	if log.Enabled(ctx, observability.LevelTrace) {
		log.Log(ctx, observability.LevelTrace, "probe_request",
			"event", "probe_request",
			"operation", operation,
			"method", req.Method,
			"url", endpoint,
			"content_type", req.Header.Get("Content-Type"),
			"size", req.ContentLength,
		)
	}
	if ctx.Err() != nil {
		return result{}, &TransportError{Err: ctx.Err()}
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		log.Debug("probe_transport_failed",
			"event", "probe_transport_failed",
			"operation", operation,
			"method", req.Method,
			"url", endpoint,
			"reason", transportReason(err),
		)
		return result{}, &TransportError{Err: err}
	}
	defer resp.Body.Close()
	body, total, truncated, rerr := readBounded(resp.Body, captureMax)
	if ctx.Err() != nil {
		rerr = ctx.Err()
	}
	pr := result{StatusCode: resp.StatusCode, Body: body, Truncated: truncated}
	if rerr != nil {
		log.Debug("probe_read_failed",
			"event", "probe_read_failed",
			"operation", operation,
			"method", req.Method,
			"url", endpoint,
			"status", resp.StatusCode,
			"reason", transportReason(rerr),
		)
		return pr, &TransportError{Err: rerr}
	}
	if log.Enabled(ctx, observability.LevelTrace) {
		log.Log(ctx, observability.LevelTrace, "probe_response",
			"event", "probe_response",
			"operation", operation,
			"method", req.Method,
			"url", endpoint,
			"status", resp.StatusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"content_type", resp.Header.Get("Content-Type"),
			"size", total,
			"truncated", truncated,
		)
	}
	return pr, nil
}

func safeEndpoint(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

func transportReason(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return "transport"
	}
}

func readBounded(r io.Reader, limit int) (body []byte, total int64, truncated bool, err error) {
	buf := make([]byte, 32*1024)
	var kept []byte
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			total += int64(n)
			if len(kept) < limit {
				remain := limit - len(kept)
				if n < remain {
					remain = n
				}
				kept = append(kept, buf[:remain]...)
			}
		}
		if errors.Is(rerr, context.Canceled) || errors.Is(rerr, context.DeadlineExceeded) {
			return kept, total, total > int64(limit), rerr
		}
		if total > int64(limit) {
			return kept, total, true, rerr
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return append([]byte(nil), kept...), total, false, rerr
		}
	}
	return append([]byte(nil), kept...), total, false, nil
}

// NewInvocationID returns a fresh Amz-Sdk-Invocation-Id.
func NewInvocationID() string {
	return newUUID()
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:16])
}
