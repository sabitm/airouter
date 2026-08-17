package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
)

func TestCursorPKCE(t *testing.T) {
	v, c, err := cursorPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 43 || len(c) != 43 {
		t.Fatalf("verifier/challenge lens %d/%d", len(v), len(c))
	}
	if strings.ContainsAny(v, "+/=") || strings.ContainsAny(c, "+/=") {
		t.Fatalf("not base64url: %q %q", v, c)
	}
	sum := sha256.Sum256([]byte(v))
	if got := base64.RawURLEncoding.EncodeToString(sum[:]); got != c {
		t.Fatalf("challenge mismatch: %q != %q", c, got)
	}
}

func TestNewCursorMachineID(t *testing.T) {
	id, err := newCursorMachineID()
	if err != nil {
		t.Fatal(err)
	}
	if !cursorMachineIDRe.MatchString(id) {
		t.Fatalf("machine id format %q", id)
	}
	id2, err := newCursorMachineID()
	if err != nil {
		t.Fatal(err)
	}
	if id == id2 {
		t.Fatal("machine ids should differ")
	}
}

var cursorMachineIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestCursorAccessExpiry(t *testing.T) {
	if got := CursorTokenExpiry("not-a-jwt"); got != 0 {
		t.Fatalf("malformed = %d", got)
	}
	if got := CursorTokenExpiry(""); got != 0 {
		t.Fatalf("empty = %d", got)
	}
	tok := jwtWithExp(t, 1_700_000_000)
	if got := CursorTokenExpiry(tok); got != 1_700_000_000 {
		t.Fatalf("exp = %d", got)
	}
}

func jwtWithExp(t *testing.T, exp int64) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(exp, 10) + `}`))
	return header + "." + payload + "."
}

