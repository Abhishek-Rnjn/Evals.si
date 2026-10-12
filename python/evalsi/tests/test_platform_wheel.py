from __future__ import annotations

import base64
import csv
import hashlib
import importlib.util
import io
import stat
import sys
import tarfile
import zipfile
from pathlib import Path
from types import ModuleType

import pytest

from evalsi import _binaries

SCRIPT = Path(__file__).parents[3] / "scripts" / "platform_wheel.py"


def load_script() -> ModuleType:
    if not SCRIPT.exists():
        pytest.skip("scripts/ is not in this source tree")
    spec = importlib.util.spec_from_file_location("platform_wheel", SCRIPT)
    assert spec
    assert spec.loader
    module = importlib.util.module_from_spec(spec)
    sys.modules["platform_wheel"] = module
    spec.loader.exec_module(module)
    return module


def pure_wheel(dest: Path, version: str = "1.2.3") -> Path:
    path = dest / f"evalsi-{version}-py3-none-any.whl"
    dist = f"evalsi-{version}.dist-info"
    with zipfile.ZipFile(path, "w") as zf:
        zf.writestr("evalsi/__init__.py", "")
        zf.writestr(
            f"{dist}/METADATA", f"Metadata-Version: 2.4\nName: evalsi\nVersion: {version}\n"
        )
        zf.writestr(
            f"{dist}/WHEEL",
            "Wheel-Version: 1.0\nGenerator: hatchling 1.32.4\n"
            "Root-Is-Purelib: true\nTag: py3-none-any\n",
        )
        zf.writestr(f"{dist}/RECORD", "evalsi/__init__.py,,\n")
    return path


def archive(
    dest: Path,
    version: str = "1.2.3",
    arch: str = "amd64",
    files: tuple[str, ...] = ("evalsid", "bwrap", "evalsi-guest", "THIRD_PARTY.md"),
) -> Path:
    path = dest / f"evalsi_{version}_linux_{arch}.tar.gz"
    top = f"evalsi_{version}_linux_{arch}"
    with tarfile.open(path, "w:gz") as tf:
        for name in files:
            data = f"{name} contents".encode()
            info = tarfile.TarInfo(f"{top}/{name}")
            info.size = len(data)
            info.mode = 0o755
            tf.addfile(info, io.BytesIO(data))
    return path


def test_platform_wheel_bundles_the_binaries_and_is_a_valid_wheel(tmp_path: Path) -> None:
    build = load_script().build
    out = build(pure_wheel(tmp_path), archive(tmp_path), tmp_path / "out")
    assert out.name == "evalsi-1.2.3-py3-none-manylinux_2_17_x86_64.whl"
    with zipfile.ZipFile(out) as zf:
        names = set(zf.namelist())
        assert {"evalsi/_bin/evalsid", "evalsi/_bin/bwrap"} <= names
        assert "evalsi/_bin/evalsi-guest" not in names  # only what the package runs
        assert "evalsi-1.2.3.dist-info/licenses/bundled/THIRD_PARTY.md" in names
        for binary in ("evalsid", "bwrap"):
            mode = zf.getinfo(f"evalsi/_bin/{binary}").external_attr >> 16
            assert mode & stat.S_IXUSR, binary
        wheel = zf.read("evalsi-1.2.3.dist-info/WHEEL").decode()
        assert "Root-Is-Purelib: false" in wheel
        assert wheel.count("Tag:") == 1
        assert "Tag: py3-none-manylinux_2_17_x86_64" in wheel
        # RECORD lists every file with its real hash and size.
        rows = list(csv.reader(zf.read("evalsi-1.2.3.dist-info/RECORD").decode().splitlines()))
        assert {r[0] for r in rows} == names
        for path, digest, size in rows:
            if path.endswith("RECORD"):
                assert (digest, size) == ("", "")
                continue
            data = zf.read(path)
            expected = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
            assert (digest, size) == (f"sha256={expected}", str(len(data))), path


def test_a_prerelease_tag_matches_its_pep_440_wheel(tmp_path: Path) -> None:
    build = load_script().build
    out = build(pure_wheel(tmp_path, "0.5.0rc1"), archive(tmp_path, "0.5.0-rc1"), tmp_path / "out")
    assert out.name == "evalsi-0.5.0rc1-py3-none-manylinux_2_17_x86_64.whl"


def test_arm64_gets_its_own_tag_and_mismatches_are_refused(tmp_path: Path) -> None:
    build = load_script().build
    assert build(
        pure_wheel(tmp_path), archive(tmp_path, arch="arm64"), tmp_path / "out"
    ).name.endswith("manylinux_2_17_aarch64.whl")
    with pytest.raises(SystemExit, match=r"archive is version 9\.9\.9, the wheel 1\.2\.3"):
        build(pure_wheel(tmp_path), archive(tmp_path, version="9.9.9"), tmp_path / "out")
    with pytest.raises(SystemExit, match="lacks"):
        build(pure_wheel(tmp_path), archive(tmp_path, files=("evalsid",)), tmp_path / "out")
    with pytest.raises(SystemExit, match="pure-Python wheel"):
        build(
            tmp_path / "evalsi-1.2.3-py3-none-manylinux_2_17_x86_64.whl",
            archive(tmp_path),
            tmp_path,
        )


def test_evalsid_lookup_prefers_the_env_then_the_bundle_then_path(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    bin_dir = tmp_path / "_bin"
    bin_dir.mkdir()
    on_path = tmp_path / "path"
    on_path.mkdir()
    for directory in (bin_dir, on_path):
        (directory / "evalsid").write_text("#!/bin/sh\n")
        (directory / "evalsid").chmod(0o755)
    monkeypatch.setattr(_binaries, "BIN_DIR", bin_dir)
    monkeypatch.setenv("PATH", str(on_path))
    monkeypatch.delenv("EVALSID", raising=False)
    assert _binaries.evalsid_path() == str(bin_dir / "evalsid")
    monkeypatch.setenv("EVALSID", "/custom/evalsid")
    assert _binaries.evalsid_path() == "/custom/evalsid"
    monkeypatch.delenv("EVALSID")
    # A bundled file that is not executable is not a bundle.
    (bin_dir / "evalsid").chmod(0o644)
    assert _binaries.evalsid_path() == str(on_path / "evalsid")
    (bin_dir / "evalsid").unlink()
    assert _binaries.bundled_evalsid() is None
    monkeypatch.setenv("PATH", str(tmp_path / "nothing"))
    assert _binaries.evalsid_path() is None
