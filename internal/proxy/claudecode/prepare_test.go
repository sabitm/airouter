package claudecode

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareKeepsToolNamesAndInjectsIdentity(t *testing.T) {
	in := []byte(`{"model":"claude-sonnet-4-5","max_tokens":8,"system":"be brief","messages":[{"role":"user","content":"hello world, this is a test"}],"tools":[{"name":"search_ide","input_schema":{"type":"object"}}]}`)
	out, err := Prepare(in, PrepareInput{
		Token:        "sk-ant-oat-test",
		SessionID:    "11111111-1111-4111-8111-111111111111",
		AccountUUID:  "acct-real",
		DeviceID:     strings.Repeat("ab", 32),
		Profile:      DefaultProfile(),
		BaseURL:      "https://api.anthropic.com",
		CacheDefault: true,
		CacheTTL:     "5m",
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	tools := m["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "search_ide" {
		t.Fatalf("tool renamed: %s", out)
	}
	if strings.Contains(string(out), "Bash") || strings.Contains(string(out), "currently unavailable") {
		t.Fatalf("decoy injected: %s", out)
	}
	sys := m["system"].([]any)
	if !strings.HasPrefix(sys[0].(map[string]any)["text"].(string), BillingHeaderPrefix) {
		t.Fatalf("billing missing: %s", out)
	}
	if sys[1].(map[string]any)["text"] != IdentityCLI {
		t.Fatalf("identity = %v", sys[1])
	}
	if !strings.Contains(sys[2].(map[string]any)["text"].(string), "be brief") {
		t.Fatalf("caller dropped: %v", sys[2])
	}
	meta := m["metadata"].(map[string]any)["user_id"].(string)
	if !strings.Contains(meta, "acct-real") || !strings.Contains(meta, "11111111-1111-4111-8111-111111111111") {
		t.Fatalf("user_id = %s", meta)
	}
	if strings.Contains(meta, "sk-ant") {
		t.Fatal("token leaked into user_id")
	}
}

func TestDefaultCacheStaysWithinFourMarkers(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"system":[{"type":"text","text":"one"},{"type":"text","text":"two"},{"type":"text","text":"three"},{"type":"text","text":"four"}],"messages":[{"role":"user","content":"hi"}]}`)
	in := PrepareInput{SessionID: "11111111-1111-4111-8111-111111111111", DeviceID: strings.Repeat("ab", 32), Profile: DefaultProfile(), BaseURL: "https://api.anthropic.com", CacheDefault: true}
	out, err := Prepare(body, in)
	if err != nil {
		t.Fatal(err)
	}
	count := countCacheMarkers(t, out)
	if count > 4 {
		t.Fatalf("synthesized %d cache markers: %s", count, out)
	}
	if count != 2 {
		t.Fatalf("default anchors = %d, want identity plus last caller block: %s", count, out)
	}
	again, err := Prepare(out, in)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(out) {
		t.Fatalf("repeat Prepare changed content or policy\nfirst %s\nsecond %s", out, again)
	}
}

func TestDefaultCacheRoundTripIsExact(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"system":[{"type":"text","text":"one"},{"type":"text","text":"two"},{"type":"text","text":"three"},{"type":"text","text":"four"}],"messages":[{"role":"user","content":"hi"}]}`)
	for _, ttl := range []string{"5m", "1h"} {
		t.Run(ttl, func(t *testing.T) {
			in := PrepareInput{SessionID: "11111111-1111-4111-8111-111111111111", DeviceID: strings.Repeat("ab", 32), Profile: DefaultProfile(), BaseURL: "https://api.anthropic.com", CacheDefault: true, CacheTTL: ttl}
			one, err := Prepare(body, in)
			if err != nil {
				t.Fatal(err)
			}
			two, err := Prepare(one, in)
			if err != nil {
				t.Fatal(err)
			}
			if string(one) != string(two) {
				t.Fatalf("repeat Prepare changed the body\nfirst %s\nsecond %s", one, two)
			}
			sys := systemBlocks(t, one)
			if len(sys) != 6 {
				t.Fatalf("system blocks = %d: %s", len(sys), one)
			}
			want := map[string]any{"type": "ephemeral", "ttl": ttl}
			if _, ok := sys[0]["cache_control"]; ok {
				t.Fatal("billing block gained a marker")
			}
			if sys[1]["text"] != IdentityCLI || !reflect.DeepEqual(sys[1]["cache_control"], want) {
				t.Fatalf("identity anchor = %#v", sys[1])
			}
			if sys[5]["text"] != "four" || !reflect.DeepEqual(sys[5]["cache_control"], want) {
				t.Fatalf("last caller anchor = %#v", sys[5])
			}
			if count := countCacheMarkers(t, one); count != 2 {
				t.Fatalf("cache markers = %d: %s", count, one)
			}
		})
	}
}

