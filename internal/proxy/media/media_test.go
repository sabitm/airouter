package media

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/proxy/ir"
)

var (
	png1x1  = mustDecode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVQI12P4z8AAAAMBAQAY3Y20AAAAAElFTkSuQmCC")
	pdfMin  = []byte("%PDF-1.4 minimal\n")
	jpegMin = []byte{0xFF, 0xD8, 0xFF, 0xD9}
)

func mustDecode(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestParseDataURL(t *testing.T) {
	du, err := ParseDataURL("data:image/png;base64," + EncodeBase64(png1x1))
	if err != nil {
		t.Fatal(err)
	}
	if du.MediaType != "image/png" || du.Data == "" || !du.Base64 {
		t.Fatalf("got %+v", du)
	}
	if _, err := ParseDataURL("data:image/png,notbase64"); err == nil {
		t.Fatal("expected error for non-base64 data URL")
	}
	if _, err := ParseDataURL("data:image/png;base64"); err == nil {
		t.Fatal("expected error for missing comma")
	}
}

func TestValidateImageBytes(t *testing.T) {
	if mt, err := ValidateImageBytes(png1x1, "image/png"); err != nil || mt != "image/png" {
		t.Fatalf("png: mt=%q err=%v", mt, err)
	}
	if _, err := ValidateImageBytes(png1x1, "image/jpeg"); err == nil {
		t.Fatal("expected mismatch for png bytes labeled jpeg")
	}
	if _, err := ValidateImageBytes(pdfMin, "image/png"); err == nil {
		t.Fatal("expected mismatch for pdf labeled png")
	}
	// Random bytes falsely labeled image/png must fail (magic required).
	if _, err := ValidateImageBytes([]byte("not-an-image"), "image/png"); err == nil {
		t.Fatal("expected spoofed image/png rejection")
	}
	if _, err := ValidateImageBytes([]byte("not-an-image"), ""); err == nil {
		t.Fatal("expected unsupported without magic")
	}
	// Valid JPEG bytes mislabeled as PNG.
	if _, err := ValidateImageBytes(jpegMin, "image/png"); err == nil {
		t.Fatal("expected jpeg-as-png mismatch")
	}
	// Valid PNG bytes mislabeled as PDF via image path.
	if _, err := ValidateImageBytes(png1x1, "application/pdf"); err == nil {
		t.Fatal("expected png-as-pdf rejection")
	}
}

func TestValidatePDFBytes(t *testing.T) {
	if err := ValidatePDFBytes(pdfMin); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePDFBytes(png1x1); err == nil {
		t.Fatal("expected non-pdf rejection")
	}
}

func TestInspectRequestAcceptsManySmall(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser}}}
	for i := 0; i < 9; i++ {
		req.Messages[0].Content = append(req.Messages[0].Content, ir.ContentBlock{
			Type:  ir.BlockImage,
			Image: &ir.Image{MediaType: "image/png", Data: EncodeBase64(png1x1)},
		})
	}
	atts, err := InspectRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 9 {
		t.Fatalf("len=%d want 9", len(atts))
	}
}

func TestInspectRequestSingleTooLarge(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{
		{Type: ir.BlockFile, File: &ir.File{Filename: "blob.bin", MediaType: "application/octet-stream", Data: EncodeBase64(make([]byte, MaxAttachmentBytes+1))}},
	}}}}
	_, err := InspectRequest(req)
	if err == nil || !errors.Is(err, ErrAttachmentTooLarge) {
		t.Fatalf("err=%v want ErrAttachmentTooLarge", err)
	}
}

