"""A Dockerfile as an image, setup commands and files, without Docker.

``FROM`` is the image, ``RUN`` steps become setup commands (run once and
snapshotted), ``COPY``/``ADD`` of local files become files in the image root, and
``WORKDIR``, ``ENV``, ``ARG`` and ``SHELL`` apply as Docker would. ``ENTRYPOINT``
and ``CMD`` are kept (a service built from a Dockerfile runs them); ``USER``,
``EXPOSE`` and the like are ignored.

Multi-stage builds are supported: the last stage is the image, and every earlier
stage it copies from (``COPY --from=build``, or from another image,
``COPY --from=ghcr.io/astral-sh/uv:0.8 /uv /bin/``) becomes a build stage that the
environment builds in a sandbox of its own, taking the named files out
(``stages`` and ``copy_from`` in the environment). Stages nothing copies from are
never built, as with BuildKit. A stage may start ``FROM`` an earlier one. Not
supported: ``COPY --from`` of a glob, ``FROM scratch``, ``RUN --mount`` of anything
but a cache or tmpfs (which only speed a build), remote ``ADD`` and instructions that
need a Docker daemon.

Copied files exist before the first ``RUN`` (Docker interleaves them); tasks that
depend on the interleaving should use a prebuilt image.
"""

from __future__ import annotations

import base64
import copy
import json
import re
import shlex
from collections.abc import Iterator
from dataclasses import dataclass, field
from pathlib import Path, PurePosixPath
from typing import Any


class TaskFormatError(ValueError):
    """The task directory cannot be turned into an Evals.si task."""


@dataclass
class StageCopyRef:
    """``COPY --from``: a file or directory of an earlier stage (or image), placed here."""

    stage: str
    source: str
    destination: str


@dataclass
class BuildStage:
    """An earlier stage, built on its own."""

    name: str
    image: str
    setup: list[str] = field(default_factory=list)
    files: dict[str, str] = field(default_factory=dict)
    executable: list[str] = field(default_factory=list)
    env: dict[str, str] = field(default_factory=dict)
    copies: list[StageCopyRef] = field(default_factory=list)


@dataclass
class Build:
    image: str = ""
    setup: list[str] = field(default_factory=list)
    files: dict[str, str] = field(default_factory=dict)
    executable: list[str] = field(default_factory=list)
    env: dict[str, str] = field(default_factory=dict)
    workdir: str = ""
    ignored: list[str] = field(default_factory=list)
    # The image's ENTRYPOINT and CMD as argv (shell forms as sh -c), when set.
    entrypoint: list[str] | None = None
    cmd: list[str] | None = None
    # Earlier stages this one copies from, in the order they must be built, and the copies.
    stages: list[BuildStage] = field(default_factory=list)
    copies: list[StageCopyRef] = field(default_factory=list)


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

# RUN flags that only speed a build up.
HARMLESS_MOUNTS = ("type=cache", "type=tmpfs")


def _instructions(text: str) -> Iterator[tuple[str, str, str | None]]:
    """(INSTRUCTION, arguments, heredoc body) triples: comments dropped, continuations and
    heredocs (``RUN <<EOF``) joined. The body is set only for ``COPY <<EOF dest``."""
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
            yield word.upper(), "\n".join(body), None
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
            yield word.upper(), "\n".join(body), None
            continue
        if inline and word.upper() in ("COPY", "ADD"):
            # COPY <<EOT /path: the body is a file.
            end, body = inline.group(1), []
            while i < len(lines) and lines[i].strip() != end:
                body.append(lines[i])
                i += 1
            i += 1
            yield word.upper(), rest, "\n".join(body) + "\n"
            continue
        yield word.upper(), rest, None


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


def _exec_or_shell(rest: str) -> list[str]:
    """ENTRYPOINT and CMD: the exec form as given, the shell form run by ``sh -c``."""
    if rest.startswith("["):
        return _json_or_words(rest)
    return ["/bin/sh", "-c", rest]


@dataclass
class _Scope:
    """The stages translated so far: by name, and in order."""

    named: dict[str, Build] = field(default_factory=dict)
    ordered: list[Build] = field(default_factory=list)


@dataclass
class _Block:
    name: str
    image: str
    instructions: list[tuple[str, str, str | None]]


