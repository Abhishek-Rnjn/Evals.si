import { context, trace, SpanStatusCode } from '@opentelemetry/api';
import { resourceFromAttributes } from '@opentelemetry/resources';
import { BasicTracerProvider, BatchSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-proto';

export function sessionSpans(tracer, report) {
  const turns = new Map();
  return (session, event) => {
    const key = session.id;
    if (event.type === 'turn/start') {
      if (turns.size >= 1024 || turns.has(key)) {
        report('dsh GenAI span capacity exhausted or duplicate turn');
        return;
      }
      turns.set(key, {span: tracer.startSpan('dsh.turn', {
        attributes: {'gen_ai.operation.name': 'invoke_agent', 'gen_ai.agent.name': 'dsh',
          'session.id': String(session.id), 'dsh.turn': event.data.turn},
      }), tools: new Map(), inputs: []});
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
      const parent = trace.setSpan(context.active(), turn.span);
      turn.tools.set(data.callId, tracer.startSpan(data.name ?? 'dsh.tool', {
        attributes: {'gen_ai.operation.name': 'execute_tool', 'gen_ai.tool.name': data.name ?? '',
          'gen_ai.tool.call.id': data.callId, 'gen_ai.tool.call.arguments': data.arguments ?? ''},
      }, parent));
    } else if (event.type === 'tool/result') {
      const tool = turn.tools.get(data.callId);
      if (tool) {
        tool.setAttribute('output.value', JSON.stringify(data.message?.content ?? data.content ?? data.result));
        if (data.message?.isError ?? data.isError) tool.setStatus({code: SpanStatusCode.ERROR});
        tool.end();
        turn.tools.delete(data.callId);
      }
    } else if (event.type === 'turn/end') {
      for (const tool of turn.tools.values()) {
        tool.setStatus({code: SpanStatusCode.ERROR});
        tool.end();
      }
      if (data.reason?.kind === 'error') turn.span.setStatus({code: SpanStatusCode.ERROR});
      turn.span.end();
      turns.delete(key);
    }
  };
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
  ctx.on('session/event', sessionSpans(provider.getTracer('evalsi.dsh'), message => ctx.logger.warn(message)));
  ctx.on('session/flush', () => provider.forceFlush());
  ctx.on('dispose', () => provider.shutdown());
}