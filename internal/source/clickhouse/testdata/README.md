# Collector trace schema

`traces_table.sql` is the unmodified `traces_table.sql` template from
OpenTelemetry Collector contrib **v0.162.0**, at
`exporter/clickhouseexporter/internal/sqltemplates/traces_table.sql`:

https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/v0.162.0/exporter/clickhouseexporter/internal/sqltemplates/traces_table.sql

The live tests render its database/table/cluster/engine/TTL placeholders as
the exporter does, then insert synthetic spans. This checks the actual Map
schema and SQL against ClickHouse; it does not run the Collector or certify
its experimental JSON schema, every Collector version or every deployment.
