package anthropic

import (
	"encoding/json"
	"regexp"
	"strings"

	"airouter/internal/proxy/ir"
)

// billingHeaderRe matches the x-anthropic-billing-header telemetry marker that
// Claude Code smuggles into the start of the system prompt body (not just as an
// HTTP header). Internal fields are "; "-separated; the terminating ";" is not
// followed by a space, which is the boundary before the real prompt text.
var billingHeaderRe = regexp.MustCompile(`^x-anthropic-billing-header:(?:[^;]*; )*[^;]*;`)

// clientIdentityOpener is the canonical Claude Code system-prompt opening line.
// Gateways fingerprint it to reject the client; we drop it (and its trailing
// newline) so translated requests do not advertise the client identity. Only
// the anchored leading occurrence is removed, so incidental mentions elsewhere
// in the prompt are preserved.
const clientIdentityOpener = "You are Claude Code, Anthropic's official CLI for Claude."

// systemToText flattens the Anthropic system field (string or array of text
// blocks) to plain text, stripping the smuggled billing-header marker and the
// client identity opener so neither leaks Claude Code identity into translated
// backend requests.
func systemToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return stripClientIdentity(s)
	}
	var blocks []anthBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return stripClientIdentity(b.String())
}

func stripClientIdentity(s string) string {
	if loc := billingHeaderRe.FindStringIndex(s); loc != nil {
		s = strings.TrimLeft(s[loc[1]:], " ")
	}
	if rest, ok := strings.CutPrefix(s, clientIdentityOpener); ok {
		s = strings.TrimPrefix(rest, "\n")
	}
	return s
}

func imageFromSource(s *anthSource) *ir.Image {
	if s == nil {
		return &ir.Image{}
	}
	if s.Type == "url" {
		return &ir.Image{URL: s.URL}
	}
	return &ir.Image{MediaType: s.MediaType, Data: s.Data}
}

func sourceFromImage(img *ir.Image) *anthSource {
	if img == nil {
		return &anthSource{}
	}
	if img.Data != "" {
		mt := img.MediaType
		if mt == "" {
			mt = "image/png"
		}
		return &anthSource{Type: "base64", MediaType: mt, Data: img.Data}
	}
	return &anthSource{Type: "url", URL: img.URL}
}

// fileFromDocument maps an Anthropic document block to IR. Only PDF sources are
// accepted as documents; callers must not relabel arbitrary files as PDF.
func fileFromDocument(b anthBlock) *ir.File {
	f := &ir.File{Filename: b.Title}
	if b.Source == nil {
		return f
	}
	s := b.Source
	switch s.Type {
	case "url":
		f.URL = s.URL
		if s.MediaType != "" {
			f.MediaType = s.MediaType
		} else {
			f.MediaType = "application/pdf"
		}
	default: // base64
		f.MediaType = s.MediaType
		if f.MediaType == "" {
			f.MediaType = "application/pdf"
		}
		f.Data = s.Data
	}
	return f
}

func sourceFromFile(f *ir.File) *anthSource {
	if f == nil {
		return &anthSource{Type: "base64", MediaType: "application/pdf"}
	}
	if f.Data != "" {
		mt := f.MediaType
		if mt == "" {
			mt = "application/pdf"
		}
		return &anthSource{Type: "base64", MediaType: mt, Data: f.Data}
	}
	return &anthSource{Type: "url", URL: f.URL}
}

// isPDFDocument reports whether f should encode as an Anthropic document block.
// Only a canonical application/pdf media type qualifies; filename suffixes alone
// must not relabel arbitrary files (e.g. text/plain named report.pdf).
func isPDFDocument(f *ir.File) bool {
	if f == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(f.MediaType), "application/pdf")
}
