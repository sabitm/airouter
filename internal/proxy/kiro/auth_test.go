package kiro

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/domain"
)

func TestTokenTypeAndHeaders(t *testing.T) {
	cases := []struct {
		name      string
		id        Identity
		tokenType string
		wantIDP   string
		wantArn   bool
		wantMode  string
		wantOpt   bool
	}{
		{name: "api key", id: Identity{Method: domain.AuthAPIKey, Auth: domain.AuthBearer, Token: "k", ProfileArn: "arn:aws:codewhisperer:eu-west-1:1:profile/A"}, tokenType: "API_KEY"},
		{name: "external", id: Identity{Method: domain.AuthOAuth, KiroAuth: "external_idp", Token: "t", ProfileArn: "arn:aws:codewhisperer:us-east-1:1:profile/A", IDP: "ExternalOIDC"}, tokenType: "EXTERNAL_IDP", wantIDP: "ExternalOIDC", wantArn: true},
		{name: "builder auth arn", id: Identity{Method: domain.AuthOAuth, KiroAuth: "builder-id", Token: "t", ProfileArn: "arn:from-auth", IDP: "BuilderId"}, tokenType: "SSO_OIDC", wantIDP: "BuilderId", wantArn: true},
		{name: "idc", id: Identity{Method: domain.AuthOAuth, KiroAuth: "idc", Token: "t", ProfileArn: "arn:idc", IDP: "enterprise", AgentMode: "spec", ContentOptOut: true}, tokenType: "SSO_OIDC", wantIDP: "AWSIdC", wantArn: true, wantMode: "spec", wantOpt: true},
		{name: "social unmarked", id: Identity{Method: domain.AuthOAuth, KiroAuth: "social", Token: "t", ProfileArn: "arn:aws:codewhisperer:us-west-2:1:profile/A"}},
		{name: "google idp", id: Identity{Method: domain.AuthOAuth, KiroAuth: "social", Token: "t", ProfileArn: "arn:google", IDP: "Google"}, wantIDP: "Google", wantArn: true},
		{name: "unknown idp omitted", id: Identity{Method: domain.AuthOAuth, KiroAuth: "social", IDP: "custom", ProfileArn: "arn:kept-in-body"}},
		{name: "missing arn", id: Identity{Method: domain.AuthOAuth, KiroAuth: "idc", IDP: "AWSIdC"}, tokenType: "SSO_OIDC", wantIDP: "AWSIdC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TokenType(tc.id); got != tc.tokenType {
				t.Fatalf("token = %s, want %s", got, tc.tokenType)
			}
			h := make(http.Header)
			ApplyAuthHeaders(h, tc.id)
			if tc.tokenType == "" {
				if h.Get(HeaderTokenType) != "" || len(h.Values(HeaderTokenType)) != 0 {
					t.Fatalf("unexpected token marker: %v", h.Values(HeaderTokenType))
				}
			} else if values := h.Values(HeaderTokenType); len(values) != 1 || values[0] != tc.tokenType {
				t.Fatalf("TokenType values = %#v", values)
			}
			if h.Get(HeaderKiroIDP) != tc.wantIDP {
				t.Fatalf("idp = %q", h.Get(HeaderKiroIDP))
			}
			if tc.wantArn != (h.Get(HeaderKiroProfile) == tc.id.ProfileArn && tc.id.ProfileArn != "") {
				t.Fatalf("profile header = %q", h.Get(HeaderKiroProfile))
			}
			if !tc.wantArn && h.Get(HeaderKiroProfile) != "" {
				t.Fatalf("unexpected profile header %q", h.Get(HeaderKiroProfile))
			}
			if h.Get(HeaderAgentMode) != tc.wantMode {
				t.Fatalf("mode = %q", h.Get(HeaderAgentMode))
			}
			if tc.wantOpt != (h.Get(HeaderOptOut) == "true") {
				t.Fatalf("optout = %q", h.Get(HeaderOptOut))
			}
			if got := ProfileArnForBody(tc.id); got != tc.id.ProfileArn {
				t.Fatalf("body profile = %q, want %q", got, tc.id.ProfileArn)
			}
		})
	}
}

func TestTokenTypeHeaderOneValueOnTransport(t *testing.T) {
	cases := []Identity{
		{Method: domain.AuthAPIKey, Auth: domain.AuthBearer, Token: "k"},
		{Method: domain.AuthOAuth, Auth: domain.AuthBearer, KiroAuth: "external_idp", Token: "t", IDP: "ExternalIdp"},
		{Method: domain.AuthOAuth, Auth: domain.AuthBearer, KiroAuth: "builder-id", Token: "t", IDP: "BuilderId"},
	}
	for _, id := range cases {
		t.Run(TokenType(id), func(t *testing.T) {
			var got []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = append([]string(nil), r.Header.Values(HeaderTokenType)...)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			ApplyAuthHeaders(req.Header, id)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if len(got) != 1 || got[0] != TokenType(id) {
				t.Fatalf("wire TokenType = %#v", got)
			}
		})
	}
}

func TestRegionAndTransportSelection(t *testing.T) {
	id := Identity{ProfileArn: "arn:aws:codewhisperer:eu-central-1:1:profile/A", Region: "us-west-2"}
	if got := Region(id); got != "eu-central-1" {
		t.Fatalf("profile region = %q", got)
	}
	id.ProfileArn = ""
	if got := Region(id); got != "us-west-2" {
		t.Fatalf("identity region = %q", got)
	}
	id.Region = "bad region"
	if got := Region(id); got != DefaultRegion {
		t.Fatalf("fallback = %q", got)
	}
	if RuntimeURL("eu-central-1") != "https://runtime.eu-central-1.kiro.dev/" {
		t.Fatal(RuntimeURL("eu-central-1"))
	}
	if ManagementURL("") != "https://management.us-east-1.kiro.dev/" {
		t.Fatal(ManagementURL(""))
	}
	if UseRuntime(Identity{}) || UseRuntime(Identity{Transport: "codewhisperer"}) {
		t.Fatal("runtime selected by default")
	}
	if !UseRuntime(Identity{Transport: "runtime"}) {
		t.Fatal("explicit runtime not selected")
	}
	if UseManagementDiscovery(Identity{}) || !UseManagementDiscovery(Identity{Discovery: "management"}) {
		t.Fatal("discovery selection mismatch")
	}
	if ProfileArnForBody(Identity{KiroAuth: "builder-id", ProfileArn: "arn:from-auth"}) != "arn:from-auth" {
		t.Fatal("auth-returned builder profile was dropped")
	}
	legacy := ChatURL("https://mirror.example/custom", Identity{})
	if legacy != "https://mirror.example/custom/generateAssistantResponse" {
		t.Fatal(legacy)
	}
	runtimeMirror := ChatURL("https://mirror.example/custom", Identity{Transport: "runtime", Region: "eu-central-1"})
	if runtimeMirror != "https://mirror.example/custom/" {
		t.Fatal(runtimeMirror)
	}
	runtimePublic := ChatURL("https://codewhisperer.us-east-1.amazonaws.com", Identity{Transport: "runtime", Region: "eu-central-1"})
	if runtimePublic != "https://runtime.eu-central-1.kiro.dev/" {
		t.Fatal(runtimePublic)
	}
	if got := ChatURL("https://mirror.example/custom/", Identity{Transport: "runtime"}); got != "https://mirror.example/custom/" {
		t.Fatal(got)
	}
	if got := ChatURL("https://mirror.example/custom?drop=1", Identity{Transport: "runtime"}); got != "https://mirror.example/custom/" {
		t.Fatal(got)
	}
}
