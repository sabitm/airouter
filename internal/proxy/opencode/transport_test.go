package opencode

import (
	"context"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/crypto/cryptobyte"
)

func freeTierRootPool(t *testing.T, srv *httptest.Server) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return pool
}

// freeTierTestTransport mirrors NewFreeTierTransport with a test CA pool so
// local TLS servers can be dialed.
func freeTierTestTransport(pool *x509.CertPool) *http.Transport {
	tr := NewFreeTierTransport()
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialFreeTierTLS(ctx, network, addr, pool)
	}
	return tr
}

func TestIsOfficialFreeTierURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://opencode.ai/inference/openai/v1/chat/completions", true},
		{"https://opencode.ai/inference/openai/v1/responses", true},
		{"https://opencode.ai/inference/anthropic/v1/messages", true},
		{"https://opencode.ai/inference/openai/v1/chat/completions/", true},
		// Legacy and previous inference paths stay accepted so a stored row or an
		// accepted official redirect is not downgraded to the standard transport.
		{"https://opencode.ai/inference/v1/chat/completions", true},
		{"https://opencode.ai/inference/v1/responses", true},
		{"https://opencode.ai/inference/v1/messages", true},
		{"https://opencode.ai/zen/v1/chat/completions", true},
		{"https://opencode.ai/zen/v1/responses", true},
		{"https://opencode.ai/zen/v1/messages", true},
		{"https://OPENCODE.AI/inference/v1/chat/completions", true},
		{"https://opencode.ai:443/inference/v1/chat/completions", true},
		// Custom ports, userinfo, and everything off the three endpoint paths
		// keep the standard transport.
		{"https://opencode.ai:8443/inference/v1/chat/completions", false},
		{"https://user:pass@opencode.ai/inference/v1/chat/completions", false},
		{"https://opencode.ai/inference/v1", false},
		{"https://opencode.ai/inference/v1/", false},
		{"https://opencode.ai/inference/v1/models", false},
		{"https://opencode.ai/inference/v1/chat/completions/extra", false},
		{"https://opencode.ai/zen/go/v1/chat/completions", false},
		{"https://opencode.ai/inference/go/openai/v1/chat/completions", false},
		{"https://opencode.ai/inference/go/anthropic/v1/messages", false},
		{"http://opencode.ai/inference/v1/chat/completions", false},
		{"https://opencode.example.com/inference/v1/chat/completions", false},
		{"https://mirror.test/v1/chat/completions", false},
		{"not a url", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsOfficialFreeTierURL(tc.url); got != tc.want {
			t.Errorf("IsOfficialFreeTierURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestFreeTierTransportRoundTrip(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Proto != "HTTP/1.1" {
			t.Errorf("negotiated proto = %s, want HTTP/1.1", r.Proto)
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	client := &http.Client{Transport: freeTierTestTransport(freeTierRootPool(t, srv))}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}

// TestFreeTierTransportStreamingNotBuffered proves the transport hands the
// live body through: the client sees the first chunk before the server writes
// the second.
func TestFreeTierTransportStreamingNotBuffered(t *testing.T) {
	var secondWrite atomic.Int64
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "data: one\n\n")
		flusher.Flush()
		<-release
		secondWrite.Store(time.Now().UnixNano())
		fmt.Fprint(w, "data: two\n\n")
		flusher.Flush()
	}))
	defer srv.Close()
	client := &http.Client{Transport: freeTierTestTransport(freeTierRootPool(t, srv))}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, len("data: one\n\n"))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	firstRead := time.Now().UnixNano()
	close(release)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != "data: two\n\n" {
		t.Fatalf("rest = %q", rest)
	}
	if firstRead > secondWrite.Load() {
		t.Fatal("first chunk was read after the server wrote the second: body was buffered")
	}
}

