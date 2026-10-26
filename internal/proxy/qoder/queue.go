package qoder

import (
	"encoding/json"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// Qoder model-queue handling. When a model is temporarily out of capacity the
// inference endpoint answers with a {statusCodeValue, body} envelope whose body
// carries code 10605 plus a nested queue object. That is a transient capacity
// signal, not a quota block: the model drains in ~30s windows and the same
// request then succeeds. The payload is frequently double- or triple-encoded
// (the queue object lives inside a JSON string inside a JSON string), so every
// lookup walks the nested tree and parses JSON-encoded string fields as it
// goes.

// QueueCode is the queue-signal error code. Accepts numeric or string
// encodings of 10605.
const QueueCode = "10605"

// QueueStatusPath is the queue-status endpoint path under the inference base
// (the base already carries the /algo prefix).
const QueueStatusPath = "/api/v2/service/ask/queue/status"

// nestedQueueKeys are the envelope fields the queue object hides behind.
var nestedQueueKeys = []string{"error", "data", "body", "message", "cause", "result", "payload"}

// queueWalkLimits bound the nested walk: cycle-prone deeply nested payloads
// cannot grow the walk without end.
const (
	queueWalkDepth = 6
	queueWalkNodes = 128
)

// QueueInfo is the normalized queue descriptor parsed from a refusal body.
// Pointer fields are nil when the payload did not carry the value.
type QueueInfo struct {
	Code              string
	SubCode           string
	ModelKey          string
	QueueType         string
	QueueCount        *int
	IsQueued          *bool
	ServiceAvailable  *bool
	RetryAfterSeconds *int
	WaitTime          *int
}

// IsQueued reports whether the payload carries a queue signal: a queue flag,
// a service-unavailable marker, the RESOURCE_LIMIT sub-code, or the queue code
// itself. Billing codes 110/112 and pricingUrl are deliberately absent — a
// billing block must never be mistaken for temporary capacity.
func (i *QueueInfo) HasQueueSignal() bool {
	if i == nil {
		return false
	}
	return (i.IsQueued != nil && *i.IsQueued) ||
		(i.ServiceAvailable != nil && !*i.ServiceAvailable) ||
		i.SubCode == "RESOURCE_LIMIT" ||
		i.Code == QueueCode
}

// ParseQueueInfo extracts the queue descriptor from a refusal body. Returns
// nil unless the body actually carries a queue signal — a plain
// code 10605 without the inner object still counts, because the code alone is
// the signal the recovery keys off.
func ParseQueueInfo(input string) *QueueInfo {
	root := walkQueueRoot(input)
	if root == nil {
		return nil
	}
	info := &QueueInfo{}
	for _, obj := range root {
		if info.Code == "" {
			info.Code = queueString(obj["code"])
		}
		if info.SubCode == "" {
			info.SubCode = queueString(obj["subCode"])
		}
		if info.ModelKey == "" {
			info.ModelKey = queueString(obj["modelKey"])
		}
		if info.QueueType == "" {
			info.QueueType = queueString(obj["queueType"])
		}
		if info.QueueCount == nil {
			info.QueueCount = queueInt(obj["queueCount"])
		}
		if info.IsQueued == nil {
			info.IsQueued = queueBool(obj["isQueued"])
		}
		if info.ServiceAvailable == nil {
			info.ServiceAvailable = queueBool(obj["serviceAvailable"])
		}
		if info.RetryAfterSeconds == nil {
			info.RetryAfterSeconds = queueInt(obj["retryAfterSeconds"])
		}
		if info.WaitTime == nil {
			info.WaitTime = queueInt(obj["waitTime"])
		}
	}
	if !info.HasQueueSignal() {
		return nil
	}
	return info
}

// walkQueueRoot breadth-first walks nested objects and JSON-encoded string
// fields, returning every plain object reachable from the root.
func walkQueueRoot(input string) []map[string]any {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil
	}
	var root any
	if err := json.Unmarshal([]byte(trimmed), &root); err != nil {
		return nil
	}
	var out []map[string]any
	type node struct {
		value any
		depth int
	}
	queue := []node{{value: root}}
	objects := 0
	seenStrings := map[string]bool{}
	for len(queue) > 0 && objects < queueWalkNodes {
		cur := queue[0]
		queue = queue[1:]
		switch v := cur.value.(type) {
		case map[string]any:
			objects++
			out = append(out, v)
			if cur.depth >= queueWalkDepth {
				continue
			}
			for _, key := range nestedQueueKeys {
				child, ok := v[key]
				if !ok || child == nil {
					continue
				}
				switch c := child.(type) {
				case map[string]any, []any:
					queue = append(queue, node{value: c, depth: cur.depth + 1})
				default:
					if s, ok := c.(string); ok && !seenStrings[s] {
						seenStrings[s] = true
						if parsed := tryParseJSON(s); parsed != nil {
							queue = append(queue, node{value: parsed, depth: cur.depth + 1})
						}
					}
				}
			}
		case []any:
			if cur.depth >= queueWalkDepth {
				continue
			}
			for _, item := range v {
				switch item.(type) {
				case map[string]any, []any:
					queue = append(queue, node{value: item, depth: cur.depth + 1})
				}
			}
		}
	}
	return out
}

