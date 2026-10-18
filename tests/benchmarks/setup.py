"""Create isolated, version-locked benchmark environments; write interpreter mapping."""

from __future__ import annotations

import argparse
import json
import subprocess
from pathlib import Path

from evaluation import ROOT, TOOLS


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--python", default="3.12")
    args = parser.parse_args()
    directory = args.directory.resolve()
    directory.mkdir(parents=True, exist_ok=True)
    subprocess.run(
        ["uv", "sync", "--locked", "--all-packages", "--python", args.python],
        cwd=ROOT / "python",
        check=True,
    )
    mapping = {"evalsi": str(ROOT / "python/.venv/bin/python")}
    for tool in TOOLS[1:]:
        environment = directory / tool
        subprocess.run(
            ["uv", "venv", "--python", args.python, str(environment)], check=True
        )
        interpreter = environment / "bin/python"
        subprocess.run(
            [
                "uv",
                "pip",
                "sync",
                "--python",
                str(interpreter),
                str(Path(__file__).parent / "requirements" / f"{tool}.txt"),
            ],
            check=True,
        )
        mapping[tool] = str(interpreter)
    output = directory / "interpreters.json"
    output.write_text(json.dumps(mapping, indent=2) + "\n")
    print(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
