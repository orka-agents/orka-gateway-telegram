#!/usr/bin/env python3
"""Run isolated Telegram adapter fault-injection scenarios against Orka.

The script only targets the dedicated orka-gateway-telegram-fi namespace. It
reads test-only Kubernetes Secrets into memory, never prints them, and never
contacts the real Telegram Bot API.
"""

from __future__ import annotations

import base64
import hashlib
import json
import os
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from datetime import datetime
from typing import Any

CONTEXT = os.environ.get("KUBE_CONTEXT", "")
NAMESPACE = "orka-gateway-telegram-fi"
LIVE_NAMESPACE = "orka-gateway-telegram"
GATEWAY = "telegram-fault-validation"
ACCOUNT_ID = 900000001
CHAT_ID = 900000002
SENDER_ID = 900000002
TERMINAL_DELIVERY_STATES = {"Delivered", "DeadLettered", "Expired", "Failed"}


def fail(message: str) -> None:
    raise RuntimeError(message)


def kubectl(*args: str, timeout: int = 60) -> str:
    command = ["kubectl", "--context", CONTEXT, *args]
    completed = subprocess.run(command, check=True, capture_output=True, text=True, timeout=timeout)
    return completed.stdout


def kube_json(*args: str) -> dict[str, Any]:
    return json.loads(kubectl(*args))


def secret_value(name: str, key: str) -> str:
    document = kube_json("-n", NAMESPACE, "get", "secret", name, "-o", "json")
    encoded = document.get("data", {}).get(key)
    if not encoded:
        fail(f"test Secret {name!r} is missing key {key!r}")
    value = base64.b64decode(encoded).decode("utf-8").strip()
    if not value:
        fail(f"test Secret {name!r} key {key!r} is empty")
    return value


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind(("127.0.0.1", 0))
        return int(listener.getsockname()[1])


class PortForward:
    def __init__(self, namespace: str, resource: str, remote_port: int, health_path: str):
        self.namespace = namespace
        self.resource = resource
        self.remote_port = remote_port
        self.health_path = health_path
        self.local_port = free_port()
        self.process: subprocess.Popen[bytes] | None = None

    @property
    def base_url(self) -> str:
        return f"http://127.0.0.1:{self.local_port}"

    def start(self) -> None:
        self.process = subprocess.Popen(
            [
                "kubectl", "--context", CONTEXT, "-n", self.namespace, "port-forward",
                self.resource, f"{self.local_port}:{self.remote_port}",
            ],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        deadline = time.time() + 30
        while time.time() < deadline:
            if self.process.poll() is not None:
                fail(f"port-forward for {self.resource} exited early")
            try:
                status, _ = http_request("GET", self.base_url + self.health_path, timeout=2)
                if status == 200:
                    return
            except Exception:
                pass
            time.sleep(0.5)
        fail(f"port-forward for {self.resource} did not become ready")

    def close(self) -> None:
        if self.process is None:
            return
        self.process.terminate()
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=5)


