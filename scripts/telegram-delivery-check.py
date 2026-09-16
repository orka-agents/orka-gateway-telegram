#!/usr/bin/env python3
"""Read-only Telegram webhook health check used by live-validate."""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import stat
import sys
import urllib.error
import urllib.request

MAX_RESPONSE_BYTES = 64 * 1024


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):  # noqa: D401
        raise urllib.error.HTTPError(req.full_url, code, "redirect refused", headers, fp)


def read_token(path: Path) -> str:
    info = path.lstat()
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600:
        raise ValueError("Telegram token file must be a regular mode-0600 file")
    token = path.read_text(encoding="utf-8").strip()
    if not token or "\n" in token or ":" not in token:
        raise ValueError("Telegram token file is empty or malformed")
    return token


def fetch(base_url: str, token: str, timeout: float) -> dict:
    request = urllib.request.Request(
        f"{base_url.rstrip('/')}/bot{token}/getWebhookInfo",
        headers={"User-Agent": "orka-gateway-telegram/live-validate"},
    )
    opener = urllib.request.build_opener(NoRedirect)
    try:
        with opener.open(request, timeout=timeout) as response:
            body = response.read(MAX_RESPONSE_BYTES + 1)
    except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError) as exc:
        raise RuntimeError("Telegram read-only status request failed") from exc
    if len(body) > MAX_RESPONSE_BYTES:
        raise RuntimeError("Telegram status response exceeded the safe size limit")
    try:
        payload = json.loads(body)
    except json.JSONDecodeError as exc:
        raise RuntimeError("Telegram returned malformed status JSON") from exc
    if not isinstance(payload, dict) or payload.get("ok") is not True:
        raise RuntimeError("Telegram did not accept the read-only status request")
    result = payload.get("result")
    if not isinstance(result, dict):
        raise RuntimeError("Telegram status response had no result object")
    return result


def run(args: argparse.Namespace) -> int:
    try:
        token = read_token(Path(args.token_file))
        info = fetch(args.api_base, token, args.timeout)
    except (OSError, ValueError, RuntimeError):
        print("Telegram delivery check failed: unable to read a safe status response.", file=sys.stderr)
        return 1

    url = info.get("url")
    configured = isinstance(url, str) and bool(url.strip())
    expected = args.expected_url.strip()
    matches = configured and bool(expected) and url.rstrip("/") == expected.rstrip("/")
    pending = info.get("pending_update_count", 0)
    if not isinstance(pending, int) or pending < 0:
        pending = 0
    print(f"Telegram message-delivery URL configured: {'yes' if configured else 'no'}")
    if expected:
        print(f"Telegram message-delivery URL matches adapter configuration: {'yes' if matches else 'no'}")
    print(f"Telegram messages waiting for delivery: {pending}")
    if info.get("last_error_date"):
        try:
            timestamp = datetime.fromtimestamp(int(info["last_error_date"]), timezone.utc).isoformat()
        except (TypeError, ValueError, OverflowError):
            timestamp = "unknown time"
        print(f"Last Telegram delivery problem recorded: {timestamp}")
        print("Telegram could not reach the adapter at the recorded time.")
    else:
        print("Last Telegram delivery problem recorded: none")
    if not configured or (expected and not matches):
        return 1
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description="Read-only Telegram delivery check")
    parser.add_argument("--token-file", required=True, help="Path to mode-0600 Telegram bot token file")
    parser.add_argument("--api-base", default="https://api.telegram.org", help="Telegram API base URL")
    parser.add_argument("--expected-url", default="", help="Expected Telegram webhook callback URL")
    parser.add_argument("--timeout", type=float, default=5.0, help="Request timeout in seconds")
    return run(parser.parse_args())


if __name__ == "__main__":
    raise SystemExit(main())