func TestFreeTierTransportRejectsUntrustedCert(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	// Empty pool: the server cert is unknown, so the handshake must fail.
	client := &http.Client{Transport: freeTierTestTransport(x509.NewCertPool())}
	if _, err := client.Get(srv.URL); err == nil {
		t.Fatal("handshake with untrusted certificate succeeded")
	}
}

func TestFreeTierDialHonorsContextCancellation(t *testing.T) {
	// A listener that accepts but never speaks: the TLS handshake blocks until
	// the context is canceled.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(done)
			return
		}
		defer conn.Close()
		<-done
	}()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = dialFreeTierTLS(ctx, "tcp", ln.Addr().String(), nil)
	if err == nil {
		t.Fatal("dial succeeded against a silent server")
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancellation did not unblock the handshake promptly")
	}
	close(done)
}

// connectProxy is a minimal HTTP CONNECT proxy for transport tests.
type connectProxy struct {
	srv      *httptest.Server
	connects atomic.Int64
	auth     atomic.Value
}

func newConnectProxy(t *testing.T) *connectProxy {
	t.Helper()
	cp := &connectProxy{}
	cp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "connect only", http.StatusMethodNotAllowed)
			return
		}
		cp.connects.Add(1)
		cp.auth.Store(r.Header.Get("Proxy-Authorization"))
		target, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, "dial failed", http.StatusBadGateway)
			return
		}
		defer target.Close()
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		client, _, err := hj.Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		done := make(chan struct{}, 2)
		go func() { io.Copy(target, client); done <- struct{}{} }()
		go func() { io.Copy(client, target); done <- struct{}{} }()
		<-done
	}))
	t.Cleanup(cp.srv.Close)
	return cp
}

func TestFreeTierTransportRoutesThroughProxy(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "through-proxy")
	}))
	defer srv.Close()
	cp := newConnectProxy(t)
	proxyURL, err := url.Parse(cp.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transportDialing(freeTierRootPool(t, srv), proxyURL)}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "through-proxy" {
		t.Fatalf("body = %q", body)
	}
	if cp.connects.Load() != 1 {
		t.Fatalf("proxy CONNECT count = %d, want 1", cp.connects.Load())
	}
	if got := cp.auth.Load(); got != "" {
		t.Fatalf("unexpected Proxy-Authorization without credentials: %q", got)
	}
}

func TestFreeTierTransportProxyCredentials(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	cp := newConnectProxy(t)
	proxyURL, err := url.Parse("http://user:pass@" + strings.TrimPrefix(cp.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transportDialing(freeTierRootPool(t, srv), proxyURL)}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want := "Basic " + basicAuth("user", "pass")
	if got := cp.auth.Load(); got != want {
		t.Fatalf("Proxy-Authorization = %q, want %q", got, want)
	}
}

// transportDialing mirrors NewFreeTierTransport with a test CA pool and a
// pinned proxy URL.
func transportDialing(pool *x509.CertPool, proxyURL *url.URL) *http.Transport {
	tr := freeTierTestTransport(pool)
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialFreeTierTLSWithProxy(ctx, network, addr, pool, proxyURL)
	}
	return tr
}

// TestProxyForHTTPSResolution checks port-aware environment proxy resolution
// including the standard localhost exclusion and NO_PROXY port matching.
func TestProxyForHTTPSResolution(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.test:8080")
	t.Setenv("NO_PROXY", "")
	u, err := proxyForHTTPS("example.test:443")
	if err != nil || u == nil || u.Host != "proxy.test:8080" {
		t.Fatalf("proxyForHTTPS(example.test:443) = %v, %v", u, err)
	}
	// A NO_PROXY entry that pins a port covers only that port.
	t.Setenv("NO_PROXY", "example.test:443")
	u, err = proxyForHTTPS("example.test:443")
	if err != nil || u != nil {
		t.Fatalf("NO_PROXY host:443 ignored: %v, %v", u, err)
	}
	u, err = proxyForHTTPS("example.test:8443")
	if err != nil || u == nil || u.Host != "proxy.test:8080" {
		t.Fatalf("NO_PROXY host:443 wrongly covered :8443: %v, %v", u, err)
	}
	// A host-only NO_PROXY entry covers every port of that host.
	t.Setenv("NO_PROXY", "example.test")
	u, err = proxyForHTTPS("example.test:8443")
	if err != nil || u != nil {
		t.Fatalf("NO_PROXY host-only ignored: %v, %v", u, err)
	}
	// httpproxy never proxies loopback targets.
	u, err = proxyForHTTPS("127.0.0.1:443")
	if err != nil || u != nil {
		t.Fatalf("loopback proxied: %v, %v", u, err)
	}
}