func TestInspectRequestTotalBudget(t *testing.T) {
	per := MaxAttachmentBytes
	under := MaxAttachmentTotalBytes / per
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser}}}
	chunk := EncodeBase64(make([]byte, per))
	file := func() ir.ContentBlock {
		return ir.ContentBlock{
			Type: ir.BlockFile,
			File: &ir.File{Filename: "blob.bin", MediaType: "application/octet-stream", Data: chunk},
		}
	}
	for i := 0; i < under; i++ {
		req.Messages[0].Content = append(req.Messages[0].Content, file())
	}
	if _, err := InspectRequest(req); err != nil {
		t.Fatalf("under budget: %v", err)
	}
	req.Messages[0].Content = append(req.Messages[0].Content, file())
	_, err := InspectRequest(req)
	if err == nil || !errors.Is(err, ErrAttachmentBudgetExceeded) {
		t.Fatalf("err=%v want ErrAttachmentBudgetExceeded", err)
	}
	if ClientErrorStatus(err) != 413 {
		t.Fatalf("status=%d want 413", ClientErrorStatus(err))
	}
}

func TestInspectRequestFileIDZeroCost(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser}}}
	for i := 0; i < 9; i++ {
		req.Messages[0].Content = append(req.Messages[0].Content, ir.ContentBlock{
			Type: ir.BlockFile,
			File: &ir.File{ID: "file-abc", MediaType: "application/pdf"},
		})
	}
	atts, err := InspectRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 9 {
		t.Fatalf("len=%d want 9", len(atts))
	}
	for i, a := range atts {
		if a.Bytes != 0 || !a.HasID {
			t.Fatalf("att[%d]=%+v want zero-byte file id", i, a)
		}
	}
}

func TestInspectRequestPDFAndFile(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{
		Role: ir.RoleUser,
		Content: []ir.ContentBlock{
			{Type: ir.BlockFile, File: &ir.File{Filename: "a.pdf", MediaType: "application/pdf", Data: EncodeBase64(pdfMin)}},
			{Type: ir.BlockFile, File: &ir.File{Filename: "note.txt", MediaType: "text/plain", Data: EncodeBase64([]byte("hi"))}},
		},
	}}}
	atts, err := InspectRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 2 || atts[0].Kind != KindPDF || atts[1].Kind != KindGeneric {
		t.Fatalf("atts=%+v", atts)
	}
}

func TestCapsIncompatible(t *testing.T) {
	img := []Attachment{{Kind: KindImage, IsImage: true, HasData: true, MediaType: "image/png"}}
	pdf := []Attachment{{Kind: KindPDF, HasData: true, MediaType: "application/pdf"}}
	fileID := []Attachment{{Kind: KindGeneric, HasID: true}}
	nestedImg := []Attachment{{Kind: KindImage, IsImage: true, HasData: true, MediaType: "image/png", InToolResult: true}}
	if reason := CapsForCodecID("qoder").Incompatible(img, true); reason == "" {
		t.Fatal("qoder should reject images")
	}
	if reason := CapsForCodecID("oai-codex").Incompatible(pdf, true); reason == "" {
		t.Fatal("codex should reject pdf")
	}
	if reason := CapsForCodecID("anth-msg").Incompatible(pdf, true); reason != "" {
		t.Fatalf("anthropic should accept pdf: %s", reason)
	}
	if reason := CapsForCodecID("oai-chat").Incompatible(pdf, true); reason != "" {
		t.Fatalf("oai-chat should accept inline pdf via file_data: %s", reason)
	}
	if reason := CapsForCodecID("oai-responses").Incompatible(pdf, true); reason != "" {
		t.Fatalf("oai-responses should accept inline pdf: %s", reason)
	}
	if reason := CapsForCodecID("oai-responses").Incompatible([]Attachment{{Kind: KindPDF, HasURL: true}}, true); reason != "" {
		t.Fatalf("oai-responses should accept pdf url: %s", reason)
	}
	if reason := CapsForCodecID("oai-chat").Incompatible(fileID, true); reason == "" {
		t.Fatal("translated file id should fail")
	}
	if reason := CapsForCodecID("oai-chat").Incompatible(fileID, false); reason != "" {
		t.Fatalf("passthrough file id should be ok: %s", reason)
	}
	if reason := CapsForCodecID("kiro").Incompatible([]Attachment{{Kind: KindImage, IsImage: true, HasURL: true}}, true); reason != "" {
		t.Fatalf("kiro should accept URL images via materialize: %s", reason)
	}
	if reason := CapsForCodecID("oai-chat").Incompatible(nestedImg, true); reason == "" {
		t.Fatal("oai-chat must reject nested tool_result media")
	}
	if reason := CapsForCodecID("oai-responses").Incompatible(nestedImg, true); reason == "" {
		t.Fatal("oai-responses must reject nested tool_result media")
	}
	if reason := CapsForCodecID("kiro").Incompatible(nestedImg, true); reason == "" {
		t.Fatal("kiro must reject nested tool_result media")
	}
	if reason := CapsForCodecID("antigravity").Incompatible(nestedImg, true); reason == "" {
		t.Fatal("antigravity must reject nested tool_result media")
	}
	if reason := CapsForCodecID("anth-msg").Incompatible(nestedImg, true); reason != "" {
		t.Fatalf("anthropic should accept nested tool_result media: %s", reason)
	}
	if reason := CapsForCodecID("claude-code").Incompatible(nestedImg, true); reason != "" {
		t.Fatalf("claude-code should accept nested tool_result media: %s", reason)
	}
}

