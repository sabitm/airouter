# AGENTS.md

Guidance for AI agents working in this repository.

## What this is

airouter is a bidirectional AI inference proxy plus a web dashboard, in a single
Go binary. It translates between the OpenAI and Anthropic API wire formats so a
client speaking one format can call a backend speaking the other. See README.md
for the user-facing model (providers, combos, access keys).

## Architecture

Translation goes through a canonical intermediate representation (IR) rather than
pairwise converters. Every wire format decodes into the IR and encodes out of it,
keeping the converter count linear in the number of formats:

```
ingress format  --decode-->  IR  --encode-->  backend format   (request)
backend format  --decode-->  IR  --encode-->  ingress format   (response, incl. SSE)
```

- `internal/proxy/ir` - canonical types. `ir.go` is the unary request/response
  model; `stream.go` is the `StreamEvent` delta union for streaming.
- `internal/proxy/openai` - OpenAI Chat Completions codec (all four directions +
  SSE). Can act as ingress or backend.
- `internal/proxy/anthropic` - Anthropic Messages codec (all four directions +
  SSE). Can act as ingress or backend.
- `internal/proxy/responses` - OpenAI Responses codec. **Ingress only**: a
  provider is never reached over the Responses API, so this package implements
  only request-decode, response-encode, and stream-encode.
- `internal/proxy/sse` - minimal SSE reader/writer shared by the streaming codecs.
- `internal/proxy/proxy.go` - the `codec` struct bundling a format's directions,
  the codec instances, and route mounting.
- `internal/proxy/serve.go` - unary request lifecycle, auth, combo resolution.
- `internal/proxy/stream.go` - streaming lifecycle (passthrough relay + translate
  pump).
- `internal/proxy/client.go` - upstream forwarding (unary + streaming variants).
- `internal/proxy/models.go` - `GET /v1/models`.

Supporting packages:

- `internal/domain` - core entities (Provider, Combo, AccessKey, Protocol).
- `internal/store` - SQLite store, migrations, repos, JSON import/export.
- `internal/crypto` - AES-256-GCM for provider API keys at rest.
- `internal/config` - flags/env loading.
- `internal/server` - HTTP wiring: CORS (answers browser preflights, reflects
  the request Origin) and the leveled request-logging middleware. At `-debug`
  (level 1) it logs access lines; at `-debug=2` it also traces full request and
  response bodies and the resolved upstream provider URL per proxied call.
- `internal/web` - templ + HTMX dashboard. `.templ` files generate `*_templ.go`.
- `cmd/airouter` - main: wires config, crypto, store, server; graceful shutdown.

## The passthrough vs translate rule

Each codec has both an `id` (the wire format) and a `protocol` (used to select a
backend codec from a provider's protocol). The passthrough decision compares
**ids**, not protocols:

- Same id (e.g. OpenAI ingress -> OpenAI provider): pass through, rewriting only
  the `model` field. Provider-specific fields the IR does not model are preserved.
- Different id: translate request and response through the IR.

This is why `oai-responses` and `oai-chat` both have `protocol = openai` but
distinct ids: a Responses request to an OpenAI provider must still translate
(Responses != Chat Completions), so it must never match for passthrough.

When adding a new ingress format, give it a unique `id`. When adding a new
backend-capable format, also set its `protocol` and `upstreamPath` and add it to
`backendCodec`.

## Conventions

- Tool results are normalized Anthropic-style in the IR: carried as
  `tool_result` blocks inside a user message. OpenAI's `role:"tool"` messages and
  Responses' `function_call_output` items fold into this on decode and expand
  back on encode. Preserve this invariant in any new codec.
- The Anthropic Messages API requires `max_tokens`. When translating from a
  format that omits it, `anthropic.DefaultMaxTokens` (4096) is used.
- A provider's auth scheme is independent of its protocol. `Provider.Auth()`
  returns the effective scheme, defaulting an unset (`default`) one by protocol
  (Anthropic -> x-api-key, OpenAI -> bearer). The credential header follows
  `Auth()`; the `anthropic-version` header follows the protocol. Preserve this
  split when touching upstream request construction (`applyUpstreamHeaders`) or
  the dashboard provider check.
- Streaming uses a no-timeout HTTP client (`Proxy.streamClient`) so long streams
  are bounded by the request context, not a client timeout.
- Errors before the first streamed byte fall back to the ingress format's unary
  error envelope; mid-stream failures terminate the response cleanly.
- Each ingress format renders its own error envelope shape (`encodeError`).
- Token usage is recorded per request for the dashboard logs. Unary parses it
  from the response body; streaming requires care: OpenAI backends omit usage
  unless `stream_options.include_usage` is set on the request, OpenAI reports
  both counts on the final chunk while Anthropic reports input at message start
  and output at message delta, and the translate pump takes input from whichever
  event carries it. Streaming passthrough sniffs usage out of the relayed SSE
  without mutating the bytes. Preserve these when touching the streaming paths.

## Build, test, regenerate

```sh
# regenerate templ output after editing any internal/web/*.templ file
templ generate            # needs: go install github.com/a-h/templ/cmd/templ@latest

go build ./...
go test ./...
go vet ./...
```

The proxy test suite (`internal/proxy/*_test.go`) is the main safety net: it
covers codec-level translation plus an httptest matrix exercising every
ingress x backend combination for both unary and streaming, including tool-call
fragment reassembly. When changing translation logic, run these and add cases for
new mappings.

## Repository conventions

- Comments are reserved for non-obvious logic and stated assumptions; simple code
  stays uncommented.
- No emojis or decorative output anywhere in code, logs, or docs.
- Do not add auxiliary tracking/report files (SUMMARY.md, CHANGELOG.md, etc.).
