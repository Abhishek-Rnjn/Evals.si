#!/usr/bin/env python3
"""Install built wheels in a fresh environment and exercise the installed workflow.

No source imports, model calls or publication. --require agent also exercises
the real namespaced sandbox and must run on a host that supports it.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.metadata
import json
import os
import platform
import shutil
import subprocess
import sys
import tempfile
import time
import venv
from pathlib import Path

from release_version import metadata


def run(command: list[str], *, expected: int = 0, env: dict[str, str] | None = None) -> str:
    result = subprocess.run(
        command, text=True, encoding="utf-8", capture_output=True, timeout=180, env=env, check=False
    )
    if result.returncode != expected:
        raise RuntimeError(
            f"{command[:3]}: expected exit {expected}, got {result.returncode}\n"
            f"{result.stdout[-2000:]}\n{result.stderr[-2000:]}"
        )
    return result.stdout


def cli(*args: str, expected: int = 0) -> str:
    return run([sys.executable, "-m", "evalsi", *args], expected=expected)


def installed(args: argparse.Namespace) -> dict[str, object]:
    import evalsi

    release = metadata(args.version, "abhishek-rnjn")
    version = release["python_version"]
    if not Path(evalsi.__file__).resolve().is_relative_to(Path(sys.prefix).resolve()):
        raise RuntimeError("evalsi was imported from outside the isolated installation")
    if evalsi.__version__ != version or importlib.metadata.version("evalsi") != version:
        raise RuntimeError("installed SDK does not match the requested release")
    if args.require != "sdk":
        import evalsi_harness
        from evalsi._binaries import bundled_evalsid, evalsid_path

        if (
            evalsi_harness.__version__ != version
            or importlib.metadata.version("evalsi-harness") != version
            or not Path(evalsi_harness.__file__)
            .resolve()
            .is_relative_to(Path(sys.prefix).resolve())
        ):
            raise RuntimeError("installed harness does not match the requested release")
        if not bundled_evalsid() or evalsid_path() != bundled_evalsid():
            raise RuntimeError("server must come from the installed platform wheel")
    run([sys.executable, "-m", "pip", "check"])
    doctor = json.loads(cli("doctor", "--require", args.require, "--format", "json"))
    if doctor["ready"] is not True:
        raise RuntimeError("installed release failed its capability check")
    cli("init", "recorded")
    os.chdir("recorded")
    cli("run", "-f", "run.yaml", "--output", "results.json")
    cli("run", "-f", "regressed.yaml", expected=3)
    cli("run", "-f", "missing.yaml", expected=1)
    cli("report", "results.json", "-o", "report.html")
    if not Path("report.html").is_file():
        raise RuntimeError("installed CLI did not create its HTML report")
    action_codes = {}
    if args.action_adapter:
        for name, spec, expected in (
            ("passing", "run.yaml", 0),
            ("regression", "regressed.yaml", 3),
            ("configuration_error", "missing.yaml", 1),
        ):
            output = Path.cwd() / f"action-{name}.txt"
            environment = {
                k: v for k, v in os.environ.items() if not k.startswith("EVALSI_ACTION_")
            }
            environment.update(
                RUNNER_TEMP=str(Path.cwd()),
                GITHUB_WORKSPACE=str(Path.cwd()),
                EVALSI_ACTION_DIRECTORY=".",
                EVALSI_ACTION_SPEC=spec,
                GITHUB_STEP_SUMMARY=str(Path.cwd() / f"summary-{name}.md"),
                GITHUB_OUTPUT=str(output),
            )
            run([sys.executable, str(args.action_adapter)], env=environment)
            values = dict(line.split("=", 1) for line in output.read_text().splitlines())
            if values["exit-code"] != str(expected):
                raise RuntimeError(f"Action {name}: expected exit-code={expected}")
            action_codes[name] = expected
    if args.require == "agent":
        os.chdir("..")
        cli("init", "agent", "--template", "agent")
        os.chdir("agent")
        cli("run", "-f", "run.yaml")
        cli("run", "-f", "regressed.yaml", expected=3)
    return {
        "version": args.version,
        "python_version": version,
        "python_runtime": platform.python_version(),
        "installed_distributions": {
            distribution.metadata["Name"]: distribution.version
            for distribution in importlib.metadata.distributions()
            if distribution.metadata["Name"]
        },
        "platform": f"{platform.system()} {platform.machine()}",
        "required_capability": args.require,
        "doctor": doctor,
        "recorded_exit_codes": {"passing": 0, "regression": 3, "configuration_error": 1},
        "html_report": True,
        "action_exit_codes": action_codes,
        "sandbox_controls": args.require == "agent",
    }


def bootstrap(args: argparse.Namespace) -> dict[str, object]:
    release = metadata(args.version, "abhishek-rnjn")
    version = release["python_version"]
    tag = "any"
    if args.require != "sdk":
        machine = platform.machine().lower()
        architectures = {
            "x86_64": "x86_64",
            "amd64": "x86_64",
            "aarch64": "aarch64",
            "arm64": "aarch64",
        }
        if sys.platform != "linux" or machine not in architectures:
            raise ValueError("server/agent checks require Linux amd64 or arm64")
        tag = f"manylinux_2_17_{architectures[machine]}"
    sdk = args.wheels / f"evalsi-{version}-py3-none-{tag}.whl"
    wheels = [sdk]
    if args.require != "sdk":
        wheels.append(args.wheels / f"evalsi_harness-{version}-py3-none-any.whl")
    if not all(wheel.is_file() for wheel in wheels):
        raise ValueError(f"missing expected {version} wheels in {args.wheels}")
    with tempfile.TemporaryDirectory(prefix="evalsi-release-") as temporary:
        root = Path(temporary)
        environment_dir = root / "venv"
        venv.EnvBuilder(with_pip=True).create(environment_dir)
        binary_dir = environment_dir / ("Scripts" if os.name == "nt" else "bin")
        python = binary_dir / ("python.exe" if os.name == "nt" else "python")
        requirements = [str(sdk) + ("[server]" if args.require != "sdk" else "")]
        requirements += [str(wheel) for wheel in wheels[1:]]
        environment = os.environ.copy()
        environment.pop("PYTHONPATH", None)
        environment.pop("EVALSID", None)
        environment["PYTHONNOUSERSITE"] = "1"
        environment["PYTHONUTF8"] = "1"
        environment["PATH"] = str(binary_dir) + os.pathsep + environment.get("PATH", "")
        run(
            [
                str(python),
                "-m",
                "pip",
                "install",
                "--disable-pip-version-check",
                "-q",
                *requirements,
            ],
            env=environment,
        )
        # Only the verifier and Action adapter are copied. Application code comes
        # from the installed wheels, with neither checkout nor PYTHONPATH in use.
        for name in ("smoke_release.py", "release_version.py"):
            shutil.copyfile(Path(__file__).parent / name, root / name)
        command = [
            str(python),
            str(root / "smoke_release.py"),
            args.version,
            "--installed",
            "--require",
            args.require,
            "--report",
            str(root / "report.json"),
        ]
        if args.action_adapter:
            shutil.copyfile(args.action_adapter, root / "action.py")
            command += ["--action-adapter", str(root / "action.py")]
        subprocess.run(command, cwd=root, env=environment, check=True, timeout=300)
        result: dict[str, object] = json.loads((root / "report.json").read_text())
    result["wheels"] = {
        wheel.name: hashlib.sha256(wheel.read_bytes()).hexdigest() for wheel in wheels
    }
    return result


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version")
    parser.add_argument("--wheels", type=Path)
    parser.add_argument("--require", choices=("sdk", "server", "agent"), default="sdk")
    parser.add_argument("--action-adapter", type=Path)
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--installed", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    args.report = args.report.resolve()
    if args.wheels:
        args.wheels = args.wheels.resolve()
    if args.action_adapter:
        args.action_adapter = args.action_adapter.resolve()
    if not args.installed and not args.wheels:
        parser.error("--wheels is required")
    started = time.monotonic()
    result = installed(args) if args.installed else bootstrap(args)
    result["elapsed_s"] = round(time.monotonic() - started, 2)
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(json.dumps(result, indent=2) + "\n")
    print(f"Release smoke passed: {args.version}, {args.require}, {result['elapsed_s']}s")


if __name__ == "__main__":
    main()
