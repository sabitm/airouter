package kirocatalog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/observability"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

func catalogJSON(id string) string {
	return `{"models":[{"modelId":"` + id + `","additionalModelRequestFieldsSchema":{"properties":{"output_config":{"properties":{"effort":{"enum":["low","medium"],"default":"medium"}}}}}}]}`
}

func provider(id int64, token, account, profile, base, region, discovery string) *domain.Provider {
	return &domain.Provider{
		ID: id, Name: "k", BaseURL: base, APIKey: token, Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey,
		OAuthCreds: &domain.OAuthCreds{AccountID: account, ProfileArn: profile, Region: region, KiroDiscovery: discovery, KiroAuth: "idc"},
	}
}

func TestCacheIsolatesAccountsAndConfig(t *testing.T) {
	var hits atomic.Int32
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		id := "shared"
		if strings.Contains(r.Header.Get("Authorization"), "token-b") {
			id = "b"
		}
		if strings.Contains(r.Header.Get("Authorization"), "token-a") {
			id = "a"
		}
		return jsonResponse(http.StatusOK, catalogJSON(id)), nil
	})}})
	ctx := context.Background()
	a := provider(7, "token-a", "acct-a", "arn:a", "https://catalog.example", "us-east-1", "")
	b := provider(7, "token-b", "acct-b", "arn:b", "https://catalog.example", "us-east-1", "")
	left, err := svc.Lookup(ctx, nil, a, "a")
	right, err2 := svc.Lookup(ctx, nil, b, "b")
	if err != nil || err2 != nil || !left.Found || !right.Found || left.Capability == nil || right.Capability == nil || hits.Load() != 2 {
		t.Fatalf("left=%+v right=%+v hits=%d err=%v %v", left, right, hits.Load(), err, err2)
	}
	if _, err := svc.Lookup(ctx, nil, a, "a"); err != nil || hits.Load() != 2 {
		t.Fatalf("cache miss hits=%d err=%v", hits.Load(), err)
	}
	a.BaseURL = "https://other.example"
	if _, err := svc.Models(ctx, nil, a); err != nil || hits.Load() != 3 {
		t.Fatalf("endpoint change hits=%d err=%v", hits.Load(), err)
	}
	a.BaseURL = "https://catalog.example"
	a.OAuthCreds.Region = "eu-central-1"
	if _, err := svc.Models(ctx, nil, a); err != nil || hits.Load() != 4 {
		t.Fatalf("region change hits=%d err=%v", hits.Load(), err)
	}
	a.OAuthCreds.Region = "us-east-1"
	a.OAuthCreds.KiroDiscovery = "management"
	if _, err := svc.Models(ctx, nil, a); err != nil || hits.Load() < 5 {
		t.Fatalf("discovery change hits=%d err=%v", hits.Load(), err)
	}
}

func TestCacheAPIKeyReplacementWithProfileFetches(t *testing.T) {
	var hits atomic.Int32
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return jsonResponse(http.StatusOK, catalogJSON("m")), nil
	})}})
	p := provider(3, "old-key", "acct", "arn:1", "https://catalog.example", "", "")
	if _, err := svc.Models(context.Background(), nil, p); err != nil {
		t.Fatal(err)
	}
	p.APIKey = "new-key"
	if _, err := svc.Models(context.Background(), nil, p); err != nil || hits.Load() != 2 {
		t.Fatalf("api key replacement reused profile cache hits=%d err=%v", hits.Load(), err)
	}
}

func TestCacheTokenRotationKeepsAccount(t *testing.T) {
	var hits atomic.Int32
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return jsonResponse(http.StatusOK, catalogJSON("m")), nil
	})}})
	p := provider(3, "old", "acct", "arn:1", "https://catalog.example", "", "")
	p.AuthMethod = domain.AuthOAuth
	p.OAuthCreds.RefreshToken = "refresh"
	if _, err := svc.Models(context.Background(), nil, p); err != nil {
		t.Fatal(err)
	}
	p.APIKey = "new"
	if _, err := svc.Models(context.Background(), nil, p); err != nil || hits.Load() != 1 {
		t.Fatalf("rotation refetched hits=%d err=%v", hits.Load(), err)
	}
}

func TestCacheAbsentProfileDoesNotMixTokens(t *testing.T) {
	var hits atomic.Int32
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return jsonResponse(http.StatusOK, catalogJSON("m")), nil
	})}})
	a := provider(9, "one", "", "", "https://catalog.example", "", "")
	b := provider(9, "two", "", "", "https://catalog.example", "", "")
	if _, err := svc.Models(context.Background(), nil, a); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Models(context.Background(), nil, b); err != nil || hits.Load() != 2 {
		t.Fatalf("absent profile mixed accounts hits=%d", hits.Load())
	}
}

