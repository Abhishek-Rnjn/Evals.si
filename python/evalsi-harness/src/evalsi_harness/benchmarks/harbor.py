"""Harbor and Terminal-Bench task directories as Evals.si agent tasks.

Two importers, both reading the task format directly (no Docker, no Harbor
install):

``harbor://<dir>``
    Harbor tasks (Terminal-Bench 2 and the many datasets Harbor adapts):
    ``task.toml``, ``instruction.md``, ``environment/`` (a ``Dockerfile`` or
    ``[environment].docker_image``), ``tests/test.sh`` and optionally
    ``solution/solve.sh``. The verifier runs ``/tests/test.sh`` after the
    agent and the reward is ``/logs/verifier/reward.json`` (its ``reward``
    key, or the mean of its values) or ``/logs/verifier/reward.txt``, as in
    Harbor; a reward of 1 passes.

``terminal-bench://<dir>``
    Terminal-Bench 1 tasks: ``task.yaml``, ``Dockerfile``, ``run-tests.sh``
    and ``tests/``. The tests run as ``bash /tests/run-tests.sh`` with
    ``TEST_DIR=/tests`` and are graded with Terminal-Bench's pytest rule:
    resolved when every test in the short summary passed.

``<dir>`` is one task, or a dataset directory whose subdirectories are tasks.

The environment is the task's image. A Dockerfile is translated rather than
built: ``FROM`` is the image, ``RUN`` steps become the environment's setup
(run once and snapshotted), ``COPY``/``ADD`` of local files become files in
the image root, and ``WORKDIR``, ``ENV``, ``ARG`` and ``SHELL`` apply as
Docker would. Multi-stage builds, remote ``ADD`` and instructions that need
a Docker daemon are refused with a clear error; ``USER``, ``CMD``,
``ENTRYPOINT``, ``EXPOSE`` and the like are ignored. Copied files exist
before the first ``RUN`` (Docker interleaves them); tasks that depend on the
interleaving should use a prebuilt image (``image=`` or ``docker_image``).

Options (query string): ``tasks`` (comma-separated names), ``image``
(override), ``workdir`` (default: the task's, the Dockerfile's last
``WORKDIR``, else /app), ``network`` (``allow`` or ``deny``; default: the
task's, ``allow`` unless it says no-network), ``git=true`` (makes the workdir a git
repository whose one commit is the task as it starts, so the agent's changes are recorded
as a diff that ``trial-tampering`` can read; needs git in the image, and does nothing in a
workdir that already is a repository), ``oracle=true`` (adds the
reference solution under /solution; an agent that runs
``bash /solution/solve.sh``, or ``solution.sh`` for Terminal-Bench 1,
validates the setup), ``reward`` (``harbor``, the default, or ``score.json``).

``reward=score.json`` reads ``/logs/verifier/score.json`` first (then the
Harbor reward files) and scores it the way Harness²'s WorkBuddy-Bench adapter
does: the first usable of ``reward``, ``overall``, ``test_pass_rate`` (each
within 0..1; an out-of-range value is skipped, not clamped), then
``tests_passed / tests_total``. A file with none of them is a checker error,
never a zero. That path and the field set come from reading Harness²'s
adapter, not from a recorded WorkBuddy-Bench verifier run.
"""

from __future__ import annotations

import fnmatch
import json
import math
import re
import shlex
import tomllib
from pathlib import Path
from typing import Any

from evalsi.types import TaskCheck
from evalsi_harness.benchmarks.compose import load_compose
from evalsi_harness.benchmarks.dockerfile import (
    Build,
    TaskFormatError,
    copy_config,
    decode_command,
    stage_config,
    translate_dockerfile,
)
from evalsi_harness.benchmarks.dockerfile import put_file as _put

LOGS = "/logs/verifier"
MARK = "@@evalsi"
DEFAULT_WORKDIR = "/app"


def _text(path: Path) -> str:
    try:
        return path.read_text(encoding="utf-8")
    except UnicodeDecodeError:
        raise TaskFormatError(f"{path.name} must be a text file") from None


