#!/usr/bin/env python3
"""Exercise both image helpers with local mock servers only."""

import base64
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import zlib

ROOT = Path(__file__).resolve().parents[1]
PYTHON_HELPER = ROOT / "scripts" / "generate_image.py"
NODE_HELPER = ROOT / "scripts" / "generate_image.mjs"


def png_chunk(kind, data):
    return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xffffffff)


PNG = (b"\x89PNG\r\n\x1a\n" + png_chunk(b"IHDR", struct.pack(">IIBBBBB", 1, 1, 8, 2, 0, 0, 0))
       + png_chunk(b"IDAT", zlib.compress(b"\x00\xff\x00\x00")) + png_chunk(b"IEND", b""))
B64 = base64.b64encode(PNG).decode("ascii")
USAGE = {"input_tokens": 0, "output_tokens": 3, "total_tokens": 3,
         "input_tokens_details": {"text_tokens": 0, "image_tokens": 0},
         "output_tokens_details": {"image_tokens": 3, "text_tokens": 0}}


def success(**overrides):
    result = {"created": 123, "data": [{"b64_json": B64, "generation_id": "gen_123"}],
              "output_format": "png", "quality": "low", "background": "opaque", "usage": USAGE}
    result.update(overrides)
    return result


class MockServer:
    def __init__(self, respond=None):
        self.requests = []
        self.entries = []
        self.active = 0
        self.max_active = 0
        self.lock = threading.Lock()
        self.respond = respond or (lambda handler, body: self.send(handler, success()))
        mock = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                with mock.lock:
                    mock.entries.append(self.path)
                raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
                try:
                    body = json.loads(raw)
                except (ValueError, UnicodeError):
                    return
                with mock.lock:
                    mock.requests.append({"path": self.path, "body": body, "headers": dict(self.headers)})
                    mock.active += 1
                    mock.max_active = max(mock.max_active, mock.active)
                try:
                    mock.respond(self, body)
                except (BrokenPipeError, ConnectionResetError):
                    pass
                finally:
                    with mock.lock:
                        mock.active -= 1

            def log_message(self, *args):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.url = f"http://127.0.0.1:{self.server.server_port}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    @staticmethod
    def send(handler, value, status=200, headers=None):
        raw = value if isinstance(value, bytes) else json.dumps(value).encode("utf-8")
        handler.send_response(status)
        handler.send_header("Content-Type", "application/json")
        handler.send_header("Content-Length", str(len(raw)))
        for key, value in (headers or {}).items():
            handler.send_header(key, value)
        handler.end_headers()
        handler.wfile.write(raw)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *args):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()


