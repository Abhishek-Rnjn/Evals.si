"""WebArena (and BrowserGym's WebArena tasks) as Evals.si browser tasks.

``webarena://<path>?options``
    ``<path>`` is WebArena's ``config_files/test.raw.json`` (a list of task configs) or
    a directory of per-task ``<id>.json`` files (what WebArena's ``generate_test_data.py``
    writes), or a directory holding ``test.raw.json``. BrowserGym's ``browsergym/webarena.<id>``
    tasks are these same configs, so the record id is ``webarena.<id>``.

Targets ``web-arena-x/webarena`` ``config_files/test.raw.json`` as on ``main`` on 2026-10-22:
812 tasks, ids 0-811, each with ``sites``, ``task_id``, ``require_login``,
``storage_state``, ``start_url`` and ``reference_url`` with ``__SHOPPING__``-style
placeholders, ``intent``, ``intent_template``, ``instantiation_dict`` and an ``eval`` of
``eval_types`` (``string_match``, ``url_match``, ``program_html``), ``reference_answers``
(``exact_match``, ``must_include`` or ``fuzzy_match``), ``reference_url``, ``url_note``,
``program_html`` and notes. The loader was run over that file (812 tasks parsed) but over
no task *run*: nothing here has driven a real WebArena site. The repository is Apache-2.0;
no task text is vendored here, and the loader reads the file you supply. BrowserGym is
checked only by its README (its WebArena tasks load these configs): no BrowserGym code is
used or imported.

The WebArena sites (shopping, shopping admin, GitLab, Reddit, OpenStreetMap, Wikipedia)
are not hosted for you. You deploy them as WebArena's README describes and give the
loader each one's base URL. A task whose sites you did not give is an error naming them.
The browser the agent drives is limited to the hosts you gave (``allow_hosts`` on the
record's ``metadata.browser``).

Options (query string): ``tasks`` (ids and ranges such as ``0,5,10-20``), the site URLs
``shopping``, ``shopping_admin``, ``gitlab``, ``reddit``, ``map``, ``wikipedia`` and
``homepage``, ``auth`` (a directory of Playwright storage-state files named as the configs'
``storage_state`` is, for example ``shopping_admin_state.json``; every task requires login,
and WebArena makes these with its ``browser_env/auto_login.py``; without ``auth`` the
agent starts logged out), ``allow_hosts`` (more allowed hosts, comma-separated),
``answer=false`` (leave the answer instructions out of the task text).

The task is run with the ``browser`` toolset (``harness.builtin.tools.builtin: [browser]``,
``tools.sandbox: false``) and scored with ``builtin/webarena-string-match`` and
``builtin/webarena-url-match``. ``fuzzy_match`` references and ``program_html`` are not
reproduced; the record's ``metadata.grading.not_reproduced`` lists what a task needs that
those two do not check. The configs are carried untouched in ``metadata.webarena.eval``
and never reach the agent.
"""

from __future__ import annotations

import json
import re
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

from evalsi_harness.benchmarks.harbor import TaskFormatError

UPSTREAM = "web-arena-x/webarena config_files/test.raw.json, main, 2026-10-22"
SITES = ("shopping", "shopping_admin", "gitlab", "reddit", "map", "wikipedia", "homepage")
_PLACEHOLDER = re.compile(r"__([A-Z_]+)__")
ANSWER_NOTE = (
    "\n\nWhen you have finished, reply with your answer only, in plain text. If the task asks "
    "you to do something rather than to find something out, reply with a short statement of "
    "what you did. If the task cannot be done, reply N/A and say why."
)


def _task_files(path: Path) -> list[Path]:
    if path.is_file():
        return [path]
    if not path.is_dir():
        raise TaskFormatError(f"{path} is not a file or a directory")
    if (path / "test.raw.json").is_file():
        return [path / "test.raw.json"]
    files = sorted(
        path.glob("*.json"),
        key=lambda p: (not p.stem.isdigit(), int(p.stem) if p.stem.isdigit() else 0, p.name),
    )
    if not files:
        raise TaskFormatError(f"no test.raw.json or <id>.json files in {path}")
    return files