def _tree(directory: Path, into: str) -> tuple[dict[str, str], list[str]]:
    files: dict[str, str] = {}
    executable: list[str] = []
    for f in sorted(p for p in directory.rglob("*") if p.is_file()):
        dest = f"{into}/{f.relative_to(directory).as_posix()}"
        _put(files, f, dest)
        if f.stat().st_mode & 0o111:
            executable.append(dest)
    return files, executable


# --- tasks ---


def _task_dirs(path: str, marker: str, wanted: list[str]) -> list[Path]:
    root = Path(path)
    if not root.is_dir():
        raise TaskFormatError(f"{path} is not a directory")
    if (root / marker).is_file():
        dirs = [root]
    else:
        dirs = sorted(d for d in root.iterdir() if (d / marker).is_file())
    if not dirs:
        raise TaskFormatError(f"no {marker} in {path} or its subdirectories")
    if wanted:
        names = {d.name: d for d in dirs}
        missing = [w for w in wanted if not any(fnmatch.fnmatch(n, w) for n in names)]
        if missing:
            raise TaskFormatError(f"no tasks {missing} in {path}")
        dirs = [d for d in dirs if any(fnmatch.fnmatch(d.name, w) for w in wanted)]
    return dirs


def _seconds(value: Any) -> str | None:
    return f"{float(value):g}s" if value else None


def _service_lifetime(config: dict[str, Any]) -> str | None:
    """Services are stopped at this hard deadline, so it must outlast the
    agent's and the checker's limits, with room for setup and image pulls:
    at least an hour, at most the 24 hours services allow."""
    agent = float(config.get("max_agent_timeout_sec") or 0)
    test = float(config.get("max_test_timeout_sec") or 180)
    return _seconds(min(max(3600.0, agent + test + 1800), 86400.0))


# Makes the workdir a git repository whose single commit is the task as it starts, so the
# agent's changes show up as a diff (``trial-tampering`` and code review read it). Left alone
# when the workdir already is a repository. Needs git in the image.
GIT_BASELINE = (
    "git rev-parse --is-inside-work-tree >/dev/null 2>&1 || "
    "(git init -q && git config user.email evalsi@example.invalid && "
    "git config user.name evalsi && git add -A && git commit -q --allow-empty -m base)"
)


def _flag(value: str | None) -> bool:
    return (value or "").lower() in ("1", "true", "yes")


def _environment(
    *,
    build: Build | None,
    image: str,
    workdir: str,
    network: str,
    env: dict[str, str],
    files: dict[str, str],
    executable: list[str],
    checker: dict[str, Any],
    setup_timeout: Any,
    git: bool = False,
) -> dict[str, Any]:
    setup = list(build.setup) if build else []
    all_files = {**(build.files if build else {}), **files}
    exec_files = [*(build.executable if build else []), *executable]
    if exec_files:
        setup.insert(0, "chmod +x " + " ".join(shlex.quote(f) for f in exec_files))
    if decode := decode_command(all_files):
        setup.insert(0, decode)
    # Setup that needs the network (package installs) widens the setup network; the git
    # baseline does not, so it must not turn a restricted task's setup into one that does.
    needs_network = bool(setup)
    if git:
        setup.append(GIT_BASELINE)
    checker = {**checker, "command": _with_decode(checker["command"], checker["files"])}
    environment: dict[str, Any] = {
        "image": image,
        "files": all_files,
        "setup": setup,
        "env": {**(build.env if build else {}), **env},
        "sandbox": {"workdir": workdir, "network": network, "min_isolation": "namespaced"},
        "checker": checker,
        "command_timeout": "10m",
    }
    if needs_network:
        # Package installs in RUN steps need the network even when the
        # agent runs without it.
        environment["setup_network"] = "allow"
    if setup_timeout:
        environment["setup_timeout"] = _seconds(setup_timeout)
    if build and build.copies:
        # A multi-stage build: earlier stages are built on their own and the files taken out.
        environment["stages"] = [stage_config(st) for st in build.stages]
        environment["copy_from"] = [copy_config(c) for c in build.copies]
        # Their RUN steps (and the downloads in them) need the network, like the target's.
        environment["setup_network"] = "allow"
    return environment


