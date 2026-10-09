from __future__ import annotations

import pytest

from evalsi.webhooks import SignatureError, sign, verify_signature

SECRET = "whsec_0123456789abcdef"
BODY = b'{"type":"run.finished"}'
VECTOR = "600685486bcb4ae8c251487c8adcf7dfc5a0f46112e6dc82ae42273e5983d70f"


def test_signature_matches_the_servers_format() -> None:
    # The same vector internal/webhooks checks in Go.
    assert sign(SECRET, BODY, timestamp=1_700_000_000) == f"t=1700000000,v1={VECTOR}"


def test_verify_accepts_a_fresh_delivery() -> None:
    verify_signature(SECRET, sign(SECRET, BODY), BODY)


@pytest.mark.parametrize(
    ("secret", "body", "header"),
    [
        ("another-secret-0123456", BODY, None),
        (SECRET, b'{"type":"tampered"}', None),
        (SECRET, BODY, "v1=abc"),
        (SECRET, BODY, "t=nope,v1=abc"),
    ],
)
def test_verify_refuses(secret: str, body: bytes, header: str | None) -> None:
    header = header if header is not None else sign(SECRET, BODY)
    with pytest.raises(SignatureError):
        verify_signature(secret, header, body)


def test_verify_refuses_a_replay() -> None:
    old = sign(SECRET, BODY, timestamp=1_700_000_000)
    with pytest.raises(SignatureError, match="tolerance"):
        verify_signature(SECRET, old, BODY, now=1_700_000_000 + 3600)
    verify_signature(SECRET, old, BODY, tolerance_s=None)
