"""Compare the release identity in Go/Helm with its Python distribution spelling."""

import re


def python_version(version: str) -> str:
    # Preserve stable, development and already canonical Python versions exactly.
    # Do not remove separators generally: 1.2.3 and 12.3 are different versions.
    return re.sub(r"-rc\.?([0-9]+)$", r"rc\1", version)