func TestCacheSingleflightAndCanceledWaiter(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var hits atomic.Int32
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if hits.Add(1) == 1 {
			close(started)
			<-release
		}
		return jsonResponse(http.StatusOK, catalogJSON("m")), nil
	})}})
	p := provider(1, "tok", "acct", "arn", "https://catalog.example", "", "")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = svc.Models(context.Background(), nil, p)
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.Models(ctx, nil, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter err=%v", err)
	}
	close(release)
	wg.Wait()
	if _, err := svc.Models(context.Background(), nil, p); err != nil || hits.Load() != 1 {
		t.Fatalf("canceled waiter poisoned cache hits=%d err=%v", hits.Load(), err)
	}
}

func TestCacheStaleAndEmpty(t *testing.T) {
	var nowMu sync.Mutex
	now := time.Unix(1000, 0)
	setNow := func(v time.Time) {
		nowMu.Lock()
		now = v
		nowMu.Unlock()
	}
	var fail atomic.Bool
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if fail.Load() {
			return nil, errors.New("down")
		}
		return jsonResponse(http.StatusOK, catalogJSON("m")), nil
	})}})
	svc.now = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	p := provider(1, "tok", "acct", "arn", "https://catalog.example", "", "")
	if _, err := svc.Models(context.Background(), nil, p); err != nil {
		t.Fatal(err)
	}
	setNow(now.Add(successTTL + time.Second))
	fail.Store(true)
	models, err := svc.Models(context.Background(), nil, p)
	if err != nil || len(models) != 1 {
		t.Fatalf("stale miss models=%v err=%v", models, err)
	}
	setNow(now.Add(successTTL + staleTTL + 2*time.Second))
	if _, err := svc.Models(context.Background(), nil, p); err == nil {
		t.Fatal("stale window exceeded")
	}
}

func TestCacheInvalidateRejectsOldGeneration(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-release
		return jsonResponse(http.StatusOK, catalogJSON("stale")), nil
	})}})
	p := provider(4, "old", "acct", "arn", "https://catalog.example", "", "")
	done := make(chan error, 1)
	go func() {
		_, err := svc.Models(context.Background(), nil, p)
		done <- err
	}()
	<-started
	svc.Invalidate(p)
	close(release)
	if err := <-done; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("invalidated starter consumed stale result: %v", err)
	}
	var hits atomic.Int32
	svc.SetClient(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return jsonResponse(http.StatusOK, catalogJSON("fresh")), nil
	})}})
	models, err := svc.Models(context.Background(), nil, p)
	if err != nil || hits.Load() != 1 || len(models) != 1 || models[0].ID != "fresh" {
		t.Fatalf("stale generation stored models=%+v hits=%d err=%v", models, hits.Load(), err)
	}
}

func TestCacheCanceledStarterDoesNotAbortWaiter(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var hits atomic.Int32
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if hits.Add(1) == 1 {
			close(entered)
			select {
			case <-r.Context().Done():
				return nil, r.Context().Err()
			case <-release:
				return jsonResponse(http.StatusOK, catalogJSON("kept")), nil
			}
		}
		return jsonResponse(http.StatusOK, catalogJSON("kept")), nil
	})}})
	p := provider(8, "tok", "acct", "arn", "https://catalog.example", "", "")
	starter, stop := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = svc.Models(starter, nil, p)
	}()
	<-started
	<-entered
	stop()
	waiterDone := make(chan error, 1)
	go func() {
		models, err := svc.Models(context.Background(), nil, p)
		if err != nil || len(models) != 1 || models[0].ID != "kept" {
			waiterDone <- fmt.Errorf("waiter lost result models=%v err=%v", models, err)
			return
		}
		waiterDone <- nil
	}()
	select {
	case <-waiterDone:
		t.Fatal("waiter finished before shared work")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-waiterDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter aborted with starter")
	}
	if hits.Load() != 1 {
		t.Fatalf("canceled starter poisoned cache hits=%d", hits.Load())
	}
}

func TestCacheRepeatedInvalidateBoundsWork(t *testing.T) {
	release := make(chan struct{})
	var current atomic.Int32
	var maxSeen atomic.Int32
	var hits atomic.Int32
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Context().Err() == nil {
			n := current.Add(1)
			hits.Add(1)
			for {
				old := maxSeen.Load()
				if n <= old || maxSeen.CompareAndSwap(old, n) {
					break
				}
			}
			defer current.Add(-1)
		}
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-release:
			return jsonResponse(http.StatusOK, catalogJSON("m")), nil
		}
	})}})
	p := provider(11, "tok", "acct", "arn", "https://catalog.example", "", "")
	var wg sync.WaitGroup
	for i := 0; i < maxInFlight*3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.Models(context.Background(), nil, p)
		}()
		time.Sleep(time.Millisecond)
		svc.Invalidate(p)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if maxSeen.Load() > maxInFlight {
		t.Fatalf("actual concurrent catalog work %d exceeded %d hits=%d", maxSeen.Load(), maxInFlight, hits.Load())
	}
}