func TestExplicitSystemCachePolicyIsUnchanged(t *testing.T) {
	policies := []map[string]any{
		{"type": "ephemeral", "scope": "org", "ttl": "5m"},
		{"type": "ephemeral", "scope": "org", "ttl": "1h"},
		{"type": "ephemeral", "scope": "global", "ttl": "1h"},
		{"type": "ephemeral", "ttl": "5m"},
		{"type": "ephemeral", "ttl": "1h"},
	}
	for _, policy := range policies {
		body, err := json.Marshal(map[string]any{
			"model":      "claude-sonnet-5",
			"max_tokens": 8,
			"system": []any{
				map[string]any{"type": "text", "text": "one"},
				map[string]any{"type": "text", "text": "two", "cache_control": policy},
			},
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		in := PrepareInput{SessionID: "11111111-1111-4111-8111-111111111111", DeviceID: strings.Repeat("ab", 32), Profile: DefaultProfile(), BaseURL: "https://api.anthropic.com", CacheDefault: true, CacheTTL: "5m"}
		one, err := Prepare(body, in)
		if err != nil {
			t.Fatal(err)
		}
		two, err := Prepare(one, in)
		if err != nil {
			t.Fatal(err)
		}
		if string(one) != string(two) {
			t.Fatalf("caller policy changed on repeat\nfirst %s\nsecond %s", one, two)
		}
		sys := systemBlocks(t, one)
		if len(sys) != 4 {
			t.Fatalf("system blocks = %d: %s", len(sys), one)
		}
		if _, ok := sys[1]["cache_control"]; ok {
			t.Fatalf("identity gained a marker with caller policy: %#v", sys[1])
		}
		if sys[3]["text"] != "two" || !reflect.DeepEqual(sys[3]["cache_control"], policy) {
			t.Fatalf("caller marker = %#v, want %#v", sys[3], policy)
		}
		if count := countCacheMarkers(t, one); count != 1 {
			t.Fatalf("cache markers = %d: %s", count, one)
		}
	}
}

func systemBlocks(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var doc struct {
		System []map[string]any `json:"system"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.System
}

func TestPrepareBlankBaseMatchesExplicitDefault(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"system":"keep","messages":[{"role":"user","content":"hi"}]}`)
	in := PrepareInput{SessionID: "11111111-1111-4111-8111-111111111111", DeviceID: strings.Repeat("ab", 32), Profile: DefaultProfile(), CacheDefault: true}
	blank, err := Prepare(body, in)
	if err != nil {
		t.Fatal(err)
	}
	in.BaseURL = DefaultBaseURL
	explicit, err := Prepare(body, in)
	if err != nil {
		t.Fatal(err)
	}
	if string(blank) != string(explicit) {
		t.Fatalf("blank base differs from explicit default\nblank %s\nexplicit %s", blank, explicit)
	}
}

func TestExplicitChangeAfterDefaultPrepareIsNotRewritten(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"system":[{"type":"text","text":"one"},{"type":"text","text":"two"}],"messages":[{"role":"user","content":"hi"}]}`)
	in := PrepareInput{SessionID: "11111111-1111-4111-8111-111111111111", DeviceID: strings.Repeat("ab", 32), Profile: DefaultProfile(), BaseURL: "https://api.anthropic.com", CacheDefault: true, CacheTTL: "5m"}
	once, err := Prepare(body, in)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(once, &doc); err != nil {
		t.Fatal(err)
	}
	sys := doc["system"].([]any)
	last := sys[len(sys)-1].(map[string]any)
	callerPolicy := map[string]any{"type": "ephemeral", "scope": "org", "ttl": "1h"}
	last["cache_control"] = callerPolicy
	changed, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := Prepare(changed, in)
	if err != nil {
		t.Fatal(err)
	}
	final := systemBlocks(t, twice)
	if !reflect.DeepEqual(final[len(final)-1]["cache_control"], callerPolicy) {
		t.Fatalf("caller changed TTL was rewritten: %#v", final[len(final)-1])
	}
	if _, ok := final[1]["cache_control"]; ok {
		t.Fatalf("identity anchor added with caller policy: %#v", final[1])
	}
	if count := countCacheMarkers(t, twice); count != 1 {
		t.Fatalf("cache markers = %d: %s", count, twice)
	}
}

func TestQuotedCacheStringDoesNotSuppressAnchors(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"system":"see cache_control in docs","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"t","input_schema":{"type":"object","properties":{"note":{"type":"string","description":"quoted cache_control"}}}}]}`)
	out, err := Prepare(body, PrepareInput{SessionID: "11111111-1111-4111-8111-111111111111", DeviceID: strings.Repeat("ab", 32), Profile: DefaultProfile(), BaseURL: "https://api.anthropic.com", CacheDefault: true})
	if err != nil {
		t.Fatal(err)
	}
	if countCacheMarkers(t, out) == 0 {
		t.Fatalf("quoted cache_control suppressed anchors: %s", out)
	}
}

