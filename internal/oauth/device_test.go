package oauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewDeviceConnectRegionSSRF(t *testing.T) {
	_, err := NewDeviceConnect("not-a-region")
	if err == nil {
		t.Fatal("want error for invalid region")
	}
}

func TestDeviceConnectHappyPath(t *testing.T) {
	origMin := devicePollMin
	devicePollMin = time.Millisecond
	t.Cleanup(func() { devicePollMin = origMin })

	var tokenHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/client/register"):
			_, _ = w.Write([]byte(`{"clientId":"dyn-cid","clientSecret":"dyn-secret"}`))
		case strings.HasSuffix(r.URL.Path, "/device_authorization"):
			_, _ = w.Write([]byte(`{"deviceCode":"dc","userCode":"ABCD","verificationUriComplete":"https://example.com/v","expiresIn":600,"interval":1}`))
		case strings.HasSuffix(r.URL.Path, "/token"):
			n := tokenHits.Add(1)
			if n == 1 {
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"accessToken":"at","refreshToken":"rt","expiresIn":3600}`))
		default:
			// ListAvailableProfiles (POST to codewhisperer host root)
			body, _ := io.ReadAll(r.Body)
			if r.Header.Get("x-amz-target") != "AmazonCodeWhispererService.ListAvailableProfiles" {
				t.Errorf("unexpected path %s body %s", r.URL.Path, body)
			}
			_, _ = w.Write([]byte(`{"profiles":[{"profileArn":"arn:aws:codewhisperer:us-east-1:1:profile/p1"}]}`))
		}
	}))
	t.Cleanup(srv.Close)

	base := srv.URL
	origReg := kiroRegisterURL
	origDev := kiroDeviceAuthURL
	origTok := kiroOIDCTokenURL
	origProf := kiroListProfilesURL
	kiroRegisterURL = func(string) string { return base + "/client/register" }
	kiroDeviceAuthURL = func(string) string { return base + "/device_authorization" }
	kiroOIDCTokenURL = func(string) string { return base + "/token" }
	kiroListProfilesURL = func(string) string { return base + "/" }
	t.Cleanup(func() {
		kiroRegisterURL = origReg
		kiroDeviceAuthURL = origDev
		kiroOIDCTokenURL = origTok
		kiroListProfilesURL = origProf
	})

	dc, err := NewDeviceConnect("")
	if err != nil {
		t.Fatal(err)
	}
	if err := dc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dc.Close() })

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		creds, err, done := dc.Result()
		if done {
			if err != nil {
				t.Fatalf("done with err: %v", err)
			}
			if creds.AccessToken != "at" || creds.RefreshToken != "rt" {
				t.Fatalf("tokens: %+v", creds)
			}
			if creds.ClientID != "dyn-cid" || creds.ClientSecret != "dyn-secret" {
				t.Fatalf("dynamic client not persisted: %+v", creds)
			}
			if creds.KiroAuth != "builder-id" || creds.Region != "us-east-1" || creds.Preset != "kiro" {
				t.Fatalf("kiro metadata: %+v", creds)
			}
			if creds.ProfileArn != "arn:aws:codewhisperer:us-east-1:1:profile/p1" {
				t.Fatalf("profileArn = %q", creds.ProfileArn)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for device connect")
}

func TestDeviceConnectAccessDenied(t *testing.T) {
	origMin := devicePollMin
	devicePollMin = time.Millisecond
	t.Cleanup(func() { devicePollMin = origMin })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/client/register"):
			_, _ = w.Write([]byte(`{"clientId":"c","clientSecret":"s"}`))
		case strings.HasSuffix(r.URL.Path, "/device_authorization"):
			_, _ = w.Write([]byte(`{"deviceCode":"dc","userCode":"X","verificationUriComplete":"https://x","expiresIn":600,"interval":1}`))
		default:
			_, _ = w.Write([]byte(`{"error":"access_denied"}`))
		}
	}))
	t.Cleanup(srv.Close)

	base := srv.URL
	origReg, origDev, origTok := kiroRegisterURL, kiroDeviceAuthURL, kiroOIDCTokenURL
	kiroRegisterURL = func(string) string { return base + "/client/register" }
	kiroDeviceAuthURL = func(string) string { return base + "/device_authorization" }
	kiroOIDCTokenURL = func(string) string { return base + "/token" }
	t.Cleanup(func() {
		kiroRegisterURL = origReg
		kiroDeviceAuthURL = origDev
		kiroOIDCTokenURL = origTok
	})

	dc, err := NewDeviceConnect("us-west-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := dc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dc.Close() })

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, err, done := dc.Result()
		if done {
			if err == nil {
				t.Fatal("want error")
			}
			if !strings.Contains(err.Error(), "access_denied") {
				t.Fatalf("err = %v", err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestDeviceConnectRegisterDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		if m["issuerUrl"] != kiroDeviceIssuerURL {
			t.Errorf("issuerUrl = %v", m["issuerUrl"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"clientId":"a","clientSecret":"b"}`))
	}))
	t.Cleanup(srv.Close)
	orig := kiroRegisterURL
	kiroRegisterURL = func(string) string { return srv.URL }
	t.Cleanup(func() { kiroRegisterURL = orig })

	dc, err := NewDeviceConnect("")
	if err != nil {
		t.Fatal(err)
	}
	cid, sec, err := dc.registerClient(context.Background())
	if err != nil || cid != "a" || sec != "b" {
		t.Fatalf("register: %q %q %v", cid, sec, err)
	}
}