func TestCacheStaleOnlyForTransient(t *testing.T) {
	var nowMu sync.Mutex
	now := time.Unix(2000, 0)
	var mode atomic.Value
	mode.Store("ok")
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch mode.Load().(string) {
		case "down":
			return nil, errors.New("down")
		case "auth":
			return jsonResponse(http.StatusForbidden, `{"message":"no"}`), nil
		case "shape":
			return jsonResponse(http.StatusOK, `{"models":`), nil
		default:
			return jsonResponse(http.StatusOK, catalogJSON("fresh")), nil
		}
	})}})
	svc.now = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	p := provider(2, "tok", "acct", "arn", "https://catalog.example", "", "")
	if _, err := svc.Models(context.Background(), nil, p); err != nil {
		t.Fatal(err)
	}
	nowMu.Lock()
	now = now.Add(successTTL + time.Second)
	nowMu.Unlock()
	mode.Store("down")
	if models, err := svc.Models(context.Background(), nil, p); err != nil || len(models) != 1 {
		t.Fatalf("transient stale failed models=%v err=%v", models, err)
	}
	mode.Store("auth")
	if _, err := svc.Models(context.Background(), nil, p); !errors.As(err, new(*StatusError)) || AuthStatus(err) != http.StatusForbidden {
		t.Fatalf("403 served stale or wrong err=%v", err)
	}
	mode.Store("shape")
	if _, err := svc.Models(context.Background(), nil, p); !errors.Is(err, ErrShape) {
		t.Fatalf("malformed served stale err=%v", err)
	}
	mode.Store("down")
	if _, err := svc.Models(context.Background(), nil, p); err == nil {
		t.Fatal("authoritative failure revived stale catalog on later transport error")
	}
}

func TestCacheMissingSchemaDiagnostic(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"models":[{"modelId":"plain"}]}`), nil
	})}})
	got, err := svc.Lookup(context.Background(), logger, provider(1, "tok", "acct", "arn", "https://catalog.example", "", ""), "plain")
	if err != nil || !got.Found || got.Capability != nil || !strings.Contains(buf.String(), "schema_missing") {
		t.Fatalf("lookup=%+v err=%v log=%s", got, err, buf.String())
	}
}

func TestExecuteOmitsTransportSecrets(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger(1, &buf)
	secret := "super-secret-token"
	c := &Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed token=" + secret + " query=" + r.URL.RawQuery)
	})}}
	req, err := http.NewRequest(http.MethodPost, "https://user:"+secret+"@catalog.example/path?access_token="+secret, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.execute(context.Background(), logger, req, "kiro_models"); err == nil {
		t.Fatal("expected transport error")
	}
	log := buf.String()
	if strings.Contains(log, secret) || strings.Contains(log, "access_token") || strings.Contains(log, "dial failed") {
		t.Fatalf("secret or raw transport error logged: %s", log)
	}
	if !strings.Contains(log, "reason=transport") || !strings.Contains(log, "url=https://catalog.example/path") {
		t.Fatalf("stable metadata missing: %s", log)
	}
}

func TestDashboardTextOmitsRawTransport(t *testing.T) {
	secret := "refresh-secret"
	text := DashboardText(&TransportError{Err: errors.New("dial " + secret)})
	if strings.Contains(text, secret) || strings.Contains(text, "dial") {
		t.Fatalf("dashboard leaked transport: %s", text)
	}
	refresh := DashboardText(&RefreshError{Err: errors.New("token " + secret)})
	if strings.Contains(refresh, secret) {
		t.Fatalf("dashboard leaked refresh: %s", refresh)
	}
}

func TestCacheLateResultCannotOverwrite(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var oldHits atomic.Int32
	svc := New(&Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.Header.Get("Authorization"), "old") {
			n := oldHits.Add(1)
			if n == 1 {
				close(started)
				<-release
				return jsonResponse(http.StatusOK, catalogJSON("stale")), nil
			}
			return jsonResponse(http.StatusOK, catalogJSON("old")), nil
		}
		return jsonResponse(http.StatusOK, catalogJSON("new")), nil
	})}})
	p := provider(4, "old", "acct", "arn", "https://catalog.example", "", "")
	oldCtx, oldCancel := context.WithCancel(context.Background())
	defer oldCancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.Models(oldCtx, nil, p)
	}()
	<-started
	svc.Invalidate(p)
	fresh := provider(4, "new", "other", "arn", "https://catalog.example", "", "")
	if _, err := svc.Models(context.Background(), nil, fresh); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	models, err := svc.Models(context.Background(), nil, fresh)
	if err != nil || len(models) != 1 || models[0].ID != "new" {
		t.Fatalf("late result changed new identity: %+v err=%v", models, err)
	}
	old := provider(4, "old", "acct", "arn", "https://catalog.example", "", "")
	models, err = svc.Models(context.Background(), nil, old)
	if err != nil || len(models) != 1 || models[0].ID != "old" {
		t.Fatalf("stale result was stored: %+v err=%v", models, err)
	}
}