func TestTopLevelCachePolicySuppressesAnchors(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"cache_control":{"type":"ephemeral","ttl":"1h"},"system":"keep","messages":[{"role":"user","content":"hi"}]}`)
	out, err := Prepare(body, PrepareInput{SessionID: "11111111-1111-4111-8111-111111111111", DeviceID: strings.Repeat("ab", 32), Profile: DefaultProfile(), BaseURL: "https://api.anthropic.com", CacheDefault: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"ttl":"5m"`) {
		t.Fatalf("top-level policy was ignored: %s", out)
	}
	if !strings.Contains(string(out), `"ttl":"1h"`) {
		t.Fatalf("top-level marker changed: %s", out)
	}
}

func countCacheMarkers(t *testing.T, body []byte) int {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	count := 0
	var walk func(any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			if _, ok := n["cache_control"]; ok {
				count++
			}
			for key, child := range n {
				if key == "cache_control" {
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(doc)
	return count
}

func TestPrepareDoesNotAnchorWhenCallerSetCache(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":1,"system":[{"type":"text","text":"keep","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"hi"}]}`)
	out, err := Prepare(in, PrepareInput{SessionID: "11111111-1111-4111-8111-111111111111", DeviceID: strings.Repeat("ab", 32), Profile: DefaultProfile(), BaseURL: "https://api.anthropic.com", CacheDefault: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"ttl":"1h"`) {
		t.Fatalf("caller marker lost: %s", out)
	}
	if strings.Contains(string(out), `"ttl":"5m"`) {
		t.Fatalf("default anchor added despite caller policy: %s", out)
	}
}

func TestCallerLongContextBetaIsModelGated(t *testing.T) {
	got := BetaHeaders(BetaRequest{Model: "claude-sonnet-5", CallerBetas: []string{BetaLongContext, "not-a-flag"}})
	if !strings.Contains(got, BetaLongContext) {
		t.Fatalf("supported caller beta dropped: %s", got)
	}
	if strings.Contains(got, "not-a-flag") {
		t.Fatalf("unknown flag admitted: %s", got)
	}
	haiku := BetaHeaders(BetaRequest{Model: "claude-haiku-4-5", CallerBetas: []string{BetaLongContext}})
	if strings.Contains(haiku, BetaLongContext) {
		t.Fatalf("unsupported model kept caller long-context flag: %s", haiku)
	}
	if strings.Contains(BetaHeaders(BetaRequest{Model: "claude-3-5-sonnet", OAuth: true, ThinkingActive: true}), BetaInterleaved) {
		t.Fatalf("claude 3.5 received interleaved thinking beta")
	}
	parsed := ParseCallerBetas("context-1m-2025-08-07, not-a-flag, context-1m-2025-08-07")
	if len(parsed) != 1 || parsed[0] != BetaLongContext {
		t.Fatalf("parsed = %v", parsed)
	}
}