func TestFreeTierTransportProxyRefusalIsError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	// A proxy that answers CONNECT with 403 must fail the dial.
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer refusing.Close()
	proxyURL, err := url.Parse(refusing.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transportDialing(freeTierRootPool(t, srv), proxyURL)}
	if _, err := client.Get(srv.URL); err == nil {
		t.Fatal("dial through a refusing proxy succeeded")
	}
}

// These values come from the initial OpenCode 1.19.5 / Bun 1.4.4 capture.
// Keep literal IDs independent of the production spec so drift fails tests.
func assertNativeClientHelloSpec(t *testing.T, spec *utls.ClientHelloSpec) {
	t.Helper()
	wantCiphers := []uint16{
		0x1301, 0x1302, 0x1303,
		0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8,
		0xc009, 0xc013, 0xc00a, 0xc014,
		0x009c, 0x009d, 0x002f, 0x0035,
	}
	if !slices.Equal(spec.CipherSuites, wantCiphers) {
		t.Fatalf("cipher suites = %x, want %x", spec.CipherSuites, wantCiphers)
	}
	if !slices.Equal(spec.CompressionMethods, []byte{0}) {
		t.Fatalf("compression = %x, want [0]", spec.CompressionMethods)
	}
	wantExtTypes := []string{
		"*tls.SNIExtension",
		"*tls.ExtendedMasterSecretExtension",
		"*tls.RenegotiationInfoExtension",
		"*tls.SupportedCurvesExtension",
		"*tls.SupportedPointsExtension",
		"*tls.SessionTicketExtension",
		"*tls.ALPNExtension",
		"*tls.StatusRequestExtension",
		"*tls.SignatureAlgorithmsExtension",
		"*tls.SCTExtension",
		"*tls.KeyShareExtension",
		"*tls.PSKKeyExchangeModesExtension",
		"*tls.SupportedVersionsExtension",
	}
	if len(spec.Extensions) != len(wantExtTypes) {
		t.Fatalf("extension count = %d, want %d", len(spec.Extensions), len(wantExtTypes))
	}
	for i, wantType := range wantExtTypes {
		if gotType := fmt.Sprintf("%T", spec.Extensions[i]); gotType != wantType {
			t.Fatalf("extension %d = %s, want %s", i, gotType, wantType)
		}
	}
	curves := spec.Extensions[3].(*utls.SupportedCurvesExtension).Curves
	if !slices.Equal(curves, []utls.CurveID{4588, 29, 23, 24}) {
		t.Fatalf("supported groups = %v, want [4588 29 23 24]", curves)
	}
	points := spec.Extensions[4].(*utls.SupportedPointsExtension).SupportedPoints
	if !slices.Equal(points, []byte{0}) {
		t.Fatalf("point formats = %v, want [0]", points)
	}
	alpn := spec.Extensions[6].(*utls.ALPNExtension).AlpnProtocols
	if !slices.Equal(alpn, []string{"http/1.1"}) {
		t.Fatalf("ALPN = %v, want [http/1.1]", alpn)
	}
	signatures := spec.Extensions[8].(*utls.SignatureAlgorithmsExtension).SupportedSignatureAlgorithms
	wantSignatures := []utls.SignatureScheme{
		0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501,
		0x0806, 0x0601, 0x0807, 0x0603, 0x0201,
	}
	if !slices.Equal(signatures, wantSignatures) {
		t.Fatalf("signature algorithms = %x, want %x", signatures, wantSignatures)
	}
	shares := spec.Extensions[10].(*utls.KeyShareExtension).KeyShares
	if len(shares) != 2 || shares[0].Group != 4588 || shares[1].Group != 29 {
		t.Fatal("key shares must be X25519MLKEM768 followed by X25519")
	}
	modes := spec.Extensions[11].(*utls.PSKKeyExchangeModesExtension).Modes
	if !slices.Equal(modes, []byte{1}) {
		t.Fatalf("PSK modes = %v, want [1]", modes)
	}
	versions := spec.Extensions[12].(*utls.SupportedVersionsExtension).Versions
	if !slices.Equal(versions, []uint16{0x0304, 0x0303}) {
		t.Fatalf("supported versions = %x, want [0304 0303]", versions)
	}
}

