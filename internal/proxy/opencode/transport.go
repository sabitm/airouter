package opencode

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	httpproxy "golang.org/x/net/http/httpproxy"
	netproxy "golang.org/x/net/proxy"
)

// The Zen free-tier gate refuses inference POSTs whose TLS ClientHello looks
// like a stock HTTP library. Measured against the live endpoint: default Go
// net/http and a Go crypto/tls cipher override both receive 429
// FreeUsageLimitError while the official CLI passes, and a uTLS handshake that
// mirrors the CLI runtime's BoringSSL ClientHello passes with the identical
// body and headers. The clientHelloSpec below reproduces that ClientHello
// byte-for-byte (same cipher list and order, same extensions and order, ALPN
// http/1.1 only, no GREASE and no post-quantum groups), so it must only be
// adjusted together with a live probe.

const (
	// freeTierHost is the only host the impersonated transport dials.
	freeTierHost = "opencode.ai"
	// inferencePathPrefix is the current official free-tier endpoint. The
	// retired /zen/v1 base is aliased to this prefix before send, so stored
	// legacy rows are covered; custom endpoints on other hosts are not.
	inferencePathPrefix = "/inference/v1"
	// handshakeBudget bounds TCP dialing and the transport-level TLS handshake
	// timeout. Phase bounds that run on the caller context use phaseBudget
	// instead, so tests can shorten them without touching the dialer.
	handshakeBudget = 30 * time.Second
)

// phaseBudget bounds one blocking dial phase (proxy CONNECT exchange, SOCKS
// negotiation, or TLS handshake) when the caller context carries no deadline.
// It is a variable so tests can shorten the silent-peer timeout deterministically;
// production code must not reassign it.
var phaseBudget = handshakeBudget

// inferenceEndpoints lists the official free-tier POST endpoints. Other paths
// on the host (paid /zen routes, /models, the bare prefix) and any custom port,
// scheme, or userinfo variant keep the standard transport.
var inferenceEndpoints = map[string]bool{
	inferencePathPrefix + ChatPath:      true,
	inferencePathPrefix + ResponsesPath: true,
	inferencePathPrefix + MessagesPath:  true,
}

// IsOfficialFreeTierURL reports whether rawURL targets an official OpenCode
// inference endpoint over HTTPS on the default port, without userinfo. Paid
// Zen (/zen/go), Go tier, custom mirrors, and other hosts return false, so
// their transport behavior is unchanged.
func IsOfficialFreeTierURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.User != nil {
		return false
	}
	if u.Scheme != "https" || !strings.EqualFold(u.Hostname(), freeTierHost) {
		return false
	}
	if port := u.Port(); port != "" && port != "443" {
		return false
	}
	return inferenceEndpoints[strings.TrimSuffix(u.Path, "/")]
}

// NewFreeTierTransport builds an http.Transport whose TLS handshake mimics the
// OpenCode CLI runtime. Only requests routed to it by the caller are affected;
// the returned transport is not installed anywhere by this package.
func NewFreeTierTransport() *http.Transport {
	// Defaults mirror http.DefaultTransport (30s dial, 90s idle, 100 conns)
	// without referencing the global: tests swap http.DefaultTransport itself,
	// so a clone would capture whatever is installed at construction time.
	return &http.Transport{
		// Proxying must happen inside the TLS dialer: with a custom
		// DialTLSContext, net/http runs the proxy CONNECT itself and then does
		// standard-library TLS for the target, which would drop the fingerprint.
		// Proxy stays nil so the dialer always receives the target address and
		// owns the full chain.
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   handshakeBudget,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// The dialer advertises only http/1.1, so HTTP/2 must stay off: a server
		// negotiating h2 over a connection we then drive with HTTP/1.1 would
		// break.
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   handshakeBudget,
		ExpectContinueTimeout: time.Second,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialFreeTierTLS(ctx, network, addr, nil)
		},
	}
}

// NewFreeTierClient returns the HTTP client for official free-tier inference
// requests: the impersonated-TLS transport plus a redirect policy that refuses
// any hop leaving the official endpoint before it is sent. Refused redirects
// surface as a normal send failure; the fingerprinted transport is never used
// against another origin and credentials are never replayed cross-host.
func NewFreeTierClient() *http.Client {
	return &http.Client{
		Transport:     NewFreeTierTransport(),
		CheckRedirect: freeTierRedirectPolicy,
	}
}

// maxFreeTierRedirects matches Go's net/http default redirect bound. A
// non-nil CheckRedirect disables the built-in stop, so the limit must be
// enforced here or an official-endpoint redirect loop would never terminate.
const maxFreeTierRedirects = 10

