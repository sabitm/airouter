package kirocatalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/observability"
)

func TestOAuthTokenOnlyIdentityIsolation(t *testing.T) {
	var hits atomic.Int32
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return jsonResponse(http.StatusOK, catalogJSON("m")), nil
	})}})
	p := provider(1, "one", "", "", "https://catalog.example", "", "")
	p.AuthMethod = domain.AuthOAuth
	for _, token := range []string{"one", "two"} {
		p.APIKey = token
		if _, err := svc.Models(context.Background(), nil, p); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("token-only identities shared catalog: hits=%d", hits.Load())
	}
}

func TestLookupRefreshUsesOneDeadlineAndRequestID(t *testing.T) {
	var first time.Time
	var hits atomic.Int32
	var refreshes atomic.Int32
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > Deadline {
			t.Error("missing bounded catalog deadline")
		}
		if observability.RequestID(r.Context()) != "catalog-request" {
			t.Error("catalog lost request ID")
		}
		if hits.Add(1) == 1 {
			first = deadline
			return jsonResponse(http.StatusUnauthorized, `{}`), nil
		}
		if !deadline.Equal(first) {
			t.Error("retry reset catalog deadline")
		}
		if r.Header.Get("Authorization") != "Bearer fresh" {
			t.Error("retry did not use refreshed token")
		}
		return jsonResponse(http.StatusOK, catalogJSON("m")), nil
	})}})
	ctx := observability.WithRequestID(context.Background(), "catalog-request")
	p := provider(1, "old", "", "", "https://catalog.example", "", "")
	result, err := svc.LookupWithRefresh(ctx, nil, p, "m", func(ctx context.Context) (string, error) {
		refreshes.Add(1)
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.Equal(first) {
			t.Error("refresh lacks original catalog deadline")
		}
		return "fresh", nil
	})
	if err != nil || !result.Found || hits.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("result=%+v err=%v hits=%d refresh=%d", result, err, hits.Load(), refreshes.Load())
	}
}

func TestWorkAdmissionIncludesSupersededFlights(t *testing.T) {
	entered := make(chan struct{}, maxInFlight)
	release := make(chan struct{})
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		<-release
		return jsonResponse(http.StatusOK, catalogJSON("m")), nil
	})}})
	done := make(chan error, maxInFlight)
	for i := 0; i < maxInFlight; i++ {
		p := provider(int64(i+1), fmt.Sprintf("key%d", i), "", "", "https://catalog.example", "", "")
		go func() { _, err := svc.Models(context.Background(), nil, p); done <- err }()
	}
	for i := 0; i < maxInFlight; i++ {
		<-entered
	}
	p := provider(1, "key0", "", "", "https://catalog.example", "", "")
	svc.Invalidate(p)
	if err := <-done; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("invalidated flight returned result: %v", err)
	}
	for _, live := range []bool{false, true} {
		other := provider(1000, "extra", "", "", "https://catalog.example", "", "")
		_, err := svc.models(context.Background(), nil, other, nil, live)
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("admitted work beyond limit: live=%v err=%v", live, err)
		}
	}
	svc.mu.Lock()
	if svc.active != maxInFlight || len(svc.inflight) >= maxInFlight {
		t.Errorf("invalid work accounting: active=%d flights=%d", svc.active, len(svc.inflight))
	}
	svc.mu.Unlock()
	close(release)
	for i := 1; i < maxInFlight; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestCheckSupersedesDiscoveryAndNeverUsesStale(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var hits atomic.Int32
	var fail atomic.Bool
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if fail.Load() {
			return jsonResponse(http.StatusBadGateway, `{}`), nil
		}
		if hits.Add(1) == 1 {
			close(entered)
			<-release
			return jsonResponse(http.StatusOK, catalogJSON("old")), nil
		}
		return jsonResponse(http.StatusOK, catalogJSON("new")), nil
	})}})
	p := provider(1, "key", "", "", "https://catalog.example", "", "")
	done := make(chan error, 1)
	go func() { _, err := svc.Models(context.Background(), nil, p); done <- err }()
	<-entered
	models, err := svc.ModelsChecked(context.Background(), nil, p, nil)
	if err != nil || len(models) != 1 || models[0].ID != "new" {
		t.Fatalf("check models=%v err=%v", models, err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("superseded discovery returned stale success: %v", err)
	}
	models, err = svc.Models(context.Background(), nil, p)
	if err != nil || models[0].ID != "new" || hits.Load() != 2 {
		t.Fatalf("late discovery replaced Check: models=%v err=%v", models, err)
	}
	fail.Store(true)
	if _, err := svc.ModelsChecked(context.Background(), nil, p, nil); err == nil {
		t.Fatal("failed live Check used cached success")
	}
}

func TestSpecialModelsUseExactCatalogLookup(t *testing.T) {
	var hits atomic.Int32
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return jsonResponse(http.StatusOK, `{"models":[{"modelId":"AUTO"},{"modelId":"simple-task","additionalModelRequestFieldsSchema":{"properties":{"reasoning":{"properties":{"effort":{"enum":["low","high"],"default":"low"}}}}}}]}`), nil
	})}})
	p := provider(1, "key", "", "", "https://catalog.example", "", "")
	if result, err := svc.Lookup(context.Background(), nil, p, "auto"); err != nil || result.Found || hits.Load() != 0 {
		t.Fatalf("auto fetched catalog: result=%+v hits=%d err=%v", result, hits.Load(), err)
	}
	for _, model := range []string{"AUTO", "simple-task(high)"} {
		result, err := svc.Lookup(context.Background(), nil, p, model)
		if err != nil || !result.Found {
			t.Fatalf("ordinary exact model omitted: model=%s result=%+v err=%v", model, result, err)
		}
		if model != "AUTO" && (result.Capability == nil || result.Capability.EffortPath != "reasoning") {
			t.Fatal("simple-task did not use catalog schema")
		}
	}
}

func TestCatalogLongParentDeadlineIsCapped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client := &Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > Deadline {
			t.Errorf("catalog accepted long parent deadline: %s", time.Until(deadline))
		}
		return jsonResponse(http.StatusOK, catalogJSON("m")), nil
	})}}
	if _, err := client.ListWithRefresh(ctx, nil, provider(1, "key", "", "", "https://catalog.example", "", ""), nil); err != nil {
		t.Fatal(err)
	}
}

func TestCancellationWinsAtCaptureBoundary(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
		for _, size := range []int{captureMax, captureMax + 1} {
			for _, failure := range []error{context.Canceled, context.DeadlineExceeded} {
				client := &Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(&errAfterReader{data: []byte(strings.Repeat("x", size)), err: failure})}, nil
				})}}
				_, err := client.ListWithRefresh(context.Background(), nil, provider(1, "key", "", "", "https://catalog.example", "", ""), func(context.Context) (string, error) {
					t.Error("canceled catalog refreshed")
					return "new", nil
				})
				if !errors.Is(err, failure) {
					t.Errorf("status=%d size=%d cancellation hidden: %v", status, size, err)
				}
			}
		}
	}
}
