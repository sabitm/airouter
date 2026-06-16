package anthropic

import (
	"encoding/json"
	"strings"

	"airouter/internal/proxy/ir"
)

// systemToText flattens the Anthropic system field (string or array of text
// blocks) to plain text.
func systemToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
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
	return b.String()
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
