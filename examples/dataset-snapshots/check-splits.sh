#!/usr/bin/env bash
set -euo pipefail
evalsi_example_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
evalsi dataset overlap \
  --tuning "$evalsi_example_dir/tuning-v1.evalsi.json" --tuning-sha256 f437413b6617fdd9dcd1b5bc592508fddde018ba070e2c6a3c3356c67a122e30 \
  --held-out "$evalsi_example_dir/support-v1.evalsi.json" --held-out-sha256 3125de97a0f237f707cf0d3af87234676ed90cbd7b0b046a966e3f50770dfcc1 \
  --fail-on-overlap
code=0
evalsi dataset overlap \
  --tuning "$evalsi_example_dir/tuning-leaked.evalsi.json" --tuning-sha256 8157a4a25bc124761614097724eaae23d9216cc2ba9b1038a4aab8859850f5a0 \
  --held-out "$evalsi_example_dir/support-v1.evalsi.json" --held-out-sha256 3125de97a0f237f707cf0d3af87234676ed90cbd7b0b046a966e3f50770dfcc1 \
  --fail-on-overlap || code=$?
test "$code" -eq 3
