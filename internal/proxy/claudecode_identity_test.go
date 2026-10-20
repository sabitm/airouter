package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/claudecode"
)

func TestClaudeSessionStableAcrossRetries(t *testing.T) {
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", "11111111-1111-4111-8111-111111111111")
	ctx := withClaudeRequest(context.Background(), h, nil)
	p := &domain.Provider{Name: "cc", Protocol: domain.ProtocolClaudeCode}
	px := &Proxy{}
	a := px.resolveClaudeSession(ctx, p, "tenant")
	b := px.resolveClaudeSession(ctx, p, "tenant")
	if a != b {
		t.Fatalf("session changed across retries: %s / %s", a, b)
	}
	if a == "11111111-1111-4111-8111-111111111111" {
		t.Fatal("client session was returned without tenant scope")
	}
	other := px.resolveClaudeSession(ctx, p, "other-tenant")
	if other == a {
		t.Fatal("same client session joined another tenant")
	}
}

func TestClaudeAnonymousRequestsStayIndependent(t *testing.T) {
	px := &Proxy{}
	p := &domain.Provider{ID: 7, Name: "cc", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode, OAuthCreds: &domain.OAuthCreds{ClaudeAccountUUID: "acct"}}
	ctx := withClaudeRequest(context.Background(), nil, nil)
	a := px.resolveClaudeSession(ctx, p, "tenant-a")
	if again := px.resolveClaudeSession(ctx, p, "tenant-a"); again != a {
		t.Fatalf("anonymous retry changed session: %s / %s", a, again)
	}
	if b := px.resolveClaudeSession(ctx, p, "tenant-b"); a == b {
		t.Fatal("tenants shared an anonymous session")
	}
	other := withClaudeRequest(context.Background(), nil, nil)
	if px.resolveClaudeSession(other, p, "tenant-a") == a {
		t.Fatal("unidentified requests were joined")
	}
}

func TestClaudeSessionScopeUsesStableProviderIdentity(t *testing.T) {
	const session = "11111111-1111-4111-8111-111111111111"
	const parent = "22222222-2222-4222-8222-222222222222"
	a := &domain.Provider{
		ID: 1, Name: "old", BaseURL: "https://One.Example/v1/", Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{ClaudeAccountUUID: "acct", AccountID: "other-protocol", AccessToken: "tok-a"},
	}
	renamed := *a
	renamed.Name = "new"
	renamed.APIKey = "tok-b"
	if claudeScopedSession("tenant", a, session) != claudeScopedSession("tenant", &renamed, session) {
		t.Fatal("provider rename or token change split the session")
	}
	otherID := *a
	otherID.ID = 2
	if claudeScopedSession("tenant", a, session) == claudeScopedSession("tenant", &otherID, session) {
		t.Fatal("distinct provider IDs joined")
	}
	otherBase := *a
	otherBase.BaseURL = "https://two.example"
	if claudeScopedSession("tenant", a, session) == claudeScopedSession("tenant", &otherBase, session) {
		t.Fatal("distinct base URLs joined")
	}
	sameBase := *a
	sameBase.BaseURL = "https://one.example/v1"
	if claudeScopedSession("tenant", a, session) != claudeScopedSession("tenant", &sameBase, session) {
		t.Fatal("canonical base URL was not stable")
	}
	otherAcct := *a
	otherAcct.OAuthCreds = &domain.OAuthCreds{ClaudeAccountUUID: "other", AccountID: "acct"}
	if claudeScopedSession("tenant", a, session) == claudeScopedSession("tenant", &otherAcct, session) {
		t.Fatal("distinct Claude accounts joined through AccountID")
	}
	if claudeScopedSession("tenant", a, session) == claudeScopedSession("other-tenant", a, session) {
		t.Fatal("tenants joined")
	}
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", session)
	h.Set("X-Claude-Code-Parent-Session-Id", parent)
	ctx := withClaudeRequest(context.Background(), h, nil)
	px := &Proxy{}
	if px.resolveClaudeSession(ctx, a, "tenant") != px.resolveClaudeSession(ctx, &renamed, "tenant") {
		t.Fatal("retry after rename changed the main session")
	}
	if claudeParentID(ctx, a) != claudeParentID(ctx, &renamed) || claudeParentID(ctx, a) == parent {
		t.Fatal("parent UUID was not scoped with the same provider identity")
	}
	if claudeParentID(ctx, a) == claudeParentID(ctx, &otherAcct) {
		t.Fatal("parent UUID joined another account")
	}
}

func TestClaudeAnonymousConcurrencyDoesNotJoin(t *testing.T) {
	px := &Proxy{}
	p := &domain.Provider{ID: 4, BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode}
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := withClaudeRequest(context.Background(), nil, nil)
			got[i] = px.resolveClaudeSession(ctx, p, "tenant")
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, id := range got {
		if id == "" || seen[id] {
			t.Fatalf("anonymous requests joined: %v", got)
		}
		seen[id] = true
	}
}

