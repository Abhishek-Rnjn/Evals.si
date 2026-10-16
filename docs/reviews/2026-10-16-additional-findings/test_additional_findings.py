"""Evidence tests for R7–R10; passing means the reported bug is present."""

import asyncio
import importlib
import json
import sys
from pathlib import Path
from unittest.mock import patch

import httpx
import pytest

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / 'python/evalsi-harness/tests'))
from harness_support import spec_of, record, run, judge_answering
from evalsi_harness import HarnessContext, Task, load_harness, run_task
from evalsi_harness.testing import LocalSandboxClient
from evalsi_harness.connectors import HTTPConnector
from evalsi_harness.codemode import Interpreter, MAX_SEQUENCE
from evalsi_harness.models import ModelTurn
from evalsi.v1alpha1 import agent_pb2


def test_slice_assignment_bypasses_code_mode_size_limit():
    source = 'values = [0] * 1000000\nvalues[:0] = values\nprint(len(values))'
    output = asyncio.run(Interpreter({}, {}).run(source))
    assert int(output) == 2 * MAX_SEQUENCE
    print(f'CONFIRMED code mode retained {output.strip()} elements; limit={MAX_SEQUENCE}')


def test_http_input_placeholder_is_reinterpreted():
    async def go():
        cfg = agent_pb2.HTTPAgent(url='https://agent.invalid', output_path='answer')
        connector = HTTPConnector(cfg, type('Task', (), {'id': 'task-1'})())
        with pytest.raises(Exception, match='body_template is not JSON') as exc:
            await connector.send('Explain {{task_id}}')
        await connector.aclose()
        print(f'CONFIRMED literal input breaks default HTTP template: {exc.value}')
    asyncio.run(go())


def test_invalid_json_strands_harness_consumer():
    async def go():
        cfg = agent_pb2.HTTPAgent(url='https://agent.invalid', output_path='answer')
        connector = HTTPConnector(cfg, type('Task', (), {'id': 'task-1'})())
        await connector.client.aclose()
        connector.client = httpx.AsyncClient(transport=httpx.MockTransport(
            lambda req: httpx.Response(200, text='<html>upstream unavailable</html>')))
        spec = spec_of({'target': {'agent': {'http': {'url': cfg.url, 'output_path': 'answer'}}}})
        async def sandboxes():
            return LocalSandboxClient()
        harness = load_harness(spec, HarnessContext(sandboxes=sandboxes))
        task = Task.build(spec, record('hello'), trial=0, run_id='review')
        producers = []
        create_task = asyncio.create_task
        def capture(coro, *args, **kwargs):
            task = create_task(coro, *args, **kwargs)
            producers.append(task)
            return task
        with patch('evalsi_harness.connectors.connector_for', return_value=connector), patch('asyncio.create_task', side_effect=capture):
            with pytest.raises(TimeoutError):
                await asyncio.wait_for(run_task(task, harness), timeout=0.25)
        await harness.aclose()
        assert any(t.done() and not t.cancelled() and isinstance(t.exception(), json.JSONDecodeError) for t in producers)
        print('CONFIRMED invalid JSON killed producer; run_task required external cancellation')
    asyncio.run(go())


def test_replay_reruns_user_simulator_and_changes_answer(tmp_path):
    class Model:
        name = 'fake-model'
        def __init__(self):
            self.turns = iter(['What is your order number?', 'Order 42 ships tomorrow.'])
        async def complete(self, *args, **kwargs):
            return ModelTurn(text=next(self.turns))
        async def aclose(self):
            pass
    def spec(mode):
        return spec_of({
            'target': {'connector': 'openai-compatible', 'model': 'fake-model', 'base_url': 'https://model.invalid'},
            'harness': {'builtin': {'recording': {'mode': mode, 'dir': str(tmp_path)},
                                    'user_simulator': {'persona': 'customer'}}}})
    responses = iter([{'message': '42', 'done': False}, {'message': '', 'done': True}])
    harness_module = importlib.import_module('evalsi_harness.harness')
    with patch.object(harness_module, 'create_model', return_value=Model()):
        original = run(spec('record'), record('Where is my order?'), LocalSandboxClient(),
                       judge=judge_answering(lambda p, s: next(responses)))
    calls = []
    def changed_judge(prompt, schema):
        calls.append(prompt)
        return {'message': '', 'done': True}
    with patch.object(harness_module, 'create_model', return_value=Model()):
        replay = run(spec('replay'), record('Where is my order?'), LocalSandboxClient(),
                     judge=judge_answering(changed_judge))
    assert original.record is not None, original.error
    assert replay.record is not None, replay.error
    before = original.record.output.as_text()
    after = replay.record.output.as_text()
    assert before != after
    assert len(calls) == 1
    print(f'CONFIRMED replay called live judge and changed output: {before!r} -> {after!r}')