def http_request(
    method: str,
    url: str,
    *,
    body: Any | None = None,
    headers: dict[str, str] | None = None,
    timeout: float = 30,
) -> tuple[int, bytes]:
    encoded = None
    request_headers = dict(headers or {})
    if body is not None:
        encoded = json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        request_headers.setdefault("Content-Type", "application/json")
    request = urllib.request.Request(url, data=encoded, headers=request_headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def json_request(*args: Any, **kwargs: Any) -> tuple[int, Any]:
    status, payload = http_request(*args, **kwargs)
    try:
        decoded = json.loads(payload.decode("utf-8")) if payload else None
    except json.JSONDecodeError:
        decoded = payload.decode("utf-8", "replace")
    return status, decoded


def parse_time(value: str) -> datetime:
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


@dataclass
class Fixture:
    adapter: PortForward
    proxy: PortForward
    api: PortForward
    webhook_secret: str
    outbound_token: str
    control_token: str
    api_token: str
    next_update_id: int

    @property
    def api_headers(self) -> dict[str, str]:
        return {"Authorization": f"Bearer {self.api_token}"}

    @property
    def control_headers(self) -> dict[str, str]:
        return {"Authorization": f"Bearer {self.control_token}"}

    def proxy_state(self) -> dict[str, Any]:
        status, payload = json_request(
            "GET", self.proxy.base_url + "/control/state", headers=self.control_headers
        )
        if status != 200 or not isinstance(payload, dict):
            fail(f"fault proxy state request failed with HTTP {status}")
        return payload

    def set_plan(self, actions: list[dict[str, Any] | str]) -> None:
        status, payload = json_request(
            "PUT",
            self.proxy.base_url + "/control/plan",
            body={"actions": actions},
            headers=self.control_headers,
        )
        if status != 200:
            fail(f"fault proxy plan request failed with HTTP {status}: {payload!r}")

    def inject(self, text: str, *, update_id: int | None = None) -> tuple[int, dict[str, Any], str]:
        if update_id is None:
            update_id = self.next_update_id
            self.next_update_id += 1
        now = int(time.time())
        payload = {
            "update_id": update_id,
            "message": {
                "message_id": update_id,
                "from": {"id": SENDER_ID, "is_bot": False, "first_name": "Fault"},
                "date": now,
                "chat": {"id": CHAT_ID, "type": "private"},
                "text": text,
            },
        }
        status, response = json_request(
            "POST",
            self.adapter.base_url + "/telegram/webhook",
            body=payload,
            headers={"X-Telegram-Bot-Api-Secret-Token": self.webhook_secret},
            timeout=30,
        )
        if status != 200:
            fail(f"shadow adapter webhook returned HTTP {status}: {response!r}")
        return update_id, payload, f"telegram:{ACCOUNT_ID}:{update_id}"

    def events(self) -> list[dict[str, Any]]:
        query = urllib.parse.urlencode({"namespace": NAMESPACE, "limit": "250"})
        status, payload = json_request(
            "GET", self.api.base_url + "/api/v1/gateway-events?" + query, headers=self.api_headers
        )
        if status != 200 or not isinstance(payload, dict):
            fail(f"Orka gateway-event list failed with HTTP {status}")
        return list(payload.get("items", []))

    def wait_event(self, external_event_id: str, timeout: float = 120) -> dict[str, Any]:
        deadline = time.time() + timeout
        while time.time() < deadline:
            matches = [item for item in self.events() if item.get("externalEventId") == external_event_id]
            if matches:
                event = matches[-1]
                if event.get("deliveryId"):
                    return event
            time.sleep(0.5)
        fail(f"Orka event {external_event_id!r} did not produce a delivery")

    def delivery(self, delivery_id: str) -> dict[str, Any]:
        query = urllib.parse.urlencode({"namespace": NAMESPACE})
        status, payload = json_request(
            "GET",
            self.api.base_url + f"/api/v1/gateway-deliveries/{delivery_id}?{query}",
            headers=self.api_headers,
        )
        if status != 200 or not isinstance(payload, dict):
            fail(f"Orka delivery {delivery_id!r} read failed with HTTP {status}")
        return payload

    def wait_delivery(self, delivery_id: str, timeout: float = 120) -> dict[str, Any]:
        deadline = time.time() + timeout
        latest: dict[str, Any] | None = None
        while time.time() < deadline:
            latest = self.delivery(delivery_id)
            if latest.get("state") in TERMINAL_DELIVERY_STATES:
                return latest
            time.sleep(0.25)
        fail(f"Orka delivery {delivery_id!r} did not become terminal; last={latest!r}")


def audit_after(state: dict[str, Any], sequence: int) -> list[dict[str, Any]]:
    return [entry for entry in state.get("audit", []) if int(entry.get("sequence", 0)) > sequence]


def max_audit_sequence(state: dict[str, Any]) -> int:
    return max((int(entry.get("sequence", 0)) for entry in state.get("audit", [])), default=0)


def run_scenario(
    fixture: Fixture,
    name: str,
    actions: list[dict[str, Any] | str],
    expected_state: str,
    expected_attempts: int,
    expected_actions: list[str],
    timeout: float = 120,
) -> dict[str, Any]:
    before = fixture.proxy_state()
    sequence = max_audit_sequence(before)
    fixture.set_plan(actions)
    _, _, external_id = fixture.inject(f"fault-validation:{name}:{time.time_ns()}")
    event = fixture.wait_event(external_id, timeout=timeout)
    delivery = fixture.wait_delivery(str(event["deliveryId"]), timeout=timeout)
    after = fixture.proxy_state()
    audit = audit_after(after, sequence)
    action_names = [str(entry.get("action")) for entry in audit]
    if delivery.get("state") != expected_state:
        fail(f"{name}: delivery state={delivery.get('state')!r}, want {expected_state!r}")
    if int(delivery.get("attemptCount", -1)) != expected_attempts:
        fail(f"{name}: attemptCount={delivery.get('attemptCount')!r}, want {expected_attempts}")
    if action_names != expected_actions:
        fail(f"{name}: proxy actions={action_names!r}, want {expected_actions!r}")
    result: dict[str, Any] = {
        "name": name,
        "eventId": event.get("id"),
        "taskName": event.get("taskName"),
        "deliveryId": delivery.get("id"),
        "state": delivery.get("state"),
        "attemptCount": delivery.get("attemptCount"),
        "proxyActions": action_names,
        "providerMessageIdPresent": bool(delivery.get("providerMessageId")),
    }
    if len(audit) >= 2:
        result["proxyAttemptSpacingSeconds"] = round(
            (parse_time(audit[1]["timestamp"]) - parse_time(audit[0]["timestamp"])).total_seconds(), 3
        )
    return result


def replay_delivery_after_restart(fixture: Fixture, delivered: dict[str, Any]) -> dict[str, Any]:
    before = fixture.proxy_state()
    sequence = max_audit_sequence(before)
    kubectl("-n", NAMESPACE, "rollout", "restart", "deployment/orka-gateway-telegram-shadow")
    kubectl(
        "-n", NAMESPACE, "rollout", "status", "deployment/orka-gateway-telegram-shadow",
        "--timeout=5m", timeout=330,
    )
    # Port-forward targets a Service and reconnects automatically only inconsistently
    # across replacement Pods, so replace it deterministically.
    fixture.adapter.close()
    fixture.adapter = PortForward(NAMESPACE, "svc/orka-gateway-telegram-shadow", 8080, "/readyz")
    fixture.adapter.start()

    request: dict[str, Any] = {
        "protocolVersion": "orka.gateway.v1",
        "deliveryId": delivered["id"],
        "idempotencyId": delivered["idempotencyId"],
        "originatingEventId": delivered["eventId"],
        "kind": delivered["kind"],
        "accountId": delivered["accountId"],
        "contextId": delivered["contextId"],
        "replyTarget": delivered["replyTarget"],
        "text": delivered["text"],
    }
    if delivered.get("threadId"):
        request["threadId"] = delivered["threadId"]
    if delivered.get("taskName"):
        request["taskRef"] = {"namespace": NAMESPACE, "name": delivered["taskName"]}
    if delivered.get("sessionName"):
        request["sessionRef"] = {"namespace": NAMESPACE, "name": delivered["sessionName"]}
    if delivered.get("metadata"):
        request["metadata"] = delivered["metadata"]
    status, response = json_request(
        "POST",
        fixture.adapter.base_url + "/v1/deliveries",
        body=request,
        headers={"Authorization": f"Bearer {fixture.outbound_token}"},
    )
    if status != 200 or not isinstance(response, dict) or response.get("status") != "delivered":
        fail(f"durable replay returned HTTP {status}: {response!r}")
    after = fixture.proxy_state()
    if audit_after(after, sequence):
        fail("durable delivery replay called the provider after adapter restart")
    if response.get("providerMessageId") != delivered.get("providerMessageId"):
        fail("durable delivery replay changed the provider message correlation")
    return {
        "status": response.get("status"),
        "providerMessageIdStable": True,
        "providerCallsAfterRestart": 0,
    }


def duplicate_webhook_after_restart(
    fixture: Fixture, update_id: int, payload: dict[str, Any], external_id: str, audit_sequence: int
) -> dict[str, Any]:
    before_events = [item for item in fixture.events() if item.get("externalEventId") == external_id]
    status, response = json_request(
        "POST",
        fixture.adapter.base_url + "/telegram/webhook",
        body=payload,
        headers={"X-Telegram-Bot-Api-Secret-Token": fixture.webhook_secret},
    )
    if status != 200:
        fail(f"duplicate webhook returned HTTP {status}: {response!r}")
    time.sleep(2)
    after_events = [item for item in fixture.events() if item.get("externalEventId") == external_id]
    provider_calls = len(audit_after(fixture.proxy_state(), audit_sequence))
    if len(before_events) != 1 or len(after_events) != 1 or provider_calls != 0:
        fail(
            f"duplicate webhook replay was not idempotent: before={len(before_events)} "
            f"after={len(after_events)} providerCalls={provider_calls} update={update_id}"
        )
    return {"orkaEvents": 1, "providerCalls": 0}


def main() -> int:
    if not CONTEXT:
        print("set KUBE_CONTEXT explicitly", file=sys.stderr)
        return 2
    if NAMESPACE == LIVE_NAMESPACE:
        print("isolated namespace guard failed", file=sys.stderr)
        return 2
    kubectl("cluster-info")
    for resource in [
        "deploy/telegram-fault-proxy",
        "deploy/orka-gateway-telegram-shadow",
        "deploy/telegram-fault-echo-runtime",
        "gateway/telegram-fault-validation",
        "gatewaybinding/telegram-fault-echo",
    ]:
        kubectl("-n", NAMESPACE, "get", resource)

    adapter = PortForward(NAMESPACE, "svc/orka-gateway-telegram-shadow", 8080, "/readyz")
    proxy = PortForward(NAMESPACE, "svc/telegram-fault-proxy", 8080, "/readyz")
    api = PortForward("orka-system", "svc/orka-api", 8080, "/readyz")
    forwards = [adapter, proxy, api]
    fixture: Fixture | None = None
    try:
        for forward in forwards:
            forward.start()
        fixture = Fixture(
            adapter=adapter,
            proxy=proxy,
            api=api,
            webhook_secret=secret_value("telegram-fault-adapter-secrets", "telegram-webhook-secret"),
            outbound_token=secret_value("telegram-fault-gateway-outbound", "token"),
            control_token=secret_value("telegram-fault-proxy-control", "control-token"),
            api_token=kubectl(
                "-n", "orka-system", "create", "token", "orka-controller-manager", "--duration=15m"
            ).strip(),
            next_update_id=(int(time.time() * 1000) % 1_500_000_000) + 100_000_000,
        )
        results: list[dict[str, Any]] = []
        success = run_scenario(fixture, "success", ["success"], "Delivered", 1, ["success"])
        results.append(success)
        results.append(
            run_scenario(
                fixture,
                "rate-limit-once",
                [{"action": "rate_limit", "retry_after": 5}, "success"],
                "Delivered",
                2,
                ["rate_limit", "success"],
            )
        )
        results.append(
            run_scenario(
                fixture,
                "server-error-once",
                ["server_error", "success"],
                "Delivered",
                2,
                ["server_error", "success"],
            )
        )
        for action in ["bad_request", "forbidden", "malformed_success", "mismatched_success", "drop_after_read"]:
            results.append(
                run_scenario(fixture, action, [action], "DeadLettered", 1, [action])
            )

        delivered = fixture.delivery(str(success["deliveryId"]))
        replay_result = replay_delivery_after_restart(fixture, delivered)
        forwards[0] = fixture.adapter
        update_id, payload, external_id = fixture.inject(f"fault-validation:duplicate:{time.time_ns()}")
        duplicate_event = fixture.wait_event(external_id)
        duplicate_delivery = fixture.wait_delivery(str(duplicate_event["deliveryId"]))
        if duplicate_delivery.get("state") != "Delivered":
            fail("duplicate-test seed delivery did not succeed")
        seed_sequence = max_audit_sequence(fixture.proxy_state())
        # Restart again so both inbound acknowledgment and provider correlation
        # must be recovered from the PVC-backed SQLite store.
        kubectl("-n", NAMESPACE, "rollout", "restart", "deployment/orka-gateway-telegram-shadow")
        kubectl(
            "-n", NAMESPACE, "rollout", "status", "deployment/orka-gateway-telegram-shadow",
            "--timeout=5m", timeout=330,
        )
        fixture.adapter.close()
        fixture.adapter = PortForward(NAMESPACE, "svc/orka-gateway-telegram-shadow", 8080, "/readyz")
        fixture.adapter.start()
        forwards[0] = fixture.adapter
        duplicate_result = duplicate_webhook_after_restart(
            fixture, update_id, payload, external_id, seed_sequence
        )
        rate_result = next(item for item in results if item["name"] == "rate-limit-once")
        spacing = float(rate_result.get("proxyAttemptSpacingSeconds", 0.0))
        summary = {
            "context": CONTEXT,
            "namespace": NAMESPACE,
            "realTelegramContacted": False,
            "scenarios": results,
            "restartDeliveryReplay": replay_result,
            "restartWebhookReplay": duplicate_result,
            "finding": {
                "telegramRetryAfterSeconds": 5,
                "observedRetrySpacingSeconds": spacing,
                "retryAfterHonored": spacing >= 5.0,
            },
        }
        print(json.dumps(summary, indent=2))
        return 0
    finally:
        current_adapter = fixture.adapter if fixture is not None else None
        if current_adapter is not None:
            current_adapter.close()
        for forward in reversed(forwards):
            if forward is not current_adapter:
                forward.close()


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as error:
        print(f"fault validation failed: {error}", file=sys.stderr)
        raise SystemExit(1)