func TestClientHelloSpecGolden(t *testing.T) {
	assertNativeClientHelloSpec(t, newClientHelloSpec())
}

func TestClientHelloWireGolden(t *testing.T) {
	client, server := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	conn := newFreeTierUConn(client, "opencode.ai", nil)
	done := make(chan error, 1)
	go func() { done <- conn.HandshakeContext(ctx) }()
	t.Cleanup(func() {
		cancel()
		client.Close()
		server.Close()
		<-done
	})
	if err := server.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 5)
	if _, err := io.ReadFull(server, header); err != nil {
		t.Fatal(err)
	}
	if header[0] != 22 || binary.BigEndian.Uint16(header[1:3]) != 0x0301 {
		t.Fatalf("TLS record header = %x, want handshake with version 0301", header)
	}
	payload := make([]byte, binary.BigEndian.Uint16(header[3:5]))
	if _, err := io.ReadFull(server, payload); err != nil {
		t.Fatal(err)
	}
	// Pin structure and lengths only; random values and public keys vary per connection.
	record := append(header, payload...)
	if len(record) != 1502 {
		t.Fatalf("ClientHello record length = %d, want 1502", len(record))
	}
	if payload[0] != 1 || int(payload[1])<<16|int(payload[2])<<8|int(payload[3]) != len(payload)-4 {
		t.Fatal("invalid ClientHello handshake framing")
	}
	spec, err := (&utls.Fingerprinter{}).RawClientHello(record)
	if err != nil {
		t.Fatal(err)
	}
	assertNativeClientHelloSpec(t, spec)

	hello := cryptobyte.String(payload[4:])
	var version uint16
	var session, ciphers, compression, extensions cryptobyte.String
	if !hello.ReadUint16(&version) || !hello.Skip(32) ||
		!hello.ReadUint8LengthPrefixed(&session) || !hello.ReadUint16LengthPrefixed(&ciphers) ||
		!hello.ReadUint8LengthPrefixed(&compression) || !hello.ReadUint16LengthPrefixed(&extensions) || !hello.Empty() {
		t.Fatal("invalid ClientHello fields")
	}
	if version != 0x0303 || len(session) != 32 {
		t.Fatalf("legacy version = %04x, session ID length = %d, want 0303 and 32", version, len(session))
	}
	for !extensions.Empty() {
		var kind uint16
		var data cryptobyte.String
		if !extensions.ReadUint16(&kind) || !extensions.ReadUint16LengthPrefixed(&data) {
			t.Fatal("invalid ClientHello extension framing")
		}
		switch kind {
		case 0:
			var names, name cryptobyte.String
			var nameType uint8
			if !data.ReadUint16LengthPrefixed(&names) || !data.Empty() ||
				!names.ReadUint8(&nameType) || !names.ReadUint16LengthPrefixed(&name) || !names.Empty() ||
				nameType != 0 || string(name) != "opencode.ai" {
				t.Fatal("SNI must contain only opencode.ai")
			}
		case 51:
			var shares cryptobyte.String
			if !data.ReadUint16LengthPrefixed(&shares) || !data.Empty() {
				t.Fatal("invalid key-share vector")
			}
			var got [][2]int
			for !shares.Empty() {
				var group uint16
				var key cryptobyte.String
				if !shares.ReadUint16(&group) || !shares.ReadUint16LengthPrefixed(&key) {
					t.Fatal("invalid key-share entry")
				}
				got = append(got, [2]int{int(group), len(key)})
			}
			want := [][2]int{{4588, 1216}, {29, 32}}
			if !slices.Equal(got, want) {
				t.Fatalf("key-share groups and lengths = %v, want %v", got, want)
			}
		}
	}
}

