package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/kirocatalog"
)

func TestOAuthDashboardDiscoveryUsesFreshSharedCache(t *testing.T) {
	var hits atomic.Int32
	catalog := kirocatalog.New(&kirocatalog.Client{HTTP: &http.Client{Transport: kiroRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"m"}]}`), nil
	})}})
	h := &Handler{kiroCatalog: catalog, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	provider := &domain.Provider{ID: 1, BaseURL: "https://catalog.example", APIKey: "token", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{RefreshToken: "refresh"}}
	if _, err := catalog.Models(context.Background(), nil, provider); err != nil {
		t.Fatal(err)
	}
	models, err := h.queryKiroModelsWithRefresh(context.Background(), provider, func(context.Context) (string, error) {
		t.Error("fresh cached discovery refreshed")
		return "fresh", nil
	})
	if err != nil || strings.Join(models, ",") != "m" || hits.Load() != 1 {
		t.Fatalf("models=%v hits=%d err=%v", models, hits.Load(), err)
	}
	if ok, text := h.checkKiroUpstream(context.Background(), provider, nil); !ok || hits.Load() != 2 {
		t.Fatalf("live Check used cache: ok=%v text=%s hits=%d", ok, text, hits.Load())
	}
}

func TestDashboardCheckRejectsLateDiscoveryResult(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var hits atomic.Int32
	catalog := kirocatalog.New(&kirocatalog.Client{HTTP: &http.Client{Transport: kiroRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		model := "new"
		if hits.Add(1) == 1 {
			close(entered)
			<-release
			model = "old"
		}
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"`+model+`"}]}`), nil
	})}})
	h := &Handler{kiroCatalog: catalog, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	provider := &domain.Provider{ID: 1, BaseURL: "https://catalog.example", APIKey: "key", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey}
	done := make(chan error, 1)
	go func() { _, err := h.queryKiroModelsWithRefresh(context.Background(), provider, nil); done <- err }()
	<-entered
	if ok, text := h.checkKiroUpstream(context.Background(), provider, nil); !ok {
		t.Fatal(text)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("superseded discovery returned success")
	}
	models, err := h.queryKiroModelsWithRefresh(context.Background(), provider, nil)
	if err != nil || strings.Join(models, ",") != "new" || hits.Load() != 2 {
		t.Fatalf("late discovery replaced Check: models=%v hits=%d err=%v", models, hits.Load(), err)
	}
}