func TestCursorConnectPendingThenSuccess(t *testing.T) {
	restore := overrideCursorEndpoints(t)
	defer restore()
	origInit, origMax, origTTL := cursorPollInitial, cursorPollMax, cursorLoginTTL
	cursorPollInitial = time.Millisecond
	cursorPollMax = 2 * time.Millisecond
	cursorLoginTTL = time.Second
	t.Cleanup(func() {
		cursorPollInitial, cursorPollMax, cursorLoginTTL = origInit, origMax, origTTL
	})

	var polls atomic.Int32
	var sawVerifier, sawContentType string
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/poll", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		n := polls.Add(1)
		sawVerifier = r.URL.Query().Get("verifier")
		sawContentType = r.Header.Get("Content-Type")
		if r.URL.Query().Get("uuid") == "" {
			t.Error("missing uuid")
		}
		if n < 2 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		exp := time.Now().Add(time.Hour).Unix()
		_ = json.NewEncoder(w).Encode(map[string]string{
			"accessToken":  jwtWithExp(t, exp),
			"refreshToken": "rt-1",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	restoreURLs := OverrideCursorURLs(srv.URL+"/loginDeepControl", srv.URL+"/auth/poll", "")
	defer restoreURLs()

	conn, err := NewCursorConnect("")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(conn.LoginURL())
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("mode") != "login" || u.Query().Get("redirectTarget") != "cli" {
		t.Fatalf("login query = %s", conn.LoginURL())
	}
	if u.Query().Get("challenge") == "" || u.Query().Get("uuid") == "" {
		t.Fatalf("missing challenge/uuid: %s", conn.LoginURL())
	}
	if u.Query().Get("uuid") != conn.State() {
		t.Fatalf("state should be login uuid")
	}
	if !cursorMachineIDRe.MatchString(conn.machineID) {
		t.Fatalf("generated machine id %q", conn.machineID)
	}

	if err := conn.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	creds := waitCursor(t, conn, 2*time.Second)
	if !creds.CursorAuth || creds.Preset != "cursor" || creds.Mode != domain.OAuthAuto {
		t.Fatalf("creds flags = %+v", creds)
	}
	if creds.RefreshToken != "rt-1" || creds.AccessToken == "" {
		t.Fatalf("tokens = %+v", creds)
	}
	if creds.MachineID != conn.machineID || creds.ExpiresAt == 0 {
		t.Fatalf("identity/exp = %+v", creds)
	}
	if sawVerifier != conn.verifier {
		t.Fatalf("poll verifier = %q want %q", sawVerifier, conn.verifier)
	}
	if sawContentType != "application/json" {
		t.Fatalf("poll content type = %q", sawContentType)
	}
	sum := sha256.Sum256([]byte(conn.verifier))
	wantChal := base64.RawURLEncoding.EncodeToString(sum[:])
	if u.Query().Get("challenge") != wantChal {
		t.Fatalf("challenge not SHA-256(verifier)")
	}
}

func TestCursorConnectPreservesMachineID(t *testing.T) {
	conn, err := NewCursorConnect("keep-this-machine")
	if err != nil {
		t.Fatal(err)
	}
	if conn.machineID != "keep-this-machine" {
		t.Fatalf("machine id = %q", conn.machineID)
	}
}

func TestCursorConnectCancel(t *testing.T) {
	restore := overrideCursorEndpoints(t)
	defer restore()
	origInit, origMax, origTTL := cursorPollInitial, cursorPollMax, cursorLoginTTL
	cursorPollInitial = 20 * time.Millisecond
	cursorPollMax = 20 * time.Millisecond
	cursorLoginTTL = time.Minute
	t.Cleanup(func() {
		cursorPollInitial, cursorPollMax, cursorLoginTTL = origInit, origMax, origTTL
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/poll", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	restoreURLs := OverrideCursorURLs(srv.URL+"/login", srv.URL+"/auth/poll", "")
	defer restoreURLs()

	conn, err := NewCursorConnect("")
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	_ = conn.Close()

	deadline := time.Now().Add(time.Second)
	for {
		_, err, done := conn.Result()
		if done {
			if err == nil {
				t.Fatal("want cancel error")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("close did not finish poll")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCursorConnectTerminalHTTPError(t *testing.T) {
	restore := overrideCursorEndpoints(t)
	defer restore()
	origInit, origMax := cursorPollInitial, cursorPollMax
	cursorPollInitial = time.Millisecond
	cursorPollMax = time.Millisecond
	t.Cleanup(func() { cursorPollInitial, cursorPollMax = origInit, origMax })

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/poll", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"denied"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	restoreURLs := OverrideCursorURLs(srv.URL+"/login", srv.URL+"/auth/poll", "")
	defer restoreURLs()

	conn, err := NewCursorConnect("")
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deadline := time.Now().Add(time.Second)
	for {
		_, err, done := conn.Result()
		if done {
			if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
				t.Fatalf("err = %v", err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCursorConnectRejectsIncompleteTokenPair(t *testing.T) {
	origInit := cursorPollInitial
	cursorPollInitial = time.Millisecond
	t.Cleanup(func() { cursorPollInitial = origInit })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "access-only"})
	}))
	t.Cleanup(srv.Close)
	restoreURLs := OverrideCursorURLs(srv.URL+"/login", srv.URL+"/auth/poll", "")
	defer restoreURLs()

	conn, err := NewCursorConnect("")
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	deadline := time.Now().Add(time.Second)
	for {
		_, err, done := conn.Result()
		if done {
			if err == nil || !strings.Contains(err.Error(), "no refreshToken") {
				t.Fatalf("err = %v", err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRefreshCursorRequestAndRotation(t *testing.T) {
	restore := overrideCursorEndpoints(t)
	defer restore()

	var hits atomic.Int32
	var sawAuth, sawCT, sawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/auth/exchange_user_api_key" {
			t.Errorf("path = %s", r.URL.Path)
		}
		sawAuth = r.Header.Get("Authorization")
		sawCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		sawBody = string(raw)
		exp := time.Now().Add(2 * time.Hour).Unix()
		_ = json.NewEncoder(w).Encode(map[string]string{
			"accessToken":  jwtWithExp(t, exp),
			"refreshToken": "rt-new",
		})
	}))
	t.Cleanup(srv.Close)
	restoreURLs := OverrideCursorURLs("", "", srv.URL+"/auth/exchange_user_api_key")
	defer restoreURLs()

	c := &domain.OAuthCreds{
		CursorAuth: true, AccessToken: "old", RefreshToken: "rt-old",
		MachineID: "mid-1", ExpiresAt: 1,
	}
	if err := refresh(context.Background(), c, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d", hits.Load())
	}
	if sawAuth != "Bearer rt-old" {
		t.Fatalf("auth = %q", sawAuth)
	}
	if sawCT != "application/json" || sawBody != "{}" {
		t.Fatalf("ct/body = %q %q", sawCT, sawBody)
	}
	if c.RefreshToken != "rt-new" || c.AccessToken == "old" {
		t.Fatalf("tokens = %+v", c)
	}
	if c.MachineID != "mid-1" {
		t.Fatalf("machine id changed: %q", c.MachineID)
	}
	if c.ExpiresAt == 0 || c.ExpiresAt == 1 {
		t.Fatalf("expires = %d", c.ExpiresAt)
	}
}

func TestRefreshCursorKeepsRefreshTokenOnOmission(t *testing.T) {
	restore := overrideCursorEndpoints(t)
	defer restore()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "tok-only"})
	}))
	t.Cleanup(srv.Close)
	restoreURLs := OverrideCursorURLs("", "", srv.URL)
	defer restoreURLs()

	c := &domain.OAuthCreds{
		CursorAuth: true, AccessToken: "old", RefreshToken: "rt-keep", MachineID: "m",
	}
	if err := refresh(context.Background(), c, time.Now()); err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "tok-only" || c.RefreshToken != "rt-keep" {
		t.Fatalf("tokens = %q/%q", c.AccessToken, c.RefreshToken)
	}
	if c.ExpiresAt != 0 {
		t.Fatalf("non-jwt expiry = %d", c.ExpiresAt)
	}
	if c.MachineID != "m" {
		t.Fatalf("machine id = %q", c.MachineID)
	}
}

func TestRefreshCursorInvalidGrant(t *testing.T) {
	restore := overrideCursorEndpoints(t)
	defer restore()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}))
	t.Cleanup(srv.Close)
	restoreURLs := OverrideCursorURLs("", "", srv.URL)
	defer restoreURLs()

	c := &domain.OAuthCreds{
		CursorAuth: true, AccessToken: "old", RefreshToken: "rt-dead", MachineID: "m",
	}
	if err := refresh(context.Background(), c, time.Now()); !IsInvalidGrant(err) {
		t.Fatalf("err = %v", err)
	}
	if c.AccessToken != "old" || c.RefreshToken != "rt-dead" || c.MachineID != "m" {
		t.Fatalf("mutated: %+v", c)
	}
}

