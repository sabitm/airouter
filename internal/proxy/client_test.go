package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"airouter/internal/domain"
)

type repeatingReader byte

func (r repeatingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func sizedResponse(req *http.Request, size int64) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(io.LimitReader(repeatingReader('x'), size)),
		ContentLength: size,
		Request:       req,
	}
}

func TestForwardUnaryResponseSizeLimit(t *testing.T) {
	if maxUnaryUpstreamResponseBytes != 64<<20 {
		t.Fatalf("maxUnaryUpstreamResponseBytes = %d, want %d", maxUnaryUpstreamResponseBytes, int64(64<<20))
	}

	provider := &domain.Provider{
		BaseURL:  "https://provider.example",
		APIKey:   "secret",
		Protocol: domain.ProtocolOpenAI,
	}
	cases := []struct {
		name    string
		size    int64
		wantErr bool
	}{
		{name: "at limit", size: maxUnaryUpstreamResponseBytes},
		{name: "over limit", size: maxUnaryUpstreamResponseBytes + 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := New(nil, nil)
			p.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return sizedResponse(req, tc.size), nil
			})}

			status, body, err := p.forward(context.Background(), provider, "/chat/completions", []byte(`{}`), nil)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "upstream response too large") {
					t.Fatalf("error = %v, want response-too-large error", err)
				}
				if body != nil {
					t.Fatalf("body length = %d, want nil", len(body))
				}
				return
			}
			if err != nil {
				t.Fatalf("forward: %v", err)
			}
			if int64(len(body)) != tc.size {
				t.Fatalf("body length = %d, want %d", len(body), tc.size)
			}
		})
	}
}

func TestOversizedUnaryResponseFailsOver(t *testing.T) {
	primary := newScriptedUpstream(t, domain.ProtocolOpenAI)
	fallback := newScriptedUpstream(t, domain.ProtocolOpenAI)
	base, token, p := setupComboProxy(t, domain.StrategyFailover,
		[]*scriptedUpstream{primary, fallback},
		[]domain.Protocol{domain.ProtocolOpenAI, domain.ProtocolOpenAI})

	var primaryHits, fallbackHits atomic.Int64
	p.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasPrefix(req.URL.String(), primary.server.URL) {
			primaryHits.Add(1)
			return sizedResponse(req, maxUnaryUpstreamResponseBytes+1), nil
		}
		fallbackHits.Add(1)
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          io.NopCloser(strings.NewReader(openaiUpstreamBody)),
			ContentLength: int64(len(openaiUpstreamBody)),
			Request:       req,
		}, nil
	})}

	resp, body := post(t, base+"/v1/chat/completions", token, oaiReq)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if primaryHits.Load() != 1 || fallbackHits.Load() != 1 {
		t.Fatalf("hits primary=%d fallback=%d, want 1/1", primaryHits.Load(), fallbackHits.Load())
	}
	if got := extractText(t, "/v1/chat/completions", body); got != "hello from openai" {
		t.Fatalf("fallback response text = %q", got)
	}
}
