"""``evalsi plugins``: the plugin index and installed plugins.

``search`` and ``show`` read the index; ``install`` takes a name from the
index, or a Wasm plugin's manifest by path or URL; ``list`` shows what this
environment has, with each pack's tier; ``remove`` deletes an installed Wasm
plugin. For a server, install Wasm plugins into its data directory
(``--dir <data_dir>/plugins``); evalsid loads them at start.
"""

from __future__ import annotations

import argparse
import re
import shutil
import sys
from pathlib import Path
from typing import Any


def add_plugin_commands(sub: Any) -> None:
    pl = sub.add_parser("plugins", help="search the plugin index and manage installed plugins")
    pl_sub = pl.add_subparsers(dest="action", required=True)

    def index_arg(p: argparse.ArgumentParser) -> None:
        p.add_argument(
            "--index",
            action="append",
            help="index URL or path (repeatable; default EVALSI_PLUGIN_INDEX, else Evals.si's)",
        )

    se = pl_sub.add_parser("search", help="search the index")
    se.add_argument("query", nargs="?", default="")
    index_arg(se)
    se.set_defaults(func=_cmd_search)

    sh = pl_sub.add_parser("show", help="show a plugin from the index")
    sh.add_argument("ref", help="name or name@version")
    index_arg(sh)
    sh.set_defaults(func=_cmd_show)

    ins = pl_sub.add_parser(
        "install", help="install from the index, or a Wasm manifest path or URL"
    )
    ins.add_argument(
        "ref", help="name[@version], ./path/evalsi-plugin.yaml or https://.../evalsi-plugin.yaml"
    )
    ins.add_argument("--dir", help="where Wasm plugins go (default: ~/.evalsi/plugins)")
    ins.add_argument("--sha256", default="", help="with a manifest URL: the manifest's sha256")
    ins.add_argument("--dry-run", action="store_true", help="say what would happen")
    index_arg(ins)
    ins.set_defaults(func=_cmd_install)

    ls = pl_sub.add_parser("list", help="installed packs and Wasm plugins, with their tiers")
    ls.set_defaults(func=_cmd_list)

    rm = pl_sub.add_parser("remove", help="remove an installed Wasm plugin")
    rm.add_argument("name")
    rm.add_argument("--dir", help="the plugins directory (default: ~/.evalsi/plugins)")
    rm.set_defaults(func=_cmd_remove)


def _dest(args: argparse.Namespace) -> Path:
    from evalsi.wasm import default_plugin_dir, plugin_dirs

    if args.dir:
        return Path(args.dir).expanduser()
    dirs = plugin_dirs()
    return dirs[0] if dirs else default_plugin_dir()


def _cmd_search(args: argparse.Namespace) -> int:
    from evalsi.plugins import load_index, search

    found = search(load_index(args.index), args.query)
    if not found:
        print("no plugins found", file=sys.stderr)
        return 1
    width = max(len(e.name) for e in found)
    for e in found:
        print(f"{e.name:<{width}}  {e.version:<10} {e.tier:<9} {e.kind:<6}  {e.description}")
    return 0


def _cmd_show(args: argparse.Namespace) -> int:
    from evalsi.plugins import find, load_index

    e = find(load_index(args.index), args.ref)
    print(f"{e.ref}  ({e.tier}, {e.kind})")
    for label, value in (
        ("description", e.description),
        ("homepage", e.homepage),
        ("license", e.license),
        ("evaluators", ", ".join(e.evaluators)),
        ("index", e.index),
    ):
        if value:
            print(f"  {label}: {value}")
    for k, v in e.source.items():
        print(f"  {e.kind}.{k}: {v}")
    return 0


def _cmd_install(args: argparse.Namespace) -> int:
    from evalsi.plugins import find, install, install_wasm, load_index

    dest = _dest(args)
    ref: str = args.ref
    if ref.endswith((".yaml", ".yml")) or re.match(r"^https?://", ref) or Path(ref).is_file():
        if args.dry_run:
            print(f"would install {ref} into {dest}")
            return 0
        path = install_wasm(ref, dest, args.sha256)
        print(f"installed {path.name} into {path}")
        return 0
    entry = find(load_index(args.index), ref)
    print(install(entry, dest, dry_run=args.dry_run))
    return 0


def _cmd_list(args: argparse.Namespace) -> int:
    from evalsi.registry import discover_packs

    packs = sorted(discover_packs(), key=lambda p: p.name)
    width = max(len(p.name) for p in packs)
    for p in packs:
        runtime = p.evaluators[0].spec.runtime if p.evaluators else "python"
        tier = p.tier or "community"
        print(f"{p.name:<{width}}  {tier:<9} {runtime:<6}  {len(p.evaluators)} evaluator(s)")
    return 0


def _cmd_remove(args: argparse.Namespace) -> int:
    from evalsi.plugins import plugin_dirname

    target = _dest(args) / plugin_dirname(args.name)
    if not (target / "evalsi-plugin.yaml").is_file():
        print(f"{args.name} is not installed in {target.parent}", file=sys.stderr)
        return 1
    shutil.rmtree(target)
    print(f"removed {args.name}")
    return 0
