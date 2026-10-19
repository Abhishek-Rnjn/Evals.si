"""scripts/probe-cli-proxy.py: report which hosts a command sends through the proxy
variables the sandbox sets."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[3] / "scripts" / "probe-cli-proxy.py"


def probe(code: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, str(SCRIPT), "--seconds", "20", "--", sys.executable, "-c", code],
        capture_output=True,
        text=True,
        timeout=60,
        check=False,
    )


def test_a_command_that_honours_the_proxy_variables_is_seen() -> None:
    # urllib routes https:// through https_proxy, so it asks the proxy to CONNECT.
    done = probe(
        "import urllib.request\n"
        "try:\n    urllib.request.urlopen('https://api.example.invalid/v1', timeout=5)\n"
        "except Exception:\n    pass\n"
    )
    assert done.returncode == 0, done.stderr
    assert "CONNECT api.example.invalid:443" in done.stdout


def test_a_command_that_never_touches_the_network_is_reported_as_not_using_it() -> None:
    done = probe("pass")
    assert done.returncode == 1
    assert "did not use the proxy variables" in done.stdout


def test_a_command_that_ignores_the_variables_is_not_credited() -> None:
    """A direct connection never reaches the proxy, so it is not reported as proxied."""
    done = probe(
        "import socket\n"
        "s = socket.socket()\ns.settimeout(1)\n"
        "try:\n    s.connect(('127.0.0.1', 9))\nexcept OSError:\n    pass\n"
    )
    assert done.returncode == 1
