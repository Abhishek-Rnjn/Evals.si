#!/usr/bin/env bash
# Runs every scenario of the TinyLM tutorials and checks its exit code:
# 0 (gates pass), 3 (a gate fails) or 1 (the run could not complete).
# It starts TinyLM itself, works in a temporary copy of this directory, and
# needs evalsi, evalsi-harness and a working sandbox (`evalsi doctor --require agent`).
#   bash examples/tinylm/check.sh
#   EVALSI="$PWD/.venv/bin/evalsi" bash ../examples/tinylm/check.sh       # from python/, in CI
set -euo pipefail

EVALSI=${EVALSI:-evalsi}
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
cp "$here"/*.yaml "$here"/*.jsonl "$here"/tinylm.py "$work"/

python3 "$work/tinylm.py" --port 0 --ready-file "$work/model-url" 2>"$work/tinylm.log" &
model=$!
cleanup() {
  if [ -n "$model" ]; then
    kill "$model" 2>/dev/null || true
    wait "$model" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT
ready=0
for _ in $(seq 50); do
  if ! kill -0 "$model" 2>/dev/null; then
    cat "$work/tinylm.log" >&2
    exit 1
  fi
  if [ -s "$work/model-url" ] && python3 - "$work/model-url" <<'PY'
import sys
import urllib.error
import urllib.request
from pathlib import Path

url = Path(sys.argv[1]).read_text().removesuffix("/v1") + "/healthz"
try:
    with urllib.request.urlopen(url, timeout=1) as response:
        if response.status != 200:
            raise SystemExit(1)
except (urllib.error.URLError, TimeoutError):
    raise SystemExit(1) from None
PY
  then
    ready=1
    break
  fi
  sleep 0.1
done
if [ "$ready" -ne 1 ]; then
  echo "TinyLM did not become ready" >&2
  cat "$work/tinylm.log" >&2
  exit 1
fi
url=$(cat "$work/model-url")
for spec in "$work"/*.yaml; do
  sed "s|http://127.0.0.1:8765/v1|$url|g" "$spec" >"$spec.tmp"
  mv "$spec.tmp" "$spec"
done

failed=0
expect() { # expect <code> <spec> [label]
  local want=$1 spec=$2 label=${3:-$2} got=0
  (cd "$work" && "$EVALSI" run -f "$spec" --output "$work/result.json" --quiet >"$work/out.txt" 2>&1) || got=$?
  if [ "$got" -eq "$want" ]; then
    if [ "$want" -ne 1 ] && ! python3 - "$work/result.json" "$work/$spec" <<'PY'
import json
import sys
from pathlib import Path

import yaml

data = json.loads(Path(sys.argv[1]).read_text())
spec = yaml.safe_load(Path(sys.argv[2]).read_text())["spec"]
assert len(data["records"]) == 3 * spec.get("trials", 1), "missing trial records"
assert data["summaries"], "no metric summaries"
assert all(s["errors"] == 0 for s in data["summaries"]), "evaluator errors"
PY
    then
      echo "FAIL  incomplete or errored results  $label"
      failed=1
      return
    fi
    echo "ok    exit $got  $label"
  else
    echo "FAIL  exit $got, want $want  $label"
    cat "$work/out.txt"
    failed=1
  fi
}
persona() { # persona <spec> <from> <to>: a copy of spec with another model
  sed "s/$2/$3/g" "$work/$1" >"$work/${1%.yaml}-$3.yaml"
  echo "${1%.yaml}-$3.yaml"
}

expect 0 run.yaml "tinylm-good fixes all three tasks"
expect 3 "$(persona run.yaml tinylm-good tinylm-lazy)" "tinylm-lazy claims success and does nothing"
expect 3 "$(persona run.yaml tinylm-good tinylm-partial)" "tinylm-partial fixes one bug per task"
expect 3 "$(persona run.yaml tinylm-good tinylm-looper)" "tinylm-looper hits max_steps"
expect 3 "$(persona run.yaml tinylm-good tinylm-hallucinator)" "tinylm-hallucinator calls a tool that does not exist"
expect 0 "$(persona run.yaml tinylm-good tinylm-clumsy)" "tinylm-clumsy recovers from malformed arguments"
expect 0 "$(persona run.yaml tinylm-good tinylm-sneaky)" "tinylm-sneaky passes a gate that ignores policy"
expect 3 flaky.yaml "tinylm-flaky fails pass^3"
expect 3 faults.yaml "injected faults: one trial runs out of steps"
expect 0 injection.yaml "tinylm-good ignores a prompt injection"
expect 3 "$(persona injection.yaml tinylm-good tinylm-gullible)" "tinylm-gullible follows it"
expect 0 approval.yaml "tinylm-pusher stops when a push is denied"
expect 3 "$(persona approval.yaml tinylm-pusher tinylm-stubborn)" "tinylm-stubborn retries the push"
expect 0 support.yaml "tinylm-support makes exactly the expected tool calls"
expect 3 "$(persona support.yaml tinylm-support tinylm-reckless)" "tinylm-reckless refunds without looking"
expect 0 record.yaml "record a session log"

kill $model
wait $model 2>/dev/null || true
model=""
sed 's/mode: record/mode: replay/' "$work/record.yaml" >"$work/replay.yaml"
expect 0 replay.yaml "replay it with the model stopped"
expect 1 run.yaml "a live run with the model stopped is an error, not a failed gate"

exit $failed
