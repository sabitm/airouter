#!/usr/bin/env python3
"""Generate or edit PNG files through Airouter without third-party packages."""

import argparse
import base64
import hashlib
import http.client
import json
import math
import os
from pathlib import Path
import re
import signal
import socket
import stat
import struct
import sys
import urllib.error
import urllib.parse
import urllib.request
import zlib

MAX_INPUT_BYTES = 1 << 20
MAX_RESPONSE_BYTES = 64 << 20
MAX_IMAGE_BYTES = 48 << 20
MAX_REQUEST_BYTES = 64 << 20
MAX_REFERENCE_BYTES = 10 << 20
MAX_REFERENCE_TOTAL_BYTES = 32 << 20
MAX_REFERENCES = 5
MAX_JOBS = 64
MAX_COUNT = (1 << 53) - 1
QUALITIES = ("auto", "low", "medium", "high")
BACKGROUNDS = ("auto", "opaque")
DETAIL_COUNTS = ("text_tokens", "image_tokens", "cached_tokens", "cache_write_tokens", "reasoning_tokens")
PNG_SIGNATURE = b"\x89PNG\r\n\x1a\n"

interrupted_flag = False


class InputError(Exception):
    pass


class JobError(Exception):
    def __init__(self, code, status=None):
        self.code = code
        self.status = status


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise InputError("invalid arguments; run --help")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def arguments():
    parser = Parser(description=__doc__, allow_abbrev=False)
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--prompt", help="one image prompt")
    source.add_argument("--prompt-file", help="UTF-8 prompt file, at most 1 MiB")
    source.add_argument("--batch", help="JSON array of prompt/output jobs, at most 64")
    parser.add_argument("--output", help="PNG path, required outside batch mode")
    parser.add_argument("--image", action="append", help="local reference image; repeat up to 5 times")
    parser.add_argument("--url", default=os.environ.get("AIROUTER_URL") or "http://localhost:31415")
    parser.add_argument("--model", default=os.environ.get("AIROUTER_IMAGE_MODEL") or "image", help="Airouter combo name")
    parser.add_argument("--quality", choices=QUALITIES, default="auto")
    parser.add_argument("--background", choices=BACKGROUNDS, default="auto")
    parser.add_argument("--timeout", type=float, default=330, help="network operation timeout in seconds (default 330)")
    return parser.parse_args()


def read_input(path):
    try:
        with open(path, "rb") as stream:
            data = stream.read(MAX_INPUT_BYTES + 1)
        if len(data) > MAX_INPUT_BYTES:
            raise InputError("input file exceeds 1 MiB")
        return data.decode("utf-8")
    except (OSError, UnicodeError):
        raise InputError("cannot read UTF-8 input file") from None


def endpoint_builder(value):
    try:
        if not isinstance(value, str) or not re.match(r"^https?://", value, re.IGNORECASE) or re.search(r"[\s\x00-\x1f\x7f\\]", value):
            raise ValueError()
        url = urllib.parse.urlsplit(value)
        if url.scheme not in ("http", "https") or not url.hostname or url.username is not None or url.password is not None:
            raise ValueError()
        if url.query or url.fragment or not url.netloc or url.port == 0:
            raise ValueError()
        root = url.path.rstrip("/")

        def build(edit):
            suffix = "/images/edits" if edit else "/images/generations"
            path = root + (suffix if root.endswith("/v1") else "/v1" + suffix)
            return urllib.parse.urlunsplit((url.scheme, url.netloc, path, "", ""))

        return build
    except ValueError:
        raise InputError("url must be an HTTP(S) base URL without credentials, whitespace, query, or fragment") from None


def reference_paths(value):
    if not isinstance(value, list) or not 1 <= len(value) <= MAX_REFERENCES:
        raise InputError("each images list must contain 1 to 5 paths")
    for item in value:
        if not isinstance(item, str) or not item.strip() or "\x00" in item:
            raise InputError("each reference must be a local file path")
    return value