func TestBasicAuthAndProxyAddr(t *testing.T) {
	if got := basicAuth("user", "pass"); got != "dXNlcjpwYXNz" {
		t.Fatalf("basicAuth = %q", got)
	}
	if got := canonicalProxyAddr(&url.URL{Scheme: "http", Host: "p.test"}); got != "p.test:80" {
		t.Fatalf("addr = %q", got)
	}
	if got := canonicalProxyAddr(&url.URL{Scheme: "https", Host: "p.test"}); got != "p.test:443" {
		t.Fatalf("addr = %q", got)
	}
	if got := canonicalProxyAddr(&url.URL{Scheme: "http", Host: "p.test:8080"}); got != "p.test:8080" {
		t.Fatalf("addr = %q", got)
	}
}

// TestWithConnDeadlineCleanupAfterCancellation pins the cleanup invariant:
// after clear returns, no cancellation callback can leave a past deadline on
// the connection, which net/http is about to pool and reuse.
func TestWithConnDeadlineCleanupAfterCancellation(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clear, err := withConnDeadline(ctx, c1)
	if err != nil {
		t.Fatal(err)
	}
	cancel() // callback fires and sets the unblock deadline
	clear()
	// A read with no writer must keep blocking: any deadline left behind would
	// return an immediate timeout error.
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := c1.Read(buf)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("read on cleaned-up connection returned %v; deadline was not cleared", err)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestWithConnDeadlineCancellationUnblocks proves the callback still breaks a
// pending blocking phase when the caller context is canceled.
func TestWithConnDeadlineCancellationUnblocks(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clear, err := withConnDeadline(ctx, c1)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	buf := make([]byte, 1)
	_, err = c1.Read(buf) // blocks until the callback sets the unblock deadline
	clear()
	if err == nil {
		t.Fatal("read succeeded with no peer writing")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancellation did not unblock pending I/O: %v", elapsed)
	}
}

// TestFreeTierRedirectPolicy pins the client redirect policy at function level:
// only official free-tier URLs may be followed, so an allowed-redirect case
// never dials the real endpoint in tests.
func TestFreeTierRedirectPolicy(t *testing.T) {
	official := "https://opencode.ai/inference/v1/chat/completions"
	u, err := url.Parse(official)
	if err != nil {
		t.Fatal(err)
	}
	if err := freeTierRedirectPolicy(&http.Request{URL: u}, nil); err != nil {
		t.Fatalf("redirect to %s refused: %v", official, err)
	}
	for _, bad := range []string{
		"https://mirror.test/inference/v1/chat/completions",
		"http://opencode.ai/inference/v1/chat/completions",
		"https://opencode.ai:8443/inference/v1/chat/completions",
		"https://user:pass@opencode.ai/inference/v1/chat/completions",
		"https://opencode.ai/zen/go/v1/chat/completions",
		"https://opencode.ai/inference/v1/models",
	} {
		u, err := url.Parse(bad)
		if err != nil {
			t.Fatal(err)
		}
		if err := freeTierRedirectPolicy(&http.Request{URL: u}, nil); err == nil {
			t.Fatalf("redirect to %s was allowed", bad)
		}
	}
}

// TestFreeTierClientRefusesCrossHostRedirect proves the refusal happens before
// the follow-up request is sent: the redirect target never receives a request.
func TestFreeTierClientRefusesCrossHostRedirect(t *testing.T) {
	var evilHits atomic.Int64
	evil := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilHits.Add(1)
	}))
	defer evil.Close()
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL, http.StatusFound)
	}))
	defer target.Close()
	pool := freeTierRootPool(t, target)
	pool.AddCert(evil.Certificate())
	client := &http.Client{Transport: freeTierTestTransport(pool), CheckRedirect: freeTierRedirectPolicy}
	_, err := client.Post(target.URL, "application/json", strings.NewReader("{}"))
	if err == nil {
		t.Fatal("cross-host redirect was followed")
	}
	if evilHits.Load() != 0 {
		t.Fatalf("redirect target received %d requests", evilHits.Load())
	}
}

