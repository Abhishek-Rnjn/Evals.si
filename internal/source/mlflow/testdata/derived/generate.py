"""Builds the Databricks Unity Catalog fixtures from the recorded MLflow 3.17.0 ones.

These are NOT recorded from a Databricks workspace. Their shape comes from
mlflow-skinny 3.17.0 (mlflow/protos/databricks_tracing_pb2: SearchTracesOperation,
BatchGetTraces.Response, Assessment); their spans are the recorded OSS spans, which
are OTLP spans in protobuf JSON in both APIs. Run: python3 generate.py
"""
import json, pathlib

here = pathlib.Path(__file__).parent
oss = json.loads((here.parent / "batchget.json").read_text())["traces"][0]
tid = "f864e522377586ed0a722b8a01c9a611"
info = {
    "trace_id": tid,
    "trace_location": {"type": "UC_SCHEMA", "uc_schema": {"catalog_name": "main", "schema_name": "agents"}},
    "request_time": oss["trace_info"]["request_time"],
    "execution_duration": oss["trace_info"]["execution_duration"],
    "state": "OK",
    "trace_metadata": {k: v for k, v in oss["trace_info"]["trace_metadata"].items() if k.startswith("mlflow.trace.size")},
    "tags": {"mlflow.traceName": "agent"},
}
out = {
    "uc_search_pending": {"name": "operations/search-1", "metadata": {}, "done": False},
    "uc_search_done": {"name": "operations/search-1", "done": True, "response": {"trace_infos": [info], "next_page_token": ""}},
    "uc_search_failed": {"name": "operations/search-1", "done": True, "error": {"error_code": "INVALID_PARAMETER_VALUE", "message": "Could not resolve a SQL warehouse ID"}},
    "uc_batchget": {"traces": [{"trace_info": info, "spans": oss["spans"]}]},
    "uc_assess_create": {
        "assessment_id": "a-uc-1", "assessment_name": "task-success", "trace_id": tid,
        "trace_location": info["trace_location"],
        "source": {"source_type": "CODE", "source_id": "evalsi/studio-agents"},
        "feedback": {"value": 1.0}, "rationale": "all tests pass", "valid": True,
    },
}
for name, body in out.items():
    (here / (name + ".json")).write_text(json.dumps(body, indent=2) + "\n")