def load_jobs(args):
    if not math.isfinite(args.timeout) or args.timeout <= 0 or args.timeout > 3600:
        raise InputError("timeout must be greater than 0 and at most 3600 seconds")
    if not args.model.strip() or len(args.model.encode("utf-8")) > 1024:
        raise InputError("model must be a nonempty combo name of at most 1024 UTF-8 bytes")
    if args.batch is not None and args.image:
        raise InputError("--image is not allowed with batch")
    if args.batch is not None:
        if args.output is not None:
            raise InputError("output is not allowed with batch")
        try:
            jobs = json.loads(read_input(args.batch))
        except ValueError:
            raise InputError("batch must be valid JSON") from None
        if not isinstance(jobs, list) or not 1 <= len(jobs) <= MAX_JOBS:
            raise InputError("batch must contain 1 to 64 jobs")
    else:
        images = args.image or []
        if len(images) > MAX_REFERENCES:
            raise InputError("at most 5 reference images are allowed")
        prompt = read_input(args.prompt_file) if args.prompt_file is not None else args.prompt
        single = {"prompt": prompt, "output": args.output}
        if images:
            single["images"] = images
        jobs = [single]
    normalized = []
    allowed = {"prompt", "output", "quality", "background", "images"}
    for job in jobs:
        if not isinstance(job, dict) or set(job) - allowed:
            raise InputError("each job allows only prompt, output, quality, background, and images")
        prompt, output = job.get("prompt"), job.get("output")
        if not isinstance(prompt, str) or not prompt.strip() or len(prompt.encode("utf-8")) > MAX_INPUT_BYTES:
            raise InputError("each prompt must be nonempty and at most 1 MiB of UTF-8")
        if not isinstance(output, str) or not output.strip() or "\x00" in output or Path(output).suffix.lower() != ".png":
            raise InputError("each output must be a nonempty PNG path")
        quality, background = job.get("quality", args.quality), job.get("background", args.background)
        if quality not in QUALITIES or background not in BACKGROUNDS:
            raise InputError("unsupported quality or background")
        images = reference_paths(job["images"]) if "images" in job else []
        normalized.append({
            "prompt": prompt, "output": output, "quality": quality, "background": background,
            "images": images, "references": [],
        })
    return normalized


def path_key(path):
    return os.path.normcase(os.fspath(path))


def file_identity(st):
    if st is None or not getattr(st, "st_ino", 0):
        return None
    return (st.st_dev, st.st_ino)


def locate_reference(value):
    if not isinstance(value, str) or not value.strip() or "\x00" in value:
        raise InputError("each reference must be a local file path")
    absolute = Path(os.path.abspath(value))
    try:
        listed = absolute.lstat()
    except OSError:
        raise InputError("reference image is missing or is not a regular file") from None
    if stat.S_ISLNK(listed.st_mode) or not stat.S_ISREG(listed.st_mode):
        raise InputError("reference image is missing or is not a regular file")
    try:
        parent = absolute.parent.resolve()
    except OSError:
        raise InputError("reference image is missing or is not a regular file") from None
    return parent / absolute.name


def open_nofollow(path):
    flags = os.O_RDONLY
    if hasattr(os, "O_CLOEXEC"):
        flags |= os.O_CLOEXEC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    if hasattr(os, "O_NONBLOCK"):
        flags |= os.O_NONBLOCK
    return os.open(path, flags)


def reference_mime(header):
    # Signature check only. This is not pixel decoding or a filesystem sandbox.
    if header.startswith(PNG_SIGNATURE):
        return "image/png"
    if header.startswith(b"\xff\xd8\xff"):
        return "image/jpeg"
    if len(header) >= 12 and header[:4] == b"RIFF" and header[8:12] == b"WEBP":
        return "image/webp"
    return None


