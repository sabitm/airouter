---
name: image-generation
description: Generate or edit images through an Airouter image combo and save PNG files in the agent workspace. Use when the user requests generated artwork, edits to existing local images, reference-based images, illustrations, visual assets, or serial batches. Select the bundled Python or Node.js helper before sending requests.
compatibility: Requires command execution, network access to Airouter, and Python 3.10+ or Node.js 22+. Uses built-in libraries only. Airouter must have an image combo backed by an enabled Codex target.
---

# Image generation

Use the bundled helper to call Airouter's native Images API. The agent's chat provider does not need image-generation support. Do not add hosted image tools to chat requests or call `/v1/responses` for this workflow.

## Before generating

1. Confirm the user's prompt, number of images, and output location. Use unique `.png` paths inside the intended workspace.
2. Check for Python 3.10 or newer. On Linux/macOS, try `python3 --version`. On Windows, try `py -3 --version` or `python --version`.
3. If a supported Python is unavailable, check `node --version` for Node.js 22 or newer. If neither runtime is available, report the requirement. Do not install a runtime or packages automatically.
4. Select one helper and run its `--help`. Resolve script paths relative to this skill's directory. Do not depend on the current working directory being the skill directory.
5. Read the configured endpoint and combo without printing credentials. Use the defaults only if they match the user's setup.
6. Follow the harness's permission rules for network calls and file creation. Requests can incur charges. Do not generate extra images beyond the requested count.

Use Python when both supported runtimes are available. Runtime detection and fallback happen before generation. Never resend with the other runtime after a timeout, HTTP failure, or missing output.

## Configuration

| Setting | Default | Override |
|---|---|---|
| Airouter base URL | `http://localhost:31415` | `AIROUTER_URL` or `--url` |
| Image combo | `image` | `AIROUTER_IMAGE_MODEL` or `--model` |
| Airouter access key | None | `AIROUTER_API_KEY` only |
| Quality | `auto` | `--quality` |
| Background | `auto` | `--background` |
| Network operation timeout | 330 seconds | `--timeout` |

CLI options override environment settings. `--model` is a combo name, not an upstream model ID. Configure the upstream image model in the Airouter dashboard.

Use an HTTP(S) server base URL, optionally ending in `/v1`. For example, both `http://localhost:31415` and `http://localhost:31415/v1` select `/v1/images/generations` or `/v1/images/edits`. Reverse-proxy path prefixes are supported. Do not put the full Images endpoint, credentials, query parameters, or a fragment in the base URL. Use HTTPS for remote authenticated endpoints.

No access key is required when Airouter runs in open mode. Otherwise, set `AIROUTER_API_KEY` through the harness's secret/environment mechanism. Never put keys in prompts, batch files, command arguments, or committed files. Do not print environment values. Do not read the Airouter database or export provider credentials.

The helpers connect directly to the configured server. They do not use environment proxy settings or follow redirects. This prevents accidental forwarding of credentials to another endpoint. A sandbox or remote agent must be able to reach the server. Its `localhost` may not be the user's machine.

## One image

Prefer a UTF-8 prompt file for long prompts or complex quoting. Put that file in the permitted workspace, not in the installed skill directory.

Python:

```sh
python3 /path/to/image-generation/scripts/generate_image.py --prompt-file prompt.txt --output images/cabin.png --quality low
```

Node.js:

```sh
node /path/to/image-generation/scripts/generate_image.mjs --prompt-file prompt.txt --output images/cabin.png --quality low
```

On Windows, replace `python3` with the supported interpreter found during detection. Use Windows paths and quote paths containing spaces.

Both helpers also accept `--prompt "A wooden cabin beside a lake, watercolor"` instead of `--prompt-file`. Supply exactly one of `--prompt`, `--prompt-file`, or `--batch`.

## Edit with local references

Add one to five `--image PATH` options to a single prompt. No `--image` selects generation. One or more references select `images/edits`. Do not pass `--image` with `--batch`.

```sh
python3 /path/to/image-generation/scripts/generate_image.py --prompt-file prompt.txt --image source.png --image palette.webp --output images/edited.png
```

The Node.js helper accepts the same repeatable `--image` option. Paths resolve from the process working directory. References must already exist as regular local files. PNG, JPEG, and WebP are accepted. GIF, remote URLs, directories, devices, and final-component symlinks are rejected before any request.

A reference is read again immediately before its request. If it is missing or its bytes changed, that job fails with `reference_changed` and is not sent. The queue then continues. There is no edit-to-generation fallback and no chaining of an earlier output into a later reference.

Local reference images are sent through Airouter to the configured provider. Enabled HAR capture can include reference image data. Check references for private content before sending. A timeout or canceled edit may still be billed. Do not resend without approval.

## Serial batches

Create a UTF-8 JSON array. Each job must have `prompt` and `output`. It may override `quality` and `background`. It may include `images` with 1-5 local path strings. When `images` is present, that job is an edit. When it is absent, that job is generation. An empty `images` array is invalid.

