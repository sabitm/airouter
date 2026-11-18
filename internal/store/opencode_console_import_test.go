package store

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"airouter/internal/proxy/opencode"
)

func TestImportOpencodeConsoleDestinations(t *testing.T) {
	valid := `{
	  "version": 1,
	  "providers": [{
	    "name": "console",
	    "base_url": "https://opencode.ai/inference/go/v1",
	    "protocol": "opencode",
	    "auth_method": "oauth",
	    "auth_scheme": "bearer",
	    "oauth": {
	      "opencode_console_auth": true,
	      "access_token": "access",
	      "refresh_token": "refresh",
	      "opencode_org_id": "org-1",
	      "opencode_org_name": "Workspace",
	      "client_id": "opencode-cli"
	    }
	  }]
	}`
	st := testStore(t)
	sum, err := st.Import(context.Background(), strings.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	if sum.ProvidersCreated != 1 || len(sum.Failures) != 0 {
		t.Fatalf("summary=%+v", sum)
	}
	got, err := st.GetProvider(context.Background(), 1)
	if err != nil || !got.OAuthCreds.OpencodeConsoleAuth || got.OAuthCreds.OpencodeOrgID != "org-1" {
		t.Fatalf("imported=%+v err=%v", got, err)
	}

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "custom token url",
			body: strings.Replace(valid, `"client_id": "opencode-cli"`, `"client_id": "opencode-cli", "token_url": "https://evil.example/token"`, 1),
			want: "token URL",
		},
		{
			name: "zen base",
			body: strings.Replace(valid, opencode.GoBaseURL, opencode.ZenBaseURL, 1),
			want: "official Go",
		},
		{
			name: "api key crossover",
			body: strings.Replace(valid, `"protocol": "opencode"`, `"protocol": "opencode", "api_key": "secret"`, 1),
			want: "API key",
		},
		{
			name: "oauth without marker",
			body: `{
			  "version": 1,
			  "providers": [{
			    "name": "bare",
			    "base_url": "https://opencode.ai/inference/go/v1",
			    "protocol": "opencode",
			    "auth_method": "oauth",
			    "oauth": {"access_token": "a", "refresh_token": "r", "token_url": "https://evil.example/token"}
			  }]
			}`,
			want: "Console connection",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			sum, err := st.Import(context.Background(), bytes.NewReader([]byte(tc.body)))
			if err != nil {
				t.Fatal(err)
			}
			if sum.ProvidersCreated != 0 || len(sum.Failures) != 1 || !strings.Contains(sum.Failures[0], tc.want) {
				t.Fatalf("summary=%+v", sum)
			}
			if strings.Contains(sum.Failures[0], "access") || strings.Contains(sum.Failures[0], "refresh") || strings.Contains(sum.Failures[0], "secret") {
				t.Fatalf("failure leaked a credential: %s", sum.Failures[0])
			}
		})
	}
}
