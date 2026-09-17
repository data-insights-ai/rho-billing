#!/bin/sh
# Unit gate: no database, no provider credentials, no network.
# Database-backed tests skip themselves when BILLING_TEST_DATABASE_URL is unset,
# so this is the whole suite minus its integration half. Run
# scripts/check-local.sh before opening a pull request.
set -eu
cd "$(dirname "$0")/.."
unset BILLING_TEST_DATABASE_URL
unset PADDLE_SANDBOX_TEST
unset PADDLE_SANDBOX_KEY_FILE
go test -race -count=1 -timeout 10m ./...
go vet ./...
sh scripts/check-external.sh
go test ./credit -run '^$' -fuzz '^FuzzCreditConservation$' -fuzztime=3s -parallel=2
go test ./usage -run '^$' -fuzz '^FuzzWeightedNeverPanicsOrReturnsNegative$' -fuzztime=3s -parallel=2
go test ./internal/canonicaljson -run '^$' -fuzz '^FuzzObjectCanonicalStable$' -fuzztime=3s -parallel=2
