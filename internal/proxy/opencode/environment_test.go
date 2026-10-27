package opencode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// mustJSON marshals a fixture. Building bodies with json.Marshal is required:
// the environment block contains literal newlines, which are invalid inside a
// raw JSON string and would silently fall into the unmarshal-failure no-op.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// decodeBody unmarshals into map[string]any for assertions.
func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v: %s", err, body)
	}
	return m
}

// countEnvMarkers counts <env> occurrences across every decoded string so
// json.Marshal's \u003c escaping of the marker on the wire cannot hide it.
func countEnvMarkers(v any) int {
	switch x := v.(type) {
	case string:
		return strings.Count(x, "<env>")
	case []any:
		n := 0
		for _, item := range x {
			n += countEnvMarkers(item)
		}
		return n
	case map[string]any:
		n := 0
		for _, value := range x {
			n += countEnvMarkers(value)
		}
		return n
	default:
		return 0
	}
}

func TestEnvironmentBlockShape(t *testing.T) {
	block := environmentBlock()
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		wd = os.TempDir()
	}
	gitRepo := "no"
	if _, err := os.Stat(filepath.Join(wd, ".git")); err == nil {
		gitRepo = "yes"
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	want := strings.Join([]string{
		envHeading,
		"<env>",
		"  Working directory: " + wd,
		"  Workspace root folder: " + wd,
		"  Is directory a git repo: " + gitRepo,
		"  Platform: " + platform,
		"</env>",
	}, "\n")
	if block != want {
		t.Fatalf("environmentBlock()\n got: %q\nwant: %q", block, want)
	}
}

func TestAppendEnvironmentChat(t *testing.T) {
	block := environmentBlock()

	t.Run("no system message prepends", func(t *testing.T) {
		in := mustJSON(t, map[string]any{"model": "big-pickle", "messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		}})
		out := AppendEnvironment(in, "opencode-chat")
		if !json.Valid(out) {
			t.Fatalf("output is not valid JSON: %s", out)
		}
		got := decodeBody(t, out)
		msgs := got["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %d, want 2", len(msgs))
		}
		first := msgs[0].(map[string]any)
		if first["role"] != "system" || first["content"] != block {
			t.Fatalf("prepended system = %#v", first)
		}
	})

	t.Run("string system appends", func(t *testing.T) {
		in := mustJSON(t, map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": "caller rules"},
			map[string]any{"role": "user", "content": "hi"},
		}})
		out := AppendEnvironment(in, "opencode-chat")
		got := decodeBody(t, out)
		msgs := got["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %d, want 2", len(msgs))
		}
		sys := msgs[0].(map[string]any)
		if sys["content"] != "caller rules\n\n"+block {
			t.Fatalf("system content = %#v", sys["content"])
		}
		if msgs[1].(map[string]any)["content"] != "hi" {
			t.Fatalf("user message changed: %#v", msgs[1])
		}
	})

	t.Run("block system appends to last text block", func(t *testing.T) {
		in := mustJSON(t, map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": []any{
				map[string]any{"type": "text", "text": "one"},
				map[string]any{"type": "text", "text": "two", "cache_control": map[string]any{"type": "ephemeral"}},
			}},
			map[string]any{"role": "user", "content": "hi"},
		}})
		out := AppendEnvironment(in, "opencode-chat")
		got := decodeBody(t, out)
		msgs := got["messages"].([]any)
		content := msgs[0].(map[string]any)["content"].([]any)
		if len(content) != 2 {
			t.Fatalf("system blocks = %d, want 2", len(content))
		}
		second := content[1].(map[string]any)
		if second["text"] != "two\n\n"+block {
			t.Fatalf("appended text = %#v", second["text"])
		}
		if second["cache_control"] == nil {
			t.Fatal("block metadata lost")
		}
		if content[0].(map[string]any)["text"] != "one" {
			t.Fatalf("first block changed: %#v", content[0])
		}
	})

	t.Run("existing environment in first system is byte-preserving no-op", func(t *testing.T) {
		in := mustJSON(t, map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": "caller\n\n" + block},
			map[string]any{"role": "user", "content": "hi"},
		}})
		once := AppendEnvironment(in, "opencode-chat")
		if !bytes.Equal(once, in) {
			t.Fatalf("first preparation changed the body:\n in:  %s\n out: %s", in, once)
		}
		twice := AppendEnvironment(once, "opencode-chat")
		if !bytes.Equal(once, twice) {
			t.Fatal("second preparation changed the body")
		}
		if got := countEnvMarkers(decodeBody(t, once)); got != 1 {
			t.Fatalf("marker count = %d, want 1", got)
		}
	})

	t.Run("environment in a later system message keeps body unchanged", func(t *testing.T) {
		// Real CLI passthrough can split system prompts across messages; the
		// caller block may not sit in the first one.
		in := mustJSON(t, map[string]any{"model": "big-pickle", "messages": []any{
			map[string]any{"role": "system", "content": "caller rules"},
			map[string]any{"role": "system", "content": "<env>\n  Working directory: /caller\n</env>"},
			map[string]any{"role": "user", "content": "hi"},
		}})
		out := AppendEnvironment(in, "opencode-chat")
		if !bytes.Equal(out, in) {
			t.Fatalf("body changed:\n in:  %s\n out: %s", in, out)
		}
		if got := countEnvMarkers(decodeBody(t, out)); got != 1 {
			t.Fatalf("marker count = %d, want 1", got)
		}
	})

	t.Run("several system messages without environment appends to the first", func(t *testing.T) {
		in := mustJSON(t, map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": "one"},
			map[string]any{"role": "system", "content": "two"},
			map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "three"}}},
			map[string]any{"role": "user", "content": "hi"},
		}})
		out := AppendEnvironment(in, "opencode-chat")
		got := decodeBody(t, out)
		msgs := got["messages"].([]any)
		if len(msgs) != 4 {
			t.Fatalf("messages = %d, want 4", len(msgs))
		}
		if msgs[0].(map[string]any)["content"] != "one\n\n"+block {
			t.Fatalf("first system content = %#v", msgs[0].(map[string]any)["content"])
		}
		if msgs[1].(map[string]any)["content"] != "two" {
			t.Fatalf("second system changed: %#v", msgs[1])
		}
		if msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "three" {
			t.Fatalf("third system changed: %#v", msgs[2])
		}
		if msgs[3].(map[string]any)["content"] != "hi" {
			t.Fatalf("user message changed: %#v", msgs[3])
		}
		if got := countEnvMarkers(got); got != 1 {
			t.Fatalf("marker count = %d, want 1", got)
		}
	})

	t.Run("non-array messages unchanged", func(t *testing.T) {
		in := []byte(`{"messages":{"role":"user"}}`)
		if got := AppendEnvironment(in, "opencode-chat"); string(got) != string(in) {
			t.Fatalf("body changed: %s", got)
		}
	})
}