def preflight_reference(value, remaining):
    path = locate_reference(value)
    try:
        fd = open_nofollow(path)
    except OSError:
        if interrupted_flag:
            raise KeyboardInterrupt()
        raise InputError("reference image is missing or is not a regular file") from None
    try:
        opened = os.fstat(fd)
        listed = os.lstat(path)
        if not stat.S_ISREG(opened.st_mode) or not stat.S_ISREG(listed.st_mode) or file_identity(opened) != file_identity(listed):
            raise InputError("reference image is missing or is not a regular file")
        if opened.st_size <= 0:
            raise InputError("unsupported reference image")
        if opened.st_size > MAX_REFERENCE_BYTES:
            raise InputError("reference exceeds size limit")
        if opened.st_size > remaining:
            raise InputError("references exceed total size budget")
        hasher = hashlib.sha256()
        header = bytearray()
        total = 0
        while True:
            if interrupted_flag:
                raise KeyboardInterrupt()
            chunk = os.read(fd, 64 * 1024)
            if not chunk:
                break
            total += len(chunk)
            if total > MAX_REFERENCE_BYTES:
                raise InputError("reference exceeds size limit")
            if len(header) < 12:
                header.extend(chunk[:12 - len(header)])
            hasher.update(chunk)
        if total != opened.st_size:
            raise InputError("reference image changed during validation")
        mime = reference_mime(bytes(header))
        if mime is None:
            raise InputError("unsupported reference image")
        return {"path": path, "id": file_identity(opened), "size": total, "sha256": hasher.digest(), "mime": mime}
    except (KeyboardInterrupt, InputError):
        raise
    except OSError:
        if interrupted_flag:
            raise KeyboardInterrupt()
        raise InputError("reference image is missing or is not a regular file") from None
    finally:
        os.close(fd)


def prepare_references(jobs):
    for job in jobs:
        metas = []
        keys = set()
        identities = set()
        total = 0
        for raw_path in job["images"]:
            meta = preflight_reference(raw_path, MAX_REFERENCE_TOTAL_BYTES - total)
            key = path_key(meta["path"])
            if key in keys or (meta["id"] is not None and meta["id"] in identities):
                raise InputError("duplicate reference in a job")
            keys.add(key)
            if meta["id"] is not None:
                identities.add(meta["id"])
            total += meta["size"]
            metas.append(meta)
        job["references"] = metas


def reserve_outputs(jobs):
    reserved = []
    seen = set()
    ref_keys = {path_key(ref["path"]) for job in jobs for ref in job["references"]}
    try:
        for job in jobs:
            path = Path(os.path.abspath(job["output"]))
            path.parent.mkdir(parents=True, exist_ok=True)
            path = path.parent.resolve() / path.name
            key = path_key(path)
            if key in seen or key in ref_keys or os.path.lexists(path):
                raise InputError("output paths must be unique and must not already exist")
            seen.add(key)
            fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            reserved.append({"path": path, "fd": fd, "saved": False})
        return reserved
    except BaseException:
        cleanup(reserved)
        raise


def cleanup(outputs):
    for output in outputs:
        if output["fd"] is not None:
            os.close(output["fd"])
            output["fd"] = None
        if not output["saved"]:
            try:
                output["path"].unlink()
            except FileNotFoundError:
                pass


def read_verified(meta):
    if interrupted_flag:
        raise KeyboardInterrupt()
    try:
        fd = open_nofollow(meta["path"])
    except OSError:
        if interrupted_flag:
            raise KeyboardInterrupt()
        raise JobError("reference_changed") from None
    try:
        opened = os.fstat(fd)
        listed = os.lstat(meta["path"])
        if (not stat.S_ISREG(opened.st_mode) or not stat.S_ISREG(listed.st_mode)
                or file_identity(opened) != meta["id"] or file_identity(opened) != file_identity(listed)
                or opened.st_size != meta["size"] or listed.st_size != meta["size"]):
            raise JobError("reference_changed")
        buf = bytearray()
        while len(buf) < meta["size"]:
            if interrupted_flag:
                raise KeyboardInterrupt()
            chunk = os.read(fd, min(64 * 1024, meta["size"] - len(buf)))
            if not chunk:
                break
            buf.extend(chunk)
        extra = os.read(fd, 1)
        after = os.fstat(fd)
        current = os.lstat(meta["path"])
        # A matching prefix is not the whole reference if the file grew during the read.
        if (len(buf) != meta["size"] or extra or not stat.S_ISREG(after.st_mode)
                or not stat.S_ISREG(current.st_mode) or file_identity(after) != meta["id"]
                or file_identity(current) != meta["id"] or after.st_size != meta["size"]
                or current.st_size != meta["size"] or hashlib.sha256(buf).digest() != meta["sha256"]):
            raise JobError("reference_changed")
        return buf
    except JobError:
        raise
    except OSError:
        if interrupted_flag:
            raise KeyboardInterrupt()
        raise JobError("reference_changed") from None
    finally:
        os.close(fd)


