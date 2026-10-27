package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// The free-tier gate requires the CLI's environment section in the system
// prompt. The block below is the shape validated live against the gate; only
// the field values differ per proxy host. Values describe the machine running
// this proxy, because the proxy cannot know the caller's: no downstream
// working directory is invented.
const (
	envHeading = "Here is some useful information about the environment you are running in:"
	envOpenTag = "<env>"
	// envMarker is the detection token for an already-present environment
	// section, matching the CLI's own block opening tag.
	envMarker = "<env>"
)

var environmentBlock = sync.OnceValue(buildEnvironmentBlock)

// buildEnvironmentBlock renders the environment section. os.Getwd and the git
// check are process-wide facts, so the result is cached. Every field describes
// the machine running this proxy; the block makes no claim about directories
// being created, approved, or reachable from the caller, because tool
// execution happens on the caller's side where this proxy has no knowledge.
func buildEnvironmentBlock() string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		wd = os.TempDir()
	}
	gitRepo := "no"
	if _, err := os.Stat(filepath.Join(wd, ".git")); err == nil {
		gitRepo = "yes"
	}
	// The CLI reports Node process.platform values; map Go's name for Windows.
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	return strings.Join([]string{
		envHeading,
		envOpenTag,
		"  Working directory: " + wd,
		"  Workspace root folder: " + wd,
		"  Is directory a git repo: " + gitRepo,
		"  Platform: " + platform,
		"</env>",
	}, "\n")
}

// AppendEnvironment inserts the CLI environment section into a free-tier
// request body for one codec family: opencode-chat appends to the first
// system message (prepending one when absent), opencode-responses appends to
// "instructions", and opencode-messages appends to the top-level "system".
// Caller text and text-block metadata are preserved; text is only appended.
// A body whose system container already carries an <env> marker — in any
// system message for chat, or in the single instructions/system value — is
// returned unchanged, so real CLI passthrough and repeated preparation are
// idempotent. Unrecognized envelopes are returned unchanged as well: the
// section is a gate requirement, not a correctness requirement, and must not
// break requests.
func AppendEnvironment(body []byte, codecID string) []byte {
	block := environmentBlock()
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return body
	}
	switch codecID {
	case "opencode-chat":
		return appendChatEnvironment(root, block, body)
	case "opencode-responses":
		return appendResponsesEnvironment(root, block, body)
	case "opencode-messages":
		return appendMessagesEnvironment(root, block, body)
	default:
		return body
	}
}

func appendChatEnvironment(root map[string]json.RawMessage, block string, original []byte) []byte {
	msgsRaw, ok := root["messages"]
	if !ok {
		return original
	}
	msgs, err := decodeRawArray(msgsRaw)
	if err != nil {
		return original
	}
	// Every system message is a possible carrier of a caller environment
	// section (real CLI passthrough can split system prompts across several
	// messages), so scan all of them. The section is appended to the first
	// system message only when none already carries the marker.
	idx := -1
	for i, raw := range msgs {
		var msg map[string]json.RawMessage
		if json.Unmarshal(raw, &msg) != nil || msg == nil {
			continue
		}
		if rawString(msg["role"]) != "system" {
			continue
		}
		if idx < 0 {
			idx = i
		}
		if contentCarriesEnv(msg["content"]) {
			return original
		}
	}
	if idx < 0 {
		system, err := json.Marshal(map[string]any{"role": "system", "content": block})
		if err != nil {
			return original
		}
		msgs = append([]json.RawMessage{system}, msgs...)
	} else {
		patched := appendTextValue(msgs[idx], "content", block)
		if patched == nil {
			return original
		}
		msgs[idx] = patched
	}
	encoded, err := json.Marshal(msgs)
	if err != nil {
		return original
	}
	root["messages"] = encoded
	return marshalRoot(root, original)
}

func appendResponsesEnvironment(root map[string]json.RawMessage, block string, original []byte) []byte {
	raw, ok := root["instructions"]
	if !ok {
		patchRootString(root, "instructions", block)
		return marshalRoot(root, original)
	}
	// A non-string instructions value has no defined append position; leave
	// the envelope untouched rather than guessing.
	current, isString := asJSONString(raw)
	if !isString || strings.Contains(current, envMarker) {
		return original
	}
	root["instructions"], _ = json.Marshal(joinSystemText(current, block))
	return marshalRoot(root, original)
}

