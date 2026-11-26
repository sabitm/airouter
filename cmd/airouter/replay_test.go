package main

import (
	"testing"

	"airouter/internal/config"
	"airouter/internal/crypto"
)

func TestConfiguredReplayModes(t *testing.T) {
	payload := []byte("replay-payload")
	devCipher := mustCipher(t, config.IsInsecureDevSecret("airouter-insecure-dev-secret"))
	if err := enableConfiguredReplay(devCipher, config.Config{}, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := devCipher.GrokBuildChatMAC(payload); ok {
		t.Fatal("development fallback signed replay")
	}
	if _, ok := devCipher.CodexChatMAC(payload); ok {
		t.Fatal("development fallback signed codex replay")
	}
	explicit := mustCipher(t, true)
	if err := enableConfiguredReplay(explicit, config.Config{Secret: "airouter-insecure-dev-secret"}, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := explicit.GrokBuildChatMAC(payload); ok {
		t.Fatal("explicit development secret signed replay")
	}
	if _, ok := explicit.CodexChatMAC(payload); ok {
		t.Fatal("explicit development secret signed codex replay")
	}
	private := mustCipher(t, false)
	if err := enableConfiguredReplay(private, config.Config{Secret: "private-instance"}, false); err != nil {
		t.Fatal(err)
	}
	sum, ok := private.GrokBuildChatMAC(payload)
	if !ok || !private.VerifyGrokBuildChatMAC(payload, sum) {
		t.Fatal("private secret did not sign replay")
	}
	codexSum, ok := private.CodexChatMAC(payload)
	if !ok || !private.VerifyCodexChatMAC(payload, codexSum) {
		t.Fatal("private secret did not sign codex replay")
	}
	if private.VerifyGrokBuildChatMAC(payload, codexSum) || private.VerifyCodexChatMAC(payload, sum) {
		t.Fatal("replay MAC domains are not separated")
	}
	restart := mustCipher(t, false)
	if err := enableConfiguredReplay(restart, config.Config{Secret: "private-instance"}, false); err != nil {
		t.Fatal(err)
	}
	if !restart.VerifyGrokBuildChatMAC(payload, sum) || !restart.VerifyCodexChatMAC(payload, codexSum) {
		t.Fatal("private replay key changed across restart")
	}
	plain := "stored-api-key"
	enc, err := private.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restart.Decrypt(enc)
	if err != nil || got != plain {
		t.Fatalf("at-rest key changed: %q %v", got, err)
	}
}

func mustCipher(t *testing.T, dev bool) *crypto.Cipher {
	t.Helper()
	secret := "private-instance"
	if dev {
		secret = "airouter-insecure-dev-secret"
	}
	c, err := crypto.New(secret)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
