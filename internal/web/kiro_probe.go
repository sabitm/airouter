package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/kiro"
)

const (
	kiroCatalogTarget     = "AmazonCodeWhispererService.ListAvailableModels"
	kiroCatalogOrigin     = "AI_EDITOR"
	kiroCatalogMaxPages   = 10
	kiroCatalogDeadline   = 15 * time.Second
	kiroCatalogJSONType   = "application/x-amz-json-1.0"
	kiroCatalogJSONAccept = "application/json"
)

var (
	errKiroInvalidBaseURL     = errors.New("invalid Kiro base URL")
	errKiroCatalogTruncated   = errors.New("Kiro catalog response truncated")
	errKiroCatalogShape       = errors.New("Kiro catalog response shape unexpected")
	errKiroCatalogEmpty       = errors.New("Kiro catalog is empty")
	errKiroCatalogPageLimit   = errors.New("Kiro catalog page limit exceeded")
	errKiroCatalogTokenRepeat = errors.New("Kiro catalog nextToken repeated")
)

// kiroCatalogClient has no client timeout. The shared catalog context is the
// deadline for every page together.
var kiroCatalogClient = &http.Client{}

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

// One deadline covers all pages, the refresh wait, and the retry. Restart the
// catalog after refresh so a result never mixes pages from different tokens.
func queryKiroModelsWithRefresh(ctx context.Context, logger *slog.Logger, p *domain.Provider, refresh func(context.Context) (string, error)) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, kiroCatalogDeadline)
	defer cancel()

	models, err := queryKiroModels(ctx, logger, p)
	if refresh == nil || ctx.Err() != nil || kiroCatalogAuthStatus(err) == 0 {
		return models, err
	}
	tok, rerr := refresh(ctx)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if rerr != nil {
		return nil, &kiroCatalogRefreshError{Err: rerr}
	}
	if tok == "" {
		return nil, &kiroCatalogRefreshError{Err: errors.New("empty access token")}
	}
	retry := *p
	retry.APIKey = tok
	return queryKiroModels(ctx, logger, &retry)
}

func kiroCatalogAuthStatus(err error) int {
	var status *kiroCatalogStatusError
	if errors.As(err, &status) && (status.Status == http.StatusUnauthorized || status.Status == http.StatusForbidden) {
		return status.Status
	}
	return 0
}

func kiroRefreshErrorText(err error) string {
	if errors.Is(err, oauth.ErrInvalidGrant) {
		return "token expired - reconnect required"
	}
	return "token refresh failed: " + err.Error()
}

func kiroCheckErrorText(err error) string {
	switch {
	case errors.Is(err, errKiroInvalidBaseURL):
		return "invalid base URL"
	case errors.Is(err, errKiroCatalogTruncated):
		return "catalog response truncated"
	case errors.Is(err, errKiroCatalogShape):
		return "catalog response shape unexpected"
	case errors.Is(err, errKiroCatalogEmpty):
		return "catalog is empty"
	case errors.Is(err, errKiroCatalogPageLimit):
		return "catalog page limit exceeded"
	case errors.Is(err, errKiroCatalogTokenRepeat):
		return "catalog pagination repeated nextToken"
	case errors.Is(err, context.DeadlineExceeded):
		return "catalog request timed out"
	case errors.Is(err, context.Canceled):
		return "catalog request canceled"
	}
	var refresh *kiroCatalogRefreshError
	if errors.As(err, &refresh) {
		return kiroRefreshErrorText(refresh.Err)
	}
	var status *kiroCatalogStatusError
	if errors.As(err, &status) {
		switch status.Status {
		case http.StatusUnauthorized:
			return "credential rejected (HTTP 401)"
		case http.StatusForbidden:
			return "access denied (HTTP 403)"
		case http.StatusNotFound:
			return "not found (HTTP 404) - check base URL"
		default:
			if status.Status >= 500 {
				return fmt.Sprintf("upstream unavailable (HTTP %d)", status.Status)
			}
			return fmt.Sprintf("upstream returned HTTP %d", status.Status)
		}
	}
	return "could not reach Kiro: " + err.Error()
}

