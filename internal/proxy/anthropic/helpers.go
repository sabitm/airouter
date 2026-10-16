package anthropic

import (
	"encoding/json"
	"fmt"
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
// backend requests. A malformed supported system hint decodes to no text; the
// request decoder rejects it before this lenient wrapper runs.
func systemToText(raw json.RawMessage) string {
	text, _, _ := decodeSystem(raw)
	return text
}

// decodeSystem parses the Anthropic system field. It returns the legacy flat
// text plus the ordered text blocks with validated cache hints. Flat text keeps
// the exact concatenation and anchored prefix stripping of systemToText; the
// blocks let the Anthropic encoder rebuild the caller's boundaries. A malformed
// supported text-block hint fails the decode. Unknown block kinds are omitted
// without reading their markers.
func decodeSystem(raw json.RawMessage) (string, []ir.SystemBlock, error) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", nil, nil
		}
		return stripClientIdentity(s), nil, nil
	}
	var blocks []anthBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return "", nil, nil
	}
	type systemText struct {
		text string
		cc   *ir.CacheControl
	}
	texts := make([]systemText, 0, len(blocks))
	var full strings.Builder
	for _, blk := range blocks {
		if blk.Type != "text" {
			continue
		}
		cc, err := cacheControlFromRaw(blk.CacheControl)
		if err != nil {
			return "", nil, fmt.Errorf("system cache_control: %w", err)
		}
		texts = append(texts, systemText{text: blk.Text, cc: cc})
		full.WriteString(blk.Text)
	}
	flat := stripClientIdentity(full.String())
	// stripClientIdentity only removes a prefix, so the byte delta maps directly
	// onto the concatenated text blocks: bytes before the delta belong to
	// consumed blocks, the rest starts inside the first surviving block.
	removed := full.Len() - len(flat)
	out := make([]ir.SystemBlock, 0, len(texts))
	for _, tb := range texts {
		if removed > 0 {
			if removed >= len(tb.text) {
				removed -= len(tb.text)
				continue
			}
			tb.text = tb.text[removed:]
			removed = 0
		}
		// Empty text carries no prefix bytes and would make an emitted block
		// invalid, so drop it rather than place it beside a marked survivor.
		if tb.text == "" {
			continue
		}
		out = append(out, ir.SystemBlock{Text: tb.text, CacheControl: tb.cc})
	}
	if len(out) == 0 {
		return flat, nil, nil
	}
	return flat, out, nil
}

// systemBlocksForEncode returns the ordered wire blocks to emit when the IR
// carries at least one surviving hint and the concatenated block text exactly
// reproduces the flat System string. It returns nil when the flat string must
// be used instead, so a changed System or a hand-built inconsistent IR never
// resurrects stale structured data. Empty blocks are dropped so they cannot
// place an invalid empty text block on the wire.
func systemBlocksForEncode(req *ir.Request) []anthBlock {
	if req == nil || req.System == "" || len(req.SystemBlocks) == 0 {
		return nil
	}
	out := make([]anthBlock, 0, len(req.SystemBlocks))
	var full strings.Builder
	hasHint := false
	for _, b := range req.SystemBlocks {
		full.WriteString(b.Text)
		if b.Text == "" {
			continue
		}
		if b.CacheControl != nil {
			hasHint = true
		}
		out = append(out, anthBlock{Type: "text", Text: b.Text, CacheControl: cacheControlRaw(b.CacheControl)})
	}
	if !hasHint || full.String() != req.System {
		return nil
	}
	return out
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
