package usage

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

// parseResetTime accepts unix seconds, unix milliseconds, numeric strings, and RFC3339.
// Values below 1e12 are treated as seconds (9router shared.js heuristic).
func parseResetTime(v any) *time.Time {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case time.Time:
		if t.IsZero() {
			return nil
		}
		u := t.UTC()
		return &u
	case *time.Time:
		if t == nil || t.IsZero() {
			return nil
		}
		u := t.UTC()
		return &u
	case float64:
		return unixToTime(t)
	case float32:
		return unixToTime(float64(t))
	case int:
		return unixToTime(float64(t))
	case int64:
		return unixToTime(float64(t))
	case json.Number:
		n, err := t.Float64()
		if err != nil {
			return parseResetTime(t.String())
		}
		return unixToTime(n)
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return nil
		}
		if isAllDigits(s) {
			n, err := strconv.ParseFloat(s, 64)
			if err == nil {
				return unixToTime(n)
			}
		}
		if parsed, err := time.Parse(time.RFC3339, s); err == nil {
			u := parsed.UTC()
			return &u
		}
		if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
			u := parsed.UTC()
			return &u
		}
		if parsed, err := time.Parse(time.RFC1123, s); err == nil {
			u := parsed.UTC()
			return &u
		}
		return nil
	default:
		return nil
	}
}

func unixToTime(n float64) *time.Time {
	if n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	sec := n
	if n >= 1e12 {
		sec = n / 1000
	}
	t := time.Unix(int64(sec), int64((sec-math.Trunc(sec))*1e9)).UTC()
	return &t
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func toFinite(v any, fallback float64) float64 {
	n, ok := asFloat(v)
	if !ok {
		return fallback
	}
	return n
}

func asFloat(v any) (float64, bool) {
	if v == nil {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return 0, false
		}
		return t, true
	case float32:
		n := float64(t)
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		n, err := t.Float64()
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		n, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func clampPct(n float64) float64 {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

func pctPtr(n float64) *float64 {
	v := clampPct(n)
	return &v
}

func lookupFirst(m map[string]any, keys ...string) any {
	if m == nil {
		return nil
	}
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}
