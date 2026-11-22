package claudecode

import (
	"reflect"
	"runtime"
	"slices"
	"testing"
)

func TestProfileAlignmentVersionAndFingerprintVectors(t *testing.T) {
	if CLIVersion != "2.1.318" || SDKPackageVersion != "0.128.0" {
		t.Fatalf("profile versions = %s / %s", CLIVersion, SDKPackageVersion)
	}
	// Independent Node evaluation of the pinned Du routine, not Go-generated values.
	for _, tc := range []struct{ text, want string }{
		{"", "da9"}, {"abc", "da9"}, {"hello world, this is a test", "78b"},
		{"abcd\U0001F600fZ", "5ab"}, {"abcd\x00xyA123456789012B", "bd2"},
	} {
		if got := Fingerprint(tc.text); got != tc.want {
			t.Errorf("fingerprint(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
	if got := DefaultProfile().UserAgent(); got != "claude-cli/2.1.318 (external, cli)" {
		t.Fatalf("inference user agent = %q", got)
	}
	if got := ProfileUserAgent(); got != "claude-code/2.1.318" {
		t.Fatalf("profile user agent = %q", got)
	}
	if !slices.Contains(StaticModels, "claude-haiku-5-5") {
		t.Fatal("Haiku 5.5 missing from fallback autocomplete")
	}
}

func TestProfileAlignmentHeadersDoNotClaimSDKHelpers(t *testing.T) {
	want := map[string]string{
		"X-Stainless-Lang": "js", "X-Stainless-Package-Version": "0.128.0",
		"X-Stainless-OS": stainlessOS(runtime.GOOS), "X-Stainless-Arch": stainlessArch(runtime.GOARCH),
		"X-Stainless-Runtime": "unknown", "X-Stainless-Runtime-Version": "unknown",
		"X-Stainless-Retry-Count": "0", "X-Stainless-Timeout": "600",
	}
	if got := StainlessRuntimeHeaders(); !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime headers = %v, want %v", got, want)
	}
}