// TestFreeTierClientRefusesDowngradeRedirect proves an https-to-http redirect
// is refused before dialing; the http target would otherwise receive the
// relayed request outside the approved endpoint scheme.
func TestFreeTierClientRefusesDowngradeRedirect(t *testing.T) {
	var plainHits atomic.Int64
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
	}))
	defer plain.Close()
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A downgrade the policy must catch by scheme alone.
		http.Redirect(w, r, "http://opencode.ai/inference/v1/chat/completions", http.StatusFound)
	}))
	defer target.Close()
	client := &http.Client{Transport: freeTierTestTransport(freeTierRootPool(t, target)), CheckRedirect: freeTierRedirectPolicy}
	_, err := client.Post(target.URL, "application/json", strings.NewReader("{}"))
	if err == nil {
		t.Fatal("downgrade redirect was followed")
	}
	if plainHits.Load() != 0 {
		t.Fatalf("plain target received %d requests", plainHits.Load())
	}
}

// TestFreeTierRedirectPolicyRedirectLimit pins Go's default 10-request bound:
// a non-nil CheckRedirect disables net/http's built-in stop, so the policy
// itself must reject at len(via) >= 10 or an official-endpoint redirect loop
// could spin forever.
func TestFreeTierRedirectPolicyRedirectLimit(t *testing.T) {
	u, err := url.Parse("https://opencode.ai/inference/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	req := &http.Request{Method: http.MethodPost, URL: u}
	for n := 0; n < maxFreeTierRedirects; n++ {
		via := make([]*http.Request, n)
		for i := range via {
			via[i] = req
		}
		if err := freeTierRedirectPolicy(req, via); err != nil {
			t.Fatalf("redirect after %d hop(s) refused: %v", n, err)
		}
	}
	for n := maxFreeTierRedirects; n <= maxFreeTierRedirects+1; n++ {
		via := make([]*http.Request, n)
		for i := range via {
			via[i] = req
		}
		if err := freeTierRedirectPolicy(req, via); err == nil {
			t.Fatalf("redirect after %d hop(s) was allowed: default Go redirect limit removed", n)
		}
	}
}

// fakeRedirectRoundTripper answers every request with a redirect back to the
// same official URL, so the client's redirect loop runs fully in memory. Past
// cap sends it returns a terminal 200: if the policy regresses to unbounded
// following, the loop still ends instead of hanging the suite.
type fakeRedirectRoundTripper struct {
	official string
	cap      int64
	sends    atomic.Int64
}

func (rt *fakeRedirectRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if rt.sends.Add(1) > rt.cap {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    r,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusFound,
		Status:     "302 Found",
		Header:     http.Header{"Location": {rt.official}},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    r,
	}, nil
}

// TestFreeTierClientOfficialRedirectLoopBounded proves the client stops a
// self-redirect loop on the official endpoint after Go's default 10 requests:
// the initial send plus 9 followed redirects, with the 10th redirect refused
// and surfaced as an error. The fake RoundTripper keeps the loop off the
// network; the context timeout is a backstop against hangs.
func TestFreeTierClientOfficialRedirectLoopBounded(t *testing.T) {
	official := "https://opencode.ai/inference/v1/chat/completions"
	rt := &fakeRedirectRoundTripper{official: official, cap: maxFreeTierRedirects + 10}
	client := &http.Client{Transport: rt, CheckRedirect: freeTierRedirectPolicy}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, official, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("official-endpoint redirect loop was not stopped")
	}
	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("error = %v, want redirect-limit error", err)
	}
	if got := rt.sends.Load(); got != maxFreeTierRedirects {
		t.Fatalf("sent %d requests, want %d", got, maxFreeTierRedirects)
	}
}