func appendMessagesEnvironment(root map[string]json.RawMessage, block string, original []byte) []byte {
	raw, ok := root["system"]
	if !ok {
		patchRootString(root, "system", block)
		return marshalRoot(root, original)
	}
	if current, isString := asJSONString(raw); isString {
		if strings.Contains(current, envMarker) {
			return original
		}
		root["system"], _ = json.Marshal(joinSystemText(current, block))
		return marshalRoot(root, original)
	}
	// decodeRawArray returns a nil slice without error for null; that is not a
	// block array and is preserved unchanged like any other unsupported shape.
	blocks, err := decodeRawArray(raw)
	if err != nil || blocks == nil {
		return original
	}
	if blocksCarryEnv(blocks) {
		return original
	}
	patched := appendTextBlock(blocks, block)
	if patched == nil {
		return original
	}
	encoded, err := json.Marshal(patched)
	if err != nil {
		return original
	}
	root["system"] = encoded
	return marshalRoot(root, original)
}

// appendTextValue patches one message field (string or text-block array) with
// block and returns the re-marshaled message, or nil when the shape is not a
// supported text container or the marker is already present.
func appendTextValue(msgRaw json.RawMessage, field, block string) json.RawMessage {
	var msg map[string]json.RawMessage
	if json.Unmarshal(msgRaw, &msg) != nil || msg == nil {
		return nil
	}
	contentRaw, ok := msg[field]
	if !ok {
		patchMsgString(msg, field, block)
		return marshalMsg(msg)
	}
	if s, isString := asJSONString(contentRaw); isString {
		if strings.Contains(s, envMarker) {
			return nil
		}
		patchMsgString(msg, field, joinSystemText(s, block))
		return marshalMsg(msg)
	}
	blocks, err := decodeRawArray(contentRaw)
	if err != nil || blocks == nil {
		return nil
	}
	if blocksCarryEnv(blocks) {
		return nil
	}
	patched := appendTextBlock(blocks, block)
	if patched == nil {
		return nil
	}
	encoded, err := json.Marshal(patched)
	if err != nil {
		return nil
	}
	msg[field] = encoded
	return marshalMsg(msg)
}

func patchMsgString(msg map[string]json.RawMessage, field, value string) {
	msg[field], _ = json.Marshal(value)
}

func marshalMsg(msg map[string]json.RawMessage) json.RawMessage {
	out, err := json.Marshal(msg)
	if err != nil {
		return nil
	}
	return out
}

// appendTextBlock appends block to the last text block so caller text and
// block metadata (cache_control and similar) stay untouched. A list without
// text blocks grows one.
func appendTextBlock(blocks []json.RawMessage, block string) []json.RawMessage {
	for i := len(blocks) - 1; i >= 0; i-- {
		var b map[string]json.RawMessage
		if json.Unmarshal(blocks[i], &b) != nil || b == nil {
			continue
		}
		if s, isString := asJSONString(b["text"]); isString {
			b["text"], _ = json.Marshal(joinSystemText(s, block))
			patched, err := json.Marshal(b)
			if err != nil {
				return nil
			}
			out := make([]json.RawMessage, len(blocks))
			copy(out, blocks)
			out[i] = patched
			return out
		}
	}
	appended, err := json.Marshal(map[string]any{"type": "text", "text": block})
	if err != nil {
		return nil
	}
	return append(blocks, appended)
}

func joinSystemText(existing, block string) string {
	if strings.TrimSpace(existing) == "" {
		return block
	}
	return existing + "\n\n" + block
}

// contentCarriesEnv reports whether a message content value (JSON string or
// text-block array) already contains the environment marker. Unsupported
// shapes report false: the caller decides whether patching can proceed.
func contentCarriesEnv(raw json.RawMessage) bool {
	if s, isString := asJSONString(raw); isString {
		return strings.Contains(s, envMarker)
	}
	blocks, err := decodeRawArray(raw)
	if err != nil {
		return false
	}
	return blocksCarryEnv(blocks)
}

func blocksCarryEnv(blocks []json.RawMessage) bool {
	for _, raw := range blocks {
		var b map[string]json.RawMessage
		if json.Unmarshal(raw, &b) != nil || b == nil {
			continue
		}
		if s, ok := asJSONString(b["text"]); ok && strings.Contains(s, envMarker) {
			return true
		}
	}
	return false
}

func asJSONString(raw json.RawMessage) (string, bool) {
	// json.Unmarshal decodes null into the zero string without error; a null
	// envelope is not a string value and is preserved unchanged.
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func patchRootString(root map[string]json.RawMessage, field, value string) {
	root[field], _ = json.Marshal(value)
}

func marshalRoot(root map[string]json.RawMessage, original []byte) []byte {
	out, err := json.Marshal(root)
	if err != nil {
		return original
	}
	return out
}