func TestClaudeSessionUsesCurrentProviderSnapshot(t *testing.T) {
	base := &domain.Provider{ID: 4, BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode, AuthMethod: domain.AuthOAuth}
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", "11111111-1111-4111-8111-111111111111")
	ctx := withClaudeRequest(context.Background(), h, nil)
	ctx = WithTraceInfo(ctx, &TraceInfo{ClaudeCodeSessionID: "99999999-9999-4999-8999-999999999999"})
	oldAccount := *base
	oldAccount.OAuthCreds = &domain.OAuthCreds{ClaudeAccountUUID: "old"}
	newAccount := *base
	newAccount.OAuthCreds = &domain.OAuthCreds{ClaudeAccountUUID: "new"}
	oldID := claudeSessionID(ctx, &oldAccount, "tenant")
	if got := claudeSessionID(ctx, &oldAccount, "tenant"); got != oldID {
		t.Fatalf("same snapshot retry changed: %s / %s", oldID, got)
	}
	if got := claudeSessionID(ctx, &newAccount, "tenant"); got == oldID {
		t.Fatal("account change reused the old scope")
	}
}

func TestClaudeBlankBaseMatchesExplicitDefault(t *testing.T) {
	blank := claudeCanonicalBase("")
	explicit := claudeCanonicalBase("https://api.anthropic.com/v1")
	if blank == "" || blank != explicit {
		t.Fatalf("blank base %q != explicit default %q", blank, explicit)
	}
}

func TestClaudeNoTraceHeaderMatchesBody(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	provider := &domain.Provider{ID: 9, BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode, OAuthCreds: &domain.OAuthCreds{ClaudeAccountUUID: "acct", ClaudeDeviceID: strings.Repeat("ab", 32)}}
	ctx := withClaudeRequest(context.Background(), nil, nil)
	check := func() string {
		wire, err := prepareClaudeCodeBody(ctx, provider, body)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, "https://upstream.example/messages", nil)
		applyClaudeCodeHeaders(req, ctx, provider, wire)
		var doc struct {
			Metadata struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(wire, &doc); err != nil {
			t.Fatal(err)
		}
		var uid claudecode.UserID
		if err := json.Unmarshal([]byte(doc.Metadata.UserID), &uid); err != nil {
			t.Fatal(err)
		}
		header := req.Header.Get(claudecode.SessionIDHeader)
		if header == "" || header != uid.SessionID {
			t.Fatalf("header %q != body %q", header, uid.SessionID)
		}
		return header
	}
	first := check()
	if again := check(); again != first {
		t.Fatalf("same request changed session without trace: %s / %s", first, again)
	}
}

func TestClaudeStaleTraceIsReplacedAfterResolve(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	provider := &domain.Provider{ID: 9, BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode, OAuthCreds: &domain.OAuthCreds{ClaudeAccountUUID: "new", ClaudeDeviceID: strings.Repeat("ab", 32)}}
	ctx := withClaudeRequest(context.Background(), nil, nil)
	trace := &TraceInfo{ClaudeCodeSessionID: "99999999-9999-4999-8999-999999999999"}
	ctx = WithTraceInfo(ctx, trace)
	wire, err := prepareClaudeCodeBody(ctx, provider, body)
	if err != nil {
		t.Fatal(err)
	}
	if trace.ClaudeCodeSessionID == "99999999-9999-4999-8999-999999999999" {
		t.Fatal("stale trace session was reused")
	}
	if got := trace.ClaudeCodeSessionID; !strings.Contains(string(wire), got) {
		t.Fatalf("trace %q does not match body %s", got, wire)
	}
}

func TestClaudeCaptureIgnoresClientAccount(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"{\"account_uuid\":\"client-claimed\",\"session_id\":\"22222222-2222-4222-8222-222222222222\"}"}}`)
	st := &claudeReqState{}
	st.capture(nil, body)
	if st.session != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("session = %q", st.session)
	}
}

func TestClaudeSessionScopeIgnoresAPIKeyOAuthConfig(t *testing.T) {
	const session = "11111111-1111-4111-8111-111111111111"
	base := &domain.Provider{
		ID: 3, BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthAPIKey, APIKey: "static",
		OAuthCreds: &domain.OAuthCreds{ClaudeAccountUUID: "stale-one"},
	}
	other := *base
	other.OAuthCreds = &domain.OAuthCreds{ClaudeAccountUUID: "stale-two"}
	if claudeScopedSession("tenant", base, session) != claudeScopedSession("tenant", &other, session) {
		t.Fatal("API-key rows joined or split on stale OAuth account config")
	}
	oauth := *base
	oauth.AuthMethod = domain.AuthOAuth
	if claudeScopedSession("tenant", base, session) == claudeScopedSession("tenant", &oauth, session) {
		t.Fatal("OAuth account was ignored")
	}
}
