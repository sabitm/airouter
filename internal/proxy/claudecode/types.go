package claudecode

// Request preparation edits the encoded Anthropic body as a JSON object.
// A narrow typed remarshal is intentionally absent: it dropped
// context_management, cache_control, thinking.display, output_config.format,
// and tool declaration fields.
