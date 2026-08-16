package usage

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
)

// Quota is one named bucket of upstream account allowance.
type Quota struct {
	Name         string
	Used         float64
	Total        float64
	Remaining    float64
	RemainingPct *float64
	ResetAt      *time.Time
	Unlimited    bool
	Unit         string
}

// Report is the dashboard-facing snapshot of a provider account's quota.
type Report struct {
	Plan         string
	Message      string
	Quotas       []Quota
	ResetCredits int
	FetchedAt    time.Time
}

// FetchOpts controls a single Fetch.
type FetchOpts struct {
	// Force bypasses the 60s result cache. It does not bypass a Claude 429 cooldown.
	Force bool
}

// Endpoint URLs are vars so tests can point them at an httptest server.
var (
	CodexUsageURL        = "https://chatgpt.com/backend-api/wham/usage"
	ClaudeUsageURL       = "https://api.anthropic.com/api/oauth/usage"
	QoderUsageURL        = "https://openapi.qoder.sh/api/v2/quota/usage"
	AntigravityModelsURL = "https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels"
	AntigravityLoadURL   = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	KiroUsageBase        = "https://codewhisperer.us-east-1.amazonaws.com"
	KiroQUsageBase       = "https://q.us-east-1.amazonaws.com"
)

const (
	cacheTTL       = 60 * time.Second
	claudeCooldown = 3 * time.Minute
	httpTimeout    = 15 * time.Second
)

var (
	// ErrUnsupported is a local misconfig: the provider protocol has no quota API.
	ErrUnsupported = errors.New("usage: unsupported protocol")
	// ErrNoToken is a local misconfig: no API key or OAuth access token to send.
	ErrNoToken = errors.New("usage: no access token")
)

// tokenResolver is the oauth.Service.Resolve seam. Tests inject a stub.
type tokenResolver interface {
	Resolve(ctx context.Context, provider *domain.Provider, force bool) (string, error)
}

type cacheEntry struct {
	report    *Report
	expiresAt time.Time
}

type inflight struct {
	done   chan struct{}
	report *Report
	err    error
}

// Service fetches live upstream account quota for supported providers.
type Service struct {
	oauth  tokenResolver
	logger *slog.Logger
	client *http.Client
	now    func() time.Time

	mu       sync.Mutex
	cache    map[int64]cacheEntry
	inflight map[int64]*inflight
	// cooldownUntil is the Claude 429 bookkeeping. The OAuth usage endpoint
	// rate-limits polling; chat on the same token still works, so we cool down
	// per provider instead of hammering it.
	cooldownUntil map[int64]time.Time
}

// NewService builds a Service. client may be nil (15s timeout). logger may be nil.
func NewService(o *oauth.Service, logger *slog.Logger, client *http.Client) *Service {
	var r tokenResolver
	if o != nil {
		r = o
	}
	return newService(r, logger, client)
}

func newService(o tokenResolver, logger *slog.Logger, client *http.Client) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}
	return &Service{
		oauth:         o,
		logger:        logger.With("component", "usage"),
		client:        client,
		now:           time.Now,
		cache:         map[int64]cacheEntry{},
		inflight:      map[int64]*inflight{},
		cooldownUntil: map[int64]time.Time{},
	}
}

// Supported reports whether the provider protocol has a quota endpoint.
// Archived filtering is the caller's job.
func Supported(p *domain.Provider) bool {
	if p == nil {
		return false
	}
	switch p.Protocol {
	case domain.ProtocolOpenAICodex, domain.ProtocolClaudeCode, domain.ProtocolKiro, domain.ProtocolQoder, domain.ProtocolAntigravity:
		return true
	default:
		return false
	}
}

// Fetch returns a cached or live quota report. Soft upstream failures are a
// Report with Message set, not an error.
func (s *Service) Fetch(ctx context.Context, p *domain.Provider) (*Report, error) {
	return s.FetchWith(ctx, p, FetchOpts{})
}

// FetchWith is Fetch plus cache-bypass control.
func (s *Service) FetchWith(ctx context.Context, p *domain.Provider, opts FetchOpts) (*Report, error) {
	if p == nil || !Supported(p) {
		return nil, ErrUnsupported
	}
	if p.ID == 0 {
		return s.fetchLive(ctx, p)
	}

	if p.Protocol == domain.ProtocolClaudeCode {
		if msg := s.claudeCooldownMessage(p.ID); msg != nil {
			return msg, nil
		}
	}

	if !opts.Force {
		s.mu.Lock()
		if f, ok := s.inflight[p.ID]; ok {
			s.mu.Unlock()
			return waitInflight(ctx, f)
		}
		if e, ok := s.cache[p.ID]; ok && s.now().Before(e.expiresAt) {
			s.mu.Unlock()
			return e.report, nil
		}
		f := &inflight{done: make(chan struct{})}
		s.inflight[p.ID] = f
		s.mu.Unlock()
		return s.finishInflight(ctx, p, f)
	}

	s.mu.Lock()
	if f, ok := s.inflight[p.ID]; ok {
		s.mu.Unlock()
		return waitInflight(ctx, f)
	}
	f := &inflight{done: make(chan struct{})}
	s.inflight[p.ID] = f
	s.mu.Unlock()
	return s.finishInflight(ctx, p, f)
}

func waitInflight(ctx context.Context, f *inflight) (*Report, error) {
	select {
	case <-f.done:
		return f.report, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) finishInflight(ctx context.Context, p *domain.Provider, f *inflight) (*Report, error) {
	defer func() {
		close(f.done)
		s.mu.Lock()
		if cur := s.inflight[p.ID]; cur == f {
			delete(s.inflight, p.ID)
		}
		s.mu.Unlock()
	}()
	report, err := s.fetchLive(ctx, p)
	f.report, f.err = report, err
	if err == nil && report != nil {
		s.mu.Lock()
		s.cache[p.ID] = cacheEntry{report: report, expiresAt: s.now().Add(cacheTTL)}
		s.mu.Unlock()
	}
	return report, err
}

func (s *Service) fetchLive(ctx context.Context, p *domain.Provider) (*Report, error) {
	s.logger.Debug("usage_fetch",
		"event", "usage_fetch",
		"provider_id", p.ID,
		"protocol", p.Protocol,
	)
	switch p.Protocol {
	case domain.ProtocolOpenAICodex:
		return s.fetchCodex(ctx, p)
	case domain.ProtocolClaudeCode:
		return s.fetchClaude(ctx, p)
	case domain.ProtocolKiro:
		return s.fetchKiro(ctx, p)
	case domain.ProtocolQoder:
		return s.fetchQoder(ctx, p)
	case domain.ProtocolAntigravity:
		return s.fetchAntigravity(ctx, p)
	default:
		return nil, ErrUnsupported
	}
}

func (s *Service) claudeCooldownMessage(id int64) *Report {
	s.mu.Lock()
	until, ok := s.cooldownUntil[id]
	s.mu.Unlock()
	if !ok || !s.now().Before(until) {
		return nil
	}
	return &Report{
		Plan:      "Claude Code",
		Message:   "Claude Code connected. Quota API cooling down after rate limit. Chat may still work.",
		FetchedAt: s.now(),
	}
}

func (s *Service) setClaudeCooldown(id int64) {
	if id == 0 {
		return
	}
	s.mu.Lock()
	s.cooldownUntil[id] = s.now().Add(claudeCooldown)
	s.mu.Unlock()
}
