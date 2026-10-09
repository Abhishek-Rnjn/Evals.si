"""Verify the signature of a webhook delivery from an Evals.si server.

A delivery carries ``Evalsi-Signature: t=<unix seconds>,v1=<hex>``, where the
hex is HMAC-SHA256 of ``"<t>.<raw body>"`` under the webhook's secret. Verify
against the raw request body, before parsing it::

    from evalsi.webhooks import verify_signature

    verify_signature(secret, request.headers["Evalsi-Signature"], request.body)
"""

from __future__ import annotations

import hashlib
import hmac
import time

DEFAULT_TOLERANCE_S = 300.0


class SignatureError(ValueError):
    """The delivery is not authentic, or is too old."""


def sign(secret: str, body: bytes, *, timestamp: float | None = None) -> str:
    """The ``Evalsi-Signature`` value for a body (what the server sends)."""
    t = int(time.time() if timestamp is None else timestamp)
    digest = hmac.new(secret.encode(), f"{t}.".encode() + body, hashlib.sha256).hexdigest()
    return f"t={t},v1={digest}"


def verify_signature(
    secret: str,
    header: str,
    body: bytes,
    *,
    tolerance_s: float | None = DEFAULT_TOLERANCE_S,
    now: float | None = None,
) -> None:
    """Raise :class:`SignatureError` unless ``header`` signs ``body`` under ``secret``.

    A timestamp further than ``tolerance_s`` from now is refused, which stops
    a captured delivery from being replayed later; pass ``None`` to skip that.
    """
    fields = dict(part.strip().split("=", 1) for part in header.split(",") if "=" in part)
    try:
        t = int(fields["t"])
        given = fields["v1"]
    except (KeyError, ValueError):
        raise SignatureError("malformed Evalsi-Signature header") from None
    if tolerance_s is not None and abs((time.time() if now is None else now) - t) > tolerance_s:
        raise SignatureError("the timestamp is outside the tolerance")
    want = sign(secret, body, timestamp=t).split("v1=", 1)[1]
    if not hmac.compare_digest(want, given):
        raise SignatureError("signature mismatch")
