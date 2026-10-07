# evalsi-collector

An OpenTelemetry Collector distribution for sending agent and LLM traces to
evalsid for online evaluation. It is the standard collector with only the
components a GenAI pipeline needs:

- OTLP in and out;
- batching and memory limits;
- attribute, filter and transform processors, to drop or redact prompt
  content before it leaves;
- Kubernetes metadata;
- bearer-token auth to an evalsid with authentication on;
- health checks.

```bash
docker build -f deploy/collector/Dockerfile -t evalsi-collector .     # or ghcr.io/<owner>/evalsi-collector:<version>

# To an evalsid with TLS and authentication (an API key, or a service-account token):
docker run -p 4317:4317 -p 4318:4318 \
  -e EVALSI_OTLP_ENDPOINT=evalsi.example.com:443 -e EVALSI_TOKEN=evk_... evalsi-collector

# To a local evalsid without TLS or authentication (evalsid serve --no-auth):
docker run --network host -e EVALSI_OTLP_ENDPOINT=127.0.0.1:8080 evalsi-collector \
  --config /etc/evalsi-collector/config-dev.yaml
```

The default config refuses to send the token over plaintext; that is
gRPC's own rule. To keep a copy of the traces elsewhere (Langfuse, Phoenix,
Jaeger), add an exporter to the `traces` pipeline in a copy of
`config.yaml`.

`builder-config.yaml` pins the components (Collector Builder v0.162.0). To
build the binary without Docker:

```bash
go run go.opentelemetry.io/collector/cmd/builder@v0.162.0 --config deploy/collector/builder-config.yaml
```
