#!/usr/bin/env python3
"""Validate a base URL supplied through an environment variable."""

import argparse
import ipaddress
import os
import re
import sys
from urllib.parse import urlsplit


# Match Orka's internal/gateway/endpoint.go direct-endpoint policy. These
# explicit ranges also cover non-public unicast classes; Python's is_global
# uses a different, version-dependent classification.
RESTRICTED_GATEWAY_NETWORKS = tuple(
    ipaddress.ip_network(prefix)
    for prefix in (
        "0.0.0.0/8",
        "10.0.0.0/8",
        "100.64.0.0/10",
        "127.0.0.0/8",
        "169.254.0.0/16",
        "172.16.0.0/12",
        "192.0.0.0/24",
        "192.0.2.0/24",
        "192.168.0.0/16",
        "198.18.0.0/15",
        "198.51.100.0/24",
        "203.0.113.0/24",
        "224.0.0.0/4",
        "240.0.0.0/4",
        "::/128",
        "::1/128",
        "64:ff9b::/96",
        "64:ff9b:1::/48",
        "100::/64",
        "2001::/23",
        "2002::/16",
        "3fff::/20",
        "5f00::/16",
        "fc00::/7",
        "fe80::/10",
        "fec0::/10",
        "ff00::/8",
        "2001:db8::/32",
    )
)


def public_gateway_host(host):
    host = host.lower()
    if (
        host in ("localhost", "svc")
        or host.endswith((".localhost", ".local", ".svc"))
        or ".svc." in host
    ):
        return False
    try:
        address = ipaddress.ip_address(host)
    except ValueError:
        # Keep rendering offline. Orka checks every resolved DNS address when
        # connecting to the endpoint.
        return True
    if isinstance(address, ipaddress.IPv6Address) and address.ipv4_mapped:
        address = address.ipv4_mapped
    return not any(address in network for network in RESTRICTED_GATEWAY_NETWORKS)


def valid_base_url(value, schemes, require_public=False):
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
        elif all(
            re.fullmatch(r"(?:[0-9]+|0[xX][0-9a-fA-F]+)", label)
            for label in host.split(".")
        ):
            # Curl accepts legacy integer, octal, and hex IPv4 spellings.
            # Require canonical dotted decimal before checking public ranges.
            ipaddress.IPv4Address(host)
        elif len(host) > 253 or any(
            not re.fullmatch(r"[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?", label)
            for label in host.split(".")
        ):
            return False
    except ValueError:
        return False
    return not require_public or public_gateway_host(host)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("environment_variable")
    parser.add_argument("schemes", nargs="+")
    parser.add_argument("--public", action="store_true", help="require an Orka direct endpoint host")
    args = parser.parse_args()
    sys.exit(
        0
        if valid_base_url(os.environ.get(args.environment_variable, ""), args.schemes, args.public)
        else 2
    )