func TestAppendEnvironmentResponses(t *testing.T) {
	block := environmentBlock()

	t.Run("missing instructions set", func(t *testing.T) {
		in := mustJSON(t, map[string]any{"model": "muse-spark-1.3-contributor-free", "input": []any{}})
		out := AppendEnvironment(in, "opencode-responses")
		if got := decodeBody(t, out)["instructions"]; got != block {
			t.Fatalf("instructions = %#v", got)
		}
	})

	t.Run("string instructions append", func(t *testing.T) {
		in := mustJSON(t, map[string]any{"instructions": "caller prompt"})
		out := AppendEnvironment(in, "opencode-responses")
		if got := decodeBody(t, out)["instructions"]; got != "caller prompt\n\n"+block {
			t.Fatalf("instructions = %#v", got)
		}
	})

	t.Run("existing environment unchanged", func(t *testing.T) {
		in := mustJSON(t, map[string]any{"instructions": "caller\n\n" + block})
		if got := AppendEnvironment(in, "opencode-responses"); !bytes.Equal(got, in) {
			t.Fatalf("body changed: %s", got)
		}
		if got := countEnvMarkers(decodeBody(t, in)); got != 1 {
			t.Fatalf("marker count = %d, want 1", got)
		}
	})

	t.Run("non-string instructions unchanged", func(t *testing.T) {
		in := mustJSON(t, map[string]any{"instructions": []any{"block"}})
		if got := AppendEnvironment(in, "opencode-responses"); !bytes.Equal(got, in) {
			t.Fatalf("body changed: %s", got)
		}
	})

	t.Run("null instructions unchanged", func(t *testing.T) {
		// json.Unmarshal(null) yields the empty string without error, so a
		// null envelope must be rejected explicitly, not treated as "".
		in := []byte(`{"instructions":null}`)
		if got := AppendEnvironment(in, "opencode-responses"); !bytes.Equal(got, in) {
			t.Fatalf("null instructions changed: %s", got)
		}
	})
}