def request_exceeds_limit(payload):
    return len(payload) > MAX_REQUEST_BYTES


def request_payload(model, job):
    if job["references"]:
        encoded = []
        for meta in job["references"]:
            raw = read_verified(meta)
            encoded.append({"image_url": "data:" + meta["mime"] + ";base64," + base64.b64encode(raw).decode("ascii")})
            raw.clear()
        body = {
            "model": model, "prompt": job["prompt"], "images": encoded, "n": 1, "size": "auto",
            "quality": job["quality"], "background": job["background"],
        }
        del encoded
    else:
        body = {
            "model": model, "prompt": job["prompt"], "n": 1, "size": "auto",
            "quality": job["quality"], "background": job["background"],
            "output_format": "png", "response_format": "b64_json", "stream": False,
        }
    payload = json.dumps(body, ensure_ascii=False).encode("utf-8")
    del body
    if request_exceeds_limit(payload):
        raise JobError("request_too_large")
    return payload


def png_dimensions(image):
    if not image.startswith(PNG_SIGNATURE):
        raise JobError("invalid_response")
    position, width, height, chunks = 8, 0, 0, 0
    has_data = False
    while position + 12 <= len(image):
        length = struct.unpack_from(">I", image, position)[0]
        end = position + length + 12
        if end > len(image):
            break
        kind = image[position + 4:position + 8]
        payload = memoryview(image)[position + 8:end - 4]
        checksum = zlib.crc32(payload, zlib.crc32(kind)) & 0xffffffff
        if checksum != struct.unpack_from(">I", image, end - 4)[0]:
            break
        if chunks == 0:
            if kind != b"IHDR" or length != 13:
                break
            width, height = struct.unpack_from(">II", payload)
            if not width or not height:
                break
        elif kind == b"IHDR":
            break
        has_data = has_data or (kind == b"IDAT" and length > 0)
        if kind == b"IEND":
            if length == 0 and has_data and end == len(image):
                return width, height
            break
        chunks += 1
        position = end
    raise JobError("invalid_response")


def valid_usage(value):
    def count(n):
        return type(n) in (int, float) and 0 <= n <= MAX_COUNT and n == int(n)

    if not isinstance(value, dict) or not all(count(value.get(key)) for key in ("input_tokens", "output_tokens")):
        return None
    out = {"input_tokens": int(value["input_tokens"]), "output_tokens": int(value["output_tokens"])}
    if "total_tokens" in value:
        if not count(value["total_tokens"]):
            return None
        out["total_tokens"] = int(value["total_tokens"])
    for key in ("input_tokens_details", "output_tokens_details"):
        if value.get(key) is not None:
            details = value[key]
            if not isinstance(details, dict) or not all(count(n) for n in details.values()):
                return None
            out[key] = {name: int(n) for name, n in details.items() if name in DETAIL_COUNTS}
    return out


def decode_response(raw):
    try:
        response = json.loads(raw.decode("utf-8"))
        if not isinstance(response, dict) or response.get("output_format", "png") != "png":
            raise ValueError()
        items = response.get("data")
        if not isinstance(items, list) or len(items) != 1 or not isinstance(items[0], dict):
            raise ValueError()
        encoded = items[0].get("b64_json")
        if not isinstance(encoded, str) or not encoded or len(encoded) > MAX_RESPONSE_BYTES:
            raise ValueError()
        if len(encoded) % 4 or re.fullmatch(r"[A-Za-z0-9+/]*={0,2}", encoded) is None:
            raise ValueError()
        image = base64.b64decode(encoded, validate=True)
        if len(image) > MAX_IMAGE_BYTES or base64.b64encode(image).decode("ascii") != encoded:
            raise ValueError()
        width, height = png_dimensions(image)
        metadata = {"bytes": len(image), "size": f"{width}x{height}", "output_format": "png"}
        for key, choices in (("quality", QUALITIES), ("background", BACKGROUNDS)):
            if response.get(key) in choices:
                metadata[key] = response[key]
        if type(response.get("created")) in (int, float) and 0 <= response["created"] <= MAX_COUNT and response["created"] == int(response["created"]):
            metadata["created"] = int(response["created"])
        generation_id = items[0].get("generation_id")
        if isinstance(generation_id, str) and re.fullmatch(r"[A-Za-z0-9_-]{1,256}", generation_id):
            metadata["generation_id"] = generation_id
        usage = valid_usage(response.get("usage"))
        if usage is not None:
            metadata["usage"] = usage
        return image, metadata
    except (ValueError, UnicodeError, RecursionError):
        raise JobError("invalid_response") from None


