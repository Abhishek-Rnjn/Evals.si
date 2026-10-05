from __future__ import annotations

from collections.abc import Callable, Iterator

import pytest

from evalsi_harness.testing import LocalSandboxClient, Script, ScriptedModelServer


@pytest.fixture
def sandboxes() -> LocalSandboxClient:
    return LocalSandboxClient()


@pytest.fixture
def model() -> Iterator[Callable[[Script], ScriptedModelServer]]:
    servers: list[ScriptedModelServer] = []

    def start(script: Script) -> ScriptedModelServer:
        server = ScriptedModelServer(script)
        servers.append(server)
        return server

    yield start
    for s in servers:
        s.close()