func TestInspectRequestToolResultPlacement(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentBlock{
			{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: EncodeBase64(png1x1)}},
		}},
		{Role: ir.RoleUser, Content: []ir.ContentBlock{
			{Type: ir.BlockToolResult, ToolUseID: "t1", ToolResult: []ir.ContentBlock{
				{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: EncodeBase64(png1x1)}},
				{Type: ir.BlockFile, File: &ir.File{Filename: "a.pdf", MediaType: "application/pdf", Data: EncodeBase64(pdfMin)}},
			}},
		}},
	}}
	atts, err := InspectRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 3 {
		t.Fatalf("len=%d want 3", len(atts))
	}
	if atts[0].InToolResult {
		t.Fatal("top-level image should not be InToolResult")
	}
	if !atts[1].InToolResult || !atts[2].InToolResult {
		t.Fatalf("nested media missing InToolResult: %+v", atts)
	}
}

func TestInspectEmptyAttachment(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{
		{Type: ir.BlockImage, Image: &ir.Image{}},
	}}}}
	if _, err := InspectRequest(req); err == nil {
		t.Fatal("expected empty image rejection")
	}
}

func TestInspectRemoteURLSyntax(t *testing.T) {
	bad := []string{
		"file:///etc/passwd",
		"ftp://example.com/a.png",
		"http://user:pass@example.com/a.png",
		"http:///nohost",
		"https://",
		"not-a-url",
	}
	for _, u := range bad {
		req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{
			{Type: ir.BlockImage, Image: &ir.Image{URL: u}},
		}}}}
		if _, err := InspectRequest(req); err == nil {
			t.Fatalf("expected reject for %q", u)
		}
	}
	reqOK := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{
		{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/a.png"}},
	}}}}
	atts, err := InspectRequest(reqOK)
	if err != nil || len(atts) != 1 || !atts[0].HasURL {
		t.Fatalf("valid https rejected: atts=%+v err=%v", atts, err)
	}
	// File remote URL syntax is also checked.
	reqFileBad := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{
		{Type: ir.BlockFile, File: &ir.File{URL: "file:///tmp/a.pdf", MediaType: "application/pdf"}},
	}}}}
	if _, err := InspectRequest(reqFileBad); err == nil {
		t.Fatal("expected file:// document URL reject")
	}
}

