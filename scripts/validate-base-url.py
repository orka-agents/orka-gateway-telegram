#!/usr/bin/env python3
"""Validate a base URL supplied through an environment variable."""

import ipaddress
import os
import re
import sys
from urllib.parse import urlsplit


def valid_base_url(value, schemes):
    if not value.isascii() or re.search(r"[\x00-\x20\x7f?#]", value):
        return False
    if not any(value.startswith(scheme + "://") for scheme in schemes):
        return False
    try:
        parsed = urlsplit(value)
        host = parsed.hostname
        port = parsed.port
        if not host or parsed.username is not None or parsed.path not in ("", "/"):
            return False
        if parsed.netloc.endswith(":") or (port is not None and not 1 <= port <= 65535):
            return False
        if parsed.netloc.startswith("["):
            if not re.fullmatch(r"\[[0-9a-fA-F:.]+\](?::[0-9]+)?", parsed.netloc):
                return False
            ipaddress.IPv6Address(host)
        elif "." in host and re.fullmatch(r"[0-9.]+", host):
            ipaddress.IPv4Address(host)
        elif len(host) > 253 or any(
            not re.fullmatch(r"[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?", label)
            for label in host.split(".")
        ):
            return False
    except ValueError:
        return False
    return True


if __name__ == "__main__":
    if len(sys.argv) < 3:
        sys.exit("Usage: validate-base-url.py ENVIRONMENT_VARIABLE SCHEME [SCHEME ...]")
    sys.exit(0 if valid_base_url(os.environ.get(sys.argv[1], ""), sys.argv[2:]) else 2)