def generate(url, model, job, key, timeout):
    payload = request_payload(model, job)
    headers = {"Content-Type": "application/json", "Accept": "application/json"}
    if key:
        headers["Authorization"] = "Bearer " + key
    request = urllib.request.Request(url, data=payload, headers=headers, method="POST")
    del payload
    # Redirects and environment proxies must not send prompts or credentials elsewhere.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    try:
        with opener.open(request, timeout=timeout) as response:
            if not 200 <= response.status < 300:
                raise JobError("http_error", response.status)
            length = response.headers.get("Content-Length", "")
            if length.isdigit() and (len(length) > 20 or int(length) > MAX_RESPONSE_BYTES):
                raise JobError("response_too_large")
            raw = bytearray()
            while True:
                chunk = response.read1(min(64 << 10, MAX_RESPONSE_BYTES + 1 - len(raw)))
                if not chunk:
                    break
                raw.extend(chunk)
                if len(raw) > MAX_RESPONSE_BYTES:
                    raise JobError("response_too_large")
            if length.isdigit() and len(raw) != int(length):
                raise JobError("invalid_response")
            return decode_response(raw)
    except urllib.error.HTTPError as error:
        error.close()
        raise JobError("http_error", error.code) from None
    except (TimeoutError, socket.timeout):
        raise JobError("timeout") from None
    except urllib.error.URLError as error:
        raise JobError("timeout" if isinstance(error.reason, TimeoutError) else "network_error") from None
    except (OSError, http.client.HTTPException):
        raise JobError("network_error") from None


def interrupt(signum, frame):
    global interrupted_flag
    interrupted_flag = True
    raise KeyboardInterrupt()


def main():
    outputs = []
    try:
        args = arguments()
        urls = endpoint_builder(args.url)
        jobs = load_jobs(args)
        key = os.environ.get("AIROUTER_API_KEY", "")
        if re.search(r"[^\x21-\x7e]", key):
            raise InputError("AIROUTER_API_KEY must contain only visible ASCII characters")
        prepare_references(jobs)
        outputs = reserve_outputs(jobs)
        failed = False
        for index, (job, output) in enumerate(zip(jobs, outputs), 1):
            result = {"index": index, "ok": False, "path": str(output["path"])}
            interrupted = False
            try:
                image, metadata = generate(urls(bool(job["references"])), args.model, job, key, args.timeout)
                with os.fdopen(output["fd"], "wb") as stream:
                    output["fd"] = None
                    stream.write(image)
                output["saved"] = True
                result.update(ok=True, **metadata)
            except JobError as error:
                result["error"] = error.code
                if error.status is not None:
                    result["http_status"] = error.status
            except OSError:
                result["error"] = "output_error"
            except KeyboardInterrupt:
                result["error"] = "interrupted"
                interrupted = True
            if not output["saved"]:
                cleanup([output])
            print(json.dumps(result, ensure_ascii=True), flush=True)
            if interrupted:
                return 130
            failed = failed or not result["ok"]
        return 1 if failed else 0
    except InputError as error:
        print(f"error: {error}", file=sys.stderr)
        return 2
    except (OSError, ValueError, UnicodeError, RecursionError):
        print("error: invalid input or inaccessible output path; no generation request was sent", file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        print("error: interrupted", file=sys.stderr)
        return 130
    finally:
        cleanup(outputs)


if __name__ == "__main__":
    if sys.version_info < (3, 10):
        sys.exit("Python 3.10 or newer is required")
    signal.signal(signal.SIGINT, interrupt)
    signal.signal(signal.SIGTERM, interrupt)
    sys.exit(main())
