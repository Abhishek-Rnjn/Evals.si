#!/bin/sh
python - <<'PYTHON'
import socket
with socket.create_connection(("db", 6379), timeout=2) as s:
    s.sendall(b"SET answer 42\r\n")
    assert s.recv(1024) == b"+OK\r\n"
print("Set the answer to 42")

PYTHON
