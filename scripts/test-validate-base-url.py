#!/usr/bin/env python3
"""Exercise offline URL validation and live DNS pinning without network access."""

import contextlib
import io
import os
from pathlib import Path
import runpy
import socket
import sys
import unittest
from unittest import mock


VALIDATOR = Path(__file__).with_name("validate-base-url.py")


def validate(value, *flags):
    output = io.StringIO()
    with (
        mock.patch.dict(os.environ, {"ADAPTER_URL": value}),
        mock.patch.object(sys, "argv", [str(VALIDATOR), "ADAPTER_URL", "https", *flags]),
        contextlib.redirect_stdout(output),
    ):
        try:
            runpy.run_path(str(VALIDATOR), run_name="__main__")
        except SystemExit as error:
            return error.code, output.getvalue()
    return 0, output.getvalue()


def dns_answers(*addresses):
    return [
        (socket.AF_INET6 if ":" in address else socket.AF_INET, socket.SOCK_STREAM,
         socket.IPPROTO_TCP, "", (address, 443))
        for address in addresses
    ]


class PublicEndpointTests(unittest.TestCase):
    def test_rendering_stays_offline(self):
        with mock.patch("socket.getaddrinfo", side_effect=AssertionError("DNS during rendering")):
            self.assertEqual(validate("https://adapter.example.com", "--public"), (0, ""))

    def test_public_dns_answers_are_pinned_once(self):
        for authority, port in (("adapter.example.com", 443), ("adapter.example.com:8443", 8443)):
            with self.subTest(authority=authority), mock.patch(
                "socket.getaddrinfo",
                return_value=dns_answers("8.8.8.8", "2606:4700:4700::1111", "8.8.8.8"),
            ) as resolver:
                self.assertEqual(
                    validate(f"https://{authority}/", "--curl-resolve"),
                    (0, f"adapter.example.com:{port}:8.8.8.8,[2606:4700:4700::1111]\n"),
                )
                resolver.assert_called_once_with("adapter.example.com", port, type=socket.SOCK_STREAM)

    def test_any_restricted_dns_answer_rejects_the_whole_request(self):
        for restricted in ("127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "fd00::1", "::ffff:127.0.0.1"):
            for addresses in ((restricted, "8.8.8.8"), ("8.8.8.8", restricted)):
                with self.subTest(addresses=addresses), mock.patch(
                    "socket.getaddrinfo", return_value=dns_answers(*addresses),
                ):
                    self.assertEqual(validate("https://adapter.example.com", "--curl-resolve"), (2, ""))

    def test_failed_or_empty_dns_rejects_the_request(self):
        with mock.patch("socket.getaddrinfo", side_effect=socket.gaierror("no such host")):
            self.assertEqual(validate("https://adapter.example.com", "--curl-resolve"), (2, ""))
        with mock.patch("socket.getaddrinfo", return_value=[]):
            self.assertEqual(validate("https://adapter.example.com", "--curl-resolve"), (2, ""))

    def test_public_literals_need_no_dns_override(self):
        with mock.patch("socket.getaddrinfo", side_effect=AssertionError("DNS for an IP literal")):
            for host in ("8.8.8.8", "[2606:4700:4700::1111]", "[::ffff:8.8.8.8]"):
                with self.subTest(host=host):
                    self.assertEqual(validate(f"https://{host}", "--curl-resolve"), (0, ""))

    def test_live_resolution_requires_a_valid_public_url(self):
        with mock.patch("socket.getaddrinfo", side_effect=AssertionError("DNS for an invalid URL")):
            for value in ("https://127.0.0.1", "https://2130706433", "https://[::1]", "https://localhost", "https://user@example.com", "http://example.com"):
                with self.subTest(value=value):
                    self.assertEqual(validate(value, "--curl-resolve"), (2, ""))


if __name__ == "__main__":
    unittest.main()
