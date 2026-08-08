package media

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Resolver looks up host addresses. Tests inject fakes; production uses net.DefaultResolver.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// cacheEntry stores a completed fetch outcome (success or failure) so duplicate
// URLs within one request do not repeat network I/O.
type cacheEntry struct {
	res *FetchResult
	err error
}

// Fetcher downloads remote images under SSRF constraints. It is request-scoped:
// a single Fetcher instance deduplicates by URL for the lifetime of one ingress
// request (including failover attempts).
type Fetcher struct {
	Resolver Resolver
	// Client is optional; when set, Dial pinning is the caller's responsibility
	// (tests use httptest with a safe resolver). Production leaves Client nil so
	// each fetch builds a transport that pins the validated dial address.
	Client *http.Client

	mu    sync.Mutex
	cache map[string]*cacheEntry
	// inflight coalesces concurrent fetches of the same URL without holding mu
	// across I/O. Only the leader performs the download; waiters block on done.
	inflight map[string]*fetchCall
}

type fetchCall struct {
	done chan struct{}
	res  *FetchResult
	err  error
}

// FetchResult is a successfully downloaded and validated image.
type FetchResult struct {
	MediaType string
	Data      string // base64, no data-URI prefix
	Bytes     int
}

// FetchImage downloads rawURL, validates it is a supported image, and returns
// inline base64. Successful and failed outcomes are cached per Fetcher. Non-image
// content and SSRF violations fail closed. Redirects are rejected.
func (f *Fetcher) FetchImage(ctx context.Context, rawURL string) (*FetchResult, error) {
	if f == nil {
		f = &Fetcher{}
	}
	u, err := ParseHTTPURL(rawURL)
	if err != nil {
		return nil, err
	}
	key := u.String()

	f.mu.Lock()
	if f.cache != nil {
		if hit, ok := f.cache[key]; ok {
			f.mu.Unlock()
			return hit.res, hit.err
		}
	}
	if f.inflight == nil {
		f.inflight = make(map[string]*fetchCall)
	}
	if call, ok := f.inflight[key]; ok {
		f.mu.Unlock()
		select {
		case <-call.done:
			return call.res, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &fetchCall{done: make(chan struct{})}
	f.inflight[key] = call
	f.mu.Unlock()

	res, err := f.downloadImage(ctx, u)

	f.mu.Lock()
	call.res, call.err = res, err
	if f.cache == nil {
		f.cache = make(map[string]*cacheEntry)
	}
	f.cache[key] = &cacheEntry{res: res, err: err}
	delete(f.inflight, key)
	close(call.done)
	f.mu.Unlock()
	return res, err
}

func (f *Fetcher) downloadImage(ctx context.Context, parsed *url.URL) (*FetchResult, error) {
	host := parsed.Hostname()
	resolver := f.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	var dialIP net.IP
	if ip := net.ParseIP(host); ip != nil {
		dialIP = ip
	} else {
		addrs, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("%w: dns: %v", ErrFetchFailed, err)
		}
		ips := make([]net.IP, 0, len(addrs))
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
		if err := AllIPsSafe(ips); err != nil {
			return nil, err
		}
		dialIP = PickDialIP(ips)
		if dialIP == nil {
			return nil, fmt.Errorf("%w: no safe address", ErrUnsafeURL)
		}
	}

	timeout := time.Duration(FetchTimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := f.Client
	if client == nil {
		client = &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return ErrRedirect
			},
			Transport: &http.Transport{
				// Pin dial to the validated address while preserving Host/SNI.
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					_, port, err := net.SplitHostPort(addr)
					if err != nil {
						port = "443"
						if parsed.Scheme == "http" {
							port = "80"
						}
					}
					d := net.Dialer{Timeout: timeout}
					return d.DialContext(ctx, network, net.JoinHostPort(dialIP.String(), port))
				},
				ForceAttemptHTTP2: false,
			},
		}
	} else {
		// Ensure redirects are never followed even when tests inject a client.
		c := *client
		c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return ErrRedirect
		}
		client = &c
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	req.Header.Set("Accept", "image/*,*/*;q=0.8")
	req.Header.Set("User-Agent", "airouter-media/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: status %d", ErrFetchFailed, resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, int64(MaxAttachmentBytes)+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	if len(raw) > MaxAttachmentBytes {
		return nil, ErrAttachmentTooLarge
	}

	declared := NormalizeMIME(resp.Header.Get("Content-Type"))
	// Empty Content-Type (and generic octet-stream) may rely on magic alone.
	// Supported image MIME must match magic; other non-image types fail closed.
	var validateDeclared string
	switch {
	case declared == "" || declared == "application/octet-stream":
		validateDeclared = ""
	case IsSupportedImageMIME(declared):
		validateDeclared = CanonicalImageMIME(declared)
	case strings.HasPrefix(declared, "image/"):
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedMedia, declared)
	default:
		// text/plain, application/pdf, etc. conflict with an image fetch.
		return nil, fmt.Errorf("%w: content-type %s", ErrSignatureMismatch, declared)
	}

	detected, err := ValidateImageBytes(raw, validateDeclared)
	if err != nil {
		return nil, err
	}

	return &FetchResult{
		MediaType: detected,
		Data:      EncodeBase64(raw),
		Bytes:     len(raw),
	}, nil
}
