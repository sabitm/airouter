package kiro

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode"

	"airouter/internal/proxy/ir"
)

const (
	toolSuffix         = "_ide"
	toolNameLimit      = 64
	toolAliasTailLimit = toolNameLimit - len(toolSuffix) - 2
	// aliasAlphabet is a conservative subset of the local kiro-cli 2.27.1
	// ^[a-zA-Z][a-zA-Z0-9_]*$ check and the linked 9router [A-Za-z0-9_-]
	// allowance. Those are client/reference evidence, not confirmed upstream
	// constraints. Hyphens are excluded so every alias also satisfies the CLI
	// pattern.
	aliasAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// decoyNames is a CLI 2.27.1 embedded subset, not a verified active wire
// catalog. Decoys are appended only after at least one usable client tool.
// They are marked unavailable and must never be forwarded or executed.
var decoyNames = []string{
	"execute_bash",
	"fs_read",
	"fs_write",
	"glob",
	"grep",
	"web_search",
	"web_fetch",
}

const decoyDescription = "This tool is currently unavailable."

var decoySchema = json.RawMessage(`{"type":"object","properties":{}}`)

// toolCatalog is the request-local cloak built from one declaration list.
// The same declarations in the same order always produce the same maps.
// Nothing here is shared across requests.
type toolCatalog struct {
	tools    []cwTool
	toWire   map[string]string
	toClient map[string]string
	decoys   map[string]bool
}

func (c toolCatalog) empty() bool {
	return len(c.tools) == 0
}

// buildToolCatalog cloaks usable client declarations and appends the decoy
// subset. Empty, whitespace-only, and exact-duplicate names are omitted.
// The first usable declaration of a repeated name wins. Schemas and
// descriptions of usable declarations are copied, not rewritten.
func buildToolCatalog(tools []ir.Tool) toolCatalog {
	usable := make([]ir.Tool, 0, len(tools))
	seenOriginal := map[string]bool{}
	for _, tool := range tools {
		name := tool.Name
		if strings.TrimSpace(name) == "" || seenOriginal[name] {
			continue
		}
		seenOriginal[name] = true
		usable = append(usable, tool)
	}
	if len(usable) == 0 {
		return toolCatalog{}
	}

	cat := toolCatalog{
		toWire:   make(map[string]string, len(usable)),
		toClient: make(map[string]string, len(usable)),
		decoys:   make(map[string]bool, len(decoyNames)),
	}
	used := map[string]bool{}
	for i, tool := range usable {
		wire := cloakWireName(tool.Name, i, used)
		cat.toWire[tool.Name] = wire
		cat.toClient[wire] = tool.Name
		used[wire] = true
		cat.tools = append(cat.tools, cwTool{ToolSpecification: cwToolSpecification{
			Name:        wire,
			Description: tool.Description,
			InputSchema: cwInputSchema{JSON: copySchema(tool.Parameters)},
		}})
	}
	for _, name := range decoyNames {
		if used[name] {
			// A sanitized client alias claimed this exact decoy spelling.
			// Dropping the decoy keeps the client mapping one-to-one.
			continue
		}
		used[name] = true
		cat.decoys[name] = true
		cat.tools = append(cat.tools, cwTool{ToolSpecification: cwToolSpecification{
			Name:        name,
			Description: decoyDescription,
			InputSchema: cwInputSchema{JSON: append(json.RawMessage(nil), decoySchema...)},
		}})
	}
	return cat
}

func copySchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return append(json.RawMessage(nil), decoySchema...)
	}
	return append(json.RawMessage(nil), raw...)
}

// cloakWireName returns a unique wire name for one original declaration.
// Ordinary names gain toolSuffix. Names that are too long or contain a
// character outside the conservative alphabet get a deterministic alias
// that still ends with toolSuffix. used is updated by the caller.
func cloakWireName(original string, index int, used map[string]bool) string {
	if wire, ok := ordinaryWireName(original); ok && !used[wire] {
		return wire
	}
	return aliasWireName(original, index, used)
}

func ordinaryWireName(original string) (string, bool) {
	if original == "" || len(original)+len(toolSuffix) > toolNameLimit {
		return "", false
	}
	for i, r := range original {
		if !aliasRune(r, i == 0) {
			return "", false
		}
	}
	return original + toolSuffix, true
}

func aliasRune(r rune, first bool) bool {
	if first {
		return unicode.IsLetter(r) && r <= unicode.MaxASCII
	}
	return (unicode.IsLetter(r) || unicode.IsDigit(r)) && r <= unicode.MaxASCII || r == '_'
}

// aliasWireName keeps the _ide marker and resolves collisions by lengthening
// a deterministic digest, then by a counter. A fixed-length hash alone is
// not the uniqueness mechanism.
func aliasWireName(original string, index int, used map[string]bool) string {
	sum := sha256.Sum256([]byte(original))
	digest := hex.EncodeToString(sum[:])
	prefix := aliasPrefix(original)
	for n := 8; n <= len(digest) && n <= toolAliasTailLimit; n += 4 {
		wire := fitAlias(prefix, digest[:n])
		if !used[wire] {
			return wire
		}
	}
	for n := 2; n < 100000; n++ {
		wire := fitAlias(prefix, digest[:12]+"_"+base36(n))
		if !used[wire] {
			return wire
		}
	}
	// The counter space above cannot fill a 64-character alphabet. This
	// fallback exists so a future change cannot loop forever.
	for n := 0; ; n++ {
		wire := fitAlias("t", base36(index)+"_"+base36(n))
		if !used[wire] {
			return wire
		}
	}
}

func aliasPrefix(original string) string {
	var b strings.Builder
	for _, r := range original {
		switch {
		case unicode.IsLetter(r) && r <= unicode.MaxASCII:
			b.WriteRune(unicode.ToLower(r))
		case unicode.IsDigit(r) && r <= unicode.MaxASCII && b.Len() > 0:
			b.WriteRune(r)
		case r == '_' && b.Len() > 0:
			b.WriteByte('_')
		default:
			if b.Len() > 0 {
				b.WriteByte('_')
			}
		}
		if b.Len() >= 20 {
			break
		}
	}
	prefix := strings.Trim(b.String(), "_")
	if prefix == "" || !aliasRune(rune(prefix[0]), true) {
		return "tool"
	}
	return prefix
}

func fitAlias(prefix, tail string) string {
	// Reserve one leading letter, the separator, and the cloak suffix even if
	// a future collision strategy supplies a longer tail.
	if len(tail) > toolAliasTailLimit {
		tail = tail[:toolAliasTailLimit]
	}
	suffix := "_" + tail + toolSuffix
	room := toolNameLimit - len(suffix)
	if len(prefix) > room {
		prefix = prefix[:room]
	}
	prefix = strings.TrimRight(prefix, "_")
	if prefix == "" || !aliasRune(rune(prefix[0]), true) {
		prefix = "t"
	}
	return prefix + suffix
}

func base36(n int) string {
	if n < 0 {
		n = 0
	}
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = aliasAlphabet[n%len(aliasAlphabet)]
		n /= len(aliasAlphabet)
	}
	return string(b[i:])
}

// resolveWireName maps one upstream tool name back to the exact client name.
// ok is false for decoys and names that are not in this request's catalog.
func (c toolCatalog) resolveWireName(wire string) (string, bool) {
	if c.decoys[wire] {
		return "", false
	}
	original, ok := c.toClient[wire]
	return original, ok
}
