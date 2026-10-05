#!/usr/bin/env bash
# Regenerates the Python protobuf and gRPC modules from proto/ into the evalsi
# package (evalsi.v1alpha1, evalsi.plugin.v1alpha1, evalsi.sandbox.v1alpha1,
# evalsi.harness.v1alpha1). Run from anywhere; uses the grpcio-tools version
# pinned in python/pyproject.toml's dev group.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/python/evalsi/src"
# Directories that hold generated code only. evalsi/sandbox and evalsi/harness
# are hand-written packages; only their v1alpha1 subpackages are generated.
# The guest agent's protocol is internal to evalsid and has no Python side.
generated=$(cd "$root/proto" && find evalsi -name '*.proto' -not -path 'evalsi/guest/*' -exec dirname {} \; | sort -u)
for dir in $generated; do
  rm -rf "${out:?}/$dir"
done
cd "$root/python"
protos=$(cd "$root/proto" && find evalsi -name '*.proto' -not -path 'evalsi/guest/*' | sort)
# shellcheck disable=SC2086
uv run --no-sync python -m grpc_tools.protoc -I "$root/proto" \
  --python_out="$out" --mypy_out="$out" --grpc_python_out="$out" --mypy_grpc_out="$out" $protos
for dir in $generated; do
  # Files without services get empty gRPC modules; drop them.
  for f in "$out/$dir"/*_pb2_grpc.py; do
    grep -q "_to_server" "$f" || rm "$f" "${f}i"
  done
  printf '"""Generated from proto/. Do not edit; run scripts/gen-python-proto.sh."""\n' > "$out/$dir/__init__.py"
  parent="$(dirname "$dir")"
  if [ "$parent" != "evalsi" ] && [ ! -f "$out/$parent/__init__.py" ]; then
    printf '"""Generated from proto/. Do not edit; run scripts/gen-python-proto.sh."""\n' > "$out/$parent/__init__.py"
  fi
done