class HelperContract(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.runtimes = [("python", [sys.executable, str(PYTHON_HELPER)])]
        node = shutil.which("node")
        if node:
            major = int(subprocess.check_output([node, "--version"], text=True).strip().lstrip("v").split(".")[0])
            if major >= 22:
                cls.runtimes.append(("node", [node, str(NODE_HELPER)]))
        if len(cls.runtimes) < 2:
            print("Node.js 22+ unavailable: only the Python contract will run", file=sys.stderr)
        spec = importlib.util.spec_from_file_location("image_helper", PYTHON_HELPER)
        helper = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(helper)
        cls.helper = helper
        cls.png_dimensions = staticmethod(helper.png_dimensions)
        print("Helper runtimes: " + ", ".join(name for name, _ in cls.runtimes), file=sys.stderr)

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def env(self, **extra):
        env = {key: value for key, value in os.environ.items() if not key.startswith("AIROUTER_")}
        env.update(PYTHONDONTWRITEBYTECODE="1", PYTHONUTF8="1", NO_COLOR="1")
        env.update(extra)
        return env

    def run_helper(self, command, args, env=None):
        return subprocess.run(command + args, cwd=self.root, env=self.env(**(env or {})),
                              text=True, capture_output=True, timeout=15)

    def rows(self, result):
        return [json.loads(line) for line in result.stdout.splitlines()]

    def assert_private(self, result):
        for marker in (B64, "private-upstream-body", "test-api-secret", "private-prompt"):
            self.assertNotIn(marker, result.stdout + result.stderr)

    def test_help_and_invalid_arguments_do_not_echo_values(self):
        for name, command in self.runtimes:
            with self.subTest(runtime=name):
                result = self.run_helper(command, ["--help"])
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("--batch", result.stdout)
                self.assertIn("--image", result.stdout)
                result = self.run_helper(command, ["--private-prompt"])
                self.assertEqual(result.returncode, 2)
                self.assert_private(result)

    def test_success_auth_endpoint_and_request_contract(self):
        for name, command in self.runtimes:
            for prefix in ("", "/v1/", "/proxy/v1"):
                with self.subTest(runtime=name, prefix=prefix), MockServer() as mock:
                    output = self.root / f"{name}-{len(prefix)}.png"
                    prompt = '  private-prompt: a "cabin"\nby a lake,  \u2603  '
                    result = self.run_helper(command, ["--prompt", prompt, "--output", str(output),
                                                      "--quality", "high", "--background", "opaque"],
                                             {"AIROUTER_URL": mock.url + prefix, "AIROUTER_IMAGE_MODEL": "configured-image",
                                              "AIROUTER_API_KEY": "test-api-secret"})
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(output.read_bytes(), PNG)
                    row = self.rows(result)[0]
                    self.assertEqual(row, {"index": 1, "ok": True, "path": str(output), "bytes": len(PNG),
                                           "size": "1x1", "output_format": "png", "quality": "low", "background": "opaque",
                                           "created": 123, "generation_id": "gen_123", "usage": USAGE})
                    request = mock.requests[0]
                    self.assertEqual(request["path"], "/proxy/v1/images/generations" if prefix.startswith("/proxy") else "/v1/images/generations")
                    self.assertEqual(request["headers"]["Authorization"], "Bearer test-api-secret")
                    self.assertEqual(request["body"], {"model": "configured-image", "prompt": prompt, "n": 1, "size": "auto",
                                                       "quality": "high", "background": "opaque", "output_format": "png",
                                                       "response_format": "b64_json", "stream": False})
                    if os.name == "posix":
                        self.assertEqual(output.stat().st_mode & 0o777, 0o600)
                    self.assert_private(result)

    def test_prompt_file_defaults_and_cli_overrides(self):
        prompt = self.root / "prompt.txt"
        prompt.write_text("A cabin with snow \u96ea\n", encoding="utf-8")
        for name, command in self.runtimes:
            with self.subTest(runtime=name), MockServer() as mock:
                result = self.run_helper(command, ["--url", mock.url, "--model", "image", "--prompt-file", str(prompt),
                                                  "--output", f"nested/{name}.png"],
                                         {"AIROUTER_URL": "invalid", "AIROUTER_IMAGE_MODEL": "ignored"})
                self.assertEqual(result.returncode, 0, result.stderr)
                request = mock.requests[0]
                self.assertEqual(request["body"]["prompt"], prompt.read_text(encoding="utf-8"))
                self.assertEqual(request["body"]["model"], "image")
                self.assertEqual(request["body"]["quality"], "auto")
                self.assertEqual(request["body"]["background"], "auto")
                self.assertNotIn("Authorization", request["headers"])

    def test_batch_is_serial_and_continues_after_error(self):
        def respond(handler, body):
            time.sleep(0.04)
            if body["prompt"] == "failure":
                MockServer.send(handler, {"error": {"message": "private-upstream-body"}}, 502)
            else:
                MockServer.send(handler, success())

        for name, command in self.runtimes:
            with self.subTest(runtime=name), MockServer(respond) as mock:
                batch = self.root / f"{name}.json"
                jobs = [{"prompt": "first", "output": f"{name}-first.png", "quality": "low"},
                        {"prompt": "failure", "output": f"{name}-failed.png"},
                        {"prompt": "third", "output": f"{name}-third.png", "background": "opaque"}]
                batch.write_text(json.dumps(jobs), encoding="utf-8")
                result = self.run_helper(command, ["--url", mock.url, "--batch", str(batch), "--quality", "medium"])
                self.assertEqual(result.returncode, 1, result.stderr)
                rows = self.rows(result)
                self.assertEqual([r["ok"] for r in rows], [True, False, True])
                self.assertEqual(rows[1]["error"], "http_error")
                self.assertEqual(rows[1]["http_status"], 502)
                self.assertEqual([r["index"] for r in rows], [1, 2, 3])
                self.assertEqual([r["body"]["prompt"] for r in mock.requests], ["first", "failure", "third"])
                self.assertEqual([r["body"]["quality"] for r in mock.requests], ["low", "medium", "medium"])
                self.assertEqual(mock.max_active, 1)
                self.assertEqual((self.root / f"{name}-first.png").read_bytes(), PNG)
                self.assertEqual((self.root / f"{name}-third.png").read_bytes(), PNG)
                self.assertFalse((self.root / f"{name}-failed.png").exists())
                self.assert_private(result)

    def test_preflight_rejects_all_jobs_before_any_send(self):
        cases = [[], [{"prompt": "first", "output": "unused.png"}, {"prompt": "", "output": "invalid.png"}],
                 [{"prompt": "a", "output": "same.png"}, {"prompt": "b", "output": "same.png"}],
                 [{"prompt": "a", "output": "existing.png"}], [{"prompt": "a", "output": "wrong.jpg"}],
                 [{"prompt": "a", "output": "unused.png", "quality": None}],
                 [{"prompt": "a", "output": "unused.png", "n": 2}],
                 [{"prompt": "a", "output": "unused.png"}] * 65]
        existing = self.root / "existing.png"
        existing.write_bytes(b"keep-me")
        for name, command in self.runtimes:
            for i, jobs in enumerate(cases):
                with self.subTest(runtime=name, case=i), MockServer() as mock:
                    batch = self.root / "jobs.json"
                    batch.write_text(json.dumps(jobs), encoding="utf-8")
                    result = self.run_helper(command, ["--url", mock.url, "--batch", str(batch)])
                    self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                    self.assertEqual(mock.requests, [])
                    self.assertEqual(existing.read_bytes(), b"keep-me")
                    self.assertFalse((self.root / "same.png").exists())
                    self.assertFalse((self.root / "unused.png").exists())

    def test_input_limits_and_validation(self):
        oversized = self.root / "large.txt"
        oversized.write_bytes(b"x" * ((1 << 20) + 1))
        invalid_utf8 = self.root / "bad.txt"
        invalid_utf8.write_bytes(b"\xff")
        malformed = self.root / "bad.json"
        malformed.write_text("{", encoding="utf-8")
        cases = [["--prompt-file", str(oversized), "--output", "out.png"],
                 ["--prompt-file", str(invalid_utf8), "--output", "out.png"],
                 ["--batch", str(malformed)],
                 ["--prompt", "p", "--output", "out.png", "--timeout", "nan"],
                 ["--prompt", "p", "--output", "out.png", "--timeout", "0"],
                 ["--prompt", "p", "--output", "out.png", "--model", " "],
                 ["--prompt", "p", "--output", "out.png", "--url", "http://user:secret@example.invalid"],
                 ["--prompt", "p", "--output", "out.png", "--url", "http://localhost/?key=secret"],
                 ["--prompt", "p", "--output", "out.png", "--url", "http:/localhost"],
                 ["--prompt", "p", "--output", "out.png", "--url", "http://localhost\\private-prompt"],
                 ["--prompt", "p", "--output", "out.png", "--url", "file:///tmp/image"],
                 ["--prompt", "p", "--output", "out.png", "--quality", "invalid"]]
        for name, command in self.runtimes:
            for i, args in enumerate(cases):
                with self.subTest(runtime=name, case=i), MockServer() as mock:
                    result = self.run_helper(command, ["--url", mock.url] + args)
                    self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                    self.assertEqual(mock.requests, [])
                    self.assertFalse((self.root / "out.png").exists())

    def test_symlink_output_refused(self):
        if not hasattr(os, "symlink"):
            self.skipTest("symlinks unavailable")
        for name, command in self.runtimes:
            with self.subTest(runtime=name), MockServer() as mock:
                output = self.root / f"{name}-link.png"
                try:
                    output.symlink_to(self.root / "absent.png")
                except OSError:
                    self.skipTest("symlinks not permitted")
                result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--output", str(output)])
                self.assertEqual(result.returncode, 2)
                self.assertTrue(output.is_symlink())
                self.assertEqual(mock.requests, [])

    def test_malformed_responses_are_not_saved_or_retried(self):
        damaged = bytearray(PNG)
        damaged[-1] ^= 1
        cases = [b"not-json", b"\xff", [], {}, success(data=[]), success(data=[{}, {}]),
                 success(data=[{"b64_json": base64.b64encode(PNG[:8] + png_chunk(b"\xc9HDR", PNG[16:29]) + PNG[33:]).decode()}]),
                 success(data=[{"url": "https://example.invalid/image.png"}]),
                 success(data=[{"b64_json": "!!!!"}]), success(data=[{"b64_json": B64 + "\n"}]),
                 success(data=[{"b64_json": base64.b64encode(b"not-png").decode()}]),
                 success(data=[{"b64_json": base64.b64encode(PNG[:-12]).decode()}]),
                 success(data=[{"b64_json": base64.b64encode(damaged).decode()}]),
                 success(output_format="jpeg"), success(output_format=None)]
        for name, command in self.runtimes:
            for i, value in enumerate(cases):
                with self.subTest(runtime=name, case=i), MockServer(lambda h, b: MockServer.send(h, value)) as mock:
                    output = self.root / f"{name}-bad.png"
                    result = self.run_helper(command, ["--url", mock.url, "--prompt", "private-prompt", "--output", str(output)])
                    self.assertEqual(result.returncode, 1, result.stderr)
                    self.assertEqual(self.rows(result)[0]["error"], "invalid_response")
                    self.assertFalse(output.exists())
                    self.assertEqual(len(mock.requests), 1)
                    self.assert_private(result)

    def test_usage_missing_invalid_and_zero(self):
        for name, command in self.runtimes:
            for i, usage in enumerate([None, {"input_tokens": True, "output_tokens": 1},
                                       {"input_tokens": 0.0, "output_tokens": 0.0, "total_tokens": 0.0}]):
                with self.subTest(runtime=name, usage=usage), MockServer(lambda h, b: MockServer.send(h, success(usage=usage))) as mock:
                    result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--output", f"{name}-usage-{i}.png"])
                    self.assertEqual(result.returncode, 0, result.stderr)
                    row = self.rows(result)[0]
                    if i == 2:
                        self.assertEqual(row["usage"], usage)
                    else:
                        self.assertNotIn("usage", row)

    def test_http_errors_and_redirects_are_not_followed(self):
        for name, command in self.runtimes:
            for status in (401, 403, 429, 500, 302, 307):
                with self.subTest(runtime=name, status=status), MockServer() as target:
                    with MockServer(lambda h, b: MockServer.send(h, b"private-upstream-body", status,
                                                               {"Location": target.url + "/v1/images/generations"})) as mock:
                        result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--output", f"{name}-http.png"],
                                                 {"AIROUTER_API_KEY": "test-api-secret"})
                        self.assertEqual(result.returncode, 1, result.stderr)
                        row = self.rows(result)[0]
                        self.assertEqual((row["error"], row["http_status"]), ("http_error", status))
                        self.assertEqual(len(mock.requests), 1)
                        self.assertEqual(target.requests, [])
                        self.assertFalse((self.root / f"{name}-http.png").exists())
                        self.assert_private(result)

    def test_declared_and_chunked_response_limits(self):
        def declared(handler, body):
            handler.send_response(200)
            handler.send_header("Content-Length", str((64 << 20) + 1))
            handler.end_headers()

        def chunked(handler, body):
            handler.send_response(200)
            handler.end_headers()
            block = b"x" * (1 << 20)
            for _ in range(65):
                handler.wfile.write(block)

        for name, command in self.runtimes:
            for label, responder in (("declared", declared), ("undeclared", chunked)):
                with self.subTest(runtime=name, case=label), MockServer(responder) as mock:
                    result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--output", f"{name}-large.png"])
                    self.assertEqual(result.returncode, 1, result.stderr)
                    self.assertEqual(self.rows(result)[0]["error"], "response_too_large")
                    self.assertFalse((self.root / f"{name}-large.png").exists())
                    self.assertEqual(len(mock.requests), 1)

    def test_timeout_and_connection_failure_no_retry(self):
        def delayed(handler, body):
            time.sleep(0.3)
            MockServer.send(handler, success())

        for name, command in self.runtimes:
            with self.subTest(runtime=name), MockServer(delayed) as mock:
                result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--output", f"{name}-timeout.png", "--timeout", "0.05"])
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertEqual(self.rows(result)[0]["error"], "timeout")
                self.assertFalse((self.root / f"{name}-timeout.png").exists())
                self.assertEqual(len(mock.requests), 1)
                url = mock.url
            result = self.run_helper(command, ["--url", url, "--prompt", "p", "--output", f"{name}-network.png"])
            self.assertEqual(result.returncode, 1, result.stderr)
            self.assertEqual(self.rows(result)[0]["error"], "network_error")
            self.assertFalse((self.root / f"{name}-network.png").exists())

    def test_existing_output_appearing_during_request_is_not_overwritten(self):
        for name, command in self.runtimes:
            with self.subTest(runtime=name):
                output = self.root / f"{name}-late.png"
                observed = []

                def respond(handler, body):
                    try:
                        with output.open("xb") as stream:
                            stream.write(b"should-not-write")
                    except FileExistsError:
                        observed.append(True)
                    MockServer.send(handler, success())

                with MockServer(respond) as mock:
                    result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--output", str(output)])
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(observed, [True])
                    self.assertEqual(output.read_bytes(), PNG)

    def test_large_png_above_sse_limit_is_accepted(self):
        image = PNG[:-12] + png_chunk(b"tEXt", b"comment\x00" + b"x" * (6 << 20)) + PNG[-12:]
        encoded = base64.b64encode(image).decode("ascii")
        self.assertGreater(len(encoded), 8 << 20)
        for name, command in self.runtimes:
            with self.subTest(runtime=name), MockServer(lambda h, b: MockServer.send(h, success(data=[{"b64_json": encoded}]))) as mock:
                result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--output", f"{name}-large-valid.png"])
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual((self.root / f"{name}-large-valid.png").read_bytes(), image)
                self.assertEqual(self.rows(result)[0]["bytes"], len(image))

    def test_png_dimensions_helper(self):
        self.assertEqual(self.png_dimensions(PNG), (1, 1))

    def test_partial_response_failures_do_not_retry(self):
        def stalled(handler, body):
            handler.send_response(200)
            handler.send_header("Content-Length", "100")
            handler.end_headers()
            handler.wfile.write(b'{"data":')
            handler.wfile.flush()
            time.sleep(0.3)

        def truncated(handler, body):
            handler.send_response(200)
            handler.send_header("Content-Length", "100")
            handler.end_headers()
            handler.wfile.write(b'{"data":')

        def reset(handler, body):
            handler.connection.shutdown(socket.SHUT_RDWR)
            handler.connection.close()

        for name, command in self.runtimes:
            for label, respond in (("stalled", stalled), ("truncated", truncated), ("reset", reset)):
                with self.subTest(runtime=name, case=label), MockServer(respond) as mock:
                    result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--output", f"{name}-partial.png", "--timeout", "0.05"])
                    self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                    error = self.rows(result)[0]["error"]
                    if label == "stalled":
                        self.assertEqual(error, "timeout")
                    else:
                        self.assertIn(error, ("invalid_response", "network_error"))
                    self.assertFalse((self.root / f"{name}-partial.png").exists())
                    self.assertEqual(len(mock.requests), 1)

    def test_unknown_usage_detail_keys_are_not_printed(self):
        usage = {"input_tokens": 1, "output_tokens": 1,
                 "input_tokens_details": {"private-upstream-body": 7, "text_tokens": 1}}
        for name, command in self.runtimes:
            with self.subTest(runtime=name), MockServer(lambda h, b: MockServer.send(h, success(usage=usage))) as mock:
                result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--output", f"{name}-private.png"])
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.rows(result)[0]["usage"]["input_tokens_details"], {"text_tokens": 1})
                self.assert_private(result)

    def test_environment_proxy_is_not_used(self):
        for name, command in self.runtimes:
            with self.subTest(runtime=name), MockServer() as mock:
                result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--output", f"{name}-direct.png"],
                                         {"http_proxy": "http://127.0.0.1:1", "HTTP_PROXY": "http://127.0.0.1:1",
                                          "NO_PROXY": "", "no_proxy": "", "NODE_USE_ENV_PROXY": "1"})
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(len(mock.requests), 1)

    def test_interruption_stops_batch_and_cleans_reserved_files(self):
        if os.name != "posix":
            self.skipTest("POSIX signal delivery test")
        started = threading.Event()

        def delayed(handler, body):
            started.set()
            handler.send_response(200)
            handler.send_header("Content-Length", "100")
            handler.end_headers()
            handler.wfile.write(b'{"data":')
            handler.wfile.flush()
            time.sleep(0.5)

        for name, command in self.runtimes:
            with self.subTest(runtime=name), MockServer(delayed) as mock:
                started.clear()
                batch = self.root / "interrupt.json"
                jobs = [{"prompt": str(i), "output": f"{name}-interrupt-{i}.png"} for i in range(3)]
                batch.write_text(json.dumps(jobs), encoding="utf-8")
                proc = subprocess.Popen(command + ["--url", mock.url, "--batch", str(batch)], cwd=self.root,
                                        env=self.env(), stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                try:
                    self.assertTrue(started.wait(5))
                    proc.send_signal(signal.SIGTERM)
                    stdout, stderr = proc.communicate(timeout=5)
                    self.assertEqual(proc.returncode, 130, stderr)
                    self.assertEqual(json.loads(stdout)["error"], "interrupted")
                    self.assertEqual(len(mock.requests), 1)
                    for job in jobs:
                        self.assertFalse((self.root / job["output"]).exists())
                finally:
                    if proc.poll() is None:
                        proc.kill()
                        proc.communicate()


    def data_url(self, mime, raw):
        return f"data:{mime};base64,{base64.b64encode(raw).decode('ascii')}"

    def write_ref(self, name, raw):
        path = self.root / name
        path.write_bytes(raw)
        return path

    def test_edit_contract_order_and_prefixes(self):
        jpeg = b"\xff\xd8\xff\x00"
        webp = b"RIFF" + struct.pack("<I", 4) + b"WEBP"
        for name, command in self.runtimes:
            for prefix in ("", "/v1/", "/proxy/v1"):
                with self.subTest(runtime=name, prefix=prefix), MockServer() as mock:
                    self.write_ref("first.png", PNG)
                    self.write_ref("second.jpg", jpeg)
                    self.write_ref("third.webp", webp)
                    prompt = '  private-prompt edit "lake"\n'
                    result = self.run_helper(command, ["--prompt", prompt, "--output", f"{name}-edit-{len(prefix)}.png",
                                                      "--image", "first.png", "--image", "second.jpg", "--image", "third.webp",
                                                      "--quality", "high", "--background", "opaque"],
                                             {"AIROUTER_URL": mock.url + prefix, "AIROUTER_IMAGE_MODEL": "configured-image",
                                              "AIROUTER_API_KEY": "test-api-secret"})
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(mock.requests[0]["path"], "/proxy/v1/images/edits" if prefix.startswith("/proxy") else "/v1/images/edits")
                    self.assertEqual(mock.requests[0]["headers"]["Authorization"], "Bearer test-api-secret")
                    self.assertEqual(mock.requests[0]["body"], {
                        "model": "configured-image", "prompt": prompt,
                        "images": [{"image_url": self.data_url("image/png", PNG)},
                                    {"image_url": self.data_url("image/jpeg", jpeg)},
                                    {"image_url": self.data_url("image/webp", webp)}],
                        "n": 1, "size": "auto", "quality": "high", "background": "opaque"})
                    self.assertNotIn("private-input", result.stdout + result.stderr)
                    self.assert_private(result)

    def test_image_flag_rejected_with_batch_and_count_limits(self):
        for name, command in self.runtimes:
            with self.subTest(runtime=name, case="flag"), MockServer() as mock:
                result = self.run_helper(command, ["--image", "missing.png", "--batch", "missing.json", "--url", mock.url])
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertEqual(mock.requests, [])
                self.assertNotIn("missing.png", result.stdout + result.stderr)
            with self.subTest(runtime=name, case="count"), MockServer() as mock:
                args = ["--url", mock.url, "--prompt", "p", "--output", "out.png"]
                for i in range(6):
                    args.extend(["--image", f"ref-{i}.png"])
                result = self.run_helper(command, args)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertEqual(mock.requests, [])
                self.assertFalse((self.root / "out.png").exists())

    def test_mixed_batch_continues_and_does_not_fallback(self):
        def respond(handler, body):
            if body["prompt"] == "failure":
                MockServer.send(handler, {"error": {"message": "private-upstream-body"}}, 500)
            else:
                MockServer.send(handler, success())

        for name, command in self.runtimes:
            with self.subTest(runtime=name), MockServer(respond) as mock:
                self.write_ref("ref.png", PNG)
                self.write_ref("other.png", PNG + b"\x00")
                jobs = [{"prompt": "generated", "output": f"{name}-gen.png"},
                        {"prompt": "edited", "output": f"{name}-edit.png", "images": ["ref.png"], "quality": "low"},
                        {"prompt": "failure", "output": f"{name}-fail.png", "images": ["other.png"]},
                        {"prompt": "after", "output": f"{name}-after.png", "background": "opaque"}]
                batch = self.root / "mixed.json"
                batch.write_text(json.dumps(jobs), encoding="utf-8")
                result = self.run_helper(command, ["--url", mock.url, "--batch", str(batch)])
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertEqual([row["ok"] for row in self.rows(result)], [True, True, False, True])
                self.assertEqual(self.rows(result)[2]["error"], "http_error")
                self.assertEqual([item["path"] for item in mock.requests],
                                 ["/v1/images/generations", "/v1/images/edits", "/v1/images/edits", "/v1/images/generations"])
                self.assertEqual(mock.requests[1]["body"]["images"], [{"image_url": self.data_url("image/png", PNG)}])
                self.assertNotIn("output_format", mock.requests[1]["body"])
                self.assertEqual(mock.requests[0]["body"]["output_format"], "png")
                self.assertEqual(mock.max_active, 1)
                self.assertFalse((self.root / f"{name}-fail.png").exists())
                self.assertTrue((self.root / f"{name}-edit.png").is_file())
                self.assert_private(result)

    def test_invalid_later_reference_sends_nothing(self):
        for name, command in self.runtimes:
            with self.subTest(runtime=name), MockServer() as mock:
                self.write_ref("ref.png", PNG)
                jobs = [{"prompt": "first", "output": "first.png", "images": ["ref.png"]},
                        {"prompt": "second", "output": "second.png", "images": ["missing-private-input.png"]}]
                batch = self.root / "later.json"
                batch.write_text(json.dumps(jobs), encoding="utf-8")
                result = self.run_helper(command, ["--url", mock.url, "--batch", str(batch)])
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertEqual(mock.requests, [])
                self.assertFalse((self.root / "first.png").exists())
                self.assertNotIn("missing-private-input.png", result.stdout + result.stderr)

    def test_changed_reference_is_not_sent_and_queue_continues(self):
        ref = self.write_ref("ref.png", PNG)

        def respond(handler, body):
            if body["prompt"] == "first":
                data = bytearray(ref.read_bytes())
                data[-1] ^= 1
                ref.write_bytes(data)
            MockServer.send(handler, success())

        for name, command in self.runtimes:
            with self.subTest(runtime=name), MockServer(respond) as mock:
                ref.write_bytes(PNG)
                jobs = [{"prompt": "first", "output": f"{name}-first.png"},
                        {"prompt": "second", "output": f"{name}-second.png", "images": ["ref.png"]},
                        {"prompt": "third", "output": f"{name}-third.png"}]
                batch = self.root / "mutate.json"
                batch.write_text(json.dumps(jobs), encoding="utf-8")
                result = self.run_helper(command, ["--url", mock.url, "--batch", str(batch)])
                self.assertEqual(result.returncode, 1, result.stderr)
                rows = self.rows(result)
                self.assertEqual([row["ok"] for row in rows], [True, False, True])
                self.assertEqual(rows[1]["error"], "reference_changed")
                self.assertEqual([item["body"]["prompt"] for item in mock.requests], ["first", "third"])
                self.assertFalse(any(item["path"].endswith("/images/edits") for item in mock.requests))
                self.assertFalse((self.root / f"{name}-second.png").exists())
                self.assertNotIn("ref.png", result.stdout + result.stderr)

    def test_reference_aliases_symlinks_and_nonregular_files(self):
        if not hasattr(os, "symlink"):
            self.skipTest("symlinks unavailable")
        for name, command in self.runtimes:
            ref = self.write_ref("ref.png", PNG)
            with self.subTest(runtime=name, case="duplicate"), MockServer() as mock:
                jobs = [{"prompt": "p", "output": "out.png", "images": ["ref.png", "./ref.png"]}]
                (self.root / "dup.json").write_text(json.dumps(jobs), encoding="utf-8")
                result = self.run_helper(command, ["--url", mock.url, "--batch", str(self.root / "dup.json")])
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertEqual(mock.requests, [])
            with self.subTest(runtime=name, case="hardlink"), MockServer() as mock:
                linked = self.root / f"{name}-linked.png"
                try:
                    if not linked.exists():
                        os.link(ref, linked)
                except OSError:
                    self.skipTest("hardlinks unavailable")
                jobs = [{"prompt": "p", "output": f"{name}-hard.png", "images": ["ref.png", linked.name]}]
                (self.root / "link.json").write_text(json.dumps(jobs), encoding="utf-8")
                result = self.run_helper(command, ["--url", mock.url, "--batch", str(self.root / "link.json")])
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertEqual(mock.requests, [])
            with self.subTest(runtime=name, case="same-output"), MockServer() as mock:
                result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--image", "ref.png", "--output", "ref.png"])
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertEqual(ref.read_bytes(), PNG)
                self.assertEqual(mock.requests, [])
            with self.subTest(runtime=name, case="symlink"), MockServer() as mock:
                link = self.root / f"{name}-link.png"
                try:
                    if not link.exists() and not link.is_symlink():
                        link.symlink_to(ref)
                except OSError:
                    self.skipTest("symlinks not permitted")
                result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--image", str(link), "--output", "out.png"])
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertTrue(link.is_symlink())
                self.assertEqual(mock.requests, [])
            with self.subTest(runtime=name, case="parent-symlink-alias"), MockServer() as mock:
                real = self.root / "real"
                real.mkdir(exist_ok=True)
                (real / "ref.png").write_bytes(PNG)
                linkdir = self.root / "linkdir"
                if not linkdir.exists():
                    linkdir.symlink_to(real, target_is_directory=True)
                result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--image", "real/ref.png", "--output", "linkdir/ref.png"])
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertEqual((real / "ref.png").read_bytes(), PNG)
                self.assertEqual(mock.requests, [])
            with self.subTest(runtime=name, case="parent-symlink-file"), MockServer() as mock:
                result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--image", "linkdir/ref.png", "--output", f"{name}-via-link.png"])
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(mock.requests[0]["path"], "/v1/images/edits")
                self.assertEqual(mock.requests[0]["body"]["images"][0]["image_url"], self.data_url("image/png", PNG))
            with self.subTest(runtime=name, case="directory"), MockServer() as mock:
                directory = self.root / "dirref"
                directory.mkdir(exist_ok=True)
                result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--image", str(directory), "--output", "out.png"])
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertEqual(mock.requests, [])
            if os.name == "posix":
                with self.subTest(runtime=name, case="fifo"), MockServer() as mock:
                    fifo = self.root / f"{name}.fifo"
                    if not fifo.exists():
                        os.mkfifo(fifo)
                    result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--image", str(fifo), "--output", "out.png"])
                    self.assertEqual(result.returncode, 2, result.stderr)
                    self.assertEqual(mock.requests, [])
                with self.subTest(runtime=name, case="device"), MockServer() as mock:
                    result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--image", "/dev/null", "--output", "out.png"])
                    self.assertEqual(result.returncode, 2, result.stderr)
                    self.assertNotIn("/dev/null", result.stdout + result.stderr)
                    self.assertEqual(mock.requests, [])

    def test_reference_format_and_budget_rejections(self):
        for name, command in self.runtimes:
            with self.subTest(runtime=name, case="gif"), MockServer() as mock:
                self.write_ref("anim.gif", b"GIF89a" + b"\x00" * 8)
                result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--image", "anim.gif", "--output", "out.png"])
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertEqual(mock.requests, [])
            with self.subTest(runtime=name, case="file-budget"), MockServer() as mock:
                huge = self.root / "huge.png"
                with huge.open("wb") as stream:
                    stream.write(b"\x89PNG\r\n\x1a\n")
                    stream.seek((10 << 20))
                    stream.write(b"\x00")
                result = self.run_helper(command, ["--url", mock.url, "--prompt", "p", "--image", str(huge), "--output", "out.png"])
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertEqual(mock.requests, [])
                self.assertFalse((self.root / "out.png").exists())
            with self.subTest(runtime=name, case="total-budget"), MockServer() as mock:
                paths = []
                for i in range(4):
                    item = self.root / f"part-{i}.png"
                    with item.open("wb") as stream:
                        stream.write(b"\x89PNG\r\n\x1a\n")
                        stream.seek((9 << 20) - 1)
                        stream.write(b"\x00")
                    paths.append(item.name)
                jobs = [{"prompt": "p", "output": "out.png", "images": paths}]
                (self.root / "budget.json").write_text(json.dumps(jobs), encoding="utf-8")
                result = self.run_helper(command, ["--url", mock.url, "--batch", str(self.root / "budget.json")])
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertEqual(mock.requests, [])
            with self.subTest(runtime=name, case="shared-reference"), MockServer() as mock:
                self.write_ref("shared.png", PNG)
                jobs = [{"prompt": "one", "output": f"{name}-one.png", "images": ["shared.png"]},
                        {"prompt": "two", "output": f"{name}-two.png", "images": ["shared.png"]}]
                (self.root / "shared.json").write_text(json.dumps(jobs), encoding="utf-8")
                result = self.run_helper(command, ["--url", mock.url, "--batch", str(self.root / "shared.json")])
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(len(mock.requests), 2)
                self.assertTrue(all(item["path"] == "/v1/images/edits" for item in mock.requests))

    def test_batch_references_resolve_from_cwd(self):
        for name, command in self.runtimes:
            with self.subTest(runtime=name), MockServer() as mock:
                batch_dir = self.root / "jobs"
                batch_dir.mkdir(exist_ok=True)
                (batch_dir / "ref.png").write_bytes(b"GIF89a-decoy")
                self.write_ref("ref.png", PNG)
                (batch_dir / "jobs.json").write_text(json.dumps([
                    {"prompt": "p", "output": f"{name}-from-cwd.png", "images": ["ref.png"]}
                ]), encoding="utf-8")
                result = self.run_helper(command, ["--url", mock.url, "--batch", str(batch_dir / "jobs.json")])
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(mock.requests[0]["body"]["images"][0]["image_url"], self.data_url("image/png", PNG))

    def test_request_limit_constants(self):
        self.assertEqual(self.helper.MAX_REQUEST_BYTES, 64 << 20)
        self.assertEqual(self.helper.MAX_REFERENCE_BYTES, 10 << 20)
        self.assertEqual(self.helper.MAX_REFERENCE_TOTAL_BYTES, 32 << 20)
        self.assertFalse(self.helper.request_exceeds_limit(b"x"))
        self.assertTrue(self.helper.request_exceeds_limit(bytes(self.helper.MAX_REQUEST_BYTES + 1)))
        node = next((command[0] for runtime, command in self.runtimes if runtime == "node"), None)
        if node is None:
            return
        script = """
import { MAX_REQUEST_BYTES, MAX_REFERENCE_BYTES, MAX_REFERENCE_TOTAL_BYTES, requestExceedsLimit } from './scripts/generate_image.mjs';
if (MAX_REQUEST_BYTES !== 67108864 || MAX_REFERENCE_BYTES !== 10485760 || MAX_REFERENCE_TOTAL_BYTES !== 33554432) process.exit(3);
if (requestExceedsLimit(Buffer.alloc(1)) || !requestExceedsLimit(Buffer.alloc(MAX_REQUEST_BYTES + 1))) process.exit(4);
"""
        result = subprocess.run([node, "--input-type=module", "-e", script], cwd=ROOT, text=True, capture_output=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stderr)

    def run_instrumented_helper(self, name, command, phase, action, args):
        reference = self.root / "ref.png"
        transport = self.root / f"{name}-transport.txt"
        if name == "python":
            script = r'''
import os
from pathlib import Path
import runpy
import signal
import sys
import urllib.request

helper, reference, transport, phase, action = sys.argv[1:6]
phase = int(phase)
original_open, original_read, original_close = os.open, os.read, os.close
opened = 0
tracked = {}
fired = False

def open_reference(filename, flags, *args, **kwargs):
    global opened
    fd = original_open(filename, flags, *args, **kwargs)
    if os.path.abspath(filename) == reference and not flags & (os.O_WRONLY | os.O_RDWR):
        opened += 1
        tracked[fd] = opened
    return fd

def read_reference(fd, size):
    global fired
    data = original_read(fd, size)
    if data and tracked.get(fd) == phase and not fired:
        fired = True
        if action == "signal":
            os.kill(os.getpid(), signal.SIGTERM)
        else:
            with open(reference, "ab") as stream:
                stream.write(b"appended-during-read")
    return data

def close_reference(fd):
    tracked.pop(fd, None)
    return original_close(fd)

original_send = urllib.request.OpenerDirector.open

def send(opener, *args, **kwargs):
    with open(transport, "a") as stream:
        stream.write("request\n")
    return original_send(opener, *args, **kwargs)

os.open, os.read, os.close = open_reference, read_reference, close_reference
urllib.request.OpenerDirector.open = send
sys.argv = [helper] + sys.argv[6:]
runpy.run_path(helper, run_name="__main__")
'''
            instrumented = [command[0], "-c", script, command[1], str(reference), str(transport), str(phase), action]
        else:
            script = r'''
import fs from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import path from 'node:path';
const { reference, transport, phase, action } = CONFIG;
const originalOpen = fs.openSync, originalRead = fs.readSync, originalClose = fs.closeSync;
let opened = 0, fired = false;
const tracked = new Map();
fs.openSync = function(filename, flags, ...args) {
  const fd = originalOpen.call(this, filename, flags, ...args);
  const readable = typeof flags === 'number' ? !(flags & (fs.constants.O_WRONLY | fs.constants.O_RDWR)) : flags === 'r';
  if (path.resolve(String(filename)) === reference && readable) tracked.set(fd, ++opened);
  return fd;
};
fs.readSync = function(fd, ...args) {
  const count = originalRead.call(this, fd, ...args);
  if (count && tracked.get(fd) === phase && !fired) {
    fired = true;
    if (action === 'signal') process.kill(process.pid, 'SIGTERM');
    else fs.appendFileSync(reference, 'appended-during-read');
  }
  return count;
};
fs.closeSync = function(fd) {
  tracked.delete(fd);
  return originalClose.call(this, fd);
};
for (const client of [http, https]) {
  const original = client.request;
  client.request = function(...args) {
    fs.appendFileSync(transport, 'request\n');
    return original.apply(this, args);
  };
}
'''
            script = script.replace("CONFIG", json.dumps({"reference": str(reference), "transport": str(transport),
                                                          "phase": phase, "action": action}))
            preload = "data:text/javascript;base64," + base64.b64encode(script.encode()).decode("ascii")
            instrumented = [command[0], "--import", preload, command[1]]
        result = self.run_helper(instrumented, args)
        sends = transport.read_text().splitlines() if transport.exists() else []
        return result, sends

    def test_reference_preflight_interruption_sends_nothing(self):
        if os.name != "posix":
            self.skipTest("POSIX signal delivery test")
        for name, command in self.runtimes:
            with self.subTest(runtime=name), MockServer() as mock:
                self.write_ref("ref.png", PNG)
                output = f"{name}-preflight-stop.png"
                result, sends = self.run_instrumented_helper(name, command, 1, "signal", [
                    "--url", mock.url, "--prompt", "private-prompt", "--image", "ref.png", "--output", output])
                self.assertEqual(result.returncode, 130, result.stderr)
                self.assertEqual(sends, [])
                self.assertEqual(mock.entries, [])
                self.assertFalse((self.root / output).exists())
                self.assert_private(result)

    def test_final_reference_read_interruption_sends_nothing(self):
        if os.name != "posix":
            self.skipTest("POSIX signal delivery test")
        for name, command in self.runtimes:
            with self.subTest(runtime=name), MockServer() as mock:
                self.write_ref("ref.png", PNG)
                output = f"{name}-final-stop.png"
                result, sends = self.run_instrumented_helper(name, command, 2, "signal", [
                    "--url", mock.url, "--prompt", "private-prompt", "--image", "ref.png", "--output", output])
                self.assertEqual(result.returncode, 130, result.stderr)
                self.assertEqual(self.rows(result)[0]["error"], "interrupted")
                self.assertEqual(sends, [])
                self.assertEqual(mock.entries, [])
                self.assertFalse((self.root / output).exists())
                self.assert_private(result)

    def test_reference_growth_during_final_read_is_not_sent(self):
        for name, command in self.runtimes:
            with self.subTest(runtime=name), MockServer() as mock:
                reference = self.write_ref("ref.png", PNG)
                jobs = [{"prompt": "private-prompt", "images": ["ref.png"], "output": f"{name}-grew.png"},
                        {"prompt": "next", "output": f"{name}-after-growth.png"}]
                batch = self.root / f"{name}-growth.json"
                batch.write_text(json.dumps(jobs), encoding="utf-8")
                result, sends = self.run_instrumented_helper(name, command, 2, "append", [
                    "--url", mock.url, "--batch", str(batch)])
                self.assertGreater(reference.stat().st_size, len(PNG))
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertEqual([row["ok"] for row in self.rows(result)], [False, True])
                self.assertEqual(self.rows(result)[0]["error"], "reference_changed")
                self.assertEqual(sends, ["request"])
                self.assertEqual(mock.entries, ["/v1/images/generations"])
                self.assertEqual([item["body"]["prompt"] for item in mock.requests], ["next"])
                self.assertFalse((self.root / jobs[0]["output"]).exists())
                self.assertTrue((self.root / jobs[1]["output"]).is_file())
                self.assert_private(result)

    def test_edit_interruption_cleans_reservations(self):
        if os.name != "posix":
            self.skipTest("POSIX signal delivery test")
        started = threading.Event()

        def delayed(handler, body):
            started.set()
            handler.send_response(200)
            handler.send_header("Content-Length", "100")
            handler.end_headers()
            handler.wfile.write(b'{"data":')
            handler.wfile.flush()
            time.sleep(0.5)

        for name, command in self.runtimes:
            with self.subTest(runtime=name), MockServer(delayed) as mock:
                started.clear()
                self.write_ref("ref.png", PNG)
                jobs = [{"prompt": str(i), "output": f"{name}-stop-{i}.png", "images": ["ref.png"]} for i in range(3)]
                batch = self.root / "stop.json"
                batch.write_text(json.dumps(jobs), encoding="utf-8")
                proc = subprocess.Popen(command + ["--url", mock.url, "--batch", str(batch)], cwd=self.root,
                                        env=self.env(), stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                try:
                    self.assertTrue(started.wait(5))
                    proc.send_signal(signal.SIGTERM)
                    stdout, stderr = proc.communicate(timeout=5)
                    self.assertEqual(proc.returncode, 130, stderr)
                    self.assertEqual(json.loads(stdout)["error"], "interrupted")
                    self.assertEqual(mock.requests[0]["path"], "/v1/images/edits")
                    self.assertEqual(len(mock.requests), 1)
                    for job in jobs:
                        self.assertFalse((self.root / job["output"]).exists())
                    self.assertTrue((self.root / "ref.png").is_file())
                finally:
                    if proc.poll() is None:
                        proc.kill()
                        proc.communicate()


if __name__ == "__main__":
    unittest.main()
