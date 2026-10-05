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
task's, ``allow`` unless it says no-network), ``oracle=true`` (adds the
reference solution under /solution; an agent that runs
``bash /solution/solve.sh``, or ``solution.sh`` for Terminal-Bench 1,
validates the setup).
"""

from __future__ import annotations

import base64
import fnmatch
import json
import math
import re
import shlex
import tomllib
from collections.abc import Iterator
from dataclasses import dataclass, field
from pathlib import Path, PurePosixPath
from typing import Any

from evalsi.types import TaskCheck

LOGS = "/logs/verifier"
MARK = "@@evalsi"
DEFAULT_WORKDIR = "/app"


class TaskFormatError(ValueError):
    """The task directory cannot be turned into an Evals.si task."""


# --- the Dockerfile ---


@dataclass
class Build:
    image: str = ""
    setup: list[str] = field(default_factory=list)
    files: dict[str, str] = field(default_factory=dict)
    executable: list[str] = field(default_factory=list)
    env: dict[str, str] = field(default_factory=dict)
    workdir: str = ""
    ignored: list[str] = field(default_factory=list)


IGNORED = {
    "USER",
    "CMD",
    "ENTRYPOINT",
    "EXPOSE",
    "LABEL",
    "MAINTAINER",
    "VOLUME",
    "STOPSIGNAL",
    "HEALTHCHECK",
    "ONBUILD",
}


def _instructions(text: str) -> Iterator[tuple[str, str]]:
    """(INSTRUCTION, arguments) pairs: comments dropped, continuations and
    heredocs (``RUN <<EOF``) joined."""
    lines = text.splitlines()
    i = 0
    while i < len(lines):
        line = lines[i].strip()
        i += 1
        if not line or line.startswith("#"):
            continue
        while line.endswith("\\") and i < len(lines):
            nxt = lines[i].strip()
            i += 1
            if nxt.startswith("#"):
                continue
            line = line[:-1].rstrip() + " " + nxt
        word, _, rest = line.partition(" ")
        rest = rest.strip()
        heredoc = re.match(r"^<<-?\s*['\"]?(\w+)['\"]?\s*$", rest)
        if heredoc:
            # BuildKit's RUN <<EOF: the body is the script.
            end, body = heredoc.group(1), []
            while i < len(lines) and lines[i].strip() != end:
                body.append(lines[i])
                i += 1
            i += 1
            yield word.upper(), "\n".join(body)
            continue
        inline = re.search(r"<<-?\s*['\"]?(\w+)['\"]?", rest)
        if inline and word.upper() == "RUN":
            # cmd <<'EOF' ... EOF: a shell heredoc, passed to the shell whole.
            end, body = inline.group(1), [rest]
            while i < len(lines):
                body.append(lines[i])
                i += 1
                if body[-1].strip() == end:
                    break
            yield word.upper(), "\n".join(body)
            continue
        if "<<" in rest and word.upper() == "COPY":
            raise TaskFormatError(f"unsupported heredoc form: {line[:80]}")
        yield word.upper(), rest


def _expand(value: str, variables: dict[str, str]) -> str:
    """``$NAME``, ``${NAME}`` and ``${NAME:-default}`` from ARG and ENV;
    unknown names are left for the shell."""
    return re.sub(r"\$\{(\w+)(?::-([^}]*))?\}|\$(\w+)", lambda m: _sub3(m, variables), value)


def _sub3(m: re.Match[str], variables: dict[str, str]) -> str:
    name = m.group(1) or m.group(3)
    if name not in variables and m.group(2) is None:
        return m.group(0)  # left for the shell
    got = variables.get(name, "")
    return got if got or m.group(2) is None else m.group(2)


def _pairs(rest: str, legacy: bool) -> dict[str, str]:
    """``ENV``/``ARG`` arguments: ``k=v k2="v 2"`` or the legacy ``k v``."""
    if legacy and "=" not in rest.split(" ", 1)[0]:
        key, _, value = rest.partition(" ")
        return {key: value.strip()}
    out: dict[str, str] = {}
    for token in shlex.split(rest):
        key, eq, value = token.partition("=")
        out[key] = value if eq else ""
    return out


def _json_or_words(rest: str) -> list[str]:
    if rest.startswith("["):
        try:
            value = json.loads(rest)
        except json.JSONDecodeError as exc:
            raise TaskFormatError(f"bad JSON form: {rest[:80]}") from exc
        return [str(v) for v in value]
    return shlex.split(rest)


def translate_dockerfile(text: str, context: Path) -> Build:
    """A Dockerfile as an image, setup commands and files (see the module docs)."""
    build = Build()
    args: dict[str, str] = {}
    workdir = ""
    shell: list[str] = []
    froms = 0
    for word, rest in _instructions(text):
        variables = {**args, **build.env}
        if word == "ARG":
            for k, v in _pairs(rest, legacy=False).items():
                args.setdefault(k, _expand(v, variables))
        elif word == "FROM":
            froms += 1
            if froms > 1:
                raise TaskFormatError(
                    "multi-stage Dockerfiles are not supported; use a prebuilt image"
                )
            parts = _expand(rest, variables).split()
            parts = [p for p in parts if not p.startswith("--platform")]
            build.image = parts[0]
        elif word == "ENV":
            for k, v in _pairs(rest, legacy=True).items():
                build.env[k] = _expand(v, variables)
        elif word == "WORKDIR":
            target = _expand(rest, variables).strip("\"'")
            workdir = str(PurePosixPath(workdir or "/") / target)
        elif word == "SHELL":
            shell = _json_or_words(rest)
        elif word == "RUN":
            flags = re.match(r"^((?:--\S+\s+)*)(.*)$", rest, re.S)
            assert flags is not None
            if "--mount" in flags.group(1):
                raise TaskFormatError("RUN --mount needs BuildKit; use a prebuilt image")
            body = flags.group(2)
            if body.startswith("["):
                command = shlex.join(_json_or_words(body))
            elif shell:
                command = shlex.join([*shell, body])
            else:
                command = body
            if workdir:
                command = (
                    f"mkdir -p {shlex.quote(workdir)} && cd {shlex.quote(workdir)} && {command}"
                )
            # ARGs are environment variables while RUN steps run, as in Docker.
            exports = " ".join(
                f"{k}={shlex.quote(v)}" for k, v in args.items() if k not in build.env
            )
            if exports:
                command = f"export {exports}; {command}"
            build.setup.append(command)
        elif word in ("COPY", "ADD"):
            _copy(word, rest, context, workdir=workdir, variables=variables, build=build)
        elif word in IGNORED:
            build.ignored.append(word)
        else:
            raise TaskFormatError(f"unsupported Dockerfile instruction {word}")
    if not build.image:
        raise TaskFormatError("the Dockerfile has no FROM")
    build.workdir = workdir
    return build


def _copy(
    word: str, rest: str, context: Path, *, workdir: str, variables: dict[str, str], build: Build
) -> None:
    tokens = _json_or_words(rest) if rest.lstrip().startswith("[") else shlex.split(rest)
    flags = [t for t in tokens if t.startswith("--")]
    paths = [_expand(t, variables) for t in tokens if not t.startswith("--")]
    if any(f.startswith("--from") for f in flags):
        raise TaskFormatError(f"{word} --from (multi-stage) is not supported; use a prebuilt image")
    if len(paths) < 2:
        raise TaskFormatError(f"{word} needs a source and a destination: {rest[:80]}")
    *sources, dest = paths
    dest_dir = dest.endswith("/") or len(sources) > 1
    base = PurePosixPath(workdir or "/")
    target = base / dest
    root = context.resolve()
    for source in sources:
        if re.match(r"^[a-z]+://", source):
            raise TaskFormatError(f"{word} of a URL is not supported: {source}")
        matches = (
            sorted(root.glob(source.lstrip("/")))
            if any(c in source for c in "*?[")
            else [root / source.lstrip("/")]
        )
        for match in matches:
            src = match.resolve()
            if root not in (src, *src.parents):
                raise TaskFormatError(f"{word} source {source} is outside the build context")
            if not src.exists():
                raise TaskFormatError(f"{word} source {source} does not exist")
            if src.is_dir():
                for f in sorted(p for p in src.rglob("*") if p.is_file()):
                    _add_file(build, f, target / f.relative_to(src).as_posix())
            else:
                into = target / src.name if dest_dir or len(matches) > 1 else target
                _add_file(build, src, into)


B64 = ".evalsi-b64"


def _put(files: dict[str, str], src: Path, dest: str) -> None:
    """A file for the sandbox. Records carry text, so binary files travel
    base64-encoded as ``<dest>.evalsi-b64`` and :func:`decode_command`
    restores them in the sandbox."""
    data = src.read_bytes()
    try:
        files[dest] = data.decode("utf-8")
    except UnicodeDecodeError:
        files[dest + B64] = base64.b64encode(data).decode("ascii")


def decode_command(files: dict[str, str]) -> str:
    """Shell that turns the base64-encoded files back into binary files."""
    paths = [shlex.quote(f.removesuffix(B64)) for f in files if f.endswith(B64)]
    if not paths:
        return ""
    return (
        f'for f in {" ".join(paths)}; do base64 -d "$f{B64}" > "$f" && rm -f "$f{B64}" '
        "|| exit 1; done"
    )


def _add_file(build: Build, src: Path, dest: PurePosixPath) -> None:
    _put(build.files, src, str(dest))
    if src.stat().st_mode & 0o111:
        build.executable.append(str(dest))


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
) -> dict[str, Any]:
    setup = list(build.setup) if build else []
    all_files = {**(build.files if build else {}), **files}
    exec_files = [*(build.executable if build else []), *executable]
    if exec_files:
        setup.insert(0, "chmod +x " + " ".join(shlex.quote(f) for f in exec_files))
    if decode := decode_command(all_files):
        setup.insert(0, decode)
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
    if setup:
        # Package installs in RUN steps need the network even when the
        # agent runs without it.
        environment["setup_network"] = "allow"
    if setup_timeout:
        environment["setup_timeout"] = _seconds(setup_timeout)
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


def _checker_script(test: str, verifier_env: dict[str, str], extra: str = "") -> str:
    exports = "".join(f"export {k}={shlex.quote(v)}\n" for k, v in verifier_env.items())
    return f"""mkdir -p {LOGS} /logs/agent /logs/artifacts
{exports}{extra}if command -v bash >/dev/null 2>&1; then run=bash; else run=sh; fi
$run {test} > {LOGS}/test-stdout.txt 2>&1
code=$?
tail -c 20000 {LOGS}/test-stdout.txt
printf '\\n{MARK} exit %s\\n' "$code"
if [ -f {LOGS}/reward.json ]; then printf '{MARK} reward.json\\n'; cat {LOGS}/reward.json
elif [ -f {LOGS}/reward.txt ]; then printf '{MARK} reward.txt\\n'; cat {LOGS}/reward.txt
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


