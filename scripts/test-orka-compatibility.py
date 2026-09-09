#!/usr/bin/env python3
"""Run an Orka checkout's gateway conformance tool against a local adapter."""

import argparse
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class FakeTelegram(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        # Request paths contain the disposable bot credential.
        pass

    def respond(self, value, status=200):
        body = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.endswith("/getMe"):
            self.respond({"ok": True, "result": {
                "id": 42, "is_bot": True, "first_name": "Compatibility fixture",
            }})
        else:
            self.respond({"ok": False}, 404)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        if self.path.endswith("/sendMessage"):
            with self.server.state_lock:
                self.server.sends += 1
                message_id = self.server.sends
            self.respond({"ok": True, "result": {
                "message_id": message_id, "date": int(time.time()),
                "chat": {"id": int(body["chat_id"]), "type": "private"},
                "text": body["text"],
            }})
        else:
            self.respond({"ok": False}, 404)


def stop(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def wait_ready(process, endpoint):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError("adapter exited before readiness")
        try:
            with opener.open(endpoint + "/readyz", timeout=0.5) as response:
                if response.status == 200:
                    return
        except (urllib.error.URLError, TimeoutError):
            pass
        time.sleep(0.05)
    raise RuntimeError("adapter readiness timed out")


def check(root, fake, adapter_binary, conformance_binary):
    with socket.socket() as reserve:
        reserve.bind(("127.0.0.1", 0))
        port = reserve.getsockname()[1]
    endpoint = f"http://127.0.0.1:{port}"
    fake_endpoint = f"http://127.0.0.1:{fake.server_port}"
    outbound = secrets.token_hex(32)
    # Avoid inheriting credentials or *_FILE settings from the caller.
    base_env = {key: os.environ[key] for key in ("PATH", "TMPDIR", "LANG", "LC_ALL")
                if key in os.environ}
    adapter_env = dict(
        base_env, LISTEN_ADDRESS=f"127.0.0.1:{port}", DATABASE_PATH=str(root / "adapter.db"),
        TELEGRAM_API_BASE_URL=fake_endpoint, TELEGRAM_BOT_TOKEN="42:" + secrets.token_urlsafe(32),
        TELEGRAM_WEBHOOK_SECRET=secrets.token_hex(32), TELEGRAM_WEBHOOK_URL="",
        ORKA_GATEWAY_INBOUND_TOKEN=secrets.token_hex(32), ORKA_GATEWAY_OUTBOUND_TOKEN=outbound,
        ORKA_GATEWAY_INGRESS_URL=fake_endpoint + "/api/v1/gateways/compat/telegram/events",
        TELEGRAM_CONFORMANCE_CHAT_ID="100", REQUEST_TIMEOUT="5s", SHUTDOWN_TIMEOUT="5s",
    )
    cli_env = dict(base_env, COMPAT_CONFORMANCE_TOKEN=outbound)
    for phase in ("fresh database", "after adapter restart"):
        with (root / "adapter.log").open("ab") as log:
            process = subprocess.Popen([str(adapter_binary)], env=adapter_env,
                                       stdout=log, stderr=log)
            try:
                wait_ready(process, endpoint)
                run = subprocess.run(
                    [str(conformance_binary), "--endpoint", endpoint,
                     "--token-env", "COMPAT_CONFORMANCE_TOKEN", "--timeout", "5s"],
                    env=cli_env, capture_output=True, text=True, timeout=25,
                )
                result = json.loads(run.stdout)
                if run.returncode != 0 or not result.get("Passed"):
                    raise RuntimeError(f"Orka conformance failed: {result.get('Message', 'no detail')}")
                print(f"Passed Orka conformance: {phase}", flush=True)
            finally:
                stop(process)
    if fake.sends != 1:
        raise RuntimeError(f"expected one provider send across replays and restart; got {fake.sends}")
    print("Passed durable replay: exactly one send to the local fake Telegram API", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--orka-dir", required=True, help="local Orka source checkout")
    args = parser.parse_args()
    if not args.orka_dir:
        parser.error("--orka-dir must name an Orka checkout")
    orka_dir = Path(args.orka_dir).expanduser().resolve()
    if not (orka_dir / "cmd/orka-gateway-conformance/main.go").is_file():
        parser.error("--orka-dir does not contain Orka's gateway conformance command")
    repo = Path(__file__).resolve().parents[1]
    revision = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=orka_dir, text=True).strip()
    print(f"Checking against Orka {revision}", flush=True)

    with tempfile.TemporaryDirectory(prefix="orka-telegram-compat-") as temporary:
        root = Path(temporary)
        adapter_binary = root / "adapter"
        conformance_binary = root / "orka-conformance"
        subprocess.run(["go", "build", "-mod=readonly", "-o", str(adapter_binary),
                        "./cmd/orka-gateway-telegram"], cwd=repo, check=True)
        subprocess.run(["go", "build", "-mod=readonly", "-o", str(conformance_binary),
                        "./cmd/orka-gateway-conformance"], cwd=orka_dir, check=True)
        fake = ThreadingHTTPServer(("127.0.0.1", 0), FakeTelegram)
        fake.state_lock = threading.Lock()
        fake.sends = 0
        thread = threading.Thread(target=fake.serve_forever, daemon=True)
        thread.start()
        try:
            check(root, fake, adapter_binary, conformance_binary)
        finally:
            fake.shutdown()
            fake.server_close()
            thread.join(timeout=3)


if __name__ == "__main__":
    main()
