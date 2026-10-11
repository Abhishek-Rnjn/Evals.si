import assert from 'node:assert/strict';
import test from 'node:test';
import { BasicTracerProvider, SimpleSpanProcessor, InMemorySpanExporter } from '@opentelemetry/sdk-trace-base';
import { sessionSpans } from './index.mjs';

test('completed dsh turns export GenAI content and linked tool spans', async () => {
  const exporter = new InMemorySpanExporter();
  const provider = new BasicTracerProvider({spanProcessors: [new SimpleSpanProcessor(exporter)]});
  const receive = sessionSpans(provider.getTracer('test'), message => assert.fail(message));
  const session = {id: 'session-1'};
  const emit = (type, data) => receive(session, {type, data: {turn: 1, ...data}});
  emit('turn/start', {});
  receive(session, {type: 'user/message', data: {content: 'calculate 1+1'}});
  receive(session, {type: 'user/message', data: {content: [{type: 'text', text: 'runtime context'}]}});
  emit('tool/call', {callId: 'tool-1', name: 'calculate', arguments: '{}'});
  emit('tool/result', {callId: 'tool-1', message: {content: '2'}});
  emit('assistant/message', {message: {content: '2'}, usage: {input: 5, output: 1}});
  emit('turn/end', {reason: {kind: 'completed'}});
  await provider.forceFlush();
  const spans = exporter.getFinishedSpans();
  assert.equal(spans.length, 2);
  const root = spans.find(span => span.name === 'dsh.turn');
  const tool = spans.find(span => span.name === 'calculate');
  assert.deepEqual(JSON.parse(root.attributes['input.value']),
    ['calculate 1+1', {type: 'text', text: 'runtime context'}]);
  assert.equal(root.attributes['output.value'], '"2"');
  assert.equal(tool.spanContext().traceId, root.spanContext().traceId);
  assert.equal(tool.parentSpanContext.spanId, root.spanContext().spanId);
  await provider.shutdown();
});
test('a turn that never ended does not block the next one, and its span is closed as an error', async () => {
  const exporter = new InMemorySpanExporter();
  const provider = new BasicTracerProvider({spanProcessors: [new SimpleSpanProcessor(exporter)]});
  const warnings = [];
  const receive = sessionSpans(provider.getTracer('test'), message => warnings.push(message));
  const session = {id: 'session-1'};
  const emit = (type, data) => receive(session, {type, data: {turn: 1, ...data}});
  emit('turn/start', {});
  emit('tool/call', {callId: 'tool-1', name: 'search', arguments: {query: 'x'}});
  // The turn is aborted: no tool/result and no turn/end. The next turn starts.
  receive(session, {type: 'turn/start', data: {turn: 2}});
  receive(session, {type: 'assistant/message', data: {message: {content: 'second'}}});
  receive(session, {type: 'turn/end', data: {turn: 2, reason: {kind: 'completed'}}});
  await provider.forceFlush();
  const spans = exporter.getFinishedSpans();
  const turns = spans.filter(span => span.name === 'dsh.turn');
  assert.equal(turns.length, 2, 'both turns are exported');
  assert.equal(turns.find(span => span.attributes['dsh.turn'] === 1).status.code, 2, 'the aborted turn is an error');
  assert.equal(turns.find(span => span.attributes['dsh.turn'] === 2).attributes['output.value'], '"second"');
  const tool = spans.find(span => span.name === 'search');
  assert.equal(tool.status.code, 2, 'the open tool call is closed as an error');
  assert.equal(typeof tool.attributes['gen_ai.tool.call.arguments'], 'string', 'object arguments become text');
  assert.deepEqual(warnings, []);
  await provider.shutdown();
});

test('tool calls without an id, and sessions beyond the cap, are handled', async () => {
  const exporter = new InMemorySpanExporter();
  const provider = new BasicTracerProvider({spanProcessors: [new SimpleSpanProcessor(exporter)]});
  const receive = sessionSpans(provider.getTracer('test'), () => {});
  const session = {id: 's'};
  receive(session, {type: 'turn/start', data: {turn: 1}});
  receive(session, {type: 'tool/call', data: {name: 'a'}});
  receive(session, {type: 'tool/call', data: {name: 'b'}});
  receive(session, {type: 'turn/end', data: {reason: {kind: 'completed'}}});
  await provider.forceFlush();
  assert.equal(exporter.getFinishedSpans().filter(span => span.name === 'a' || span.name === 'b').length, 2,
    'two calls with no id are two spans, not one');
  exporter.reset();
  for (let index = 0; index < 1100; index += 1) {
    receive({id: `many-${index}`}, {type: 'turn/start', data: {turn: 1}});
  }
  await provider.forceFlush();
  // The oldest turns were closed to make room; nothing is refused or left open for good.
  assert.equal(exporter.getFinishedSpans().length, 1100 - 1024);
  receive.endAll();
  await provider.forceFlush();
  assert.equal(exporter.getFinishedSpans().length, 1100);
  await provider.shutdown();
});
