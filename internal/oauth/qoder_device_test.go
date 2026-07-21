package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
)

func TestParseQoderExpiry(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := parseQoderExpiry(float64(now.Add(time.Hour).UnixMilli()), nil, now); got != now.Add(time.Hour).Unix() {
		t.Fatalf("ms number = %d", got)
	}
	ms := now.Add(2 * time.Hour).UnixMilli()
	if got := parseQoderExpiry(strconv.FormatInt(ms, 10), nil, now); got != now.Add(2*time.Hour).Unix() {
		t.Fatalf("ms string = %d", got)
	}
	if got := parseQoderExpiry(now.Add(3*time.Hour).Format(time.RFC3339), nil, now); got != now.Add(3*time.Hour).Unix() {
		t.Fatalf("rfc = %d", got)
	}
	if got := parseQoderExpiry(nil, float64(60), now); got != now.Add(60*time.Second).Unix() {
		t.Fatalf("expires_in = %d", got)
	}
	if got := parseQoderExpiry(nil, nil, now); got != now.Add(qoderDefaultTokenTTL).Unix() {
		t.Fatalf("default = %d", got)
	}
}

func TestQoderPKCE(t *testing.T) {
	v, c, err := qoderPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 43 || len(c) != 43 {
		t.Fatalf("verifier/challenge lens %d/%d", len(v), len(c))
	}
	if strings.ContainsAny(v, "+/=") || strings.ContainsAny(c, "+/=") {
		t.Fatalf("not base64url: %q %q", v, c)
	}
}

func TestRefreshQoder(t *testing.T) {
	if err := refreshQoder(context.Background(), nil, time.Now()); err != ErrInvalidGrant {
		t.Fatalf("err=%v", err)
	}
}

func TestShouldRefreshQoder(t *testing.T) {
	now := time.Now()
	c := &domain.OAuthCreds{QoderAuth: true, ExpiresAt: now.Add(-time.Hour).Unix()}
	if shouldRefresh(c, now) {
		t.Fatal("qoder should never proactive refresh")
	}
}

func TestQoderDeviceConnectPoll(t *testing.T) {
	origLogin, origPoll, origInfo := qoderLoginURL, qoderDeviceTokenURL, qoderUserInfoURL
	t.Cleanup(func() {
		qoderLoginURL, qoderDeviceTokenURL, qoderUserInfoURL = origLogin, origPoll, origInfo
	})
	// speed up poll
	origMin := devicePollMin
	devicePollMin = 10 * time.Millisecond
	t.Cleanup(func() { devicePollMin = origMin })

	var polls int
	mux := http.NewServeMux()
	mux.HandleFunc("/poll", func(w http.ResponseWriter, r *http.Request) {
		polls++
		if polls < 2 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "dt-abc", "user_id": "uid-1", "expires_in": 3600,
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dt-abc" {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"name": "Ada", "email": "a@b.c", "organization_id": "org-1",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	qoderLoginURL = srv.URL + "/login"
	qoderDeviceTokenURL = srv.URL + "/poll"
	qoderUserInfoURL = srv.URL + "/userinfo"

	conn, err := NewQoderDeviceConnect()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conn.VerificationURI(), "/login?") {
		t.Fatalf("uri=%s", conn.VerificationURI())
	}
	if err := conn.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		creds, err, done := conn.Result()
		if done {
			if err != nil {
				t.Fatal(err)
			}
			if creds.AccessToken != "dt-abc" || creds.UserID != "uid-1" || !creds.QoderAuth {
				t.Fatalf("creds=%+v", creds)
			}
			if creds.Email != "a@b.c" || creds.DisplayName != "Ada" || creds.OrganizationID != "org-1" {
				t.Fatalf("profile=%+v", creds)
			}
			if creds.MachineID == "" || creds.ExpiresAt == 0 {
				t.Fatalf("machine/exp missing: %+v", creds)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for device connect")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestApplyPresetQoder(t *testing.T) {
	p, ok := PresetByName("qoder")
	if !ok {
		t.Fatal("missing preset")
	}
	prov, creds := Apply(p)
	if prov.Protocol != domain.ProtocolQoder {
		t.Fatalf("proto=%s", prov.Protocol)
	}
	if !strings.Contains(prov.BaseURL, "qoder") {
		t.Fatalf("base=%s", prov.BaseURL)
	}
	if creds.Preset != "qoder" || !creds.QoderAuth {
		t.Fatalf("preset/qoder_auth = %q/%v", creds.Preset, creds.QoderAuth)
	}
}