def _blocks(text: str) -> tuple[list[_Block], dict[str, str]]:
    """The Dockerfile split into stages, and the ARGs declared before the first FROM."""
    args: dict[str, str] = {}
    blocks: list[_Block] = []
    for word, rest, body in _instructions(text):
        if word == "FROM":
            parts = [p for p in _expand(rest, args).split() if not p.startswith("--platform")]
            if not parts:
                raise TaskFormatError("FROM needs an image")
            name = parts[2] if len(parts) >= 3 and parts[1].lower() == "as" else ""
            blocks.append(_Block(name, parts[0], []))
        elif not blocks:
            if word == "ARG":
                for k, v in _pairs(rest, legacy=False).items():
                    args.setdefault(k, _expand(v, args))
            else:
                raise TaskFormatError("the Dockerfile has no FROM")
        else:
            blocks[-1].instructions.append((word, rest, body))
    if not blocks:
        raise TaskFormatError("the Dockerfile has no FROM")
    return blocks, args


def translate_dockerfile(text: str, context: Path) -> Build:
    """A Dockerfile as an image, setup commands and files (see the module docs)."""
    blocks, global_args = _blocks(text)
    scope = _Scope()
    done, by_index = scope.named, scope.ordered
    for block in blocks:
        base = done.get(block.image)
        build = copy.deepcopy(base) if base is not None else Build(image=block.image)
        if base is not None:
            build.image = base.image
        if block.image == "scratch":
            raise TaskFormatError("FROM scratch is not supported; use a prebuilt image")
        _apply(block, build, context, global_args, scope)
        if block.name:
            done[block.name] = build
        by_index.append(build)
    target = by_index[-1]
    needed: dict[str, BuildStage] = {}
    order: list[BuildStage] = []

    def need(ref: str) -> None:
        if ref in needed:
            return
        stage = done.get(ref)
        if stage is None:
            # Another image: nothing to build, just a root to take files from.
            needed[ref] = BuildStage(ref, ref)
            order.append(needed[ref])
            return
        for dep in stage.copies:
            need(dep.stage)
        needed[ref] = BuildStage(
            ref, stage.image, stage.setup, stage.files, stage.executable, stage.env, stage.copies
        )
        order.append(needed[ref])

    for c in target.copies:
        need(c.stage)
    target.stages = order
    return target


def _stage_key(ref: str, done: dict[str, Build], by_index: list[Build]) -> str:
    """The name a ``--from`` value stands for: an earlier stage by name or index, else an image."""
    if ref in done:
        return ref
    if ref.isdigit() and int(ref) < len(by_index):
        names = [n for n, b in done.items() if b is by_index[int(ref)]]
        if names:
            return names[0]
        raise TaskFormatError(f"COPY --from={ref} names a stage that has no name; name it with AS")
    return ref


def _apply(
    block: _Block,
    build: Build,
    context: Path,
    global_args: dict[str, str],
    scope: _Scope,
) -> None:
    args = dict(global_args)
    workdir = build.workdir
    shell: list[str] = []
    for word, rest, body in block.instructions:
        variables = {**args, **build.env}
        if word == "ARG":
            for k, v in _pairs(rest, legacy=False).items():
                args.setdefault(k, _expand(v, variables))
        elif word == "ENV":
            for k, v in _pairs(rest, legacy=True).items():
                build.env[k] = _expand(v, variables)
        elif word == "WORKDIR":
            target = _expand(rest, variables).strip("\"'")
            workdir = str(PurePosixPath(workdir or "/") / target)
        elif word == "SHELL":
            shell = _json_or_words(rest)
        elif word == "RUN":
            _run(rest, build, args, workdir, shell)
        elif word in ("COPY", "ADD"):
            _copy(
                word,
                rest,
                context,
                workdir=workdir,
                variables=variables,
                build=build,
                body=body,
                scope=scope,
            )
        elif word in ("ENTRYPOINT", "CMD"):
            argv = _exec_or_shell(rest)
            if word == "ENTRYPOINT":
                build.entrypoint = argv
                build.cmd = None  # Docker clears an inherited CMD when ENTRYPOINT is set
            else:
                build.cmd = argv
            build.ignored.append(word)
        elif word in IGNORED:
            build.ignored.append(word)
        else:
            raise TaskFormatError(f"unsupported Dockerfile instruction {word}")
    build.workdir = workdir


