# Derived fixtures (not recorded)

Unlike the files in `..`, which are responses recorded from a live MLflow 3.17.0 server, nothing here
was recorded from Databricks or SageMaker. Each file says what it was built from.

| File | Built from |
|---|---|
| `uc_search_pending.json`, `uc_search_done.json`, `uc_search_failed.json` | `SearchTracesOperation` in `mlflow/protos/databricks_tracing_pb2` (mlflow-skinny 3.17.0): `name`, `done`, `error`, and `response.trace_infos`/`next_page_token`. The failure message is the one `DatabricksTracingRestStore._call_endpoint` matches ("Could not resolve a SQL warehouse ID"). |
| `uc_batchget.json` | `BatchGetTraces.Response` (`traces[].trace_info`, `traces[].spans`), with `trace_location.uc_schema` for the location. The spans are the recorded OSS spans, which are OTLP spans in protobuf JSON in both APIs. |
| `uc_assess_create.json` | `Assessment` in the same module, with `trace_location`. |
| `sigv4_vectors.json` | Signatures botocore 1.x `SigV4Auth` produced for fixed requests, credentials and time (the AWS documentation's example key pair). `sagemaker-mlflow` 0.5.0 signs with botocore's `SigV4Auth`. |

`generate.py` rebuilds the `uc_*` files from the recorded ones.

The request paths, methods and parameters the tests expect come from mlflow-skinny 3.17.0
(`mlflow/store/tracking/databricks_rest_store.py`: `/api/4.0/mlflow/traces/search-long-running`,
`.../search/operations/{name}`, `.../{location}/batchGet`, `.../{location}/{trace_id}/assessments[/{id}]`,
`sql_warehouse_id` as a query parameter on the assessment calls), and the SageMaker host, header and service
names from `sagemaker-mlflow` 0.5.0 (`mlflow_sagemaker_helpers.py`, `mlflow_sagemaker_request_header_provider.py`,
`auth_provider.py`). A real workspace or tracking server can still differ; see docs/guides/trace-sources.md.