// lowerPhaseBudget shortens the dial-phase budget for deterministic tests of
// silent peers and returns a restore function.
func lowerPhaseBudget(t *testing.T, d time.Duration) func() {
	t.Helper()
	old := phaseBudget
	phaseBudget = d
	return func() { phaseBudget = old }
}

// socksProxy is a minimal SOCKS5 server for transport tests. In silent mode it
// accepts the TCP connection and never answers, so negotiation hangs unless
// the client bounds it. holdDone closes when a held connection is released.
type socksProxy struct {
	ln       net.Listener
	silent   bool
	connects atomic.Int64
	holdDone chan struct{}
}

func newSOCKSProxy(t *testing.T, silent bool) *socksProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &socksProxy{ln: ln, silent: silent, holdDone: make(chan struct{})}
	go p.serve()
	t.Cleanup(func() { ln.Close() })
	return p
}

func (p *socksProxy) addr() string { return p.ln.Addr().String() }

func (p *socksProxy) serve() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.connects.Add(1)
		if p.silent {
			go func(c net.Conn) {
				defer close(p.holdDone)
				defer c.Close()
				// Discard client bytes; never answer. Read unblocks when the
				// client releases the connection.
				buf := make([]byte, 32)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(conn)
			continue
		}
		go p.handle(conn)
	}
}

func (p *socksProxy) handle(conn net.Conn) {
	defer conn.Close()
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	if _, err := io.ReadFull(conn, make([]byte, int(head[1]))); err != nil {
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = string(b)
	case 0x04:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	default:
		return
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return
	}
	target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))))
	if err != nil {
		conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer target.Close()
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(target, conn); done <- struct{}{} }()
	go func() { io.Copy(conn, target); done <- struct{}{} }()
	<-done
}

// TestSOCKSNegotiationBoundedWithoutCallerDeadline pins the silent-proxy fix:
// with no caller deadline (as http.Transport hands pooled dials), the SOCKS
// negotiation must still fail within the phase budget.
func TestSOCKSNegotiationBoundedWithoutCallerDeadline(t *testing.T) {
	defer lowerPhaseBudget(t, 250*time.Millisecond)()
	p := newSOCKSProxy(t, true)
	socksURL, err := url.Parse("socks5://" + p.addr())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = dialFreeTierTLSWithProxy(context.Background(), "tcp", "opencode.ai:443", nil, socksURL)
	if err == nil {
		t.Fatal("dial through a silent SOCKS proxy succeeded")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("silent SOCKS negotiation not bounded: %v", elapsed)
	}
	// The failed dial must release its connection: the proxy's hold goroutine
	// exits once the client side closes.
	select {
	case <-p.holdDone:
	case <-time.After(2 * time.Second):
		t.Fatal("connection to the silent proxy was leaked")
	}
}

// TestSOCKSNegotiationHonorsCancellation proves caller cancellation unblocks
// the negotiation even though the phase budget is still in the future.
func TestSOCKSNegotiationHonorsCancellation(t *testing.T) {
	// A 5s budget backstops the assertion if cancellation regressed.
	defer lowerPhaseBudget(t, 5*time.Second)()
	p := newSOCKSProxy(t, true)
	socksURL, err := url.Parse("socks5://" + p.addr())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = dialFreeTierTLSWithProxy(ctx, "tcp", "opencode.ai:443", nil, socksURL)
	if err == nil {
		t.Fatal("dial through a silent SOCKS proxy succeeded")
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancellation did not unblock the negotiation promptly: %v", elapsed)
	}
}

