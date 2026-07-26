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
- `internal/proxy/responses` - OpenAI Responses codec. Bidirectional: ingress
  when a client calls `/v1/responses`, and backend when a provider's protocol is
  `openai-responses` (an upstream that only exposes `/responses`) or
  `openai-codex` (ChatGPT Codex, using a Codex-specific request envelope).
  Implements all four directions plus both stream directions.
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
- `internal/crypto` - AES-256-GCM for provider API keys and OAuth tokens at rest.
- `internal/oauth` - OAuth connect (authorization code + PKCE) and token refresh
  for providers whose `auth_method` is `oauth`. Provider-agnostic: every
  connection carries its full config inline (`domain.OAuthCreds`), so connect and
  refresh read config from that struct, not a registry. `presets.go` holds the
  built-in prefills (xAI/Grok, OpenAI Codex, Cline/ClinePass) applied at create time.
  `Service.Resolve` is the
  single entry point the proxy and dashboard probes call to get an effective
  bearer token, refreshing proactively (near expiry) or on a forced reactive
  retry. `Connect` drives one connect attempt (loopback callback or manual paste)
  and holds the PKCE verifier/state. To avoid a store->oauth import cycle the
  service depends on the narrow `ProviderStore` interface, which `*store.Store`
  satisfies; `oauth.Service` is constructed internally by `proxy.New` and
  `web.NewHandler`, so neither constructor's signature changed.
- `internal/config` - flags/env loading.
- `internal/server` - HTTP wiring: CORS (answers browser preflights, reflects
  the request Origin) and the leveled request-logging middleware. At `-debug`
  (level 1) it logs access lines; at `-debug=2` it also traces request and
  response bodies and the resolved upstream provider URL per proxied call. With
  `-log-file`, the file sink receives full bodies while stderr stays truncated.
- `internal/web` - templ + HTMX dashboard. `.templ` files generate `*_templ.go`.
  Dashboard outbound provider probes (Check/model autocomplete) follow the same
  trace split: full bodies to `-log-file`, truncated bodies to stderr.
- `cmd/airouter` - main: wires config, crypto, store, server; graceful shutdown.

## The passthrough vs translate rule

Each codec has both an `id` (the wire format) and a `protocol` (used to select a
backend codec from a provider's protocol). The passthrough decision compares
**ids**, not protocols:

- Same id (e.g. OpenAI ingress -> OpenAI provider): pass through, rewriting only
  the `model` field. Provider-specific fields the IR does not model are preserved.
- Different id: translate request and response through the IR.

This is why `oai-responses` (protocol `openai-responses`) and `oai-chat`
(protocol `openai`) have distinct ids: a Responses request to a Chat-Completions
provider must still translate (Responses != Chat Completions), so their ids
differ and they never match for passthrough. A Responses request to a Responses
provider does share the id, so it passes through with only the model rewritten.

When adding a new ingress format, give it a unique `id`. When adding a new
backend-capable format, also set its `protocol` and `upstreamPath` and add it to
`backendCodec`.

## OpenAI Codex backend

`openai-codex` is a backend protocol for the ChatGPT Codex API. It reuses the
Responses IR mapping but has Codex-specific upstream behavior:

- The codec id is distinct from `oai-responses`, so Responses ingress to Codex
  translates rather than passing through. Preserve this; the Codex request shape
  is not the public OpenAI Responses shape.
- Upstream requests use the Codex CLI identity headers (`User-Agent`,
  `originator`, `session_id`, and `chatgpt-account-id` when available). The
  `session_id` header and the request body's `prompt_cache_key` must match.
- Codex upstream responses are SSE-only. Unary client requests to a Codex backend
  are sent upstream as a stream and collected back into a unary IR response.
- Codex model discovery for the dashboard uses
  `/models?client_version=<CodexCLIVersion>` with the same identity headers.
  Do not probe `/responses` with a hardcoded model; model availability is
  account-dependent.

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
- A provider's auth *method* (`Provider.Method()`: `apikey` or `oauth`, empty
  defaults to `apikey`) is separate from its auth *scheme*. OAuth providers store
  no static key and always resolve to a `bearer` token. The proxy and dashboard
  probes call `oauth.Service.Resolve` to get the effective token, then overwrite
  the request-local hydrated `provider.APIKey` with it - so `applyUpstreamHeaders`
  and the header split above stay unchanged and credential construction never
  needs to special-case oauth. The hot path resolves proactively before each send
  and, on a 401/403 from an oauth provider, force-refreshes and retries once
  before any response byte is committed. Preserve this token-injection point and
  the single reactive retry when touching `client.go`/`serve.go`/`stream.go`.