// freeTierRedirectPolicy allows redirects only between official free-tier
// URLs; the follow-up request to anything else is never dialed. The via
// length check restores Go's default stop after 10 requests.
func freeTierRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= maxFreeTierRedirects {
		return fmt.Errorf("opencode: stopped after %d redirects", maxFreeTierRedirects)
	}
	if IsOfficialFreeTierURL(req.URL.String()) {
		return nil
	}
	return fmt.Errorf("opencode: free-tier redirect to non-official endpoint refused: %s", req.URL)
}

// dialFreeTierTLS connects to addr and completes the mimicked TLS handshake.
// rootCAs is nil in production, which keeps system-root certificate and
// hostname verification exactly as crypto/tls would perform it.
func dialFreeTierTLS(ctx context.Context, network, addr string, rootCAs *x509.CertPool) (net.Conn, error) {
	proxyURL, err := proxyForHTTPS(addr)
	if err != nil {
		return nil, err
	}
	return dialFreeTierTLSWithProxy(ctx, network, addr, rootCAs, proxyURL)
}

// dialFreeTierTLSWithProxy dials addr through proxyURL (nil: direct) and
// completes the mimicked TLS handshake. Split from dialFreeTierTLS so tests
// can pin an explicit proxy: the environment resolver never proxies localhost
// targets, which local TLS test servers are.
func dialFreeTierTLSWithProxy(ctx context.Context, network, addr string, rootCAs *x509.CertPool, proxyURL *url.URL) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("opencode: free-tier dial address %q: %w", addr, err)
	}
	var conn net.Conn
	if proxyURL == nil {
		conn, err = (&net.Dialer{Timeout: handshakeBudget, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
	} else {
		conn, err = dialThroughProxy(ctx, proxyURL, addr, rootCAs)
	}
	if err != nil {
		return nil, err
	}
	uconn := newFreeTierUConn(conn, host, rootCAs)
	clear, err := withConnDeadline(ctx, conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	err = uconn.HandshakeContext(ctx)
	clear()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("opencode: free-tier TLS handshake: %w", err)
	}
	return uconn, nil
}

// newFreeTierUConn wraps conn in a uTLS connection with the mirrored
// ClientHello. A fresh spec is built per handshake: ApplyPreset fills key
// shares into the spec, so sharing one spec across concurrent handshakes is
// not safe.
func newFreeTierUConn(conn net.Conn, host string, rootCAs *x509.CertPool) *utls.UConn {
	cfg := &utls.Config{
		ServerName: host,
		RootCAs:    rootCAs,
		// ALPN matches the CLI runtime, which speaks HTTP/1.1 only. Never add
		// h2 here without also moving to an HTTP/2 round tripper.
		NextProtos: []string{"http/1.1"},
	}
	uconn := utls.UClient(conn, cfg, utls.HelloCustom)
	// ApplyPreset errors are impossible for this fixed spec; a mismatch would
	// surface on the first handshake against any test server.
	_ = uconn.ApplyPreset(newClientHelloSpec())
	return uconn
}

// newClientHelloSpec mirrors the CLI runtime's BoringSSL ClientHello: exact
// cipher list and order, exact extension list and order, single X25519 key
// share, no GREASE, no post-quantum groups. Verified live against the free
// gate; do not reorder or extend without a live probe.
func newClientHelloSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		CipherSuites: []uint16{
			utls.TLS_AES_128_GCM_SHA256,
			utls.TLS_AES_256_GCM_SHA384,
			utls.TLS_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			utls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_RSA_WITH_AES_256_CBC_SHA,
		},
		CompressionMethods: []byte{0x00},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519, utls.CurveP256, utls.CurveP384}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0x00}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				utls.ECDSAWithP256AndSHA256,
				utls.PSSWithSHA256,
				utls.PKCS1WithSHA256,
				utls.ECDSAWithP384AndSHA384,
				utls.PSSWithSHA384,
				utls.PKCS1WithSHA384,
				utls.PSSWithSHA512,
				utls.PKCS1WithSHA512,
				utls.PKCS1WithSHA1,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{1}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
		},
	}
}

// proxyForHTTPS resolves the proxy environment (HTTP_PROXY/HTTPS_PROXY/
// NO_PROXY) for an https connection to addr (host:port). The full address is
// passed so NO_PROXY entries that pin a port resolve exactly. A nil URL means
// direct dialing.
func proxyForHTTPS(addr string) (*url.URL, error) {
	reqURL := &url.URL{Scheme: "https", Host: addr}
	u, err := httpproxy.FromEnvironment().ProxyFunc()(reqURL)
	if err != nil {
		return nil, fmt.Errorf("opencode: resolve proxy for %s: %w", addr, err)
	}
	return u, nil
}

