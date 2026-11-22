# airouter

A self-hosted AI inference router with an embedded web dashboard. Point OpenAI,
Anthropic, or compatible clients at one endpoint and route them to different
providers under your own model names.

- OpenAI Chat Completions, OpenAI Responses, and Anthropic Messages APIs
- Bidirectional format translation for unary and streaming requests
- Text, images, PDFs, reasoning, tool calls, and tool results
- Ordered failover or round-robin routing across providers
- API key and OAuth provider connections
- Single Go binary with embedded SQLite; no external services

## Quick start

Download a binary from [Releases](https://github.com/sabitm/airouter/releases), or
build from source with Go 1.26.1+:

```sh
go build -o airouter ./cmd/airouter
```

Generate a secret, keep it stable across restarts, and run:

```sh
export AIROUTER_SECRET="$(openssl rand -hex 32)"
./airouter
```

Open [http://localhost:31415/dashboard](http://localhost:31415/dashboard), then:

1. Add and check a provider.
2. Create a combo that maps a public model name to one or more provider models.
3. Generate an access key.
4. Use the airouter URL, access key, and combo name in your client.

The dashboard also shows ready-to-copy base URLs for Claude Code and Codex.

![Dashboard](dashboard.png)

## Supported providers

Generic connections support OpenAI Chat Completions, OpenAI Responses, and
Anthropic-compatible APIs. The dashboard also includes guided connections for:

| Connection | Authentication |
|---|---|
| Grok (xAI) | OAuth |
| OpenAI Codex (ChatGPT) | OAuth |
| Cline / ClinePass | OAuth |
| Kiro (AWS CodeWhisperer) | API key or OAuth |
| Qoder | OAuth device flow |
| Antigravity (Google Cloud Code) | OAuth; unofficial |
| Cursor | OAuth or imported CLI/IDE token; unofficial |
| Claude Code | OAuth; unofficial CLI profile, not a supported proxy |
| OpenCode | zen tier: key `public`; go tier: API key or OpenCode Console sign-in |

Provider credentials and OAuth tokens are encrypted in SQLite using
`AIROUTER_SECRET`. OAuth tokens are refreshed automatically when supported.

### OpenCode notes

Select the Zen or Go tier in the dashboard. Zen uses the key `public`. Go
accepts an opencode.ai API key, or Sign in with OpenCode Console. Console
sign-in is Go only. It stores the connected workspace and does not accept a
pasted token. airouter selects the upstream API format and endpoint for each
model automatically. Custom and self-hosted URLs are used unchanged for API-key
rows. Console credentials are sent only to official Go endpoints.

A successful dashboard Check for an API key confirms model discovery, not
inference access. A Console Check calls the Go usage endpoint and does not
treat public model discovery as authorization. The Usage tab shows the same
Go allowance for a Console connection. It does not show Zen, a Go API key, or
a custom URL. Free-model access depends on OpenCode's upstream restrictions
and is not guaranteed. Paid Zen models and OpenCode Go are the supported
paths for third-party clients.

## Client API

| Endpoint | Format |
|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses` | OpenAI Responses |
| `POST /v1/messages` | Anthropic Messages |
| `POST /v1/images/generations` | OpenAI-compatible Images generation (Codex only) |
| `POST /v1/images/edits` | JSON image edits with local data-URL references (Codex only) |
| `GET /v1/models` | Configured combos |

The same routes are available without `/v1`. Use a combo name as the request's
`model`. When at least one access key exists, authenticate with either:

```text
Authorization: Bearer sk-air-...
x-api-key: sk-air-...
```

Example:

```sh
curl http://localhost:31415/v1/chat/completions \
  -H "Authorization: Bearer sk-air-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"default","messages":[{"role":"user","content":"hello"}]}'
```

When no access keys exist, proxy endpoints run in open mode.

Custom Chat Completions models that route to OpenAI Codex should set
`sendSessionAffinityHeaders=true` and `sessionAffinityFormat=openai`. Those
headers are the authoritative conversation identity for Codex caching. When no
identity arrives, Airouter derives a fallback from an available initial
conversation prefix. If no safe bounded anchor is available, Airouter uses a
request-local key.

## Codex image generation

Use an existing Codex provider and create a combo whose upstream model is an
image model. No separate credential is needed. For example, configure a combo
named `image` with the upstream model `gpt-image-2.6-flare`:

```sh
curl http://localhost:31415/v1/images/generations \
  -H "Authorization: Bearer sk-air-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"image","prompt":"Draw a cabin beside a lake"}'
```

The response contains one PNG image in `data[0].b64_json`. Decode that base64
string to save the image. The route is also available as `/images/generations`.
The configured upstream model ID is sent unchanged. Model availability depends
on the provider and account. The linked references list `gpt-image-2`,
`gpt-image-2.6-flare`, and `gpt-image-2.6-sunburst`; this does not establish an
account entitlement or a bare `gpt-image-2.6` alias.

| Field | Supported values |
|---|---|
| `model` | Required combo name |
| `prompt` | Required nonempty string |
| `n` | `1` (default) |
| `size` | `auto` (default) |
| `quality` | `auto` (default), `low`, `medium`, `high` |
| `background` | `auto` (default), `opaque` |
| `output_format` | `png` (default) |
| `response_format` | `b64_json` (default) |
| `stream` | Omitted or `false` |
| `user` | Optional string, accepted locally but not sent upstream |

Unsupported fields and controls return HTTP 400 before an upstream send.
Generation rejects reference fields. Masks, fixed sizes, multiple outputs,
transparent backgrounds, alternate formats, compression, streaming, and
image-generation tools through `/v1/responses` are not supported. Accepted
controls follow the native request format. Live behavior can still depend on
the upstream.

Only enabled targets using the Codex protocol can serve this route. Incompatible
targets are skipped without a health penalty. Existing combo round-robin and
provider backoff rules still apply. Image failures can defer the same provider
for later chat requests. Failover before a response is sent can generate and
bill twice if an earlier upstream completed but its response was lost.

Image requests use the five-minute unary timeout and the existing 64 MiB request
and upstream-response limits. Returned base64 and its PNG signature are checked
before a successful response is sent. Reported token usage is preserved and is
not estimated from image bytes. HAR capture remains opt-in and truncates each
body at 1 MiB, so a captured image can be incomplete.

### Codex image edits

`POST /v1/images/edits` and `/images/edits` accept the same fixed controls as
generation, plus a required `images` array. This is a restricted JSON contract.
It is not OpenAI multipart compatibility. Each item must be an object with only
`image_url`, and that value must be a base64 data URL. Send 1-5 references in
order. PNG, JPEG, and WebP are accepted. `image/jpg` is normalized to
`image/jpeg`. The detected signature must match the declared type.

```sh
curl http://localhost:31415/v1/images/edits \
  -H "Authorization: Bearer sk-air-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"image","prompt":"Add a red roof","images":[{"image_url":"data:image/png;base64,iVBORw0KGgo..."}]}'
```

The upstream body is `model`, `prompt`, `images`, `n=1`, `size=auto`,
`quality`, and `background`. Airouter does not send `output_format`,
`response_format`, `stream`, or `user`. Codex CLI omits `n`; this proxy sends
`n=1`. That choice follows the typed request. It does not prove that every
account accepts the field.

Reference limits are local safety caps, not provider entitlements. Each decoded
reference can be at most 10 MiB. All references together can be at most 32 MiB.
The HTTP body remains limited to 64 MiB. These caps apply together. A request
over a reference or body cap returns HTTP 413 and does not contact upstream.
Malformed references, remote URLs, file IDs, GIF, masks, and the singular
`image` field return HTTP 400 with no upstream contact and no health penalty.

Base64 must be canonical padded standard encoding with no whitespace. URL-safe
and raw unpadded forms are rejected. The response decoder is the same PNG
decoder used for generation. Missing usage stays missing. A configured model
string is not proof that the account can edit images.

Local reference images are sent through Airouter to the configured provider.
Enabled HAR capture can contain reference image data, truncated at 1 MiB per
body. Check references for private content before sending. A timeout or canceled
edit may still be billed. Do not resend without approval.

### Agent skill

The distributable [`image-generation` skill](skills/image-generation/SKILL.md)
uses these endpoints without adding tools to chat requests. It saves decoded
PNGs in the agent's workspace. The chat provider does not need image support.
The image combo still requires an enabled Codex target.

Copy the complete `skills/image-generation/` directory into your harness's skill
location. Pi supports `.agents/skills/image-generation/` inside a project or
`~/.agents/skills/image-generation/` for the current user. Other harnesses may
use different locations. Reload skills or restart the harness after installing.
In Pi, invoke `/skill:image-generation` explicitly if automatic selection does
not load it. Installation does not change Airouter or install a runtime.

The skill checks for Python 3.10+ first, then Node.js 22+. Both bundled helpers
use built-in libraries only and accept the same arguments. If neither runtime
is present, install one separately. Do not switch runtimes and resend after a
generation failure because the first request may already have incurred charges.

From this repository, generate one image with either command:

```sh
python3 skills/image-generation/scripts/generate_image.py --prompt "A cabin beside a lake, watercolor" --output images/cabin.png
node skills/image-generation/scripts/generate_image.mjs --prompt "A cabin beside a lake, watercolor" --output images/cabin-node.png
```

Use only one command for each requested image. On Windows, use `py -3` or
`python` instead of `python3` when that is the supported interpreter. Set
`AIROUTER_URL`, `AIROUTER_IMAGE_MODEL`, and optional `AIROUTER_API_KEY` in the
agent environment. The URL and combo default to `http://localhost:31415` and
`image`. `--url` and `--model` override those defaults. No key is sent in open
mode when `AIROUTER_API_KEY` is unset. Keep remote authenticated connections on
HTTPS. The helpers connect directly without environment proxies or redirects.

For batches, pass `--batch jobs.json` with a JSON array of `prompt`/`output`
objects. Jobs may override `quality` and `background`. An optional `images`
array of 1-5 local paths selects an edit for that job. Omit it to generate.
Repeat `--image PATH` for one prompt. Do not combine `--image` with `--batch`.
Relative reference and output paths resolve from the process working directory,
not from the batch file. The queue runs serially, preserves successful files,
and reports each result as one JSON line without base64 data or input paths.
It never overwrites outputs, retries a failed job, or falls back from an edit
to generation. See the skill for examples, limits, and timeout guidance.

## Routing

- A **provider** is an upstream URL, protocol, and credential. Optional tags
  (lowercase letters, digits, and internal hyphens) label providers for the
  dashboard. The Providers and Usage tabs can filter by one tag or Untagged.
  Usage **Load all** loads only providers that match the active filter.
- A **combo** is the model name clients use. Each target maps a provider to its
  real upstream model ID.
- An **access key** authenticates clients to airouter. Its full value is shown
  once; only its hash is stored.

`failover` tries targets in order. `roundrobin` rotates the first target and still
fails over if it cannot start a response. Repeatedly failing providers are
temporarily deprioritized.

Matching API formats pass through with only the model rewritten, preserving
provider-specific fields. Different formats are translated automatically,
including live streams.

Anthropic thinking signatures and redacted thinking blocks are preserved when
both the client format and the selected provider format are Anthropic Messages,
Claude Code, or OpenCode Messages. A request that already contains signed or
redacted thinking is rejected with HTTP 400 when no enabled combo target can
preserve that history. Other client formats receive only readable thinking text.
The proxy does not copy Anthropic signatures into OpenAI encrypted content and
does not return redacted data as text.

Claude Code uses a pinned CLI profile (version 2.1.318, SDK 0.128.0), not full
CLI wire parity. The billing builder writes the placeholder `cch=00000` only
for an official host. A custom host does not receive that field. Each Messages
send then replaces one placeholder in the final body when the resolved upstream
URL contains `/v1/messages` and the compact system marker has that placeholder
in its 300-byte window. The patch does not add its own host check. The
replacement is five lowercase hex digits. It is not the three-hex `cc_version`
fingerprint. Discovery requests are not patched. This covers plaintext
activation, raw byte exclusions, and the five-digit overwrite only. It does not
prove gzip ordering, random padding, HTTP/2 framing, CLI runtime behavior, or
provider acceptance. Caller tool names stay unchanged, unlike the CLI's
conditional MCP name mapping. A dashboard Check
calls `GET /models?limit=1000` only. A passing Check does not prove that Messages
requests will be accepted, and it does not send an inference request. The
profile does not include the Claude Agent SDK runtime, cloud gateway, remote
workers, or telemetry. A browser connect uses the official authorize and token
endpoints, but that login path is not a verified first-party login. Existing
connections keep their stored endpoints and are not expanded to newer scopes.
The oauth beta uses a stored grant only when that grant is paired with the
effective token and contains the exact scope `user:inference`. A known grant
without that scope does not receive the oauth beta. An unknown grant, a
whitespace-only scope list, or a token that does not match its stored
credential uses the legacy `sk-ant-oat` check. That fallback includes imported
static bearer tokens. An API-key provider ignores OAuth credential scopes.
This rule classifies only the oauth beta. It does not authorize inference,
change the authentication scheme, or claim full CLI or provider conformance.

Inference advertises interleaved-thinking and context-management betas for
supported, known models even when the body does not use those features.
Redact-thinking still requires active, nonsummarized thinking. The cache-scope
beta is automatic on the official API host; custom endpoints retain explicit
cache-scope requests. These headers do not add cache markers or change TTLs.
A target's `[1m]` selector does not add the long-context beta on pinned
native-1M models. Explicit beta requests retain the existing capability gate.
Structured-output beta requests exclude Opus 4.0 and Sonnet 4.0, but allow
Opus 4.1. Other beta flags keep their existing feature gates. This is a bounded
proxy policy, not the CLI's organization flags, served model overrides, or
beta-rejection retry policy. Discovery does not advertise inference features.

Claude Code stream completion is stricter than generic Anthropic decoding. A
successful stream needs a valid `message_start`, no open text, tool, thinking,
or server block, and either `message_stop` or a `message_delta` whose
`stop_reason` is a JSON string. A clean end after that terminal delta is
complete. `message_stop` is not required in that case. A stream that only
starts, a content event before `message_start`, or an open block at the end is
an error. The proxy does not report that truncated output as a successful
finish. An explicit `message_stop` with no stop reason still ends as
`end_turn`. This is a bounded proxy rule. It does not reproduce CLI retries,
partial salvage, or server-side model fallback, and it is not full CLI or
provider conformance.

Claude Code stream usage merges `message_start` and each `message_delta` for
ordinary input, cache read, cache write, and output only. A positive ordinary
input, cache read, or aggregate cache write replaces the prior value. An
omitted, null, or zero value keeps that prior value. A positive
`cache_creation_input_tokens` wins over the current frame's
`ephemeral_1h_input_tokens` plus `ephemeral_5m_input_tokens`. Otherwise a
positive sum of those two current-frame fields becomes cache write. The proxy
does not add TTL values from earlier frames. Output uses nullish coalescing:
omitted and null keep the prior output, and an explicit zero replaces it. A
successful finish is marked when any supported non-null numeric counter was
present, including explicit zero, a partial report, or a recognized TTL
counter. Missing, null, and unknown-only usage are not marked. Inclusive input
is ordinary plus cache read plus cache write. Cache buckets are not added
again. Negative counters and sums that overflow are rejected without copying
upstream usage JSON or counter values into the error. A usage-only delta does
not complete a stream. The proxy does not store TTL breakdowns, thinking
tokens, server-tool counts, service tier, speed, iterations, inference
geography, or fallback credit, and it does not apply fallback-credit
adjustment. It does not change pricing, quota, or unary response decoding.
Generic Anthropic and OpenCode streams keep their existing usage replacement.
A marked Claude Code finish is the log snapshot, including zero and partial
reports, and it bypasses the request-size estimate. Unmarked usage keeps zero
wire counts and the existing log-only estimate. The Logs page caps aggregate
input and output totals to the signed 64-bit range without changing the
stored count for any request. This is not full CLI usage accounting or
provider conformance.

## Configuration

Flags override environment variables.

| Flag | Environment | Default | Purpose |
|---|---|---|---|
| `-listen` | `AIROUTER_LISTEN` | `:31415` | HTTP listen address |
| `-db` | `AIROUTER_DB` | `airouter.db` | SQLite database path |
| `-secret` | `AIROUTER_SECRET` | insecure development key | Credential encryption secret |
| `-debug` | `AIROUTER_DEBUG` | `0` | `1` for request diagnostics; `2` for metadata traces |
| `-har-file` | `AIROUTER_HAR_FILE` | unset | Always-on HAR capture and shutdown output path |
| `-disable-dashboard` | `AIROUTER_DISABLE_DASHBOARD` | `false` | Do not mount the web dashboard or `/static` assets |
| `-version` | — | `false` | Print the version and exit |

Debug logs never include request or response bodies. Capture full exchanges from
**Dashboard > Settings**, or use `-har-file`. HAR files include prompts,
responses, authorization headers, and provider credentials.

### OpenCode session diagnostics

With `-debug=2`, official OpenCode free-model requests log session and connection
metadata without adding requests or retries. Session identifiers are masked;
prompts, bodies, and credentials are not logged. Keep these logs private because
they can link requests within one server run.

## Security and data

- Always set and retain a strong `AIROUTER_SECRET`. Changing it makes existing
  encrypted credentials unreadable.
- The dashboard controls providers and exposes plaintext config exports. It has
  no built-in login; keep it on a trusted network or protect it with an
  authenticated reverse proxy. In production, prefer `-disable-dashboard` so
  the UI, `/static` assets, config import/export, and OAuth connect flows are
  not mounted.
- Config exports include provider API keys, OAuth tokens, and tags in plaintext.
  Access keys and request logs are not exported.
- HAR captures contain sensitive headers and bodies. Store and share them as
  secrets. `-disable-dashboard` does not disable `GET /debug/har`; that
  endpoint remains mounted and may serve captured prompts and credentials.

State is stored in one automatically migrated SQLite database using WAL mode.
Provider credentials are encrypted at rest; access keys are hashed.

## Development

```sh
templ generate   # after editing internal/web/*.templ
go test ./...
go vet ./...
```

Validate both skill helpers against local mock servers with Python 3.10+ and
Node.js 22+ installed:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 skills/image-generation/tests/test_helpers.py
```

The suite reports when Node.js is unavailable and only the Python contract ran.
It does not call a live provider. Windows users can omit the environment prefix
and use their Python command.

[MIT License](LICENSE)