func tryParseJSON(s string) any {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return nil
	}
	switch v.(type) {
	case map[string]any, []any:
		return v
	default:
		return nil
	}
}

func queueString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return ""
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}

func queueInt(v any) *int {
	f, ok := queueFloat(v)
	if !ok {
		return nil
	}
	n := int(f)
	return &n
}

func queueBool(v any) *bool {
	b, ok := v.(bool)
	if !ok {
		return nil
	}
	return &b
}

func queueFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return 0, false
		}
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// QueueStatus is the parsed queue-status poll response. IsQueued nil means the
// body did not have the expected shape and the endpoint is unusable.
type QueueStatus struct {
	IsQueued          *bool
	ServiceAvailable  *bool
	ModelKey          string
	QueueType         string
	QueueCount        *int
	RetryAfterSeconds *int
	WaitTime          *int
}

// ParseQueueStatus parses a queue-status response. Returns nil when the body
// is not the expected shape; the caller treats that as "status endpoint
// unusable" and falls back to timed replays.
func ParseQueueStatus(input string) *QueueStatus {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || trimmed[0] != '{' {
		return nil
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(trimmed), &root); err != nil || root == nil {
		return nil
	}
	isQueued := queueBool(root["isQueued"])
	if isQueued == nil {
		return nil
	}
	return &QueueStatus{
		IsQueued:          isQueued,
		ServiceAvailable:  queueBool(root["serviceAvailable"]),
		ModelKey:          queueString(root["modelKey"]),
		QueueType:         queueString(root["queueType"]),
		QueueCount:        queueInt(root["queueCount"]),
		RetryAfterSeconds: queueInt(root["retryAfterSeconds"]),
		WaitTime:          queueInt(root["waitTime"]),
	}
}

// QueueStatusURL builds the signed queue-status poll URL. All three query
// parameters are required by the upstream; empty values are omitted so the
// caller can decide whether the poll is well-formed.
func QueueStatusURL(base, requestSetID, modelKey, queueType string) string {
	params := url.Values{}
	if requestSetID != "" {
		params.Set("requestSetId", requestSetID)
	}
	if modelKey != "" {
		params.Set("modelKey", modelKey)
	}
	if queueType != "" {
		params.Set("queueType", queueType)
	}
	return strings.TrimRight(base, "/") + QueueStatusPath + "?" + params.Encode()
}

// IsBillingBlock reports whether an error body indicates a billing/quota
// block: code 110 (daily count exceeded), code 112 (quota exhausted), or a
// pricingUrl field. Code 10605 is deliberately not billing — it is the
// transient model-queue throttle.
func IsBillingBlock(inner string) bool {
	if inner == "" {
		return false
	}
	if strings.Contains(strings.ToLower(inner), "pricingurl") {
		return true
	}
	switch extractErrorCode(inner) {
	case "110", "112":
		return true
	}
	// Legacy exact-shape match for non-JSON-parseable variants.
	return billingCodeRegex(inner)
}

// billingCodeRegex matches the legacy {"code":"112",...} shape without
// pulling in regexp: find a quoted code key followed by a quoted 110 or 112.
func billingCodeRegex(s string) bool {
	idx := strings.Index(s, `"code"`)
	for idx != -1 {
		rest := s[idx+len(`"code"`):]
		rest = strings.TrimLeft(rest, " \t\r\n:")
		if strings.HasPrefix(rest, `"110"`) || strings.HasPrefix(rest, `"112"`) {
			return true
		}
		next := strings.Index(s[idx+1:], `"code"`)
		if next == -1 {
			return false
		}
		idx = idx + 1 + next
	}
	return false
}
