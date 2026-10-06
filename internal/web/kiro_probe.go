package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"airouter/internal/domain"
	"airouter/internal/kirocatalog"
	"airouter/internal/oauth"
	"airouter/internal/proxy/kiro"
)

const (
	kiroCatalogTarget     = "AmazonCodeWhispererService.ListAvailableModels"
	kiroManagementTarget  = "KiroControlPlaneBearerService.ListAvailableModels"
	kiroCatalogOrigin     = "AI_EDITOR"
	kiroCatalogMaxPages   = 10
	kiroCatalogDeadline   = kirocatalog.Deadline
	kiroCatalogJSONType   = "application/x-amz-json-1.0"
	kiroCatalogJSONAccept = "application/json"
)

var (
	errKiroInvalidBaseURL     = kirocatalog.ErrInvalidBaseURL
	errKiroCatalogTruncated   = kirocatalog.ErrTruncated
	errKiroCatalogShape       = kirocatalog.ErrShape
	errKiroCatalogEmpty       = kirocatalog.ErrEmpty
	errKiroCatalogPageLimit   = kirocatalog.ErrPageLimit
	errKiroCatalogTokenRepeat = kirocatalog.ErrTokenRepeat
)

// kiroCatalogClient is the package fetch client used by tests that call the
// package wrappers directly. A nil HTTP client reads the replaceable default.
var kiroCatalogClient = &kirocatalog.Client{}

// checkKiroUpstream confirms catalog access for both API-key and OAuth Kiro
// providers. Success requires at least one usable model ID. It confirms catalog
// access only, not chat readiness, quota, or capacity. refresh may retry a
// saved OAuth credential once; nil disables refresh.
func checkKiroUpstream(ctx context.Context, logger *slog.Logger, p *domain.Provider, refresh func(context.Context) (string, error)) (bool, string) {
	models, err := queryKiroModelsWithRefresh(ctx, logger, p, refresh)
	if err != nil {
		return false, kiroCheckErrorText(err)
	}
	return true, fmt.Sprintf("OK - catalog access confirmed (%d models). This does not confirm chat readiness, quota, or capacity.", len(models))
}

func (h *Handler) catalogClient() *kirocatalog.Client {
	if h != nil && h.kiroCatalog != nil {
		return h.kiroCatalog.Client()
	}
	return kiroCatalogClient
}

func (h *Handler) queryKiroModelsWithRefresh(ctx context.Context, p *domain.Provider, refresh func(context.Context) (string, error)) ([]string, error) {
	logger := (*slog.Logger)(nil)
	if h != nil {
		logger = h.logger
	}
	if h != nil && h.kiroCatalog != nil {
		models, err := h.kiroCatalog.ModelsWithRefresh(ctx, logger, p, refresh)
		if err != nil {
			return nil, err
		}
		return modelIDs(models), nil
	}
	models, err := h.catalogClient().ListWithRefresh(ctx, logger, p, refresh)
	if err != nil {
		return nil, err
	}
	return modelIDs(models), nil
}

func queryKiroModelsWithRefresh(ctx context.Context, logger *slog.Logger, p *domain.Provider, refresh func(context.Context) (string, error)) ([]string, error) {
	models, err := kiroCatalogClient.ListWithRefresh(ctx, logger, p, refresh)
	if err != nil {
		return nil, err
	}
	return modelIDs(models), nil
}

func (h *Handler) checkKiroUpstream(ctx context.Context, p *domain.Provider, refresh func(context.Context) (string, error)) (bool, string) {
	// Check bypasses cached and stale success, but shares bounded service work.
	logger := (*slog.Logger)(nil)
	if h != nil {
		logger = h.logger
	}
	var models []kirocatalog.Model
	var err error
	if h != nil && h.kiroCatalog != nil {
		models, err = h.kiroCatalog.ModelsChecked(ctx, logger, p, refresh)
	} else {
		models, err = h.catalogClient().ListWithRefresh(ctx, logger, p, refresh)
	}
	if err != nil {
		return false, kiroCheckErrorText(err)
	}
	return true, fmt.Sprintf("OK - catalog access confirmed (%d models). This does not confirm chat readiness, quota, or capacity.", len(models))
}

func queryKiroModels(ctx context.Context, logger *slog.Logger, p *domain.Provider) ([]string, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, kiroCatalogDeadline)
	defer cancel()
	models, err := kiroCatalogClient.List(ctx, logger, p)
	if err != nil {
		return nil, err
	}
	return modelIDs(models), nil
}

func queryKiroManagementModels(ctx context.Context, logger *slog.Logger, p *domain.Provider, id kiro.Identity) ([]string, error) {
	if p == nil {
		return nil, errKiroInvalidBaseURL
	}
	probe := *p
	if probe.OAuthCreds == nil {
		probe.OAuthCreds = &domain.OAuthCreds{}
	} else {
		copied := *probe.OAuthCreds
		probe.OAuthCreds = &copied
	}
	probe.OAuthCreds.KiroDiscovery = kiro.DiscoveryManagement
	probe.OAuthCreds.ProfileArn = id.ProfileArn
	probe.OAuthCreds.Region = id.Region
	probe.OAuthCreds.KiroAuth = id.KiroAuth
	probe.OAuthCreds.KiroIDP = id.IDP
	probe.APIKey = id.Token
	return queryKiroModels(ctx, logger, &probe)
}

func modelIDs(models []kirocatalog.Model) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, model.ID)
	}
	return out
}

func kiroCatalogAuthStatus(err error) int {
	return kirocatalog.AuthStatus(err)
}

func kiroRefreshErrorText(err error) string {
	if errors.Is(err, oauth.ErrInvalidGrant) {
		return "token expired - reconnect required"
	}
	return "token refresh failed"
}

func kiroCheckErrorText(err error) string {
	return kirocatalog.DashboardText(err)
}

type kiroCatalogRefreshError = kirocatalog.RefreshError
type kiroCatalogStatusError = kirocatalog.StatusError

func newCheckUUID() string {
	return kirocatalog.NewInvocationID()
}