func TestInspectMultipleSources(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{
		Role: ir.RoleUser,
		Content: []ir.ContentBlock{
			{Type: ir.BlockFile, File: &ir.File{Data: EncodeBase64(pdfMin), ID: "file-1", MediaType: "application/pdf"}},
		},
	}}}
	if _, err := InspectRequest(req); err == nil {
		t.Fatal("expected multiple source rejection")
	}
	req2 := &ir.Request{Messages: []ir.Message{{
		Role: ir.RoleUser,
		Content: []ir.ContentBlock{
			{Type: ir.BlockImage, Image: &ir.Image{Data: EncodeBase64(png1x1), URL: "https://example.com/a.png", MediaType: "image/png"}},
		},
	}}}
	if _, err := InspectRequest(req2); err == nil {
		t.Fatal("expected image multi-source rejection")
	}
}

func TestInspectSpoofedImage(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{{
		Role: ir.RoleUser,
		Content: []ir.ContentBlock{
			{Type: ir.BlockImage, Image: &ir.Image{MediaType: "image/png", Data: EncodeBase64([]byte("random-bytes-not-png"))}},
		},
	}}}
	if _, err := InspectRequest(req); err == nil {
		t.Fatal("expected spoofed png rejection")
	}
}

func TestUnsafeIP(t *testing.T) {
	cases := []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "169.254.169.254", "0.0.0.0", "100.64.0.1", "::1", "fc00::1", "2001:db8::1"}
	for _, s := range cases {
		if !unsafeIP(net.ParseIP(s)) {
			t.Errorf("%s should be unsafe", s)
		}
	}
	if unsafeIP(net.ParseIP("8.8.8.8")) {
		t.Error("8.8.8.8 should be safe")
	}
}

func TestParseHTTPURL(t *testing.T) {
	if _, err := ParseHTTPURL("http://user:pass@example.com/x"); err == nil {
		t.Fatal("expected userinfo reject")
	}
	if _, err := ParseHTTPURL("file:///etc/passwd"); err == nil {
		t.Fatal("expected scheme reject")
	}
	if _, err := ParseHTTPURL("ftp://example.com/a.png"); err == nil {
		t.Fatal("expected ftp reject")
	}
	if _, err := ParseHTTPURL("http://127.0.0.1/x"); err == nil {
		t.Fatal("expected loopback reject")
	}
	if _, err := ParseHTTPURL("http:///nohost"); err == nil {
		t.Fatal("expected missing host reject")
	}
	if _, err := ParseHTTPURL("https://"); err == nil {
		t.Fatal("expected empty https host reject")
	}
	if _, err := ParseHTTPURL("https://example.com/a.png"); err != nil {
		t.Fatal(err)
	}
}

type staticResolver struct {
	ips []net.IP
	err error
}

func (s staticResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make([]net.IPAddr, len(s.ips))
	for i, ip := range s.ips {
		out[i] = net.IPAddr{IP: ip}
	}
	return out, nil
}

