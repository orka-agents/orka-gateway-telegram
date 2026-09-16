#!/usr/bin/env python3
import json
import os
import stat
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

SCRIPT = Path(__file__).with_name("telegram-delivery-check.py")


class Handler(BaseHTTPRequestHandler):
    payload = {"ok": True, "result": {"url": "https://adapter.example/telegram/webhook", "pending_update_count": 2}}
    redirect = False
    def do_GET(self):
        if self.redirect:
            self.send_response(302); self.send_header("Location", "/redirected"); self.end_headers(); return
        body = json.dumps(self.payload).encode()
        self.send_response(200); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *_args):
        pass


def run_case(payload, expected=0, redirect=False):
    Handler.payload, Handler.redirect = payload, redirect
    server = HTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    with tempfile.TemporaryDirectory() as tmp:
        token = Path(tmp) / "token"; token.write_text("42:fake-secret\n"); token.chmod(stat.S_IRUSR | stat.S_IWUSR)
        env = os.environ.copy(); env.pop("HTTP_PROXY", None); env.pop("HTTPS_PROXY", None)
        result = subprocess.run([sys.executable, str(SCRIPT), "--token-file", str(token), "--api-base", f"http://127.0.0.1:{server.server_port}", "--expected-url", "https://adapter.example/telegram/webhook"], text=True, capture_output=True, env=env)
    server.shutdown()
    assert result.returncode == expected, (result.stdout, result.stderr)
    assert "fake-secret" not in result.stdout + result.stderr


run_case({"ok": True, "result": {"url": "https://adapter.example/telegram/webhook", "pending_update_count": 2}})
run_case({"ok": True, "result": {"url": "", "pending_update_count": 0}}, expected=1)
run_case({"ok": True, "result": {"url": "https://adapter.example/telegram/webhook", "pending_update_count": 0}}, redirect=True, expected=1)
print("Telegram delivery check tests passed")