func TestRefreshCursorRejectsNon2xxTokenPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"accessToken":  "must-not-apply",
			"refreshToken": "must-not-apply",
		})
	}))
	t.Cleanup(srv.Close)
	restoreURLs := OverrideCursorURLs("", "", srv.URL)
	defer restoreURLs()

	c := &domain.OAuthCreds{
		CursorAuth: true, AccessToken: "old", RefreshToken: "rt-old", MachineID: "mid",
	}
	err := refresh(context.Background(), c, time.Now())
	if err == nil || IsInvalidGrant(err) {
		t.Fatalf("err = %v, want transient HTTP error", err)
	}
	if c.AccessToken != "old" || c.RefreshToken != "rt-old" || c.MachineID != "mid" {
		t.Fatalf("mutated credentials: %+v", c)
	}
}

func TestRefreshCursorMalformedBodyOn500(t *testing.T) {
	restore := overrideCursorEndpoints(t)
	defer restore()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream boom"))
	}))
	t.Cleanup(srv.Close)
	restoreURLs := OverrideCursorURLs("", "", srv.URL)
	defer restoreURLs()

	c := &domain.OAuthCreds{CursorAuth: true, AccessToken: "old", RefreshToken: "rt"}
	err := refresh(context.Background(), c, time.Now())
	if err == nil || IsInvalidGrant(err) {
		t.Fatalf("err = %v, want transient decode error", err)
	}
	if c.AccessToken != "old" {
		t.Fatalf("access mutated: %q", c.AccessToken)
	}
}

func TestRefreshCursorAccessOnlyInvalidGrant(t *testing.T) {
	restore := overrideCursorEndpoints(t)
	defer restore()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("access-only cursor must not hit exchange")
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)
	restoreURLs := OverrideCursorURLs("", "", srv.URL)
	defer restoreURLs()

	c := &domain.OAuthCreds{CursorAuth: true, AccessToken: "ide-tok"}
	if err := refresh(context.Background(), c, time.Now()); !IsInvalidGrant(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveCursorRefreshPersistsMachineID(t *testing.T) {
	restore := overrideCursorEndpoints(t)
	defer restore()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exp := time.Now().Add(time.Hour).Unix()
		_ = json.NewEncoder(w).Encode(map[string]string{
			"accessToken": jwtWithExp(t, exp),
		})
	}))
	t.Cleanup(srv.Close)
	restoreURLs := OverrideCursorURLs("", "", srv.URL)
	defer restoreURLs()

	store := newFakeStore()
	creds := &domain.OAuthCreds{
		CursorAuth: true, AccessToken: "old", RefreshToken: "rt",
		MachineID: "stable-mid", ExpiresAt: 1,
	}
	store.creds[3] = creds
	s := New(store)
	p := &domain.Provider{ID: 3, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}
	tok, err := s.Resolve(context.Background(), p, true)
	if err != nil {
		t.Fatal(err)
	}
	if tok == "old" {
		t.Fatal("token not rotated")
	}
	store.mu.Lock()
	got := store.creds[3]
	store.mu.Unlock()
	if got.MachineID != "stable-mid" || got.RefreshToken != "rt" {
		t.Fatalf("persisted = %+v", got)
	}
}

func TestApplyPresetCursor(t *testing.T) {
	p, ok := PresetByName("cursor")
	if !ok {
		t.Fatal("missing preset")
	}
	prov, creds := Apply(p)
	if prov.Protocol != domain.ProtocolCursor {
		t.Fatalf("proto=%s", prov.Protocol)
	}
	if creds.Preset != "cursor" || !creds.CursorAuth {
		t.Fatalf("preset/cursor_auth = %q/%v", creds.Preset, creds.CursorAuth)
	}
}

func waitCursor(t *testing.T, conn *CursorConnect, d time.Duration) *domain.OAuthCreds {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		creds, err, done := conn.Result()
		if done {
			if err != nil {
				t.Fatal(err)
			}
			return creds
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for cursor connect")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func overrideCursorEndpoints(t *testing.T) func() {
	t.Helper()
	return OverrideCursorURLs("", "", "")
}
