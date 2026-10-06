"""Evaluator packs and reference resolution.

Packs are discovered through the ``evalsi.packs`` entry-point group, the same
way for built-in and third-party packs. A reference is ``namespace/name``,
optionally pinned with ``@version``; a bare ``name`` resolves when it is
unambiguous across installed packs.

Naming an evaluator explicitly (in ``evaluate()``, a run spec or on the
command line) is itself the opt-in. Pack enablement in project config, which
decides what policies may run and what a catalog shows, arrives with the
server in Phase 1.
"""

from __future__ import annotations

import logging
from collections.abc import Iterable
from dataclasses import dataclass, field, replace
from importlib.metadata import entry_points

from evalsi.evaluator import EvaluatorConfigError, EvaluatorDef

logger = logging.getLogger(__name__)


@dataclass
class Pack:
    name: str
    description: str
    evaluators: list[EvaluatorDef] = field(default_factory=list)
    on_by_default: bool = False


def _builtin_packs() -> list[Pack]:
    from evalsi.packs import agent, code, core, judge, rag, rl, safety, text

    return [core.PACK, judge.PACK, text.PACK, rag.PACK, safety.PACK, agent.PACK, code.PACK, rl.PACK]


class Registry:
    def __init__(self, packs: Iterable[Pack] | None = None) -> None:
        self.packs: dict[str, Pack] = {}
        self._by_name: dict[str, EvaluatorDef] = {}
        for pack in packs if packs is not None else discover_packs():
            self.add_pack(pack)

    def add_pack(self, pack: Pack) -> None:
        if pack.name in self.packs:
            raise EvaluatorConfigError(f"pack {pack.name!r} is registered twice")
        self.packs[pack.name] = pack
        for definition in pack.evaluators:
            self.add(definition, pack=pack.name)

    def add(self, definition: EvaluatorDef, pack: str = "") -> None:
        name = definition.spec.name
        if name in self._by_name:
            raise EvaluatorConfigError(f"evaluator {name!r} is registered twice")
        if pack and not definition.spec.pack:
            definition.spec = replace(definition.spec, pack=pack)
        self._by_name[name] = definition

    def evaluators(self) -> list[EvaluatorDef]:
        return sorted(self._by_name.values(), key=lambda d: d.spec.name)

    def resolve(self, ref: str) -> EvaluatorDef:
        name, _, version = ref.partition("@")
        if "/" in name:
            definition = self._by_name.get(name)
        else:
            matches = [d for n, d in self._by_name.items() if n.rsplit("/", 1)[-1] == name]
            if len(matches) > 1:
                options = ", ".join(sorted(d.spec.name for d in matches))
                raise EvaluatorConfigError(f"{name!r} is ambiguous; use one of: {options}")
            definition = matches[0] if matches else None
        if definition is None:
            raise EvaluatorConfigError(
                f"unknown evaluator {ref!r}; run `evalsi catalog` to see what is installed"
            )
        if version and version != definition.spec.version:
            raise EvaluatorConfigError(
                f"{ref!r} is pinned, but the installed version is {definition.spec.version}"
            )
        return definition


def discover_packs() -> list[Pack]:
    """Built-in packs plus every pack registered under the ``evalsi.packs`` entry point."""
    packs = {p.name: p for p in _builtin_packs()}
    for ep in entry_points(group="evalsi.packs"):
        if ep.name in packs:
            continue  # built-ins register themselves too; the in-tree copy wins
        try:
            pack = ep.load()
        except Exception:
            logger.exception("could not load evaluator pack %r", ep.name)
            continue
        if not isinstance(pack, Pack):
            logger.warning("entry point %r is not an evalsi Pack; ignoring it", ep.name)
            continue
        packs[pack.name] = pack
    return list(packs.values())


_default: Registry | None = None


def default_registry() -> Registry:
    global _default  # noqa: PLW0603 - a lazily built process-wide registry
    if _default is None:
        _default = Registry()
    return _default
