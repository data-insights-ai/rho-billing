#!/bin/sh
set -eu

: "${BILLING_TEST_DATABASE_URL:?Set BILLING_TEST_DATABASE_URL to an isolated PostgreSQL database}"
rows=${ROWS:-1000}
chunksize=${CHUNKSIZE:-500}
seed=${SEED:-1}
output=${OUTPUT_JSON:-artifacts/close-${rows}.json}
mkdir -p "$(dirname "$output")"
# Record the executable harness source independently of its evolving report.
BILLING_LOAD_HARNESS_HASH=$(python3 -c 'import hashlib; from pathlib import Path; print(hashlib.sha256(Path("internal/cmd/billingload/main.go").read_bytes()).hexdigest())')
export BILLING_LOAD_HARNESS_HASH
go run ./internal/cmd/billingload -rows "$rows" -chunksize "$chunksize" -seed "$seed" -output-json "$output"

if [ "$rows" -eq 100000 ]; then
  python3 - "$output" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    result = json.load(f)
if result["total_seconds"] > 60:
    raise SystemExit(f"100k close exceeded 60s: {result['total_seconds']:.3f}s")
if result["chunk_max"] > 2_000_000_000:
    raise SystemExit(f"close chunk exceeded 2s: {result['chunk_max']}")
PY
fi
