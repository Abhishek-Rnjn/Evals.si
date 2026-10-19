"""Create self-contained offline starter projects, without modifying existing directories."""

from __future__ import annotations

import argparse
import copy
import json
import shlex
from pathlib import Path
from typing import Any

import yaml


def add_init_command(sub: Any) -> None:
    p = sub.add_parser("init", help="create a runnable offline evaluation starter")
    p.add_argument("directory", nargs="?", default="evals", help="new directory (default: evals)")
    p.add_argument("--template", choices=["recorded", "agent"], default="recorded")
    p.set_defaults(func=_cmd_init)


def starter_files(template: str) -> dict[str, str]:
    agent = template == "agent"
    records = [
        {"id": "france", "input": "Capital of France?", "output": "Paris", "reference": "Paris"},
        {"id": "japan", "input": "Capital of Japan?", "output": "Tokyo", "reference": "Tokyo"},
    ]
    spec: dict[str, Any] = {
        "dataset": {"path": "data.jsonl"},
        "evaluators": [{"ref": "exact-match"}],
        "gates": [
            {"metric": "exact-match", "min": 1.0},
            {"metric": "exact-match", "stat": "GATE_STAT_ERROR_RATE", "max": 0.0},
        ],
    }
    if agent:
        records = [{"id": "greeting", "input": "Write hello into answer.txt."}]
        spec = {
            "dataset": {"path": "data.jsonl"},
            "target": {"agent": {"cli": {"command": ["python3", "agent.py"]}}},
            "harness": {},
            "environment": {
                "files": {
                    "agent.py": (
                        "from pathlib import Path\nPath('answer.txt').write_text('hello\\n')\n"
                    )
                },
                "sandbox": {"min_isolation": "namespaced", "network": "deny"},
                "checker": {"command": ["sh", "-c", 'test "$(cat answer.txt)" = hello']},
            },
            "evaluators": [{"ref": "task-success"}],
            "gates": [
                {"metric": "task-success", "min": 1.0},
                {"metric": "task-success", "stat": "GATE_STAT_ERROR_RATE", "max": 0.0},
            ],
        }
    doc = {
        "apiVersion": "evals.si/v1alpha1",
        "kind": "EvalRun",
        "metadata": {"name": "starter-agent" if agent else "starter-recorded"},
        "spec": spec,
    }
    broken = copy.deepcopy(doc)
    negative_records = copy.deepcopy(records)
    if agent:
        broken["spec"]["environment"]["files"]["agent.py"] = "print('No file was written.')\n"
    else:
        broken["spec"]["dataset"]["path"] = "regressed.jsonl"
        negative_records[1]["output"] = "Kyoto"
    diagnose = "evalsi doctor --require agent" if agent else "evalsi doctor"
    extra = (
        "This template runs a synthetic CLI agent in a real namespaced Linux sandbox. "
        "Install the matching evalsi-harness package. Use a supported Linux host/container "
        "on macOS/Windows. Replace target.agent.cli and environment.files in run.yaml "
        "with your own agent and task setup. The checker verifies the final file state."
        if agent
        else "This template scores synthetic recorded outputs: "
        "no server, sandbox, model or API key "
        "is needed. Replace data.jsonl with approved records from your own application."
    )
    readme = f"""# Evals.si starter

{extra}

```bash
{diagnose}
evalsi run -f run.yaml --output results.json
evalsi report results.json -o report.html
```

The working example exits 0. Verify the negative control separately:

```bash
evalsi run -f regressed.yaml --output results-regressed.json
```

That command must exit **3** (a deliberately failed gate). Exit 1 means an
execution/configuration error, not an expected regression. Both specs also gate
evaluation errors. Never substitute the negative-control spec for the CI spec.

The cases are synthetic demonstrations, not evidence about your own agent.
Review the expected behavior, add representative cases and choose appropriate
thresholds before enforcing a gate. Raw results/reports can contain private data;
they are ignored by this starter's .gitignore.

For CI integration and baseline comparisons, see
https://evalsi.dev/docs/guides/gate-an-agent-in-ci/.
"""

    def encode(rows: list[dict[str, str]]) -> str:
        return "".join(json.dumps(r) + "\n" for r in rows)

    files = {
        "run.yaml": yaml.safe_dump(doc, sort_keys=False),
        "regressed.yaml": yaml.safe_dump(broken, sort_keys=False),
        "data.jsonl": encode(records),
        "README.md": readme,
        ".gitignore": "results*.json\nreport*.html\nreport*.md\n.evalsi*/\n",
    }
    if not agent:
        files["regressed.jsonl"] = encode(negative_records)
    return files


def _cmd_init(args: argparse.Namespace) -> int:
    directory = Path(args.directory).expanduser()
    files = starter_files(args.template)
    try:
        # An existing directory (including an empty one or symlink) is never modified.
        directory.mkdir(parents=True, exist_ok=False)
        for name, content in files.items():
            with (directory / name).open("x", encoding="utf-8") as f:
                f.write(content)
    except OSError as exc:
        raise ValueError(f"cannot create starter: {exc}; choose a new directory") from exc
    print(f"Created {args.template} starter in {directory}")
    print(f"  cd {shlex.quote(str(directory))}")
    print("  evalsi doctor" + (" --require agent" if args.template == "agent" else ""))
    print("  evalsi run -f run.yaml --output results.json")
    print("  evalsi report results.json -o report.html")
    return 0