def _with_decode(command: list[str], files: dict[str, str]) -> list[str]:
    decode = decode_command(files)
    if not decode:
        return command
    return [*command[:-1], f"{decode}\n{command[-1]}"]


def _network(option: str | None, task_mode: str) -> str:
    if option:
        if option not in ("allow", "deny"):
            raise TaskFormatError(f"network must be allow or deny, not {option!r}")
        return option
    return "deny" if task_mode in ("no-network", "none") else "allow"


def _chmod(paths: list[str]) -> str:
    return f"chmod +x {' '.join(shlex.quote(p) for p in paths)}\n" if paths else ""


def _checker_script(
    test: str, verifier_env: dict[str, str], extra: str = "", reward_files: tuple[str, ...] = ()
) -> str:
    names = reward_files or ("reward.json", "reward.txt")
    reads = "\n".join(
        f"{'if' if i == 0 else 'elif'} [ -f {LOGS}/{n} ]; "
        f"then printf '{MARK} {n}\\n'; cat {LOGS}/{n}"
        for i, n in enumerate(names)
    )
    exports = "".join(f"export {k}={shlex.quote(v)}\n" for k, v in verifier_env.items())
    return f"""mkdir -p {LOGS} /logs/agent /logs/artifacts
{exports}{extra}if command -v bash >/dev/null 2>&1; then run=bash; else run=sh; fi
$run {test} > {LOGS}/test-stdout.txt 2>&1
code=$?
tail -c 20000 {LOGS}/test-stdout.txt
printf '\\n{MARK} exit %s\\n' "$code"
{reads}
fi
"""


def load_harbor(path: str, **options: str) -> list[dict[str, Any]]:
    """Records for ``harbor://<task or dataset dir>?options``."""
    rows = []
    wanted = [t for t in options.get("tasks", "").split(",") if t]
    for d in _task_dirs(path, "task.toml", wanted):
        try:
            rows.append(_harbor_task(d, options))
        except TaskFormatError as exc:
            raise TaskFormatError(f"{d.name}: {exc}") from None
    return rows


def _reward_files(option: str | None) -> tuple[str, ...]:
    if not option or option == "harbor":
        return ()
    if option == "score.json":
        return ("score.json", "reward.json", "reward.txt")
    raise TaskFormatError(f"reward must be harbor or score.json, not {option!r}")