func TestBetaHeadersAreFeatureGated(t *testing.T) {
	got := BetaHeaders(BetaRequest{Model: "claude-haiku-4-5", OAuth: true, ThinkingActive: true})
	if strings.Contains(got, BetaClaudeCode) {
		t.Fatalf("haiku got claude-code beta: %s", got)
	}
	if !strings.Contains(got, BetaOAuth) {
		t.Fatalf("oauth beta missing: %s", got)
	}
	full := BetaHeaders(BetaRequest{Model: "claude-sonnet-5", ContextManagement: true, Effort: true, CacheScope: true, ThinkingActive: true})
	for _, want := range []string{BetaClaudeCode, BetaInterleaved, BetaContextMgmt, BetaEffort, BetaPromptCache, BetaRedactThinking} {
		if !strings.Contains(full, want) {
			t.Fatalf("missing %s in %s", want, full)
		}
	}
	summary := BetaHeaders(BetaRequest{Model: "claude-sonnet-5", ThinkingActive: true, ThinkingSummarized: true})
	if strings.Contains(summary, BetaRedactThinking) {
		t.Fatalf("summarized display got redact beta: %s", summary)
	}
	if strings.Contains(full, "token-efficient-tools") || strings.Contains(full, "between_tools") {
		t.Fatalf("removed flag present: %s", full)
	}
}

func TestUserAgentDoesNotClaimNode(t *testing.T) {
	ua := DefaultProfile().UserAgent()
	if !strings.HasPrefix(ua, "claude-cli/"+CLIVersion+" (external, cli)") {
		t.Fatalf("ua = %s", ua)
	}
	h := StainlessRuntimeHeaders()
	if h["X-Stainless-Runtime"] == "node" {
		t.Fatal("runtime claimed node")
	}
	if h["X-Stainless-Package-Version"] != SDKPackageVersion {
		t.Fatalf("sdk = %s", h["X-Stainless-Package-Version"])
	}
}

func boolPtr(v bool) *bool { return &v }

// prepareEvictionInput is the shared prepared-body input for eviction tests.
func prepareEvictionInput() PrepareInput {
	return PrepareInput{
		SessionID:    "11111111-1111-4111-8111-111111111111",
		DeviceID:     strings.Repeat("ab", 32),
		Profile:      DefaultProfile(),
		BaseURL:      "https://api.anthropic.com",
		CacheDefault: true,
		CacheTTL:     "5m",
	}
}

// systemEvictionMarks returns the cache_control value of the caller text block
// with the given text, or nil when unmarked.
func systemEvictionMarks(t *testing.T, body []byte, text string) map[string]any {
	t.Helper()
	var doc struct {
		System []map[string]any `json:"system"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("system decode: %v; body=%s", err, body)
	}
	for _, block := range doc.System {
		if block["text"] == text {
			cc, _ := block["cache_control"].(map[string]any)
			return cc
		}
	}
	t.Fatalf("system block %q missing: %s", text, body)
	return nil
}

// TestCallerEvictionMarkersSurvivePrepare keeps explicit evict_on_complete true
// and false across a Prepare that rewrites the system array, and confirms the
// generated default anchors never carry the eviction field.
func TestCallerEvictionMarkersSurvivePrepare(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":1,` +
		`"system":[{"type":"text","text":"one","cache_control":{"type":"ephemeral","evict_on_complete":true}},{"type":"text","text":"two","cache_control":{"type":"ephemeral","ttl":"5m","evict_on_complete":false}}],` +
		`"tools":[{"name":"t","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","evict_on_complete":true}}],` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	out, err := Prepare(in, prepareEvictionInput())
	if err != nil {
		t.Fatal(err)
	}
	one := systemEvictionMarks(t, out, "one")
	if one["type"] != "ephemeral" || one["evict_on_complete"] != true || len(one) != 2 {
		t.Fatalf("eviction true marker changed: %v (%s)", one, out)
	}
	two := systemEvictionMarks(t, out, "two")
	if two["ttl"] != "5m" || two["evict_on_complete"] != false || len(two) != 3 {
		t.Fatalf("eviction false marker changed: %v (%s)", two, out)
	}
	// Explicit policy suppresses the default anchors entirely.
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	sys := doc["system"].([]any)
	if len(sys) != 4 {
		t.Fatalf("system blocks = %d, want billing+identity+two caller: %s", len(sys), out)
	}
	if _, ok := sys[1].(map[string]any)["cache_control"]; ok {
		t.Fatalf("identity gained an anchor despite caller policy: %s", out)
	}
	var tools []map[string]any
	toolBytes, _ := json.Marshal(doc["tools"])
	if err := json.Unmarshal(toolBytes, &tools); err != nil {
		t.Fatal(err)
	}
	toolMark, _ := tools[0]["cache_control"].(map[string]any)
	if toolMark["evict_on_complete"] != true {
		t.Fatalf("tool eviction marker lost: %s", out)
	}
	// Repeated preparation is stable, including the eviction values.
	twice, err := Prepare(out, prepareEvictionInput())
	if err != nil {
		t.Fatal(err)
	}
	if string(twice) != string(out) {
		t.Fatalf("repeated prepare unstable: %s -> %s", out, twice)
	}
}

// TestDefaultAnchorsNeverCarryEviction keeps the synthesized default pair free
// of the eviction field when the caller set no cache policy at all.
func TestDefaultAnchorsNeverCarryEviction(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":1,"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"messages":[{"role":"user","content":"hi"}]}`)
	out, err := Prepare(in, prepareEvictionInput())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"ttl":"5m"`) {
		t.Fatalf("default anchors missing: %s", out)
	}
	if strings.Contains(string(out), "evict_on_complete") {
		t.Fatalf("generated default gained eviction: %s", out)
	}
}