// queryKiroModels loads the live CodeWhisperer catalog from the configured
// provider base URL. Check and autocomplete share this query so request shape
// and parsing stay equal. It does not mutate p.
func queryKiroModels(ctx context.Context, logger *slog.Logger, p *domain.Provider) ([]string, error) {
	if p == nil || strings.TrimSpace(p.BaseURL) == "" {
		return nil, errKiroInvalidBaseURL
	}
	ctx, cancel := context.WithTimeout(ctx, kiroCatalogDeadline)
	defer cancel()

	var models []string
	seen := map[string]struct{}{}
	seenTokens := map[string]struct{}{}
	nextToken := ""
	for page := 1; page <= kiroCatalogMaxPages; page++ {
		body, err := kiroCatalogBody(p, nextToken)
		if err != nil {
			return nil, err
		}
		req, err := kiroCatalogRequest(ctx, p, body)
		if err != nil {
			return nil, err
		}
		pr, err := executeProbe(ctx, logger, kiroCatalogClient, req, "kiro_models")
		if err != nil {
			return nil, err
		}
		if pr.Truncated {
			return nil, errKiroCatalogTruncated
		}
		if pr.StatusCode < 200 || pr.StatusCode >= 300 {
			return nil, &kiroCatalogStatusError{Status: pr.StatusCode}
		}
		pageModels, token, err := parseKiroCatalog(pr.Body)
		if err != nil {
			return nil, err
		}
		for _, id := range pageModels {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			models = append(models, id)
		}
		if token == "" {
			if len(models) == 0 {
				return nil, errKiroCatalogEmpty
			}
			return models, nil
		}
		if _, ok := seenTokens[token]; ok {
			return nil, errKiroCatalogTokenRepeat
		}
		seenTokens[token] = struct{}{}
		nextToken = token
	}
	return nil, errKiroCatalogPageLimit
}

func kiroCatalogBody(p *domain.Provider, nextToken string) ([]byte, error) {
	body := map[string]string{"origin": kiroCatalogOrigin}
	if arn := kiroCatalogProfileArn(p); arn != "" {
		body["profileArn"] = arn
	}
	if nextToken != "" {
		body["nextToken"] = nextToken
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func kiroCatalogRequest(ctx context.Context, p *domain.Provider, body []byte) (*http.Request, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/") + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errKiroInvalidBaseURL
	}
	req.Header.Set("Content-Type", kiroCatalogJSONType)
	req.Header.Set("Accept", kiroCatalogJSONAccept)
	req.Header.Set("X-Amz-Target", kiroCatalogTarget)
	req.Header.Set("User-Agent", kiro.UserAgent)
	req.Header.Set("X-Amz-User-Agent", kiro.XAmzUserAgent)
	req.Header.Set("Amz-Sdk-Request", kiro.AmzSdkRequest)
	req.Header.Set("Amz-Sdk-Invocation-Id", newCheckUUID())
	switch p.Auth() {
	case domain.AuthXAPIKey:
		req.Header.Set("x-api-key", p.APIKey)
	default:
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	if p.Method() == domain.AuthAPIKey {
		req.Header.Set("tokentype", "API_KEY")
	} else if p.OAuthCreds != nil && strings.EqualFold(strings.TrimSpace(p.OAuthCreds.KiroAuth), "external_idp") {
		req.Header.Set("TokenType", "EXTERNAL_IDP")
	}
	return req, nil
}

// kiroCatalogProfileArn keeps a configured profile for non-Builder-ID auth.
// Builder ID does not support profile lookup, so its ARN is omitted rather
// than replaced with a placeholder.
func kiroCatalogProfileArn(p *domain.Provider) string {
	if p == nil || p.OAuthCreds == nil || kiroBuilderID(p.OAuthCreds.KiroAuth) {
		return ""
	}
	return strings.TrimSpace(p.OAuthCreds.ProfileArn)
}

func kiroBuilderID(auth string) bool {
	switch strings.ToLower(strings.TrimSpace(auth)) {
	case "builder-id", "builder_id", "builderid", "builder id":
		return true
	default:
		return false
	}
}

func parseKiroCatalog(body []byte) ([]string, string, error) {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(body)))
	dec.UseNumber()
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, "", errKiroCatalogShape
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, "", errKiroCatalogShape
	}
	modelsRaw, ok := raw["models"]
	if !ok || strings.TrimSpace(string(modelsRaw)) == "null" {
		return nil, "", errKiroCatalogShape
	}
	var models []map[string]any
	if err := json.Unmarshal(modelsRaw, &models); err != nil || models == nil {
		return nil, "", errKiroCatalogShape
	}
	out := make([]string, 0, len(models))
	for _, model := range models {
		rawID, ok := model["modelId"]
		if !ok || rawID == nil {
			continue
		}
		id, ok := rawID.(string)
		if !ok {
			return nil, "", errKiroCatalogShape
		}
		if strings.TrimSpace(id) == "" {
			continue
		}
		out = append(out, id)
	}
	token := ""
	if tokenRaw, ok := raw["nextToken"]; ok && strings.TrimSpace(string(tokenRaw)) != "null" {
		if err := json.Unmarshal(tokenRaw, &token); err != nil {
			return nil, "", errKiroCatalogShape
		}
	}
	return out, token, nil
}

type kiroCatalogRefreshError struct {
	Err error
}

func (e *kiroCatalogRefreshError) Error() string {
	return "Kiro catalog token refresh failed"
}

func (e *kiroCatalogRefreshError) Unwrap() error {
	return e.Err
}

type kiroCatalogStatusError struct {
	Status int
}

func (e *kiroCatalogStatusError) Error() string {
	return fmt.Sprintf("HTTP %d", e.Status)
}

func newCheckUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:16])
}
