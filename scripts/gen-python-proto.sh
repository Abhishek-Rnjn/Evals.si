#!/usr/bin/env bash
# Regenerates the Python protobuf and gRPC modules from proto/ into the evalsi
# package (evalsi.v1alpha1, evalsi.plugin.v1alpha1). Run from anywhere; uses the
# grpcio-tools version pinned in python/pyproject.toml's dev group.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/python/evalsi/src"
rm -rf "$out/evalsi/v1alpha1" "$out/evalsi/plugin"
cd "$root/python"
protos=$(cd "$root/proto" && find evalsi -name '*.proto' | sort)
# shellcheck disable=SC2086
uv run --no-sync python -m grpc_tools.protoc -I "$root/proto" \
  --python_out="$out" --mypy_out="$out" --grpc_python_out="$out" --mypy_grpc_out="$out" $protos
# Files without services get empty gRPC modules; drop them.
for f in "$out"/evalsi/v1alpha1/*_pb2_grpc.py "$out"/evalsi/plugin/v1alpha1/*_pb2_grpc.py; do
  grep -q "_to_server" "$f" || rm "$f" "${f}i"
done
for dir in "$out/evalsi/v1alpha1" "$out/evalsi/plugin" "$out/evalsi/plugin/v1alpha1"; do
  printf '"""Generated from proto/. Do not edit; run scripts/gen-python-proto.sh."""\n' > "$dir/__init__.py"
done