// TestFreeTierTransportThroughSOCKSProxy covers a full request through a
// working SOCKS5 proxy and, by reusing the pooled connection after the phase
// budget has elapsed, that handshake deadlines were cleared for streaming.
func TestFreeTierTransportThroughSOCKSProxy(t *testing.T) {
	defer lowerPhaseBudget(t, 300*time.Millisecond)()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "via-socks")
	}))
	defer srv.Close()
	p := newSOCKSProxy(t, false)
	socksURL, err := url.Parse("socks5://" + p.addr())
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transportDialing(freeTierRootPool(t, srv), socksURL)}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "via-socks" {
		t.Fatalf("body = %q", body)
	}
	if p.connects.Load() != 1 {
		t.Fatalf("proxy connects = %d, want 1", p.connects.Load())
	}
	// A leaked phase deadline would expire while the connection sits idle and
	// fail the reused connection's next read.
	time.Sleep(400 * time.Millisecond)
	resp2, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("reused connection after phase budget failed: %v", err)
	}
	resp2.Body.Close()
	if p.connects.Load() != 1 {
		t.Fatalf("second request dialed a new connection (%d connects), reuse expected", p.connects.Load())
	}
}

// TestFreeTierTransportConcurrentReuse exercises pooled connection reuse under
// concurrency with -race.
func TestFreeTierTransportConcurrentReuse(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	client := &http.Client{Transport: freeTierTestTransport(freeTierRootPool(t, srv))}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get(srv.URL)
			if err != nil {
				t.Errorf("request: %v", err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
}

// TestIsOfficialFreeTierURLAllFamilies pins every accepted official path and
// the current Go and family-prefix exclusions in one place.
func TestIsOfficialFreeTierURLAllFamilies(t *testing.T) {
	allowed := []string{
		"https://opencode.ai/inference/openai/v1/chat/completions",
		"https://opencode.ai/inference/openai/v1/responses",
		"https://opencode.ai/inference/anthropic/v1/messages",
		"https://opencode.ai/inference/v1/chat/completions",
		"https://opencode.ai/inference/v1/responses",
		"https://opencode.ai/inference/v1/messages",
		"https://opencode.ai/zen/v1/chat/completions",
		"https://opencode.ai/zen/v1/responses",
		"https://opencode.ai/zen/v1/messages",
	}
	for _, raw := range allowed {
		if !IsOfficialFreeTierURL(raw) {
			t.Errorf("official free-tier path rejected: %s", raw)
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := freeTierRedirectPolicy(&http.Request{URL: u}, nil); err != nil {
			t.Errorf("official redirect refused: %s: %v", raw, err)
		}
	}
	refused := []string{
		"https://opencode.ai/inference/go/openai/v1/chat/completions",
		"https://opencode.ai/inference/go/openai/v1/responses",
		"https://opencode.ai/inference/go/anthropic/v1/messages",
		"https://opencode.ai/zen/go/v1/chat/completions",
		"https://opencode.ai/inference/openai/v1",
		"https://opencode.ai/inference/anthropic/v1",
		"https://opencode.ai/inference/openai/v1/models",
		"https://opencode.ai/inference/v1/models",
		"https://opencode.ai/inference/go/v1/models",
	}
	for _, raw := range refused {
		if IsOfficialFreeTierURL(raw) {
			t.Errorf("non-free-tier path accepted: %s", raw)
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := freeTierRedirectPolicy(&http.Request{URL: u}, nil); err == nil {
			t.Errorf("non-free-tier redirect allowed: %s", raw)
		}
	}
}
