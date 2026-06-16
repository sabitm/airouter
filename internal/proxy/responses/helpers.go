package responses

import (
	"encoding/json"
	"strings"

	"airouter/internal/proxy/ir"
)

func contentToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var parts []contentPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// outputToText flattens a function_call_output value (string or content parts).
func outputToText(raw json.RawMessage) string {
	return contentToText(raw)
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

func rawArgs(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}

// imageURLString extracts the URL from a Responses input_image field, which may
// be a bare string or a {"url": "..."} object.
func imageURLString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var obj struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(raw, &obj)
	return obj.URL
}

func imageFromURL(url string) *ir.Image {
	const prefix = "data:"
	if strings.HasPrefix(url, prefix) {
		if comma := strings.IndexByte(url, ','); comma > 0 {
			meta := url[len(prefix):comma]
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