func TestFetchImageSafe(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/redir" {
			http.Redirect(w, r, "/img.png", http.StatusFound)
			return
		}
		if r.URL.Path == "/big" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(make([]byte, MaxAttachmentBytes+10))
			return
		}
		if r.URL.Path == "/bad" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(w, "not-image")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png1x1)
	}))
	t.Cleanup(srv.Close)

	// httptest uses 127.0.0.1 — production ParseHTTPURL rejects it. Tests inject a
	// client that talks to the server and a resolver that claims a public IP so
	// the SSRF host check on the URL string is bypassed by using the server URL
	// only through the injected client path... Actually ParseHTTPURL still sees
	// 127.0.0.1. Use a custom host via srv.Listener and rewrite:
	// Simpler: unit-test download with Client pointed at srv and URL that
	// ParseHTTPURL accepts by using example.com + custom transport dialing srv.

	f := &Fetcher{
		Resolver: staticResolver{ips: []net.IP{net.ParseIP("8.8.8.8")}},
		Client:   srv.Client(),
	}
	// srv.URL is http://127.0.0.1:port — rejected by ParseHTTPURL.
	// Build a public-looking URL and route via custom transport to the real server.
	u := strings.Replace(srv.URL, "127.0.0.1", "example.com", 1)
	u = strings.Replace(u, "localhost", "example.com", 1)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial(network, strings.TrimPrefix(srv.URL, "http://"))
		},
	}
	f.Client = &http.Client{Transport: tr, Timeout: 5 * time.Second}

	res, err := f.FetchImage(context.Background(), u+"/img.png")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.MediaType != "image/png" || res.Data == "" {
		t.Fatalf("res=%+v", res)
	}
	// Dedupe success
	if _, err := f.FetchImage(context.Background(), u+"/img.png"); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d want 1 (deduped)", hits.Load())
	}

	if _, err := f.FetchImage(context.Background(), u+"/redir"); err == nil {
		t.Fatal("expected redirect reject")
	}
	// Failed fetch is also cached (redirect path counted once).
	redirHitsBefore := hits.Load()
	if _, err := f.FetchImage(context.Background(), u+"/redir"); err == nil {
		t.Fatal("expected cached redirect reject")
	}
	if hits.Load() != redirHitsBefore {
		t.Fatalf("failed fetch not deduped: hits %d -> %d", redirHitsBefore, hits.Load())
	}
	if _, err := f.FetchImage(context.Background(), u+"/bad"); err == nil {
		t.Fatal("expected bad magic reject")
	}
	if _, err := f.FetchImage(context.Background(), u+"/big"); err == nil {
		t.Fatal("expected oversized reject")
	}
}

func TestFetchImageContentTypeMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/plain":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write(png1x1)
		case "/octet":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(png1x1)
		case "/emptyct":
			_, _ = w.Write(png1x1)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	u := strings.Replace(srv.URL, "127.0.0.1", "example.com", 1)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial(network, strings.TrimPrefix(srv.URL, "http://"))
		},
	}
	f := &Fetcher{
		Resolver: staticResolver{ips: []net.IP{net.ParseIP("8.8.8.8")}},
		Client:   &http.Client{Transport: tr, Timeout: 5 * time.Second},
	}
	if _, err := f.FetchImage(context.Background(), u+"/plain"); err == nil {
		t.Fatal("text/plain + png bytes must fail")
	}
	if res, err := f.FetchImage(context.Background(), u+"/octet"); err != nil || res.MediaType != "image/png" {
		t.Fatalf("octet-stream should accept via magic: res=%+v err=%v", res, err)
	}
	if res, err := f.FetchImage(context.Background(), u+"/emptyct"); err != nil || res.MediaType != "image/png" {
		t.Fatalf("empty ct should accept via magic: res=%+v err=%v", res, err)
	}
}

func TestFetchImagePrivateDNS(t *testing.T) {
	f := &Fetcher{Resolver: staticResolver{ips: []net.IP{net.ParseIP("10.0.0.5")}}}
	if _, err := f.FetchImage(context.Background(), "https://evil.example/a.png"); err == nil {
		t.Fatal("expected private DNS reject")
	}
	// Mixed public+private fails closed.
	f = &Fetcher{Resolver: staticResolver{ips: []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("127.0.0.1")}}}
	if _, err := f.FetchImage(context.Background(), "https://evil.example/a.png"); err == nil {
		t.Fatal("expected mixed DNS reject")
	}
}

func TestFetchImageCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = w.Write(png1x1)
	}))
	t.Cleanup(srv.Close)
	u := strings.Replace(srv.URL, "127.0.0.1", "example.com", 1)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial(network, strings.TrimPrefix(srv.URL, "http://"))
		},
	}
	f := &Fetcher{
		Resolver: staticResolver{ips: []net.IP{net.ParseIP("1.1.1.1")}},
		Client:   &http.Client{Transport: tr},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.FetchImage(ctx, u+"/img.png"); err == nil {
		t.Fatal("expected cancel error")
	}
}

func TestAllIPsSafeEmpty(t *testing.T) {
	if err := AllIPsSafe(nil); err == nil {
		t.Fatal("expected empty reject")
	}
}
