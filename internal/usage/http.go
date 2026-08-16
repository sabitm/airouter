package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
)

const maxBody = 1 << 20

type httpResult struct {
	Status int
	Body   []byte
}

func (s *Service) resolveToken(ctx context.Context, p *domain.Provider, force bool) (string, error) {
	if p.Method() == domain.AuthAPIKey {
		tok := strings.TrimSpace(p.APIKey)
		if tok == "" {
			return "", ErrNoToken
		}
		return tok, nil
	}
	if s.oauth == nil {
		if p.OAuthCreds != nil && strings.TrimSpace(p.OAuthCreds.AccessToken) != "" {
			return p.OAuthCreds.AccessToken, nil
		}
		return "", ErrNoToken
	}
	tok, err := s.oauth.Resolve(ctx, p, force)
	if err != nil && oauth.IsInvalidGrant(err) && strings.TrimSpace(tok) == "" {
		return "", err
	}
	if strings.TrimSpace(tok) == "" {
		if p.OAuthCreds != nil {
			tok = p.OAuthCreds.AccessToken
		}
	}
	if strings.TrimSpace(tok) == "" {
		if err != nil {
			return "", err
		}
		return "", ErrNoToken
	}
	return tok, nil
}

func (s *Service) doJSON(ctx context.Context, method, rawURL, token string, headers map[string]string, body any) (httpResult, error) {
	// Logs stay metadata-only: host, status, duration. Never tokens, auth headers, or bodies.
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return httpResult{}, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return httpResult{}, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := s.client.Do(req)
	host := requestHost(rawURL)
	if err != nil {
		s.logger.Debug("usage_transport_failed",
			"event", "usage_transport_failed",
			"method", method,
			"host", host,
			"error", err,
			"duration_ms", time.Since(start).Milliseconds(),
		)
		return httpResult{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	s.logger.Debug("usage_response",
		"event", "usage_response",
		"method", method,
		"host", host,
		"status", resp.StatusCode,
		"duration_ms", time.Since(start).Milliseconds(),
		"size", len(data),
	)
	return httpResult{Status: resp.StatusCode, Body: data}, nil
}

// doAuthed sends req and, for OAuth providers, force-refreshes once on 401/403.
func (s *Service) doAuthed(ctx context.Context, p *domain.Provider, method, rawURL string, extra map[string]string, body any) (httpResult, string, error) {
	token, err := s.resolveToken(ctx, p, false)
	if err != nil {
		return httpResult{}, "", err
	}
	res, err := s.doJSON(ctx, method, rawURL, token, extra, body)
	if err != nil {
		return res, token, err
	}
	if (res.Status == http.StatusUnauthorized || res.Status == http.StatusForbidden) && p.Method() == domain.AuthOAuth {
		refreshed, rerr := s.resolveToken(ctx, p, true)
		if rerr == nil && refreshed != "" && refreshed != token {
			token = refreshed
			res, err = s.doJSON(ctx, method, rawURL, token, extra, body)
		}
	}
	return res, token, err
}

func requestHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}

func soft(plan, msg string) *Report {
	return &Report{Plan: plan, Message: msg, FetchedAt: time.Now()}
}

func softf(plan, format string, args ...any) *Report {
	return soft(plan, fmt.Sprintf(format, args...))
}

func isLocalErr(err error) bool {
	return errors.Is(err, ErrNoToken) || errors.Is(err, ErrUnsupported) || errors.Is(err, oauth.ErrInvalidGrant)
}

func decodeMap(body []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return m, nil
}
