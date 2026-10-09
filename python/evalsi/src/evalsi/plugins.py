"""The plugin index: find, inspect and install evaluator plugins.

An index is a YAML (or JSON) document listing plugin versions (see
``plugins/index.yaml`` in the repository, the default). Entries are
``native`` (ships with evalsi), ``python`` (a pip requirement), ``wasm`` (a
manifest URL pinned by sha256, which in turn pins its module) or ``image``
(an OCI image serving the plugin protocol). Every download is checked
against the sha256 that the index or the manifest pins before anything is
written.
"""

from __future__ import annotations

import hashlib
import os
import re
import shutil
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from urllib.parse import urljoin

DEFAULT_INDEX = "https://raw.githubusercontent.com/abhishek-rnjn/evals.si/main/plugins/index.yaml"
KINDS = ("native", "python", "wasm", "image")
TIERS = ("native", "wrapped", "community")


class PluginError(RuntimeError):
    """An index, a download or an installation went wrong."""


@dataclass
class Entry:
    name: str
    version: str
    kind: str
    tier: str = "community"
    description: str = ""
    homepage: str = ""
    license: str = ""
    evaluators: list[str] = field(default_factory=list)
    source: dict[str, Any] = field(default_factory=dict)
    index: str = ""

    @property
    def ref(self) -> str:
        return f"{self.name}@{self.version}"


def index_sources(explicit: list[str] | None = None) -> list[str]:
    if explicit:
        return explicit
    env = os.environ.get("EVALSI_PLUGIN_INDEX", "").split()
    return env or [DEFAULT_INDEX]


def _fetch(location: str, base: str = "") -> bytes:
    if base and not re.match(r"^[a-z]+://", location) and not os.path.isabs(location):
        location = (
            urljoin(base, location)
            if re.match(r"^[a-z]+://", base)
            else str(Path(base).parent / location)
        )
    if re.match(r"^https?://", location):
        import httpx

        resp = httpx.get(location, follow_redirects=True, timeout=60)
        if resp.status_code != 200:
            raise PluginError(f"fetching {location}: HTTP {resp.status_code}")
        return resp.content
    if location.startswith("file://"):
        location = location[len("file://") :]
    try:
        return Path(location).read_bytes()
    except OSError as e:
        raise PluginError(f"reading {location}: {e}") from e


def _resolve(location: str, base: str) -> str:
    if re.match(r"^[a-z]+://", location) or os.path.isabs(location):
        return location
    if re.match(r"^[a-z]+://", base):
        return urljoin(base, location)
    return str(Path(base).parent / location)


def parse_index(raw: bytes, source: str) -> list[Entry]:
    import yaml

    doc = yaml.safe_load(raw)
    if not isinstance(doc, dict) or doc.get("kind") != "PluginIndex":
        raise PluginError(f"{source}: not a PluginIndex document")
    out = []
    for i, p in enumerate(doc.get("plugins") or []):
        kind = p.get("kind", "")
        if kind not in KINDS:
            raise PluginError(f"{source}: plugins[{i}]: kind must be one of {', '.join(KINDS)}")
        if not p.get("name") or not p.get("version"):
            raise PluginError(f"{source}: plugins[{i}]: name and version are required")
        tier = p.get("tier", "community")
        if tier not in TIERS:
            raise PluginError(f"{source}: plugins[{i}]: tier must be one of {', '.join(TIERS)}")
        src = p.get(kind) or {}
        if kind == "python" and not src.get("requirement"):
            raise PluginError(f"{source}: {p['name']}: python.requirement is required")
        if kind == "wasm" and (not src.get("manifest") or len(str(src.get("sha256", ""))) != 64):
            raise PluginError(f"{source}: {p['name']}: wasm.manifest and its sha256 are required")
        if kind == "image" and "@sha256:" not in str(src.get("ref", "")):
            raise PluginError(
                f"{source}: {p['name']}: image.ref must be pinned by digest (...@sha256:...)"
            )
        if kind == "wasm":
            src = {**src, "manifest": _resolve(src["manifest"], source)}
        out.append(
            Entry(
                name=p["name"],
                version=str(p["version"]),
                kind=kind,
                tier=tier,
                description=p.get("description", ""),
                homepage=p.get("homepage", ""),
                license=p.get("license", ""),
                evaluators=list(p.get("evaluators") or []),
                source=src,
                index=source,
            )
        )
    return out


def load_index(sources: list[str] | None = None) -> list[Entry]:
    entries: list[Entry] = []
    for source in index_sources(sources):
        entries.extend(parse_index(_fetch(source), source))
    return entries


def _version_key(v: str) -> tuple[Any, ...]:
    parts = re.split(r"[.+-]", v)
    return tuple((0, int(p)) if p.isdigit() else (1, p) for p in parts)


