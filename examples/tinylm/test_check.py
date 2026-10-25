from __future__ import annotations

import os
import subprocess
from pathlib import Path


def test_a_failed_model_start_never_runs_the_matrix(tmp_path: Path) -> None:
    python = tmp_path / "python3"
    python.write_text("#!/bin/sh\necho MODEL_START_FAILED >&2\nexit 1\n")
    python.chmod(0o755)
    cli = tmp_path / "evalsi"
    marker = tmp_path / "cli-called"
    cli.write_text(f"#!/bin/sh\ntouch '{marker}'\nexit 0\n")
    cli.chmod(0o755)
    result = subprocess.run(
        ["bash", str(Path(__file__).with_name("check.sh"))],
        env={**os.environ, "PATH": f"{tmp_path}:{os.environ['PATH']}", "EVALSI": str(cli)},
        capture_output=True,
        text=True,
        timeout=15,
        check=False,
    )
    assert result.returncode == 1
    assert "MODEL_START_FAILED" in result.stderr
    assert not marker.exists()
