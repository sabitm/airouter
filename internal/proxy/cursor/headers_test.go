package cursor

import (
	"strings"
	"testing"
)

func TestBuildHeaders(t *testing.T) {
	orig := nowMillis
	nowMillis = func() int64 { return 1_000_000_000_000 }
	t.Cleanup(func() { nowMillis = orig })

	h := BuildHeaders("sess::tok", "m1", true)
	if got := h.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("authorization = %q, want stripped bearer", got)
	}
	if got := h.Get("Content-Type"); got != ConnectContentType {
		t.Errorf("content-type = %q", got)
	}
	if got := h.Get("User-Agent"); got != UserAgent {
		t.Errorf("user-agent = %q", got)
	}
	if got := h.Get("X-Cursor-Client-Version"); got != ClientVersion || !strings.HasPrefix(got, "cli-") {
		t.Errorf("client-version = %q, want %q with cli- prefix", got, ClientVersion)
	}
	if got := h.Get("X-Cursor-Client-Type"); got != ClientType {
		t.Errorf("client-type = %q", got)
	}
	if got := h.Get("X-Ghost-Mode"); got != "true" {
		t.Errorf("ghost-mode = %q", got)
	}
	// checksum suffix is the machine id.
	if got := h.Get("X-Cursor-Checksum"); !strings.HasSuffix(got, "m1") {
		t.Errorf("checksum = %q, want suffix m1", got)
	}
	if got := h.Get("X-Session-Id"); got == "" {
		t.Error("session-id empty")
	}
	if got := h.Get("X-Client-Key"); len(got) != 64 {
		t.Errorf("client-key len = %d", len(got))
	}
	if got := h.Get("Connect-Protocol-Version"); got != "1" {
		t.Errorf("connect-protocol-version = %q", got)
	}
}

func TestBuildHeadersOmitsIDEOnlyHeaders(t *testing.T) {
	// An IDE-shaped identity triggers a false "usage limit" on Run.
	h := BuildHeaders("tok", "m1", true)
	for _, name := range []string{
		"X-Cursor-Client-Commit",
		"X-Cursor-Client-OS",
		"X-Cursor-Client-Arch",
		"X-Cursor-Client-Device-Type",
		"X-Cursor-Config-Version",
		"X-Cursor-Timezone",
		"X-Amzn-Trace-Id",
	} {
		if got := h.Get(name); got != "" {
			t.Errorf("%s = %q, want empty", name, got)
		}
	}
}

func TestBuildHeadersGhostFalse(t *testing.T) {
	h := BuildHeaders("tok", "m1", false)
	if got := h.Get("X-Ghost-Mode"); got != "false" {
		t.Errorf("ghost-mode = %q, want false", got)
	}
}

func TestBuildHeadersMachineIDFallback(t *testing.T) {
	h := BuildHeaders("tok", "", true)
	if got := h.Get("X-Cursor-Checksum"); !strings.HasSuffix(got, machineIDFallback("tok")) {
		t.Errorf("checksum should fall back to token-derived machine id: %q", got)
	}
}

func TestBuildModelsHeadersDropsConnectFraming(t *testing.T) {
	h := BuildModelsHeaders("tok", "m1", true)
	if h.Get("Connect-Accept-Encoding") != "" {
		t.Error("models headers should drop connect-accept-encoding")
	}
	if h.Get("Connect-Protocol-Version") != "" {
		t.Error("models headers should drop connect-protocol-version")
	}
	if got := h.Get("Content-Type"); got != ProtoContentType {
		t.Errorf("models content-type = %q", got)
	}
	if got := h.Get("Accept"); got != ProtoContentType {
		t.Errorf("models accept = %q", got)
	}
}