def search(entries: list[Entry], query: str = "") -> list[Entry]:
    """The newest version of each plugin matching query (name, description, evaluators)."""
    q = query.lower()
    newest: dict[str, Entry] = {}
    for e in entries:
        text = " ".join([e.name, e.description, *e.evaluators]).lower()
        if q and q not in text:
            continue
        if e.name not in newest or _version_key(e.version) > _version_key(newest[e.name].version):
            newest[e.name] = e
    return sorted(newest.values(), key=lambda e: e.name)


def find(entries: list[Entry], ref: str) -> Entry:
    name, _, version = ref.partition("@")
    matches = [e for e in entries if e.name == name and (not version or e.version == version)]
    if not matches:
        raise PluginError(f"no plugin {ref} in the index; try `evalsi plugins search`")
    return max(matches, key=lambda e: _version_key(e.version))


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def plugin_dirname(name: str) -> str:
    return re.sub(r"[^A-Za-z0-9._-]", "__", name)


def install_wasm(manifest_location: str, dest_root: Path, manifest_sha: str = "") -> Path:
    """Fetch (or copy) a Wasm plugin, verify it, and write it under dest_root.

    The manifest is checked against manifest_sha when given (an index pins
    it); the module always against the sha256 the manifest pins.
    """
    import yaml

    raw = _fetch(manifest_location)
    if manifest_sha and _sha256(raw) != manifest_sha.lower():
        raise PluginError(
            f"{manifest_location}: sha256 is {_sha256(raw)}, the index pins {manifest_sha}"
        )
    doc = yaml.safe_load(raw)
    if not isinstance(doc, dict):
        raise PluginError(f"{manifest_location}: not a plugin manifest")
    wasm = ((doc.get("runtime") or {}).get("wasm")) or {}
    module, pinned = wasm.get("module"), str(wasm.get("sha256", "")).lower()
    if not doc.get("name") or not module or len(pinned) != 64:
        raise PluginError(
            f"{manifest_location}: needs name, runtime.wasm.module and runtime.wasm.sha256"
        )
    if "/" in module or "\\" in module or module.startswith("."):
        raise PluginError(
            f"{manifest_location}: runtime.wasm.module must be a file next to the manifest"
        )
    code = _fetch(module, base=manifest_location)
    if _sha256(code) != pinned:
        raise PluginError(f"{module}: sha256 is {_sha256(code)}, the manifest pins {pinned}")
    dest = dest_root / plugin_dirname(doc["name"])
    tmp = dest.with_name(dest.name + ".tmp")
    shutil.rmtree(tmp, ignore_errors=True)
    tmp.mkdir(parents=True)
    (tmp / "evalsi-plugin.yaml").write_bytes(raw)
    (tmp / module).write_bytes(code)
    shutil.rmtree(dest, ignore_errors=True)
    tmp.rename(dest)
    return dest


def pip_command(requirement: str) -> list[str]:
    if shutil.which("uv") and os.environ.get("VIRTUAL_ENV"):
        return ["uv", "pip", "install", "--python", sys.executable, requirement]
    return [sys.executable, "-m", "pip", "install", requirement]


def install(entry: Entry, dest_root: Path, *, dry_run: bool = False) -> str:
    """Install an index entry; returns what happened, for the user."""
    if entry.kind == "native":
        return (
            f"{entry.name} ships with evalsi; nothing to install "
            "(enable it per project with evaluators.packs)"
        )
    if entry.kind == "python":
        cmd = pip_command(entry.source["requirement"])
        if dry_run:
            return "would run: " + " ".join(cmd)
        result = subprocess.run(cmd, check=False)
        if result.returncode != 0:
            raise PluginError(f"pip exited with {result.returncode}")
        return f"installed {entry.ref} ({entry.source['requirement']})"
    if entry.kind == "wasm":
        if dry_run:
            return f"would install {entry.source['manifest']} into {dest_root}"
        dest = install_wasm(entry.source["manifest"], dest_root, entry.source.get("sha256", ""))
        return f"installed {entry.ref} into {dest}"
    ref = entry.source["ref"]
    return (
        f"{entry.name} runs as an image; deploy it as an Evaluator resource or a remote worker:\n"
        "  apiVersion: evals.si/v1alpha1\n  kind: Evaluator\n"
        f"  metadata: {{name: {plugin_dirname(entry.name).lower()}}}\n"
        f"  spec: {{image: {ref}}}"
    )


def installed(dirs: list[Path] | None = None) -> list[dict[str, Any]]:
    """Installed Wasm plugins: their manifests, with the directory."""
    from evalsi.wasm import find_manifests, read_manifest

    out = []
    for path in find_manifests(dirs):
        doc = read_manifest(path)
        doc["_dir"] = str(path.parent)
        out.append(doc)
    return out