def _harbor_task(d: Path, options: dict[str, str]) -> dict[str, Any]:
    reward_files = _reward_files(options.get("reward"))
    config = tomllib.loads((d / "task.toml").read_text())
    if config.get("steps") or (d / "steps").is_dir():
        raise TaskFormatError("multi-step tasks are not supported")
    env_cfg = config.get("environment") or {}
    verifier = config.get("verifier") or {}
    agent = config.get("agent") or {}
    if (verifier.get("environment_mode") or "") == "separate" or verifier.get("environment"):
        raise TaskFormatError("verifiers in a separate environment are not supported")
    if not (d / "instruction.md").is_file():
        raise TaskFormatError("no instruction.md")
    if not (d / "tests" / "test.sh").is_file():
        raise TaskFormatError("no tests/test.sh")
    build = None
    image = options.get("image") or env_cfg.get("docker_image") or ""
    if not image:
        dockerfile = d / "environment" / "Dockerfile"
        if not dockerfile.is_file():
            raise TaskFormatError("no environment/Dockerfile or [environment].docker_image")
        build = translate_dockerfile(dockerfile.read_text(), d / "environment")
        image = build.image
    workdir = (
        options.get("workdir")
        or env_cfg.get("workdir")
        or (build.workdir if build and build.workdir not in ("", "/") else "")
        or DEFAULT_WORKDIR
    )
    mode = str(env_cfg.get("network_mode") or "")
    if env_cfg.get("allow_internet") is False:
        mode = "no-network"
    tests, tests_exec = _tree(d / "tests", "/tests")
    files: dict[str, str] = {}
    executable: list[str] = []
    if options.get("oracle", "").lower() in ("1", "true", "yes") and (d / "solution").is_dir():
        files, executable = _tree(d / "solution", "/solution")
    checker = {
        "command": [
            "sh",
            "-c",
            _checker_script(
                "/tests/test.sh",
                dict(verifier.get("env") or {}),
                _chmod(tests_exec),
                reward_files,
            ),
        ],
        "files": tests,
        "timeout": _seconds(verifier.get("timeout_sec") or 600),
        "parser": "evalsi_harness.benchmarks.harbor:grade_harbor",
    }
    task = config.get("task") or {}
    name = task.get("name") or d.name
    return {
        "id": name,
        "input": (d / "instruction.md").read_text().strip(),
        "metadata": {
            "benchmark": "harbor",
            "task_dir": d.name,
            "harbor": {
                "metadata": config.get("metadata") or {},
                "agent_timeout_sec": agent.get("timeout_sec"),
                "ignored_dockerfile_instructions": build.ignored if build else [],
                **({"reward": options["reward"]} if reward_files else {}),
            },
            "environment": _environment(
                build=build,
                image=image,
                workdir=workdir,
                network=_network(options.get("network"), mode),
                env={str(k): str(v) for k, v in (env_cfg.get("env") or {}).items()},
                files=files,
                executable=executable,
                checker=checker,
                setup_timeout=env_cfg.get("build_timeout_sec"),
                git=_flag(options.get("git")),
            ),
        },
    }


def load_terminal_bench(path: str, **options: str) -> list[dict[str, Any]]:
    """Records for ``terminal-bench://<task or dataset dir>?options`` (Terminal-Bench 1)."""
    rows = []
    wanted = [t for t in options.get("tasks", "").split(",") if t]
    for d in _task_dirs(path, "task.yaml", wanted):
        try:
            rows.append(_tb_task(d, options))
        except TaskFormatError as exc:
            raise TaskFormatError(f"{d.name}: {exc}") from None
    return rows