def _read_configs(path: Path) -> list[dict[str, Any]]:
    configs: list[dict[str, Any]] = []
    for file in _task_files(path):
        try:
            data = json.loads(file.read_text(encoding="utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise TaskFormatError(f"{file.name} is not valid JSON: {exc}") from None
        items = data if isinstance(data, list) else [data]
        for item in items:
            if not isinstance(item, dict) or "task_id" not in item or "intent" not in item:
                raise TaskFormatError(f"{file.name}: a task needs task_id and intent")
            configs.append(item)
    ids = [c["task_id"] for c in configs]
    if len(set(map(str, ids))) != len(ids):
        raise TaskFormatError("task ids are not unique")
    return configs


def _wanted(spec: str) -> set[int] | None:
    if not spec:
        return None
    out: set[int] = set()
    for raw in spec.split(","):
        part = raw.strip()
        if not part:
            continue
        lo, sep, hi = part.partition("-")
        try:
            out.update(range(int(lo), int(hi) + 1) if sep else [int(lo)])
        except ValueError:
            raise TaskFormatError(
                f"tasks: {part!r} is not an id or a range such as 10-20"
            ) from None
    return out


def _resolve(text: str, urls: dict[str, str], missing: set[str]) -> str:
    def swap(match: re.Match[str]) -> str:
        site = match.group(1).lower()
        if site in urls:
            return urls[site].rstrip("/")
        missing.add(site)
        return match.group(0)

    return _PLACEHOLDER.sub(swap, text)


def load_webarena(path: str, **options: str) -> list[dict[str, Any]]:
    """Records for ``webarena://<config file or directory>?options``."""
    urls = {s: options[s] for s in SITES if options.get(s)}
    for site, url in urls.items():
        parts = urlsplit(url)
        if parts.scheme not in ("http", "https") or not parts.netloc:
            raise TaskFormatError(f"{site}={url!r} must be an http(s) URL")
    unknown = set(options) - {*SITES, "tasks", "auth", "allow_hosts", "answer"}
    if unknown:
        raise TaskFormatError(f"unknown option(s) {', '.join(sorted(unknown))}")
    wanted = _wanted(options.get("tasks", ""))
    configs = _read_configs(Path(path))
    if wanted is not None:
        have = {int(c["task_id"]) for c in configs}
        absent = sorted(wanted - have)
        if absent:
            raise TaskFormatError(f"no tasks {absent} in {path}")
        configs = [c for c in configs if int(c["task_id"]) in wanted]
    auth = Path(options["auth"]) if options.get("auth") else None
    if auth is not None and not auth.is_dir():
        raise TaskFormatError(f"auth={auth} is not a directory")
    extra = [h for h in options.get("allow_hosts", "").split(",") if h]
    answer_note = (
        "" if options.get("answer", "true").lower() in ("0", "false", "no") else ANSWER_NOTE
    )
    rows = [_record(c, urls, auth, extra, answer_note) for c in configs]
    return rows


def _record(
    config: dict[str, Any], urls: dict[str, str], auth: Path | None, extra: list[str], note: str
) -> dict[str, Any]:
    task_id = int(config["task_id"])
    missing: set[str] = set()
    start = _resolve(str(config.get("start_url") or ""), urls, missing)
    ev = json.loads(json.dumps(config.get("eval") or {}))  # a copy; the config is not changed
    if isinstance(ev.get("reference_url"), str):
        ev["reference_url"] = _resolve(ev["reference_url"], urls, missing)
    for target in ev.get("program_html") or []:
        if isinstance(target, dict) and isinstance(target.get("url"), str):
            target["url"] = _resolve(target["url"], urls, missing)
    if missing:
        needed = ", ".join(f"{s}=<url>" for s in sorted(missing))
        raise TaskFormatError(f"webarena.{task_id}: give the site URLs it uses: {needed}")
    storage = ""
    if auth is not None and config.get("storage_state"):
        storage_file = auth / Path(str(config["storage_state"])).name
        if not storage_file.is_file():
            raise TaskFormatError(f"webarena.{task_id}: no storage state {storage_file}")
        storage = str(storage_file.resolve())
    hosts = sorted({urlsplit(u).netloc for u in urls.values()} | set(extra))
    types = [str(t) for t in ev.get("eval_types") or []]
    refs = ev.get("reference_answers")
    not_reproduced = []
    if isinstance(refs, dict) and "fuzzy_match" in refs:
        not_reproduced.append("fuzzy_match (an LLM judge)")
    if "program_html" in types:
        not_reproduced.append("program_html (reads the live page's DOM)")
    return {
        "id": f"webarena.{task_id}",
        "input": str(config["intent"]).strip() + note,
        "metadata": {
            "benchmark": "webarena",
            "browser": {"start_url": start, "allow_hosts": hosts, "storage_state": storage},
            "webarena": {
                "task_id": task_id,
                "sites": [str(s) for s in config.get("sites") or []],
                "intent_template_id": config.get("intent_template_id"),
                "require_login": bool(config.get("require_login")),
                "logged_in": bool(storage),
                "eval": ev,
                "browsergym_id": f"browsergym/webarena.{task_id}",
                "upstream": UPSTREAM,
            },
            "grading": {
                "method": "webarena-eval",
                "reproduced_offline": False,
                "evaluators": ["builtin/webarena-string-match", "builtin/webarena-url-match"],
                "not_reproduced": not_reproduced,
            },
        },
    }


__all__ = ["load_webarena"]
