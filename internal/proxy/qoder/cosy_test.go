package qoder

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"
)

func TestBuildCosyHeaders(t *testing.T) {
	body := []byte("hello qoder")
	h, err := BuildCosyHeaders(body, ChatURL, Creds{
		UserID: "u1", AuthToken: "dt-test", Name: "n", Email: "e@x", MachineID: "m-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h["Authorization"], "Bearer COSY.") {
		t.Fatalf("auth=%q", h["Authorization"])
	}
	sum := md5.Sum(body)
	if h["Cosy-Bodyhash"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("bodyhash=%q", h["Cosy-Bodyhash"])
	}
	if h["Cosy-Bodylength"] != "11" {
		t.Fatalf("bodylength=%q", h["Cosy-Bodylength"])
	}
	if h["Cosy-Sigpath"] != "/api/v2/service/pro/sse/agent_chat_generation" {
		t.Fatalf("sigpath=%q", h["Cosy-Sigpath"])
	}
	if h["Cosy-User"] != "u1" || h["Cosy-Machineid"] != "m-1" {
		t.Fatalf("user/machine = %q/%q", h["Cosy-User"], h["Cosy-Machineid"])
	}
	for _, k := range []string{"Cosy-Key", "Cosy-Date", "Cosy-Version", "X-Request-Id"} {
		if h[k] == "" {
			t.Fatalf("missing %s", k)
		}
	}
}

func TestBuildCosyHeadersRequiresIdentity(t *testing.T) {
	if _, err := BuildCosyHeaders(nil, ChatURL, Creds{AuthToken: "t"}); err == nil {
		t.Fatal("want error without user id")
	}
	if _, err := BuildCosyHeaders(nil, ChatURL, Creds{UserID: "u"}); err == nil {
		t.Fatal("want error without token")
	}
}