- Cline/ClinePass OAuth is a marker-gated variant on self-contained `OAuthCreds`
  (`ClineAuth`), not a new protocol: upstream remains OpenAI Chat Completions.
  Connect skips client_id/PKCE and builds a Cline authorize URL
  (`client_type=extension`, `callback_url`+`redirect_uri`); exchange prefers a
  base64-embedded token JSON in the redirect `code`, with a JSON token-endpoint
  fallback. Refresh posts camelCase JSON to optional `RefreshURL` (else
  `TokenURL`). Access tokens are stored and sent with an idempotent `workos:`
  prefix; Cline identity headers are applied at the same upstream-header seam as
  Codex/Kiro (`applyUpstreamHeaders` and dashboard probes), not inside
  `Service.Resolve`. Preserve the marker + header-seam split when touching
  connect/refresh or upstream construction.
- Qoder (`ProtocolQoder`) is a backend-only protocol: device-flow OAuth against
  `openapi.qoder.sh`, chat against `api3.qoder.sh` with COSY-signed WAF-encoded
  bodies and SSE-only responses (`streamOnly`). The codec id is `qoder`, so every
  ingress translates through the IR. `prepareUpstreamRequest` injects the live
  `model_config` (fail-closed when unknown) and WAF-encodes; `applyUpstreamHeaders`
  then COSY-signs those wire bytes last (overwriting Authorization) and sets
  `X-Model-Key`/`X-Model-Source` from the request-local `TraceInfo`.
  `OAuthCreds.QoderAuth` marks the connection: `shouldRefresh` skips it,
  `refreshQoder` returns `ErrInvalidGrant` (device tokens do not refresh; the
  reactive 401 path surfaces reconnect), and Check probes userinfo with plain
  Bearer rather than COSY. UserID/MachineID live on `OAuthCreds` and feed COSY
  via `CredsFromProvider`. Preserve the prepare-then-sign order, fail-closed
  model_config, and no-refresh behavior when touching Qoder paths.
- Antigravity (`ProtocolAntigravity`) is backend-only Google Cloud Code chat:
  Google OAuth auth-code + `loadCodeAssist`/`onboardUser` project bootstrap; chat
  is SSE-only (`streamOnly`) through codec id `antigravity`. `prepareUpstreamRequest`
  fail-closes without `OAuthCreds.ProjectID` and injects it into the envelope;
  headers force the IDE User-Agent. Encode applies schema clean, thoughtSignature
  backfill, then `_ide` tool cloaking + native decoys; decode strips `_ide` on tool
  names. Refresh is generic Google form OAuth. Preserve fail-closed project,
  cloak/decloak pair, and finalize-on-connect when touching Antigravity paths.