// TestExplicitEvictionMarkIsNotSynthesizedDefault proves a caller eviction
// marker on the only marked block of an already-prepared body is explicit
// policy: the strip-as-default-anchor branch must not swallow it (that branch
// compares the last block mark against the identity anchor).
func TestExplicitEvictionMarkIsNotSynthesizedDefault(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":1,"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"messages":[{"role":"user","content":"hi"}]}`)
	prepared, err := Prepare(in, prepareEvictionInput())
	if err != nil {
		t.Fatal(err)
	}
	for _, evict := range []bool{true, false} {
		var doc map[string]any
		if err := json.Unmarshal(prepared, &doc); err != nil {
			t.Fatal(err)
		}
		sys := doc["system"].([]any)
		// Keep only the last caller mark, replaced by the explicit eviction pair:
		// exactly the shape where a wrong defaultAnchor/sameCacheMark would strip it.
		delete(sys[len(sys)-2].(map[string]any), "cache_control")
		sys[len(sys)-1].(map[string]any)["cache_control"] = map[string]any{"type": "ephemeral", "ttl": "5m", "evict_on_complete": evict}
		mutated, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		out, err := Prepare(mutated, prepareEvictionInput())
		if err != nil {
			t.Fatal(err)
		}
		mark := systemEvictionMarks(t, out, "b")
		if mark == nil || mark["evict_on_complete"] != evict || mark["ttl"] != "5m" {
			t.Fatalf("explicit eviction %v rewritten as synthesized default: %s", evict, out)
		}
		// Explicit policy suppresses re-anchoring: no identity or borrowed marks.
		var outDoc map[string]any
		if err := json.Unmarshal(out, &outDoc); err != nil {
			t.Fatal(err)
		}
		outSys := outDoc["system"].([]any)
		if _, ok := outSys[1].(map[string]any)["cache_control"]; ok {
			t.Fatalf("identity anchor survived explicit eviction %v: %s", evict, out)
		}
		a := outSys[2].(map[string]any)
		if _, ok := a["cache_control"]; ok {
			t.Fatalf("unmarked caller block gained a mark for eviction %v: %s", evict, out)
		}
	}
}

// TestCacheMarkEvictionSemantics pins the marker struct: presence/value are
// preserved, an explicit pointer disqualifies the synthesized default shape,
// and equality respects both presence and value.
func TestCacheMarkEvictionSemantics(t *testing.T) {
	raw := map[string]any{"type": "ephemeral", "evict_on_complete": true}
	mark := cacheMark(raw)
	if mark == nil || mark.EvictOnComplete == nil || !*mark.EvictOnComplete {
		t.Fatalf("true eviction lost: %+v", mark)
	}
	raw["evict_on_complete"] = false
	mark = cacheMark(raw)
	if mark == nil || mark.EvictOnComplete == nil || *mark.EvictOnComplete {
		t.Fatalf("explicit false lost: %+v", mark)
	}
	delete(raw, "evict_on_complete")
	mark = cacheMark(raw)
	if mark == nil || mark.EvictOnComplete != nil {
		t.Fatalf("omitted eviction became a pointer: %+v", mark)
	}
	// A non-bool eviction value is not a marker field the struct reads.
	raw["evict_on_complete"] = "true"
	mark = cacheMark(raw)
	if mark == nil || mark.EvictOnComplete != nil {
		t.Fatalf("non-bool eviction became a pointer: %+v", mark)
	}
	// Default-anchor classification: only the pointer-free unscoped TTL pair.
	for _, evict := range []*bool{boolPtr(true), boolPtr(false)} {
		if defaultAnchor(&CacheMark{TTL: "5m", EvictOnComplete: evict}) {
			t.Fatalf("explicit eviction %v classified as default anchor", *evict)
		}
	}
	if !defaultAnchor(&CacheMark{TTL: "5m"}) || !defaultAnchor(&CacheMark{TTL: "1h"}) {
		t.Fatal("plain default anchor no longer recognized")
	}
	if defaultAnchor(&CacheMark{TTL: "5m", Scope: "global"}) {
		t.Fatal("scoped anchor classified as default")
	}
	// sameCacheMark respects eviction presence and value.
	a := &CacheMark{TTL: "5m", EvictOnComplete: boolPtr(true)}
	if sameCacheMark(a, &CacheMark{TTL: "5m"}) {
		t.Fatal("eviction presence ignored by sameCacheMark")
	}
	if sameCacheMark(a, &CacheMark{TTL: "5m", EvictOnComplete: boolPtr(false)}) {
		t.Fatal("eviction value ignored by sameCacheMark")
	}
	if !sameCacheMark(a, &CacheMark{TTL: "5m", EvictOnComplete: boolPtr(true)}) {
		t.Fatal("equal eviction marks compared unequal")
	}
	// The emitted map carries the key only for a non-nil pointer.
	if m := cacheControlMap(&CacheMark{TTL: "5m", EvictOnComplete: boolPtr(false)}); m["evict_on_complete"] != false {
		t.Fatalf("explicit false not emitted: %v", m)
	}
	if m := cacheControlMap(&CacheMark{TTL: "5m"}); len(m) != 2 {
		t.Fatalf("omitted eviction emitted: %v", m)
	}
}

// TestCacheFeaturesEvictionDetection pins eviction detection to the API marker
// positions: schema properties, quoted text, nested tool-result content, an
// invented type, a string value, and an explicit false never activate it.
func TestCacheFeaturesEvictionDetection(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"top-level", `{"cache_control":{"type":"ephemeral","evict_on_complete":true},"messages":[]}`, true},
		{"system", `{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","evict_on_complete":true}}],"messages":[]}`, true},
		{"tools", `{"tools":[{"name":"t","cache_control":{"type":"ephemeral","evict_on_complete":true}}],"messages":[]}`, true},
		{"message block", `{"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","evict_on_complete":true}}]}]}`, true},
		{"outer tool_result", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","cache_control":{"type":"ephemeral","evict_on_complete":true},"content":"x"}]}]}`, true},
		{"nested tool_result content", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"n","cache_control":{"type":"ephemeral","evict_on_complete":true}}]}]}]}`, false},
		{"schema property", `{"tools":[{"name":"t","input_schema":{"type":"object","properties":{"evict_on_complete":{"const":true},"type":{"const":"ephemeral"}}}}],"messages":[]}`, false},
		{"quoted text", `{"system":"set evict_on_complete to true and type ephemeral","messages":[]}`, false},
		{"invented type evict", `{"cache_control":{"type":"evict"},"messages":[]}`, false},
		{"string true", `{"cache_control":{"type":"ephemeral","evict_on_complete":"true"},"messages":[]}`, false},
		{"explicit false", `{"cache_control":{"type":"ephemeral","evict_on_complete":false},"messages":[]}`, false},
		{"missing type", `{"cache_control":{"evict_on_complete":true},"messages":[]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, evict := CacheFeatures([]byte(tc.body))
			if evict != tc.want {
				t.Fatalf("evict = %v, want %v (%s)", evict, tc.want, tc.body)
			}
		})
	}
}