def _run(rest: str, build: Build, args: dict[str, str], workdir: str, shell: list[str]) -> None:
    flags = re.match(r"^((?:--\S+\s+)*)(.*)$", rest, re.S)
    assert flags is not None
    for flag in flags.group(1).split():
        if flag.startswith("--mount="):
            if not any(flag.removeprefix("--mount=").startswith(m) for m in HARMLESS_MOUNTS):
                raise TaskFormatError(
                    f"RUN {flag.split(',')[0]} needs BuildKit; use a prebuilt image"
                )
        elif flag.startswith("--security"):
            raise TaskFormatError(f"RUN {flag} needs BuildKit; use a prebuilt image")
        # --network=... only picks the build's network, which setup_network decides here.
    body = flags.group(2)
    if body.startswith("["):
        command = shlex.join(_json_or_words(body))
    elif shell:
        command = shlex.join([*shell, body])
    else:
        command = body
    if workdir:
        command = f"mkdir -p {shlex.quote(workdir)} && cd {shlex.quote(workdir)} && {command}"
    # ARGs are environment variables while RUN steps run, as in Docker.
    exports = " ".join(f"{k}={shlex.quote(v)}" for k, v in args.items() if k not in build.env)
    if exports:
        command = f"export {exports}; {command}"
    build.setup.append(command)


def _copy(
    word: str,
    rest: str,
    context: Path,
    *,
    workdir: str,
    variables: dict[str, str],
    build: Build,
    body: str | None,
    scope: _Scope,
) -> None:
    done, by_index = scope.named, scope.ordered
    tokens = _json_or_words(rest) if rest.lstrip().startswith("[") else shlex.split(rest)
    flags = [t for t in tokens if t.startswith("--")]
    paths = [_expand(t, variables) for t in tokens if not t.startswith("--")]
    base = PurePosixPath(workdir or "/")
    if body is not None:
        # COPY <<EOT /path: the heredoc is the file.
        *names, dest = paths
        if len(names) != 1 or not names[0].startswith("<<"):
            raise TaskFormatError(f"unsupported heredoc form: {word} {rest[:80]}")
        name = names[0].lstrip("<-").strip("\"'")
        target = base / dest
        _put_text(build, str(target / name) if dest.endswith("/") else str(target), body)
        return
    source_flag = next((f for f in flags if f.startswith("--from")), "")
    if len(paths) < 2:
        raise TaskFormatError(f"{word} needs a source and a destination: {rest[:80]}")
    *sources, dest = paths
    if source_flag:
        ref = source_flag.partition("=")[2]
        if not ref:
            raise TaskFormatError(f"{word} --from needs a stage or image")
        stage = _stage_key(ref, done, by_index)
        stage_workdir = done[stage].workdir if stage in done else ""
        for source in sources:
            if any(c in source for c in "*?["):
                raise TaskFormatError(
                    f"{word} --from of a pattern ({source}) is not supported; "
                    "name the file or directory"
                )
            from_src = source
            if not source.startswith("/"):
                from_src = str(PurePosixPath(stage_workdir or "/") / source)
            from_into = str(base / dest)
            if dest.endswith("/") or len(sources) > 1:
                from_into = from_into.rstrip("/") + "/"
            build.copies.append(StageCopyRef(stage, str(PurePosixPath(from_src)), from_into))
        return
    dest_dir = dest.endswith("/") or len(sources) > 1
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


def put_file(files: dict[str, str], src: Path, dest: str) -> None:
    """A file for the sandbox. Records carry text, so binary files travel
    base64-encoded as ``<dest>.evalsi-b64`` and :func:`decode_command`
    restores them in the sandbox."""
    data = src.read_bytes()
    try:
        files[dest] = data.decode("utf-8")
    except UnicodeDecodeError:
        files[dest + B64] = base64.b64encode(data).decode("ascii")


def _put_text(build: Build, dest: str, text: str) -> None:
    build.files[dest] = text


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
    put_file(build.files, src, str(dest))
    if src.stat().st_mode & 0o111:
        build.executable.append(str(dest))


def prepared_setup(build: Build | BuildStage) -> list[str]:
    """A build's setup with the steps that make its files usable first: decoding the
    binary ones and restoring execute bits."""
    setup = list(build.setup)
    if build.executable:
        setup.insert(0, "chmod +x " + " ".join(shlex.quote(f) for f in build.executable))
    if decode := decode_command(build.files):
        setup.insert(0, decode)
    return setup


def stage_config(stage: BuildStage) -> dict[str, Any]:
    """A build stage as the environment's ``stages`` entry."""
    out: dict[str, Any] = {"name": stage.name, "image": stage.image}
    if stage.files:
        out["files"] = stage.files
    if setup := prepared_setup(stage):
        out["setup"] = setup
    if stage.env:
        out["env"] = stage.env
    if stage.copies:
        out["copy_from"] = [copy_config(c) for c in stage.copies]
    return out


def copy_config(c: StageCopyRef) -> dict[str, str]:
    return {"stage": c.stage, "source": c.source, "destination": c.destination}
