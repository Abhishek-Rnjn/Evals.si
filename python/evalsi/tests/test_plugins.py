from __future__ import annotations

import hashlib
from pathlib import Path

import pytest

from evalsi.cli import main
from evalsi.evaluator import Scope, ScoreType
from evalsi.plugins import PluginError, find, install, install_wasm, load_index, parse_index, search
from evalsi.registry import _builtin_packs
from evalsi.wasm import discover_wasm_packs, load_plugin

REPO_INDEX = Path(__file__).parents[3] / "plugins" / "index.yaml"


def test_repo_index_lists_every_builtin_pack() -> None:
    entries = load_index([str(REPO_INDEX)])
    native = {e.name for e in entries if e.kind == "native"}
    assert native == {p.name for p in _builtin_packs()}
    assert all(e.tier == "wrapped" for e in entries if e.kind == "python")


def test_index_validation_and_search() -> None:
    sha = "a" * 64
    doc = f"""
apiVersion: evals.si/v1alpha1
kind: PluginIndex
plugins:
  - {{name: acme/checks, version: 1.2.0, kind: wasm, description: JSON checks,
      wasm: {{manifest: plugins/checks/evalsi-plugin.yaml, sha256: "{sha}"}}}}
  - {{name: acme/checks, version: 1.10.0, kind: wasm, evaluators: [acme/json-valid],
      wasm: {{manifest: https://example.com/c/evalsi-plugin.yaml, sha256: "{sha}"}}}}
  - {{name: big, version: 2.0.0, kind: image, tier: community,
      image: {{ref: "ghcr.io/a/b@sha256:{sha}"}}}}
"""
    entries = parse_index(doc.encode(), "/srv/index.yaml")
    assert entries[0].source["manifest"] == "/srv/plugins/checks/evalsi-plugin.yaml"
    [checks] = search(entries, "json-valid")
    assert checks.version == "1.10.0"  # versions compare numerically
    assert find(entries, "acme/checks@1.2.0").version == "1.2.0"
    with pytest.raises(PluginError, match="no plugin"):
        find(entries, "acme/nope")
    assert "Evaluator" in install(entries[2], Path("/unused"))
    for bad, match in [
        ("plugins: [{name: x, version: 1, kind: zip}]", "kind must be"),
        ("plugins: [{name: x, version: 1, kind: wasm, wasm: {manifest: m.yaml}}]", "sha256"),
        (
            "plugins: [{name: x, version: 1, kind: image, image: {ref: 'ghcr.io/a:latest'}}]",
            "digest",
        ),
        (
            "plugins: [{name: x, version: 1, kind: python, tier: gold, python: {requirement: x}}]",
            "tier",
        ),
    ]:
        with pytest.raises(PluginError, match=match):
            parse_index(f"kind: PluginIndex\n{bad}".encode(), "i.yaml")


def _plugin(tmp_path: Path, module: bytes = b"\0asm fake", pin: str | None = None) -> Path:
    src = tmp_path / "src"
    src.mkdir(exist_ok=True)
    (src / "checks.wasm").write_bytes(module)
    sha = pin or hashlib.sha256(module).hexdigest()
    manifest = src / "evalsi-plugin.yaml"
    manifest.write_text(
        f"""name: acme/checks
version: 1.0.0
runtime: {{wasm: {{module: checks.wasm, sha256: {sha}}}}}
evaluators:
  - name: acme/json-valid
    requires: {{output: true}}
    outputs: [{{name: json-valid, type: passed, higher_is_better: true}}]
    params_schema:
      type: object
      properties: {{strict: {{type: boolean, default: false}}, keys: {{type: array}}}}
      required: [keys]
  - name: acme/distinct
    scope: dataset
    outputs: [{{name: distinct, type: number, min: 0, max: 1}}]
"""
    )
    return manifest


def test_install_wasm_verifies_pins(tmp_path: Path) -> None:
    manifest = _plugin(tmp_path)
    dest = install_wasm(str(manifest), tmp_path / "plugins")
    assert dest.name == "acme__checks"
    assert (dest / "checks.wasm").read_bytes() == b"\0asm fake"
    # The index pins the manifest; the manifest pins the module.
    with pytest.raises(PluginError, match="the index pins"):
        install_wasm(str(manifest), tmp_path / "p2", "b" * 64)
    bad = _plugin(tmp_path, pin="c" * 64)
    with pytest.raises(PluginError, match="the manifest pins"):
        install_wasm(str(bad), tmp_path / "p3")
    escape = tmp_path / "src" / "evalsi-plugin.yaml"
    escape.write_text(escape.read_text().replace("module: checks.wasm", "module: ../x.wasm"))
    with pytest.raises(PluginError, match="next to the manifest"):
        install_wasm(str(escape), tmp_path / "p4")


def test_wasm_plugins_become_packs(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    install_wasm(str(_plugin(tmp_path)), tmp_path / "plugins")
    monkeypatch.setenv("EVALSI_PLUGIN_PATH", str(tmp_path / "plugins"))
    [pack] = discover_wasm_packs()
    assert pack.name == "acme/checks"
    assert pack.tier == "community"
    valid, distinct = pack.evaluators
    assert valid.spec.runtime == "wasm"
    assert valid.spec.outputs[0].type == ScoreType.PASSED
    assert valid.spec.params["strict"] is False
    assert "keys" in valid.spec.params
    assert distinct.spec.scope == Scope.DATASET
    assert (
        load_plugin(tmp_path / "plugins" / "acme__checks" / "evalsi-plugin.yaml").name
        == "acme/checks"
    )


def test_cli(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    plugins = tmp_path / "plugins"
    monkeypatch.setenv("EVALSI_PLUGIN_PATH", str(plugins))
    index = ["--index", str(REPO_INDEX)]
    assert main(["plugins", "search", "rag", *index]) == 0
    assert "ragas" in capsys.readouterr().out
    assert main(["plugins", "show", "ragas", *index]) == 0
    assert "subdirectory=python/adapters/ragas" in capsys.readouterr().out
    assert main(["plugins", "install", "ragas", "--dry-run", *index]) == 0
    assert "would run:" in capsys.readouterr().out
    assert main(["plugins", "install", "core", *index]) == 0
    assert "ships with evalsi" in capsys.readouterr().out

    assert main(["plugins", "install", str(_plugin(tmp_path))]) == 0
    assert (plugins / "acme__checks" / "checks.wasm").exists()
    assert main(["plugins", "list"]) == 0
    out = capsys.readouterr().out
    assert "acme/checks" in out
    assert "community wasm" in " ".join(out.split())
    assert "core" in out
    assert main(["plugins", "remove", "acme/checks"]) == 0
    assert not (plugins / "acme__checks").exists()
    assert main(["plugins", "remove", "acme/checks"]) == 1
