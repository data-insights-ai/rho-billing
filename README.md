# rho-billing

Go library for SaaS billing that the host application embeds. It owns
entitlements, credit lots, usage rating, settlement and purchase intents.
Money collection stays in a payment provider. This repository contains no
payment-provider SDK.

Module: `github.com/data-insights-ai/rho-billing`. License: Apache 2.0. Go 1.26.
Release: `v0.3.0`.

The host owns authentication, membership, HTTP, workers and secrets.
A sibling adapter (`rho-paddle`) talks to Paddle. Constructors do no I/O
and start no workers.

## Install

```
require github.com/data-insights-ai/rho-billing v0.3.0
```

## What you get

| You need | Package |
|---|---|
| IDs, errors, clock, provider scope/reference/capability | `github.com/data-insights-ai/rho-billing` |
| Plans, entitlements, billing accounts | `catalog` |
| Credit lots, reservations, FEFO, spend limits, allowances | `credit` |
| Usage, rating, internal cost, settlement batches | `usage` |
| Quotes, payments, refunds, fulfillment | `purchase` |
| Subscription snapshots and seats | `subscription` |
| Inbox/outbox and host-run `ProcessDue`/`Reconcile` | `integration` |
| Postgres | `postgres` (`New`, `Migrate`, then `Credits()`, `Purchases()`, `Queue()`, `Settlements()`) |
| Read API | `query` |
| Conformance helpers | `billingtest` |

SQL lives in `internal/pg`. Do not import `internal/`.

The payment provider is the financial system of record: cards, tax, invoices,
dunning. This library is the application system of record: who may do what,
which credits exist, which usage is billable.

## Postgres

```go
db, err := sql.Open("pgx", os.Getenv("BILLING_DATABASE_URL"))
if err != nil {
    return err
}
store := postgres.New(db)
if err := store.Migrate(ctx); err != nil {
    return err
}
engine := credit.New(store.Credits(), nil)
```

`New` does not migrate. `Migrate` applies one baseline file
(`internal/pg/migrations/001_release.sql`) in one transaction. It does
not drop the database. A ledger from an earlier unreleased draft is
refused with `ErrConflict`.

Local fixture (loopback, test credentials only):

```sh
docker compose up -d --wait
export BILLING_TEST_DATABASE_URL='postgres://billing:billing-local-only@127.0.0.1:55438/billing?sslmode=disable'
sh scripts/check-local.sh
```

`check-local.sh` runs race tests, vet, fuzz and examples. It does not
call a payment provider.

## Examples

`examples/prepaid`, `examples/postpaid` and `examples/recovery` each
create an isolated schema, run one workflow, print JSON, and drop the
schema. They need `BILLING_DATABASE_URL`.

## Documentation

Exported types and tests are the contract. Beyond that:

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — the boundary model and the
  invariants a signature does not show.
- [docs/PROVIDER_CAPABILITIES.md](docs/PROVIDER_CAPABILITIES.md) — what a
  provider supports, and what fails closed.
- [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md),
  [CHANGELOG.md](CHANGELOG.md).

## Status

`v0.3.0`. Pre-1.0: the exported API may change between minor versions.
