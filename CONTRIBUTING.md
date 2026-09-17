# Contributing

## Before you change behaviour

Read the [README](README.md) and [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).
The architecture document covers the invariants that a signature does not show:
the two systems of record, unknown outcomes, money arithmetic, time and
transaction boundaries. Most review comments on a first contribution come from
one of those.

## Local setup

Go 1.26 and Docker.

```sh
docker compose up -d --wait
export BILLING_TEST_DATABASE_URL='postgres://billing:billing-local-only@127.0.0.1:55438/billing?sslmode=disable'
sh scripts/check-local.sh
```

There are two gates.

`scripts/check.sh` needs no database: race tests, vet, the external host and
adapter modules and the fuzz targets. Database-backed tests skip themselves
when `BILLING_TEST_DATABASE_URL` is unset. This is what CI runs on every push.

`scripts/check-local.sh` is the full gate and additionally runs everything
against Postgres plus the three examples. That half is integration testing, so
CI runs it nightly and on demand rather than per push — which means **it is on
you to run it before opening a pull request.** Most of the concurrency, locking
and recovery coverage lives there.

Neither reads provider credentials or calls a payment provider.

The compose credentials are a loopback fixture and are the only credentials
that ever appear in this repository.

## What the review looks for

**Tests and exported APIs are the contract.** A behaviour change without a
named test that fails before it and passes after is not finished. Tests drive
the shipped code path — no hard-coded expected values that bypass the
computation, no starting halfway past the thing under test, no re-implementing
the logic in the test.

**Public surface is frozen by test.** `TestPublicPackageSetIsFrozen` and the
inventory tests in `internal/cmd/apiinventory` fail if a package appears or an
exported signature leaks an internal type. Adding a public package is a
deliberate act, not a side effect.

**Money and identity are reviewed hardest.** Anything touching credit lots,
rating, allocation or provider scope needs a Postgres test, not only a
memory-store one, because the concurrency and the constraints are the point.

**Comments explain constraints, not mechanics.** Write a comment when the rule
cannot be read off the code — why an identity excludes a field, why a retry is
unsafe. Do not narrate what the next line does. Stale prose is worse than none:
`TestLivingDocsCiteShippedPackages` fails if documentation names an API that no
longer exists.

**No provider SDK in this module.** Provider transport belongs in an adapter
module such as `rho-paddle`.

**Never commit a credential**, a customer payload or a secret-bearing log line
— in source, tests, fixtures, commit messages or documentation.

## Breaking changes

Pre-1.0, the exported API may change. Commercial plan and rating history is
immutable regardless: a released credit unit's scale never changes, and frozen
rating evidence is not rewritten. Migrations are additive and never implicitly
drop data.

## Pull requests

One concern per pull request. Say what changed and why, and paste the
`check-local.sh` result. If you could not run part of the gate, say which part
and why rather than leaving it unstated.
