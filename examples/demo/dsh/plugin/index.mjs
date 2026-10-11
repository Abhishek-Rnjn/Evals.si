import { context, trace, SpanStatusCode } from '@opentelemetry/api';
import { resourceFromAttributes } from '@opentelemetry/resources';
import { BasicTracerProvider, BatchSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-proto';

const MAX_OPEN_TURNS = 1024;

// sessionSpans turns dsh session events into GenAI spans. A turn that never
// ends (the session aborted, or dsh was disposed) must not stay open for good,
// block its session's next turn, or fill the table: it is closed as an error.
export function sessionSpans(tracer, report) {
  const turns = new Map();
  const closeTurn = (key, reason) => {
    const turn = turns.get(key);
    if (!turn) return;
    for (const tool of turn.tools.values()) {
      tool.setStatus({code: SpanStatusCode.ERROR});
      tool.end();
    }
    if (reason) turn.span.setStatus({code: SpanStatusCode.ERROR, message: reason});
    turn.span.end();
    turns.delete(key);
  };
  const handle = (session, event) => {
    const key = session.id;
    if (event.type === 'turn/start') {
      if (turns.has(key)) closeTurn(key, 'the previous turn did not end');
      if (turns.size >= MAX_OPEN_TURNS) {
        report('dsh GenAI span capacity reached; closing the oldest open turn');
        closeTurn(turns.keys().next().value, 'closed to make room');
      }
      turns.set(key, {span: tracer.startSpan('dsh.turn', {
        attributes: {'gen_ai.operation.name': 'invoke_agent', 'gen_ai.agent.name': 'dsh',
          'session.id': String(session.id), 'dsh.turn': event.data.turn},
      }), tools: new Map(), inputs: [], calls: 0});
      return;
    }
    const turn = turns.get(key);
    if (!turn) return;
    const data = event.data;
    if (event.type === 'user/message') {
      const content = data.message?.content ?? data.content;
      turn.inputs.push(...(Array.isArray(content) ? content : [content]));
      turn.span.setAttribute('input.value', JSON.stringify(turn.inputs));
    } else if (event.type === 'assistant/message') {
      turn.span.setAttribute('output.value', JSON.stringify(data.message?.content));
      if (data.usage) {
        turn.span.setAttribute('gen_ai.usage.input_tokens', data.usage.inputTokens ?? data.usage.input ?? 0);
        turn.span.setAttribute('gen_ai.usage.output_tokens', data.usage.outputTokens ?? data.usage.output ?? 0);
      }
    } else if (event.type === 'tool/call') {
      // A call with no id would share one map slot with every other such call.
      const callId = String(data.callId ?? `call-${turn.calls += 1}`);
      const args = typeof data.arguments === 'string' ? data.arguments : JSON.stringify(data.arguments ?? '');
      turn.tools.get(callId)?.end();
      const parent = trace.setSpan(context.active(), turn.span);
      turn.tools.set(callId, tracer.startSpan(data.name ?? 'dsh.tool', {
        attributes: {'gen_ai.operation.name': 'execute_tool', 'gen_ai.tool.name': data.name ?? '',
          'gen_ai.tool.call.id': callId, 'gen_ai.tool.call.arguments': args},
      }, parent));
    } else if (event.type === 'tool/result') {
      const tool = turn.tools.get(String(data.callId));
      if (tool) {
        tool.setAttribute('output.value', JSON.stringify(data.message?.content ?? data.content ?? data.result));
        if (data.message?.isError ?? data.isError) tool.setStatus({code: SpanStatusCode.ERROR});
        tool.end();
        turn.tools.delete(String(data.callId));
      }
    } else if (event.type === 'turn/end') {
      closeTurn(key, data.reason?.kind === 'error' ? 'the turn failed' : undefined);
    }
  };
  // endAll closes every open turn, for when the process is shutting down.
  handle.endAll = () => {
    for (const key of [...turns.keys()]) closeTurn(key, 'the session ended before the turn did');
  };
  return handle;
}

export default function apply(ctx) {
  const endpoint = process.env.DEMO_OTLP_ENDPOINT;
  if (!endpoint) return;
  const headers = {};
  if (process.env.DEMO_MLFLOW_EXPERIMENT_ID) {
    headers['x-mlflow-experiment-id'] = process.env.DEMO_MLFLOW_EXPERIMENT_ID;
  }
  if (process.env.DEMO_OTLP_API_KEY) headers.Authorization = `Bearer ${process.env.DEMO_OTLP_API_KEY}`;
  const exporter = new OTLPTraceExporter({url: endpoint, headers, timeoutMillis: 10000});
  const provider = new BasicTracerProvider({
    resource: resourceFromAttributes({'service.name': 'evalsi-demo-dsh'}),
    spanProcessors: [new BatchSpanProcessor(exporter, {
      maxQueueSize: 1024, maxExportBatchSize: 64, scheduledDelayMillis: 100,
      exportTimeoutMillis: 10000,
    })],
  });
  const spans = sessionSpans(provider.getTracer('evalsi.dsh'), message => ctx.logger.warn(message));
  ctx.on('session/event', spans);
  ctx.on('session/flush', () => provider.forceFlush());
  ctx.on('dispose', () => {
    spans.endAll();
    return provider.shutdown();
  });
}