package cursor

import "strings"

// visibleComposerContentFromThinking returns the content after the last
// closing-think tag, trimmed of leading whitespace. Empty when no closing tag
// is present (the thinking block is still open, or this is a non-composer stream
// whose thinking carries no marker). Composer models embed their visible answer
// in the thinking field after the think block; DecodeStream buffers thinking and
// emits this slice incrementally.
func visibleComposerContentFromThinking(thinking string) string {
	if thinking == "" {
		return ""
	}
	const endTag = "</think>"
	idx := strings.LastIndex(thinking, endTag)
	if idx < 0 {
		return ""
	}
	return strings.TrimLeft(thinking[idx+len(endTag):], " \t\r\n")
}