- Cursor (`ProtocolCursor`) is a backend-only protocol for the Cursor IDE: Connect-RPC
  protobuf chat (ChatService `StreamUnifiedChatWithTools`), stream-only, codec id
  `cursor` so every ingress translates through the IR. Auth is a pasted IDE access
  token + machine id (no OAuth flow, no refresh); `CursorAuth` marks the connection
  and `MachineID` feeds the jyh-cipher `x-cursor-checksum` header (use
  `cursorChecksum.js`, not the older `oauth/services/cursor.js` cipher that omits
  the `+i%256` term). `applyUpstreamHeaders` overwrites the full identity header set
  after the client-header copy. Tool results are flattened to XML text in user
  messages (protobuf `tool_results` loop on partial schemas); tools encode as
  `mcp_custom_*` and decode strips that prefix. `CanRefresh` is false (mirrors
  Qoder); `refreshCursor` returns `ErrInvalidGrant` so the reactive 401 path prompts
  re-paste. v1 ships ChatService only; the AgentService text path (HTTP/2 duplex)
  is deferred. Preserve the manual-paste/no-refresh contract and the XML-tool-result
  invariant when touching Cursor paths.
- Claude Code (`ProtocolClaudeCode`) is a backend-only protocol that speaks the
  Anthropic Messages wire format while impersonating the Claude Code CLI: it
  reuses the `anthropic` codec's encoder/stream-encoder/error envelope but has a
  distinct codec id (`claude-code`) so Anthropic ingress never passes through and
  always translates through the cloak prepare step. `prepareUpstreamRequest`
  generates a per-request session id (stored on `TraceInfo.ClaudeCodeSessionID`)
  and calls `claudecode.ApplyOAuthCloaking`, gated on the `sk-ant-oat` marker of
  the stored access token (read from `OAuthCreds` at prepare time, since the
  resolved token lands on `provider.APIKey` only later inside `forward`). The
  cloak injects an `x-anthropic-billing-header` system block (cch over the
  pre-injection body), a fake `metadata.user_id` seeded from the refresh token
  (stable across access-token refresh), and the `_ide` tool suffix + CC decoy
  tools; `decodeResponse`/`decodeStream` strip one `_ide` (`DecloakName`) so the
  client sees its original tool names. `applyUpstreamHeaders` sets the CLI
  fingerprint (anthropic-version/beta, User-Agent, X-Stainless-*) and
  `X-Claude-Code-Session-Id` (matching `metadata.user_id.session_id`) after the
  client-header copy. OAuth is claude.ai auth-code + PKCE with a JSON token
  exchange (`ClaudeCodeAuth` routes `exchangeClaudeCode`) against
  `api.anthropic.com/v1/oauth/token`, mirroring 9router's proven config: the
  3-scope inference grant (org:create_api_key user:profile user:inference)
  issues a valid sk-ant-oat token the cloak gates on. The real CLI's newer
  claude.com/cai gateway + 6-scope flow needs first-party gateway session context
  a direct browser hit from the connect page cannot establish, so it is not used;
  refresh reuses the generic `RefreshJSON` path. apikey providers get identity
  headers but no cloak.
  Preserve the prepare-then-header session-id pairing, the OAuth-only cloak gate,
  and the distinct-id/never-passthrough rule when touching Claude Code paths.
- Streaming uses a no-timeout HTTP client (`Proxy.streamClient`) so long streams
  are bounded by the request context, not a client timeout.
- Errors before the first streamed byte fall back to the ingress format's unary
  error envelope; mid-stream failures terminate the response cleanly.
- Each ingress format renders its own error envelope shape (`encodeError`).
- Failover backoff is per-provider and request-count-based, held in memory on the
  `Proxy` (`bo`), not persisted (it resets on restart). A target that fails before
  committing any bytes is penalized (`penalizeProvider`); `orderTargets` then
  defers that provider behind healthy targets for an exponentially growing number
  of subsequent requests (2, 4, 8, ... clamped at `backoffMaxSkips`), consuming
  one skip credit per unique provider per request (`providerBackedOff`), and never
  drops it - an all-backed-off combo still resolves its least-bad option. A
  committed success clears the penalty (`clearBackoff`). Archived providers and
  disabled targets, by contrast, are dropped from resolution entirely. Preserve
  this drop-vs-defer distinction when touching
  `orderTargets`/`penalizeProvider`/`providerBackedOff`.
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
