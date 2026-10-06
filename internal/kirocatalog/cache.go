package kirocatalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"airouter/internal/domain"
	"airouter/internal/observability"
	"airouter/internal/proxy/kiro"
	"airouter/internal/proxy/thinking"
)

const (
	successTTL  = 300 * time.Second
	emptyTTL    = 30 * time.Second
	staleTTL    = 60 * time.Second
	maxEntries  = 128
	maxInFlight = 32
)

// Service owns bounded, identity-scoped catalog work. Caller cancellation stops
// only that caller's wait. Invalidation cancels work and rejects its result.
type Service struct {
	client   *Client
	now      func() time.Time
	mu       sync.Mutex
	entries  map[string]*entry
	inflight map[string]*flight
	order    []string
	active   int
}

type entry struct {
	models     []Model
	expires    time.Time
	staleUntil time.Time
}

type flight struct {
	done   chan struct{}
	cancel context.CancelFunc
	models []Model
	err    error
	closed bool
	live   bool
}

func New(client *Client) *Service {
	if client == nil {
		client = &Client{}
	}
	return &Service{client: client, now: time.Now, entries: map[string]*entry{}, inflight: map[string]*flight{}}
}

func (s *Service) Client() *Client {
	if s == nil {
		return NewClient(nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (s *Service) SetClient(client *Client) {
	if s == nil {
		return
	}
	if client == nil {
		client = &Client{}
	}
	s.mu.Lock()
	s.client = client
	s.mu.Unlock()
}

func (s *Service) Invalidate(p *domain.Provider) {
	if s == nil {
		return
	}
	key := identityKey(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
	s.detach(key)
}

func (s *Service) detach(key string) {
	if f := s.inflight[key]; f != nil {
		delete(s.inflight, key)
		f.cancel()
		closeFlight(f, nil, ErrUnavailable)
	}
}

func (s *Service) Lookup(ctx context.Context, logger *slog.Logger, p *domain.Provider, model string) (LookupResult, error) {
	return s.LookupWithRefresh(ctx, logger, p, model, nil)
}

// LookupWithRefresh omits controls on discovery failure. The shared flight owns
// one deadline across pagination, fallback, and one optional auth refresh.
func (s *Service) LookupWithRefresh(ctx context.Context, logger *slog.Logger, p *domain.Provider, model string, refresh func(context.Context) (string, error)) (LookupResult, error) {
	if ctx.Err() != nil {
		return LookupResult{}, ctx.Err()
	}
	base := thinking.StripSuffix(strings.TrimSpace(model))
	if base == "auto" || base == "" {
		return LookupResult{Reason: "model_unset"}, nil
	}
	models, err := s.ModelsWithRefresh(ctx, logger, p, refresh)
	if err != nil {
		if ctx.Err() != nil {
			return LookupResult{}, ctx.Err()
		}
		logUnavailable(ctx, logger, "unavailable")
		return LookupResult{Reason: "unavailable"}, nil
	}
	for _, model := range models {
		if model.ID == base {
			if model.Capability == nil {
				logUnavailable(ctx, logger, "schema_missing")
			}
			return LookupResult{ModelID: model.ID, Capability: model.Capability.Clone(), Found: true}, nil
		}
	}
	logUnavailable(ctx, logger, "unsupported")
	return LookupResult{ModelID: base, Reason: "unsupported"}, nil
}

type LookupResult struct {
	ModelID    string
	Capability *kiro.Capability
	Found      bool
	Reason     string
}

func (s *Service) Models(ctx context.Context, logger *slog.Logger, p *domain.Provider) ([]Model, error) {
	return s.ModelsWithRefresh(ctx, logger, p, nil)
}

func (s *Service) ModelsWithRefresh(ctx context.Context, logger *slog.Logger, p *domain.Provider, refresh func(context.Context) (string, error)) ([]Model, error) {
	return s.models(ctx, logger, p, refresh, false)
}

// ModelsChecked never serves cached or stale success. It supersedes older
// discovery work, but concurrent checks can share the new authoritative fetch.
func (s *Service) ModelsChecked(ctx context.Context, logger *slog.Logger, p *domain.Provider, refresh func(context.Context) (string, error)) ([]Model, error) {
	return s.models(ctx, logger, p, refresh, true)
}

func (s *Service) models(ctx context.Context, logger *slog.Logger, p *domain.Provider, refresh func(context.Context) (string, error), live bool) ([]Model, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if s == nil {
		return nil, ErrUnavailable
	}
	key := identityKey(p)
	s.mu.Lock()
	if ent := s.entries[key]; !live && s.inflight[key] == nil && ent != nil && s.now().Before(ent.expires) {
		models := cloneModels(ent.models)
		s.mu.Unlock()
		return models, nil
	}
	if f := s.inflight[key]; f != nil && (!live || f.live) {
		s.mu.Unlock()
		return s.waitFlight(ctx, key, f, live)
	}
	if live {
		delete(s.entries, key)
		s.detach(key)
	}
	// Superseded workers retain their slot until they actually return. This
	// bounds both goroutines and HTTP work even if a transport delays cancel.
	if s.active >= maxInFlight {
		s.mu.Unlock()
		return nil, ErrUnavailable
	}
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), Deadline)
	f := &flight{done: make(chan struct{}), cancel: cancel, live: live}
	s.inflight[key] = f
	s.active++
	client := s.client
	probe := cloneProvider(p)
	s.mu.Unlock()
	go func() {
		defer cancel()
		models, err := client.ListWithRefresh(fetchCtx, logger, probe, refresh)
		if fetchCtx.Err() != nil {
			err = fetchCtx.Err()
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.active--
		if s.inflight[key] == f {
			delete(s.inflight, key)
			if ent := s.entries[key]; !live && err != nil && !transientCatalog(err) && ent != nil {
				ent.staleUntil = s.now()
			}
			if err == nil {
				s.store(key, models)
			}
			closeFlight(f, models, err)
		}
	}()
	return s.waitFlight(ctx, key, f, live)
}

// All writes happen before done closes. Invalidated flights are never written
// again, so waiters can read their immutable outcome without the service lock.
func closeFlight(f *flight, models []Model, err error) {
	if f.closed {
		return
	}
	f.models, f.err, f.closed = cloneModels(models), err, true
	close(f.done)
}

func (s *Service) waitFlight(ctx context.Context, key string, f *flight, live bool) ([]Model, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if f.err != nil {
			if !live {
				if models, ok := s.stale(key, f.err); ok {
					return models, nil
				}
			}
			return nil, f.err
		}
		return cloneModels(f.models), nil
	}
}

func (s *Service) stale(key string, err error) ([]Model, bool) {
	if !transientCatalog(err) {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ent := s.entries[key]
	if ent == nil || !s.now().Before(ent.staleUntil) {
		return nil, false
	}
	return cloneModels(ent.models), true
}

func transientCatalog(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var status *StatusError
	if errors.As(err, &status) {
		return status.Status == http.StatusNotFound || status.Status >= 500
	}
	var transport *TransportError
	return errors.As(err, &transport)
}

func (s *Service) store(key string, models []Model) {
	now := s.now()
	ttl := successTTL
	if len(models) == 0 {
		ttl = emptyTTL
	}
	if _, ok := s.entries[key]; !ok {
		s.order = append(s.order, key)
	}
	s.entries[key] = &entry{models: cloneModels(models), expires: now.Add(ttl), staleUntil: now.Add(ttl + staleTTL)}
	for len(s.entries) > maxEntries && len(s.order) > 0 {
		old := s.order[0]
		s.order = s.order[1:]
		delete(s.entries, old)
	}
	if len(s.order) > maxEntries*2 {
		seen := map[string]bool{}
		next := make([]string, 0, len(s.entries))
		for _, key := range s.order {
			if s.entries[key] != nil && !seen[key] {
				seen[key] = true
				next = append(next, key)
			}
		}
		s.order = next
	}
}

func cloneModels(in []Model) []Model {
	if in == nil {
		return nil
	}
	out := make([]Model, len(in))
	for i := range in {
		out[i] = Model{ID: in[i].ID, Capability: in[i].Capability.Clone()}
	}
	return out
}

func logUnavailable(ctx context.Context, logger *slog.Logger, reason string) {
	observability.Logger(ctx, logger).Debug("kiro_capability_unavailable", "event", "kiro_capability_unavailable", "reason", reason)
}

// Credentials are fingerprinted in memory only. Account or profile labels
// alone never justify sharing a catalog between different credentials.
func identityKey(p *domain.Provider) string {
	id := kiro.IdentityFromProvider(p)
	base := ""
	providerID := int64(0)
	account, refresh := "", ""
	if p != nil {
		base, providerID = strings.TrimSpace(p.BaseURL), p.ID
		if p.OAuthCreds != nil {
			account, refresh = p.OAuthCreds.AccountID, p.OAuthCreds.RefreshToken
		}
	}
	credential := id.Token
	if id.Method == domain.AuthOAuth && refresh != "" {
		credential = refresh
	}
	parts := []string{strconv.FormatInt(providerID, 10), string(id.Method), string(id.Auth), id.KiroAuth, id.IDP, id.ProfileArn, kiro.Region(id), id.Discovery, base, account, fingerprint(credential)}
	if kiro.UseManagementDiscovery(id) {
		parts = append(parts, kiro.ManagementURL(kiro.Region(id)))
	}
	return strings.Join(parts, "\x1f")
}

func fingerprint(secret string) string {
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func cloneProvider(p *domain.Provider) *domain.Provider {
	if p == nil {
		return nil
	}
	out := *p
	if p.OAuthCreds != nil {
		creds := *p.OAuthCreds
		out.OAuthCreds = &creds
	}
	out.Tags = append([]string(nil), p.Tags...)
	return &out
}