```json
[
  {
    "prompt": "A wooden cabin beside a lake at sunrise, watercolor",
    "output": "images/cabin-sunrise.png",
    "quality": "low"
  },
  {
    "prompt": "A wooden cabin beside a lake at dusk, watercolor",
    "output": "images/cabin-dusk.png",
    "quality": "low",
    "background": "opaque",
    "images": ["sources/cabin.png"]
  }
]
```

Run the selected helper with `--batch jobs.json`. Do not also supply `--output`.

```sh
python3 /path/to/image-generation/scripts/generate_image.py --batch jobs.json
```

The Node.js helper accepts the same options and batch format. Relative reference and output paths resolve from the process working directory, not from the batch file. All jobs use the same endpoint, combo, timeout, and access key.

Batches contain 1-64 jobs and execute one at a time. Each job sends one request for one image. Do not start concurrent helper processes or wrap the helper in a parallel executor. A failed job is reported once, then the queue proceeds to the next different job. Successful files remain available.

The helpers validate all job definitions, reference files, and outputs before sending the first request. They then reserve every output. Existing files, directories, and symlinks at output paths are refused. A reference path or file identity must not alias an output. Duplicate references inside one job are refused. The same reference may be used by more than one job. Parent directories are created when needed. Reserved files are temporarily empty until their jobs complete. Failed or normally interrupted jobs remove their reserved files. A forced process kill can leave empty or partial files. Inspect those files before using new paths or requesting another generation.

Reference checks use a bounded streaming read, a file-signature test, and SHA-256. They do not decode pixels and they are not a filesystem sandbox. Harness and user file permissions remain the outer boundary. Each reference can be at most 10 MiB. References for one job can total at most 32 MiB. The serialized request can be at most 64 MiB. These are local helper limits.

## Supported request controls

Generation supports this narrow contract:

| Field | Supported values |
|---|---|
| `model` | Configured image combo name |
| `prompt` | Nonempty text, preserved without trimming |
| `n` | `1` only |
| `size` | `auto` only |
| `quality` | `auto`, `low`, `medium`, `high` |
| `background` | `auto`, `opaque` |
| `output_format` | `png` only |
| `response_format` | `b64_json` only |
| `stream` | `false` only |
| `user` | Optional string accepted by Airouter, not forwarded upstream |

Edits use the same fixed controls. The helper sends `images` and omits `output_format`, `response_format`, and `stream`. Airouter keeps `user` local and does not forward it. The helpers do not send `user`.

Do not request masks, remote references, file IDs, fixed dimensions, transparent backgrounds, alternate formats, compression, GIF, or streaming. Report these limitations rather than silently changing the requested output. A successful route does not prove that the configured model or account can edit images.

## Results and failures

Each completed job writes one JSON line to stdout. A success includes:

```json
{"index":1,"ok":true,"path":"/workspace/images/cabin.png","bytes":3506567,"size":"1536x1024","output_format":"png","quality":"low","background":"opaque","usage":{"input_tokens":15,"output_tokens":343,"total_tokens":358}}
```

Dimensions come from the PNG header. Other metadata and usage appear only when supplied in a valid response. Never estimate image token usage or add image-token subtotals to inclusive totals. Do not infer billing amounts from token counts.

A failed job includes `ok:false` and an error code. HTTP failures include `http_status`. `reference_changed` means a reference was missing or changed after preflight, so that job was not sent. Helpers do not print upstream response bodies, prompts, credentials, input paths, or image base64. Use the status and Airouter's metadata logs to diagnose failures without dumping secrets.

Exit codes:
- `0`: Every job succeeded, or help was shown.
- `1`: At least one job failed. Successful files are retained.
- `2`: Input or output preflight failed. No request was sent.
- `130`: Normal interruption. The queue stopped.

No automatic retries occur. A timeout or cancellation does not prove that upstream generation stopped. Airouter's own upstream auth retry/failover can still occur and may cause duplicate billing after uncertain failures. Ask for approval before repeating a failed generation.

The timeout applies to network operations/inactivity, not to the whole batch. Allow at least the server's five-minute unary timeout for ordinary use. Set the harness's command deadline long enough for all serial jobs. Do not restart a running batch merely because it has not printed its first result yet.

## Output handling and limits

The helpers accept responses up to 64 MiB and decoded images up to 48 MiB. Prompt and batch files are limited to 1 MiB. They check canonical base64, the PNG signature, dimensions, chunk checksums, and complete chunk framing. They do not perform full pixel decoding.

Use the harness's image-reading tool to inspect a successful file when available. Otherwise, report the saved path without claiming visual verification. Never print the base64 data into model context. The helpers save PNGs only, not full raw response JSON.

Keep outputs and prompt/batch files outside the skill directory. Return paths relative to the user's workspace when useful. Do not overwrite existing artwork or delete failed-run artifacts without permission.