def _tb_task(d: Path, options: dict[str, str]) -> dict[str, Any]:
    import yaml

    config = yaml.safe_load((d / "task.yaml").read_text()) or {}
    parser = config.get("parser_name") or "pytest"
    if parser != "pytest":
        raise TaskFormatError(f"parser {parser!r} is not supported (only pytest)")
    compose = load_compose(d, d.name)
    services = compose.services if compose else []
    instruction = config.get("instruction") or ""
    if not instruction:
        descriptions = config.get("descriptions") or []
        base = [x for x in descriptions if x.get("key") == "base"] or descriptions[:1]
        instruction = base[0].get("description", "") if base else ""
    if not instruction:
        raise TaskFormatError("task.yaml has no instruction")
    if not (d / "run-tests.sh").is_file():
        raise TaskFormatError("no run-tests.sh")
    build = None
    image = options.get("image") or (compose.client_image if compose else "")
    if not image:
        # The client's build is where Compose says (a subdirectory, a named Dockerfile,
        # build args, a target stage), else the Dockerfile at the task's root.
        spec = compose.client if compose else None
        dockerfile = spec.dockerfile if spec else d / "Dockerfile"
        if not dockerfile.is_file():
            raise TaskFormatError("no Dockerfile")
        build = translate_dockerfile(
            dockerfile.read_text(),
            spec.context if spec else d,
            build_args=spec.args if spec else None,
            target=spec.target if spec else "",
        )
        image = build.image
    workdir = (
        options.get("workdir")
        or (compose.workdir if compose else "")
        or (build.workdir if build and build.workdir not in ("", "/") else "")
        or DEFAULT_WORKDIR
    )
    if compose:
        compose.finalize(workdir)
    tests: dict[str, str] = {}
    if (d / "tests").is_dir():
        tests, tests_exec = _tree(d / "tests", "/tests")
    else:
        tests_exec = []
    tests["/tests/run-tests.sh"] = _text(d / "run-tests.sh")
    files: dict[str, str] = {}
    if options.get("oracle", "").lower() in ("1", "true", "yes") and (d / "solution.sh").is_file():
        files["/solution/solution.sh"] = _text(d / "solution.sh")
    if compose:
        files.update(compose.files)
        if compose.setup and build is not None:
            build.setup.extend(compose.setup)
    checker = {
        "command": [
            "sh",
            "-c",
            _checker_script("/tests/run-tests.sh", {"TEST_DIR": "/tests"}, _chmod(tests_exec)),
        ],
        "files": tests,
        "timeout": _seconds(config.get("max_test_timeout_sec") or 180),
        "parser": "evalsi_harness.benchmarks.harbor:grade_pytest",
    }
    return {
        "id": d.name,
        "input": instruction.strip(),
        "metadata": {
            "benchmark": "terminal-bench",
            "terminal_bench": {
                "difficulty": config.get("difficulty"),
                "category": config.get("category"),
                "tags": config.get("tags") or [],
                "agent_timeout_sec": config.get("max_agent_timeout_sec"),
                "ignored_dockerfile_instructions": build.ignored if build else [],
                **({"compose_ignored": compose.ignored} if compose and compose.ignored else {}),
            },
            "environment": {
                **(
                    {"services": services, "service_lifetime": _service_lifetime(config)}
                    if services
                    else {}
                ),
                **({"volumes": compose.volumes} if compose and compose.volumes else {}),
                **_environment(
                    build=build,
                    image=image,
                    workdir=workdir,
                    # Services share a private network with no way out, so the agent has
                    # none either unless the options say otherwise.
                    network=_network(options.get("network"), "no-network" if services else ""),
                    env={"TEST_DIR": "/tests", **(compose.env if compose else {})},
                    files=files,
                    executable=list(compose.executable) if compose else [],
                    checker=checker,
                    setup_timeout=None,
                    git=_flag(options.get("git")),
                ),
            },
        },
    }


# --- grading ---


def _sections(stdout: str) -> tuple[str, int | None, str, str]:
    """(test output, test exit code, reward kind, reward text)."""
    out, _, tail = stdout.partition(f"\n{MARK} exit ")
    code_text, _, rest = tail.partition("\n")
    code = int(code_text) if code_text.strip().lstrip("-").isdigit() else None
    kind, reward = "", ""
    if rest.startswith((f"{MARK} reward.", f"{MARK} score.json")):
        header, _, reward = rest.partition("\n")
        kind = header.removeprefix(f"{MARK} ")
    return out, code, kind, reward


def _tail(text: str, limit: int = 2000) -> str:
    return text if len(text) <= limit else "..." + text[-limit:]


def _unit(value: Any) -> float | None:
    try:
        number = float(value)
    except (TypeError, ValueError):
        return None
    return number if 0.0 <= number <= 1.0 else None


def score_from_payload(payload: dict[str, Any]) -> float | None:
    """A WorkBuddy-Bench ``score.json`` as one score in 0..1, with the
    precedence of Harness²'s adapter (read from its source): ``reward``,
    ``overall``, ``test_pass_rate``, then ``tests_passed / tests_total``.
    Values outside 0..1 are skipped, not clamped; ``None`` means unscored."""
    for key in ("reward", "overall", "test_pass_rate"):
        score = _unit(payload.get(key))
        if score is not None:
            return score
    try:
        passed, total = int(payload.get("tests_passed")), int(payload.get("tests_total"))  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return None
    return max(0.0, min(passed / total, 1.0)) if total > 0 else None


