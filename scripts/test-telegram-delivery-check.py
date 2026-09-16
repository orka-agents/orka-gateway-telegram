#!/usr/bin/env python3
"""Exercise offline read-only Telegram delivery checks without network or cluster access."""

import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

SCRIPT = Path(__file__).with_name("telegram-delivery-check.py")
FAKE_SECRET = "987654321:AAFakeSecretTokenForTestingOnly_12345"


class MockTelegramHandler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        # Suppress logging to keep test output clean and avoid secret leakage
        pass

    def do_GET(self):
        server = self.server
        with server.state_lock:
            server.last_method = self.command
            server.last_path = self.path
            server.last_headers = dict(self.headers)
            server.request_count += 1
            delay = server.response_delay
            status = server.response_status
            headers = dict(server.response_headers)
            body = server.response_body

        if delay > 0:
            time.sleep(delay)

        self.send_response(status)
        for k, v in headers.items():
            self.send_header(k, v)
        self.end_headers()
        if body:
            try:
                self.wfile.write(body)
            except (BrokenPipeError, ConnectionResetError):
                pass


class MockServer:
    def __init__(self):
        self.server = HTTPServer(("127.0.0.1", 0), MockTelegramHandler)
        self.server.state_lock = threading.Lock()
        self.server.response_delay = 0.0
        self.server.response_status = 200
        self.server.response_headers = {"Content-Type": "application/json"}
        self.server.response_body = b""
        self.server.last_method = None
        self.server.last_path = None
        self.server.last_headers = {}
        self.server.request_count = 0
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    @property
    def port(self) -> int:
        return self.server.server_port

    @property
    def base_url(self) -> str:
        return f"http://127.0.0.1:{self.port}"

    def set_response(self, status=200, payload=None, raw_body=None, headers=None, delay=0.0):
        with self.server.state_lock:
            self.server.response_status = status
            self.server.response_delay = delay
            h = {"Content-Type": "application/json"}
            if headers:
                h.update(headers)
            if raw_body is not None:
                body = raw_body
            elif payload is not None:
                body = json.dumps(payload).encode("utf-8")
            else:
                body = b""
            h["Content-Length"] = str(len(body))
            self.server.response_headers = h
            self.server.response_body = body

    def shutdown(self):
        self.server.shutdown()
        self.server.server_close()


