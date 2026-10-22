package ir

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestResponseIncompatibilityClassification(t *testing.T) {
	direct := IncompatibleResponse()
	if _, ok := AsResponseIncompatibility(direct); !ok {
		t.Fatal("direct incompatibility was not recognized")
	}
	if _, ok := AsRequestValidation(direct); ok {
		t.Fatal("response incompatibility was classified as request validation")
	}
	if _, ok := AsStreamFailure(direct); ok {
		t.Fatal("response incompatibility was classified as a stream failure")
	}

	wrapped := fmt.Errorf("codec: %w", direct)
	if _, ok := AsResponseIncompatibility(wrapped); !ok {
		t.Fatal("wrapped incompatibility was not recognized")
	}
	if _, ok := AsResponseIncompatibility(errors.New("other")); ok {
		t.Fatal("unrelated error was classified as response incompatibility")
	}
}

func TestResponseIncompatibilityMessageIsFixed(t *testing.T) {
	const want = "upstream response is not representable in the ingress format"
	if got := IncompatibleResponse().Error(); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	var nilErr *ResponseIncompatibilityError
	if got := nilErr.Error(); got != want {
		t.Fatalf("nil message = %q, want %q", got, want)
	}
	if strings.Contains(want, "compaction") || strings.Contains(want, "server") || strings.Contains(want, "secret") {
		t.Fatalf("fixed message exposes response detail: %q", want)
	}
}
