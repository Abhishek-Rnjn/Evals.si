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