def _grade_score_json(text: str, code: int, out: str) -> TaskCheck:
    try:
        payload = json.loads(text)
    except json.JSONDecodeError as exc:
        return TaskCheck(passed=False, error=f"unreadable score.json: {exc}", details=_tail(out))
    score = score_from_payload(payload) if isinstance(payload, dict) else None
    if score is None:
        return TaskCheck(
            passed=False,
            error=(
                "score.json has no usable score "
                "(reward, overall, test_pass_rate or tests_passed/tests_total)"
            ),
            details=_tail(text),
        )
    return TaskCheck(
        passed=score >= 1.0,
        score=score,
        details=f"score {score:g} (score.json, tests exit {code})",
    )


def grade_harbor(stdout: str, stderr: str, exit_code: int, task: Any) -> TaskCheck:
    """Harbor's reward files: reward.json first, then reward.txt; with
    ``reward=score.json``, WorkBuddy-Bench's ``score.json`` first."""
    out, code, kind, text = _sections(stdout)
    if code is None:
        return TaskCheck(
            passed=False, error="the verifier did not run", details=_tail(stdout + stderr)
        )
    if not kind:
        return TaskCheck(
            passed=False,
            details=f"the tests wrote no reward file (exit {code})\n" + _tail(out),
        )
    if kind == "score.json":
        return _grade_score_json(text, code, out)
    try:
        if kind == "reward.json":
            data = json.loads(text)
            values = data if isinstance(data, dict) else {"reward": data}
            rewards = {str(k): float(v) for k, v in values.items()}
            score = (
                rewards["reward"] if "reward" in rewards else sum(rewards.values()) / len(rewards)
            )
        else:
            score = float(text.strip())
            rewards = {"reward": score}
    except (ValueError, TypeError, ZeroDivisionError, json.JSONDecodeError) as exc:
        return TaskCheck(passed=False, details=f"unreadable {kind}: {exc}\n" + _tail(out))
    if not all(math.isfinite(v) for v in rewards.values()):
        return TaskCheck(passed=False, details=f"non-finite reward in {kind}")
    return TaskCheck(
        passed=score >= 1.0,
        score=score,
        details=f"reward {score:g} ({kind}, tests exit {code})",
        tests={k: "passed" if v >= 1.0 else "failed" for k, v in rewards.items() if k != "reward"},
    )


PYTEST_PASSED = {"PASSED", "XFAIL", "SKIPPED"}
PYTEST_FAILED = {"FAILED", "XPASS", "ERROR"}


def grade_pytest(stdout: str, stderr: str, exit_code: int, task: Any) -> TaskCheck:
    """Terminal-Bench's pytest rule: every test in the short summary passed."""
    out, code, _, _ = _sections(stdout)
    if code is None:
        return TaskCheck(
            passed=False, error="the tests did not run", details=_tail(stdout + stderr)
        )
    parts = re.split(r"=+\s*short test summary info\s*=+", out, maxsplit=1, flags=re.I)
    if len(parts) < 2:
        return TaskCheck(
            passed=False, details=f"no pytest short test summary (exit {code})\n" + _tail(out)
        )
    tests: dict[str, str] = {}
    for line in parts[1].splitlines():
        status, _, rest = line.strip().partition(" ")
        status = status.strip(":")
        if status not in PYTEST_PASSED | PYTEST_FAILED or not rest:
            continue
        if status == "FAILED" and " - " in rest:
            rest = rest.split(" - ", 1)[0]
        name = rest.strip().split("::", 1)[-1]
        if name:
            tests[name] = "passed" if status in PYTEST_PASSED else "failed"
    passed = sum(v == "passed" for v in tests.values())
    return TaskCheck(
        passed=bool(tests) and passed == len(tests),
        score=passed / len(tests) if tests else 0.0,
        details=f"{passed}/{len(tests)} tests passed",
        tests=tests,
    )


__all__ = [
    "Build",
    "TaskFormatError",
    "grade_harbor",
    "grade_pytest",
    "load_harbor",
    "load_terminal_bench",
    "score_from_payload",
    "translate_dockerfile",
]
