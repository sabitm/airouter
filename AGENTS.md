# AGENTS.md

Instructions for coding agents working in this repository.

## Project

airouter is a single Go binary containing a bidirectional AI inference proxy and
a templ/HTMX dashboard. It exposes OpenAI Chat Completions, OpenAI Responses,
and Anthropic Messages APIs, routes public combo names to provider models, and
translates requests and responses when client and backend formats differ.

See `README.md` for user-facing behavior.

## Architecture

Translation uses a canonical intermediate representation (IR):

```text
ingress -> decode -> IR -> encode -> backend
backend -> decode -> IR -> encode -> ingress
```

Do not add pairwise format converters.

Core packages:

- `internal/proxy/ir`: canonical unary types and streaming events.
- `internal/proxy/{openai,anthropic,responses}`: bidirectional codecs.
- `internal/proxy/{kiro,qoder,antigravity,cursor,claudecode}`: specialized
  backend codecs.
- `internal/proxy/proxy.go`: codec registry and ingress routes.
- `internal/proxy/serve.go`, `stream.go`, `client.go`: request lifecycle,
  failover, streaming, and upstream forwarding.
- `internal/domain`: providers, combos, access keys, protocols, and auth types.
- `internal/store`: SQLite repositories, migrations, and import/export.
- `internal/oauth`: OAuth connect, token resolution, and refresh.
- `internal/server`: HTTP middleware, CORS, request IDs, and HAR integration.
- `internal/web`: templ/HTMX dashboard and provider probes.
- `internal/observability`, `internal/harlog`: metadata logging and HAR capture.

OpenAI, Anthropic, and Responses codecs can be ingress formats. Codex, Kiro,
Qoder, Antigravity, Cursor, and Claude Code are backend-only variants.

## Core invariants

### Translation and codecs

- Passthrough compares codec `id`, not provider `protocol`. Equal IDs preserve
  the original body and rewrite only `model`; different IDs translate via IR.
- Distinct wire or provider-specific envelopes need distinct codec IDs. Add new
  backend protocols to `backendCodec` with the correct `upstreamPath`.
- Normalize tool results in IR as `tool_result` blocks in user messages. Decode
  OpenAI `role: "tool"` and Responses `function_call_output` into that form and
  expand them only during encoding.
- Anthropic requests require `max_tokens`; use `anthropic.DefaultMaxTokens` when
  the ingress format omits it.
- Keep each ingress format's own error envelope.

### Authentication

- Auth method (`apikey` or `oauth`), auth scheme (`bearer` or `x-api-key`), and
  protocol are independent. Use `Provider.Method()` and `Provider.Auth()` rather
  than inferring one from another.
- OAuth credentials remain self-contained in `domain.OAuthCreds`.
  `oauth.Service.Resolve` is the single effective-token path for proxy requests
  and dashboard probes.
- Resolve OAuth before each send. On 401/403, force-refresh and retry once only,
  before any response bytes are committed. Inject the resolved token into the
  request-local provider copy; keep generic header construction unchanged.

### Streaming, failover, and usage

- Use the no-timeout stream client; request context controls stream lifetime.
- Before the first streamed byte, failures may use the ingress unary error and
  fail over. After commitment, terminate cleanly and never attempt another
  target.
- Provider failure backoff is in-memory and defers unhealthy providers; it does
  not remove them. Archived providers and disabled targets are removed from
  resolution. Preserve this defer-versus-drop distinction and retain a usable
  least-bad target when all providers are backed off.
- Preserve token usage accounting. OpenAI streams need
  `stream_options.include_usage`; Anthropic reports input and output in separate
  events. Passthrough streaming must sniff usage without changing relayed bytes.

### Observability

- Terminal Debug/Trace logs contain metadata only, never HTTP bodies or auth
  headers. Full bodies belong only in explicitly enabled HAR capture.
- Preserve request IDs across context, response headers, logs, `TraceInfo`, and
  HAR pages.
- HAR uses a request-pinned recorder lease for both proxy legs. Do not eagerly
  drain request bodies or bypass `MaxBytesReader` while observing them.

## Backend-specific invariants

- **Codex:** Keep an ID distinct from public Responses. It is stream-only; unary
  calls collect SSE. The `session_id` header must match `prompt_cache_key`.
  Discover models through the account-aware `/models` endpoint with Codex CLI
  headers, not by probing `/responses` with a fixed model.
- **Cline/ClinePass:** This is an `OAuthCreds.ClineAuth` variant of OpenAI, not a
  protocol. Keep token prefixing and identity headers at the upstream-header
  seam, separate from token resolution.
- **Kiro:** Backend-only, stream-only AWS EventStream. Preserve profile/region
  config, CodeWhisperer request shape, and the protocol-specific auth/refresh
  paths.
- **Qoder:** Backend-only and stream-only. Prepare and WAF-encode the final body
  before COSY signing; fail closed without live `model_config`. Device tokens do
  not refresh, and Check uses plain Bearer userinfo.
- **Antigravity:** Backend-only and stream-only. Fail closed without `ProjectID`.
  Preserve project bootstrap, schema cleanup, thought-signature backfill, and
  tool cloak/decloak symmetry.
- **Cursor:** Backend-only Connect-RPC protobuf and stream-only. Authentication is
  an imported IDE token plus machine ID with no refresh. Preserve the current
  checksum algorithm, full identity-header override, `mcp_custom_` name mapping,
  and XML-flattened tool results.
- **Claude Code:** Keep an ID distinct from Anthropic so requests always pass
  through preparation. Preserve per-request session ID pairing between body and
  headers, OAuth-token-gated cloaking, tool decloaking, and CLI identity headers.

## Development

After editing `internal/web/*.templ`, regenerate committed output:

```sh
templ generate
```

Validate changes with:

```sh
go build ./...
go test ./...
go vet ./...
```

The proxy tests are the main safety net. Codec or translation changes require
focused unary and streaming tests, including tool-call fragment reassembly and
relevant ingress/backend combinations.

## Repository conventions

- Add comments only for non-obvious logic, assumptions, and fragile invariants.
- Do not add emojis, decorative output, or stylized headers to code, logs, or
  documentation.
- Do not add auxiliary reports or tracking files such as `SUMMARY.md`,
  `REPORT.md`, or `CHANGELOG.md`.