def _harbor_task(d: Path, options: dict[str, str]) -> dict[str, Any]:
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
            _checker_script("/tests/test.sh", dict(verifier.get("env") or {}), _chmod(tests_exec)),
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
    compose = d / "docker-compose.yaml"
    if compose.is_file():
        services = (yaml.safe_load(compose.read_text()) or {}).get("services") or {}
        if len(services) > 1:
            raise TaskFormatError(
                f"tasks with several services ({', '.join(services)}) are not supported"
            )
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
    image = options.get("image") or ""
    if not image:
        dockerfile = d / "Dockerfile"
        if not dockerfile.is_file():
            raise TaskFormatError("no Dockerfile")
        build = translate_dockerfile(dockerfile.read_text(), d)
        image = build.image
    workdir = (
        options.get("workdir")
        or (build.workdir if build and build.workdir not in ("", "/") else "")
        or DEFAULT_WORKDIR
    )
    tests: dict[str, str] = {}
    if (d / "tests").is_dir():
        tests, tests_exec = _tree(d / "tests", "/tests")
    else:
        tests_exec = []
    tests["/tests/run-tests.sh"] = _text(d / "run-tests.sh")
    files: dict[str, str] = {}
    if options.get("oracle", "").lower() in ("1", "true", "yes") and (d / "solution.sh").is_file():
        files["/solution/solution.sh"] = _text(d / "solution.sh")
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
            },
            "environment": _environment(
                build=build,
                image=image,
                workdir=workdir,
                network=_network(options.get("network"), ""),
                env={"TEST_DIR": "/tests"},
                files=files,
                executable=[],
                checker=checker,
                setup_timeout=None,
            ),
        },
    }


# --- grading ---


def _sections(stdout: str) -> tuple[str, int | None, str, str]:
    """(test output, test exit code, reward kind, reward text)."""
    out, _, tail = stdout.partition(f"\n{MARK} exit ")
    code_text, _, rest = tail.partition("\n")
    code = int(code_text) if code_text.strip().lstrip("-").isdigit() else None
    kind, reward = "", ""
    if rest.startswith(f"{MARK} reward."):
        header, _, reward = rest.partition("\n")
        kind = header.removeprefix(f"{MARK} ")
    return out, code, kind, reward


def _tail(text: str, limit: int = 2000) -> str:
    return text if len(text) <= limit else "..." + text[-limit:]


def grade_harbor(stdout: str, stderr: str, exit_code: int, task: Any) -> TaskCheck:
    """Harbor's reward files: reward.json first, then reward.txt."""
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
    "TaskFormatError",
    "grade_harbor",
    "grade_pytest",
    "load_harbor",
    "load_terminal_bench",
    "translate_dockerfile",
]
