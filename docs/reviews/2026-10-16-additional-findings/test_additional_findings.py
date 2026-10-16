"""Historical reproduction entry point, now running the fixed-behavior regressions."""
from pathlib import Path
import pytest

if __name__ == "__main__":
    root = Path(__file__).resolve().parents[3]
    raise SystemExit(pytest.main([str(root / "python/evalsi-harness/tests/test_review_regressions.py"), "-q"]))
