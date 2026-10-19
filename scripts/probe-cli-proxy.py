#!/usr/bin/env python3
"""Which hosts does a CLI try to reach through the proxy variables the sandbox sets?

The sandbox puts a logging egress proxy in HTTP_PROXY, HTTPS_PROXY, ALL_PROXY and their
lower-case forms, and a localhost-only NO_PROXY (internal/sandbox/host.go). A CLI agent
runs inside the allowlist only if it honours them. This runs a command with those
variables pointed at a local listener that logs each request line and refuses it, then
prints the hosts. A ``CONNECT api.example.com:443`` line means the command routed that
connection through the variable; a command that talks to the network without ever
showing up here goes around the proxy.

    scripts/probe-cli-proxy.py [--seconds 30] -- codex exec "hi" --skip-git-repo-check

Run it from a scratch directory with a throwaway HOME and a fake API key: the command
is run as given. Exit status is 0 when at least one request was seen, 1 when none was.
"""

from __future__ import annotations

import argparse
import os
import socket
import subprocess
import sys
import threading
import time
from collections import Counter

VARS = ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy")
NO_PROXY = "localhost,127.0.0.1,::1"


def listen(requests: list[str], stop: threading.Event) -> tuple[socket.socket, int]:
    server = socket.socket()
    server.bind(("127.0.0.1", 0))
    server.listen(64)
    server.settimeout(0.2)

    def handle(conn: socket.socket) -> None:
        try:
            conn.settimeout(3)
            first = conn.recv(4096).decode("latin1", "replace").split("\r\n", 1)[0]
            if first:
                requests.append(first)
            conn.sendall(
                b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
            )
        except OSError:
            pass
        finally:
            conn.close()

    def serve() -> None:
        while not stop.is_set():
            try:
                conn, _ = server.accept()
            except TimeoutError:
                continue
            threading.Thread(target=handle, args=(conn,), daemon=True).start()

    threading.Thread(target=serve, daemon=True).start()
    return server, server.getsockname()[1]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--seconds", type=float, default=30, help="stop the command after this")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not command:
        parser.error("give a command after --")
    requests: list[str] = []
    stop = threading.Event()
    server, port = listen(requests, stop)
    env = dict(os.environ)
    for name in VARS:
        env[name] = f"http://127.0.0.1:{port}"
    env["NO_PROXY"] = env["no_proxy"] = NO_PROXY
    deadline = time.time() + args.seconds
    try:
        process = subprocess.Popen(
            command, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
        )
        while process.poll() is None and time.time() < deadline:
            time.sleep(0.1)
        if process.poll() is None:
            process.kill()
        process.wait()
    finally:
        time.sleep(0.3)
        stop.set()
        server.close()
    seen = Counter(requests)
    for line, count in sorted(seen.items()):
        print(f"{count:4d}  {line}")
    if not seen:
        print("no request reached the proxy: the command did not use the proxy variables")
    return 0 if seen else 1


if __name__ == "__main__":
    sys.exit(main())
