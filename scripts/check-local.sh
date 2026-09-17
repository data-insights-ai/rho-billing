#!/bin/sh
# Full local core gate. Use an isolated Postgres database; examples create and
# remove their own schemas. No billing-provider credentials are read.
set -eu
unset PADDLE_SANDBOX_TEST
unset PADDLE_SANDBOX_KEY_FILE
: "${BILLING_TEST_DATABASE_URL:?Set BILLING_TEST_DATABASE_URL to an isolated Postgres database}"
go test -race -count=1 -timeout 20m ./...
go vet ./...
sh scripts/check-external.sh
go test ./credit -run '^$' -fuzz '^FuzzCreditConservation$' -fuzztime=3s -parallel=2
go test ./usage -run '^$' -fuzz '^FuzzWeightedNeverPanicsOrReturnsNegative$' -fuzztime=3s -parallel=2
go test ./internal/canonicaljson -run '^$' -fuzz '^FuzzObjectCanonicalStable$' -fuzztime=3s -parallel=2
BILLING_DATABASE_URL="$BILLING_TEST_DATABASE_URL" go run ./examples/prepaid
BILLING_DATABASE_URL="$BILLING_TEST_DATABASE_URL" go run ./examples/postpaid
BILLING_DATABASE_URL="$BILLING_TEST_DATABASE_URL" go run ./examples/recovery