class TelegramDeliveryCheckTests(unittest.TestCase):
    mock_server: MockServer

    @classmethod
    def setUpClass(cls):
        cls.mock_server = MockServer()

    @classmethod
    def tearDownClass(cls):
        cls.mock_server.shutdown()

    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        self.token_path = Path(self.temp_dir.name) / "telegram-bot-token"
        self.token_path.write_text(f"{FAKE_SECRET}\n", encoding="utf-8")
        self.token_path.chmod(stat.S_IRUSR | stat.S_IWUSR)  # 0600

    def tearDown(self):
        self.temp_dir.cleanup()

    def run_check(self, *extra_args, token_path=None, timeout=2.0) -> subprocess.CompletedProcess:
        tp = token_path if token_path is not None else self.token_path
        args = [
            sys.executable,
            str(SCRIPT),
            "--token-file",
            str(tp),
            "--api-base",
            self.mock_server.base_url,
            "--timeout",
            str(timeout),
            *extra_args,
        ]
        env = os.environ.copy()
        for k in ("HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"):
            env.pop(k, None)
        result = subprocess.run(args, capture_output=True, text=True, env=env)
        # Verify secret token is NEVER leaked in stdout or stderr
        self.assertNotIn(FAKE_SECRET, result.stdout, "Secret token leaked in stdout!")
        self.assertNotIn(FAKE_SECRET, result.stderr, "Secret token leaked in stderr!")
        return result

    def test_healthy_webhook_configuration(self):
        self.mock_server.set_response(
            payload={
                "ok": True,
                "result": {
                    "url": "https://adapter.example.com/telegram/webhook",
                    "has_custom_certificate": False,
                    "pending_update_count": 0,
                },
            }
        )
        res = self.run_check("--expected-url", "https://adapter.example.com/telegram/webhook")
        self.assertEqual(res.returncode, 0, f"Expected 0, got {res.returncode}: {res.stderr}")
        self.assertIn("Telegram message-delivery URL configured: yes", res.stdout)
        self.assertIn("Telegram message-delivery URL matches adapter configuration: yes", res.stdout)
        self.assertIn("Telegram messages waiting for delivery: 0", res.stdout)
        self.assertIn("Last Telegram delivery problem recorded: none", res.stdout)

    def test_pending_updates_reported(self):
        self.mock_server.set_response(
            payload={
                "ok": True,
                "result": {
                    "url": "https://adapter.example.com/telegram/webhook",
                    "pending_update_count": 4,
                },
            }
        )
        res = self.run_check("--expected-url", "https://adapter.example.com/telegram/webhook")
        self.assertEqual(res.returncode, 0)
        self.assertIn("Telegram messages waiting for delivery: 4", res.stdout)

    def test_last_error_date_reported_safely(self):
        self.mock_server.set_response(
            payload={
                "ok": True,
                "result": {
                    "url": "https://adapter.example.com/telegram/webhook",
                    "pending_update_count": 0,
                    "last_error_date": 1700000000,
                    "last_error_message": "Wrong response from the webhook: 502 Bad Gateway at /internal/secret",
                },
            }
        )
        res = self.run_check("--expected-url", "https://adapter.example.com/telegram/webhook")
        self.assertEqual(res.returncode, 0)
        self.assertIn("Last Telegram delivery problem recorded: 2023-11-14T22:13:20+00:00", res.stdout)
        self.assertIn("Telegram could not reach the adapter at the recorded time.", res.stdout)
        # Ensure provider raw error strings are never leaked
        self.assertNotIn("502 Bad Gateway", res.stdout)
        self.assertNotIn("internal/secret", res.stdout)

    def test_missing_webhook_url(self):
        self.mock_server.set_response(
            payload={
                "ok": True,
                "result": {
                    "url": "",
                    "pending_update_count": 0,
                },
            }
        )
        res = self.run_check("--expected-url", "https://adapter.example.com/telegram/webhook")
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram message-delivery URL configured: no", res.stdout)
        self.assertIn("Telegram message-delivery URL matches adapter configuration: no", res.stdout)

    def test_mismatched_webhook_url(self):
        self.mock_server.set_response(
            payload={
                "ok": True,
                "result": {
                    "url": "https://other-service.example.com/webhook",
                    "pending_update_count": 0,
                },
            }
        )
        res = self.run_check("--expected-url", "https://adapter.example.com/telegram/webhook")
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram message-delivery URL configured: yes", res.stdout)
        self.assertIn("Telegram message-delivery URL matches adapter configuration: no", res.stdout)
        self.assertNotIn("other-service.example.com", res.stdout)

    def test_no_expected_url_provided(self):
        self.mock_server.set_response(
            payload={
                "ok": True,
                "result": {
                    "url": "https://adapter.example.com/telegram/webhook",
                    "pending_update_count": 0,
                },
            }
        )
        res = self.run_check()
        self.assertEqual(res.returncode, 0)
        self.assertIn("Telegram message-delivery URL configured: yes", res.stdout)
        self.assertNotIn("matches adapter configuration", res.stdout)

    def test_trailing_slash_tolerance(self):
        self.mock_server.set_response(
            payload={
                "ok": True,
                "result": {
                    "url": "https://adapter.example.com/telegram/webhook/",
                    "pending_update_count": 0,
                },
            }
        )
        res = self.run_check("--expected-url", "https://adapter.example.com/telegram/webhook")
        self.assertEqual(res.returncode, 0)
        self.assertIn("Telegram message-delivery URL matches adapter configuration: yes", res.stdout)

    def test_http_401_unauthorized(self):
        self.mock_server.set_response(
            status=401,
            payload={"ok": False, "error_code": 401, "description": "Unauthorized"},
        )
        res = self.run_check()
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram delivery check failed: unable to read a safe status response.", res.stderr)

    def test_http_404_not_found(self):
        self.mock_server.set_response(
            status=404,
            payload={"ok": False, "error_code": 404, "description": "Not Found"},
        )
        res = self.run_check()
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram delivery check failed: unable to read a safe status response.", res.stderr)

    def test_http_500_server_error(self):
        self.mock_server.set_response(
            status=500,
            raw_body=b"Internal Server Error",
        )
        res = self.run_check()
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram delivery check failed: unable to read a safe status response.", res.stderr)

    def test_redirect_refused(self):
        self.mock_server.set_response(
            status=302,
            headers={"Location": "/unexpected-redirect"},
        )
        res = self.run_check()
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram delivery check failed: unable to read a safe status response.", res.stderr)

    def test_oversized_response(self):
        self.mock_server.set_response(
            status=200,
            raw_body=b" " * (65 * 1024),
        )
        res = self.run_check()
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram delivery check failed: unable to read a safe status response.", res.stderr)

    def test_malformed_json(self):
        self.mock_server.set_response(
            status=200,
            raw_body=b"{not valid json...",
        )
        res = self.run_check()
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram delivery check failed: unable to read a safe status response.", res.stderr)

    def test_api_response_ok_false(self):
        self.mock_server.set_response(
            status=200,
            payload={"ok": False, "error_code": 400, "description": "Bad Request"},
        )
        res = self.run_check()
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram delivery check failed: unable to read a safe status response.", res.stderr)

    def test_api_response_result_not_dict(self):
        self.mock_server.set_response(
            status=200,
            payload={"ok": True, "result": "invalid result type"},
        )
        res = self.run_check()
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram delivery check failed: unable to read a safe status response.", res.stderr)

    def test_timeout(self):
        self.mock_server.set_response(
            delay=0.4,
            payload={"ok": True, "result": {"url": "https://adapter.example.com"}},
        )
        res = self.run_check(timeout=0.1)
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram delivery check failed: unable to read a safe status response.", res.stderr)

    def test_token_file_missing(self):
        missing_path = Path(self.temp_dir.name) / "does-not-exist"
        res = self.run_check(token_path=missing_path)
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram delivery check failed: unable to read a safe status response.", res.stderr)

    def test_token_file_is_symlink(self):
        link_path = Path(self.temp_dir.name) / "symlink-token"
        link_path.symlink_to(self.token_path)
        res = self.run_check(token_path=link_path)
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram delivery check failed: unable to read a safe status response.", res.stderr)

    def test_token_file_insecure_permissions_0644(self):
        self.token_path.chmod(stat.S_IRUSR | stat.S_IWUSR | stat.S_IRGRP | stat.S_IROTH)  # 0644
        res = self.run_check()
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram delivery check failed: unable to read a safe status response.", res.stderr)

    def test_token_file_insecure_permissions_0755(self):
        self.token_path.chmod(0o755)
        res = self.run_check()
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram delivery check failed: unable to read a safe status response.", res.stderr)

    def test_token_file_empty(self):
        self.token_path.write_text("", encoding="utf-8")
        res = self.run_check()
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram delivery check failed: unable to read a safe status response.", res.stderr)

    def test_token_file_malformed_no_colon(self):
        self.token_path.write_text("invalidtokendatanoformat\n", encoding="utf-8")
        res = self.run_check()
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram delivery check failed: unable to read a safe status response.", res.stderr)

    def test_token_file_multiline(self):
        self.token_path.write_text(f"{FAKE_SECRET}\nextra_line\n", encoding="utf-8")
        res = self.run_check()
        self.assertEqual(res.returncode, 1)
        self.assertIn("Telegram delivery check failed: unable to read a safe status response.", res.stderr)

    def test_request_method_path_and_headers(self):
        self.mock_server.set_response(
            payload={"ok": True, "result": {"url": "https://adapter.example.com/telegram/webhook"}}
        )
        res = self.run_check()
        self.assertEqual(res.returncode, 0)
        with self.mock_server.server.state_lock:
            self.assertEqual(self.mock_server.server.last_method, "GET")
            self.assertEqual(self.mock_server.server.last_path, f"/bot{FAKE_SECRET}/getWebhookInfo")
            self.assertEqual(
                self.mock_server.server.last_headers.get("User-Agent"),
                "orka-gateway-telegram/live-validate",
            )


if __name__ == "__main__":
    unittest.main()