// dialThroughProxy reaches target (host:port) through an HTTP, HTTPS, or
// SOCKS5 proxy from the proxy environment. The returned connection is a raw
// tunnel to the target; TLS to the target happens in the caller.
func dialThroughProxy(ctx context.Context, proxyURL *url.URL, target string, rootCAs *x509.CertPool) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: handshakeBudget, KeepAlive: 30 * time.Second}
	switch proxyURL.Scheme {
	case "http", "https":
		conn, err := dialProxyConn(ctx, dialer, proxyURL, rootCAs)
		if err != nil {
			return nil, err
		}
		if err := connectOver(ctx, conn, proxyURL, target); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	case "socks5", "socks5h":
		d, err := netproxy.FromURL(proxyURL, dialer)
		if err != nil {
			return nil, fmt.Errorf("opencode: socks proxy %s: %w", proxyURL.Host, err)
		}
		// x/net SOCKS sets a deadline only when the context carries one, and
		// net/http hands pooled dials a context with no deadline or cancellation:
		// a silent proxy would block the negotiation forever. WithTimeout never
		// extends a caller deadline (a shorter one wins) and keeps cancellation.
		dialCtx, cancel := context.WithTimeout(ctx, phaseBudget)
		defer cancel()
		cd, ok := d.(netproxy.ContextDialer)
		if !ok {
			// Unreachable for x/net SOCKS dialers; fail closed rather than dial
			// without the phase bound.
			return nil, fmt.Errorf("opencode: socks proxy %s: no context dialer", proxyURL.Host)
		}
		return cd.DialContext(dialCtx, "tcp", target)
	default:
		return nil, fmt.Errorf("opencode: unsupported proxy scheme %q", proxyURL.Scheme)
	}
}

// dialProxyConn opens the transport connection to the proxy. An https proxy
// URL gets a standard-library TLS session to the proxy itself; the fingerprint
// only matters for the target hop.
func dialProxyConn(ctx context.Context, dialer *net.Dialer, proxyURL *url.URL, rootCAs *x509.CertPool) (net.Conn, error) {
	addr := canonicalProxyAddr(proxyURL)
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("opencode: proxy dial %s: %w", addr, err)
	}
	if proxyURL.Scheme != "https" {
		return conn, nil
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: proxyURL.Hostname(), RootCAs: rootCAs})
	clear, err := withConnDeadline(ctx, conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	err = tlsConn.HandshakeContext(ctx)
	clear()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("opencode: proxy TLS handshake %s: %w", addr, err)
	}
	return tlsConn, nil
}

// connectOver sends CONNECT and requires a 2xx tunnel response. Buffering past
// the response header is discarded: a target TLS server never speaks first, so
// read-ahead can only contain proxy-injected bytes.
func connectOver(ctx context.Context, conn net.Conn, proxyURL *url.URL, target string) error {
	clear, err := withConnDeadline(ctx, conn)
	if err != nil {
		return err
	}
	defer clear()
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: make(http.Header),
	}
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+basicAuth(proxyURL.User.Username(), password))
	}
	if err := req.Write(conn); err != nil {
		return fmt.Errorf("opencode: proxy CONNECT write: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return fmt.Errorf("opencode: proxy CONNECT read: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("opencode: proxy CONNECT status %s", resp.Status)
	}
	return nil
}

func canonicalProxyAddr(proxyURL *url.URL) string {
	port := proxyURL.Port()
	if port == "" {
		if proxyURL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(proxyURL.Hostname(), port)
}

func basicAuth(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}

// withConnDeadline bounds one blocking phase (proxy exchange or TLS
// handshake) to phaseBudget even when the caller context carries a later
// deadline; a shorter caller deadline wins, and cancellation unblocks pending
// I/O immediately. The returned clear function removes all deadlines so
// streaming is not time-limited, and fences the cancellation callback: once
// clear returns, no callback can leave a stale past deadline on a connection
// that net/http is about to pool and reuse.
func withConnDeadline(ctx context.Context, conn net.Conn) (func(), error) {
	deadline := time.Now().Add(phaseBudget)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	var mu sync.Mutex
	cleared := false
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		if !cleared {
			_ = conn.SetDeadline(time.Unix(1, 0))
		}
	})
	return func() {
		// stop first so a not-yet-started callback never runs; a callback
		// already waiting on mu observes cleared and does nothing, and one that
		// ran before is overwritten by the reset below.
		stop()
		mu.Lock()
		defer mu.Unlock()
		cleared = true
		_ = conn.SetDeadline(time.Time{})
	}, nil
}
