#!/usr/bin/env python3
"""Classify stable/RC releases, stamp Python versions and plan registry tags.

Standard library only. Release identities use X.Y.Z or X.Y.Z-rc.N (the older
X.Y.Z-rcN spelling also works); Python distributions use X.Y.ZrcN.
"""

from __future__ import annotations

import argparse
import json
import re
from pathlib import Path

NUMBER = r"(?:0|[1-9][0-9]*)"
RELEASE = re.compile(rf"({NUMBER}\.{NUMBER}\.{NUMBER})(?:-rc\.?({NUMBER}))?")


def metadata(version: str, owner: str) -> dict[str, str]:
    match = RELEASE.fullmatch(version)
    if match is None:
        raise ValueError("version must be X.Y.Z or X.Y.Z-rc.N (no leading v)")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9-]*", owner):
        raise ValueError("owner must be a GitHub user or organization name")
    prerelease = match[2] is not None
    result = {
        "version": version,
        "python_version": match[1] + (f"rc{match[2]}" if prerelease else ""),
        "owner": owner.lower(),
        "prerelease": str(prerelease).lower(),
        "make_latest": str(not prerelease).lower(),
    }
    for image, output in (("evalsi", "image_tags"), ("evalsi-collector", "collector_tags")):
        repository = f"ghcr.io/{owner.lower()}/{image}"
        tags = [f"{repository}:{version}"]
        if not prerelease:
            tags.append(f"{repository}:latest")
        result[output] = ",".join(tags)
    return result


def stamp(directory: Path, version: str) -> None:
    updates: dict[Path, str] = {}
    for package in ("evalsi", "evalsi-harness"):
        module = package.replace("-", "_")
        path = directory / package / "src" / module / "_version.py"
        text, count = re.subn(
            r'^__version__ = "[^"]*"$',
            f'__version__ = "{version}"',
            path.read_text(encoding="utf-8"),
            flags=re.M,
        )
        if count != 1:
            raise ValueError(f"{path}: expected exactly one version assignment")
        updates[path] = text
    project = directory / "evalsi-harness" / "pyproject.toml"
    text, count = re.subn(
        r'"evalsi\[server\]==[^\"]*"',
        f'"evalsi[server]=={version}"',
        project.read_text(encoding="utf-8"),
    )
    if count != 1:
        raise ValueError("evalsi-harness: expected exactly one evalsi dependency pin")
    updates[project] = text
    for path, text in updates.items():
        path.write_text(text, encoding="utf-8")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version")
    parser.add_argument("--owner", default="abhishek-rnjn")
    output = parser.add_mutually_exclusive_group()
    output.add_argument("--github-output", type=Path)
    output.add_argument("--stamp", type=Path)
    output.add_argument("--python-version", action="store_true")
    args = parser.parse_args()
    try:
        release = metadata(args.version, args.owner)
        if args.stamp:
            stamp(args.stamp, release["python_version"])
        elif args.github_output:
            with args.github_output.open("a", encoding="utf-8") as stream:
                stream.write("".join(f"{key}={value}\n" for key, value in release.items()))
        elif args.python_version:
            print(release["python_version"])
        else:
            print(json.dumps(release, indent=2))
    except (ValueError, OSError) as exc:
        parser.exit(1, f"{exc}\n")


if __name__ == "__main__":
    main()