func TestAppendEnvironmentMessages(t *testing.T) {
	block := environmentBlock()

	t.Run("missing system set", func(t *testing.T) {
		in := mustJSON(t, map[string]any{"model": "minimax-m3-free", "messages": []any{}})
		out := AppendEnvironment(in, "opencode-messages")
		if got := decodeBody(t, out)["system"]; got != block {
			t.Fatalf("system = %#v", got)
		}
	})

	t.Run("string system append", func(t *testing.T) {
		in := mustJSON(t, map[string]any{"system": "caller"})
		out := AppendEnvironment(in, "opencode-messages")
		if got := decodeBody(t, out)["system"]; got != "caller\n\n"+block {
			t.Fatalf("system = %#v", got)
		}
	})

	t.Run("block system append keeps metadata", func(t *testing.T) {
		in := mustJSON(t, map[string]any{"system": []any{
			map[string]any{"type": "text", "text": " sys", "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}},
		}})
		out := AppendEnvironment(in, "opencode-messages")
		sys := decodeBody(t, out)["system"].([]any)
		if len(sys) != 1 {
			t.Fatalf("system blocks = %d, want 1", len(sys))
		}
		b := sys[0].(map[string]any)
		if b["text"] != " sys\n\n"+block {
			t.Fatalf("text = %#v", b["text"])
		}
		if b["cache_control"] == nil {
			t.Fatal("cache_control lost")
		}
	})

	t.Run("existing environment unchanged", func(t *testing.T) {
		in := mustJSON(t, map[string]any{"system": "caller\n\n" + block})
		if got := AppendEnvironment(in, "opencode-messages"); !bytes.Equal(got, in) {
			t.Fatalf("body changed: %s", got)
		}
		if got := countEnvMarkers(decodeBody(t, in)); got != 1 {
			t.Fatalf("marker count = %d, want 1", got)
		}
	})

	t.Run("block system with environment unchanged", func(t *testing.T) {
		in := mustJSON(t, map[string]any{"system": []any{
			map[string]any{"type": "text", "text": "x\n\n" + block},
		}})
		if got := AppendEnvironment(in, "opencode-messages"); !bytes.Equal(got, in) {
			t.Fatalf("body changed: %s", got)
		}
	})

	t.Run("non-string non-array system unchanged", func(t *testing.T) {
		in := []byte(`{"system":5}`)
		if got := AppendEnvironment(in, "opencode-messages"); !bytes.Equal(got, in) {
			t.Fatalf("body changed: %s", got)
		}
	})

	t.Run("null system unchanged", func(t *testing.T) {
		in := []byte(`{"system":null}`)
		if got := AppendEnvironment(in, "opencode-messages"); !bytes.Equal(got, in) {
			t.Fatalf("null system changed: %s", got)
		}
	})
}

func TestAppendEnvironmentUnknownCodecAndInvalidBody(t *testing.T) {
	in := []byte(`{"model":"big-pickle"}`)
	if got := AppendEnvironment(in, "opencode-unknown"); string(got) != string(in) {
		t.Fatalf("unknown codec changed body: %s", got)
	}
	if got := AppendEnvironment([]byte("not json"), "opencode-chat"); string(got) != "not json" {
		t.Fatalf("invalid body changed: %s", got)
	}
	if got := AppendEnvironment(nil, "opencode-chat"); got != nil {
		t.Fatalf("nil body changed: %s", got)
	}
}

// TestAppendEnvironmentPreservesUnrelatedFields pins that only the system
// container changes and sibling keys, including number formatting, survive.
func TestAppendEnvironmentPreservesUnrelatedFields(t *testing.T) {
	in := mustJSON(t, map[string]any{
		"model":       "big-pickle",
		"max_tokens":  16,
		"temperature": 0.5,
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	out := AppendEnvironment(in, "opencode-chat")
	if !json.Valid(out) {
		t.Fatalf("output is not valid JSON: %s", out)
	}
	got := decodeBody(t, out)
	if got["model"] != "big-pickle" || got["max_tokens"] != float64(16) || got["temperature"] != 0.5 {
		t.Fatalf("sibling fields changed: %#v", got)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if !reflect.DeepEqual(msgs[1].(map[string]any), map[string]any{"role": "user", "content": "hi"}) {
		t.Fatalf("user message changed: %#v", msgs[1])
	}
}
