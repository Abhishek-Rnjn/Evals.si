#!/usr/bin/env python3
"""Builds a platform wheel of evalsi: the pure-Python wheel plus the release
archive's ``evalsid`` and static ``bwrap`` in ``evalsi/_bin``.

    scripts/platform_wheel.py PURE.whl ARCHIVE.tar.gz OUT_DIR

The archive is ``evalsi_<version>_linux_<arch>.tar.gz`` from
``scripts/release-binaries.sh``; the arch in its name picks the platform tag
(``manylinux_2_17_x86_64`` or ``manylinux_2_17_aarch64``: the binaries are
static, so the tag only tells installers which machines the wheel is for). The
archive's version must be the wheel's. Standard library only.
"""

from __future__ import annotations

import base64
import csv
import hashlib
import io
import re
import stat
import sys
import tarfile
import zipfile
from pathlib import Path

PLATFORMS = {"amd64": "manylinux_2_17_x86_64", "arm64": "manylinux_2_17_aarch64"}
BUNDLED = ("evalsid", "bwrap")
# Only the binaries the package runs: the notices go in the dist-info licenses.
NOTICES = ("bwrap.COPYING", "bwrap.SOURCE", "THIRD_PARTY.md")


def _record_hash(data: bytes) -> str:
    digest = hashlib.sha256(data).digest()
    return "sha256=" + base64.urlsafe_b64encode(digest).rstrip(b"=").decode()


def _normal(version: str) -> str:
    """The RC archive and PEP 440 spellings identify the same release."""
    return re.sub(r"-rc\.?([0-9]+)$", r"rc\1", version)


def _write(out: Path, members: dict[str, tuple[bytes, int]], dist_info: str) -> Path:
    out.parent.mkdir(parents=True, exist_ok=True)
    record = io.StringIO()
    writer = csv.writer(record, lineterminator="\n")
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zf:
        for path in sorted(members):
            data, mode = members[path]
            info = zipfile.ZipInfo(path, date_time=(1980, 1, 1, 0, 0, 0))
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = (stat.S_IFREG | mode) << 16
            zf.writestr(info, data)
            writer.writerow([path, _record_hash(data), len(data)])
        writer.writerow([f"{dist_info}/RECORD", "", ""])
        info = zipfile.ZipInfo(f"{dist_info}/RECORD", date_time=(1980, 1, 1, 0, 0, 0))
        info.external_attr = (stat.S_IFREG | 0o644) << 16
        zf.writestr(info, record.getvalue())
    return out


def build(pure_wheel: Path, archive: Path, out_dir: Path) -> Path:
    m = re.fullmatch(r"(?P<name>[^-]+)-(?P<version>[^-]+)-py3-none-any\.whl", pure_wheel.name)
    if not m:
        raise SystemExit(
            f"{pure_wheel.name}: expected a pure-Python wheel (name-version-py3-none-any.whl)"
        )
    name, version = m["name"], m["version"]
    a = re.fullmatch(r"evalsi_(?P<version>.+)_linux_(?P<arch>amd64|arm64)\.tar\.gz", archive.name)
    if not a:
        raise SystemExit(f"{archive.name}: expected evalsi_<version>_linux_<amd64|arm64>.tar.gz")
    if _normal(a["version"]) != _normal(version):
        raise SystemExit(f"the archive is version {a['version']}, the wheel {version}")
    tag = PLATFORMS[a["arch"]]
    dist_info = f"{name}-{version}.dist-info"

    members: dict[str, tuple[bytes, int]] = {}  # path -> (data, unix mode)
    with zipfile.ZipFile(pure_wheel) as zf:
        for info in zf.infolist():
            if info.is_dir() or info.filename == f"{dist_info}/RECORD":
                continue
            members[info.filename] = (zf.read(info), (info.external_attr >> 16) or 0o644)

    found: set[str] = set()
    with tarfile.open(archive) as tf:
        for member in tf.getmembers():
            base = Path(member.name).name
            if not member.isfile():
                continue
            fileobj = tf.extractfile(member)
            assert fileobj is not None
            data = fileobj.read()
            if base in BUNDLED:
                members[f"{name}/_bin/{base}"] = (data, 0o755)
                found.add(base)
            elif base in NOTICES:
                members[f"{dist_info}/licenses/bundled/{base}"] = (data, 0o644)
    if found != set(BUNDLED):
        raise SystemExit(f"{archive.name} lacks {sorted(set(BUNDLED) - found)}")

    wheel_path = f"{dist_info}/WHEEL"
    text = members[wheel_path][0].decode()
    text = re.sub(r"(?m)^Root-Is-Purelib: .*$", "Root-Is-Purelib: false", text)
    text = re.sub(r"(?m)^Tag: .*$", "", text).rstrip("\n") + f"\nTag: py3-none-{tag}\n"
    members[wheel_path] = (text.encode(), 0o644)

    return _write(out_dir / f"{name}-{version}-py3-none-{tag}.whl", members, dist_info)


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    print(build(Path(argv[0]), Path(argv[1]), Path(argv[2])))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
