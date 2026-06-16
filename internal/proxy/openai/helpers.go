package openai

import (
	"encoding/json"
	"strings"

	"airouter/internal/proxy/ir"
)

// parseStop normalizes the OpenAI `stop` field (string or []string) to a slice.
func parseStop(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return []string{s}
		}
		return nil
	}
	var arr []string
	_ = json.Unmarshal(raw, &arr)
	return arr
}

// contentToText flattens a message content field (string, null, or array of
// parts) into plain text, concatenating any text parts.
func contentToText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var parts []chatPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func toolResultText(b ir.ContentBlock) string {
	var sb strings.Builder
	for _, r := range b.ToolResult {
		if r.Type == ir.BlockText {
			sb.WriteString(r.Text)
		}
	}
	return sb.String()
}

// imageFromURL parses an OpenAI image_url, splitting an inline data URI into
// media type + base64 data, or keeping a remote URL as-is.
func imageFromURL(url string) *ir.Image {
	const prefix = "data:"
	if strings.HasPrefix(url, prefix) {
		if comma := strings.IndexByte(url, ','); comma > 0 {
			meta := url[len(prefix):comma] // e.g. image/png;base64
			data := url[comma+1:]
			mediaType := meta
			if semi := strings.IndexByte(meta, ';'); semi >= 0 {
				mediaType = meta[:semi]
			}
			return &ir.Image{MediaType: mediaType, Data: data}
		}
	}
	return &ir.Image{URL: url}
}

// imageToURL renders an IR image back to an OpenAI image_url string.
func imageToURL(img *ir.Image) string {
	if img == nil {
		return ""
	}
	if img.Data != "" {
		mt := img.MediaType
		if mt == "" {
			mt = "image/png"
		}
		return "data:" + mt + ";base64," + img.Data
	}
	return img.URL
}

func mustText(s string) json.RawMessage {
	raw, _ := json.Marshal(s)
	return raw
}

// rawOrNull returns valid JSON for a tool argument payload, defaulting empty to
// an empty object so downstream parsers never choke on "".
func rawOrNull(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}
