# Architecture

Why the library is shaped this way. The exported types and the tests are the
contract; this page covers the decisions you cannot read off a signature.

## Two systems of record

The payment provider is the financial system of record. It owns cards, tax,
invoices, dunning and the money itself.

This library is the application system of record. It owns who may do what,
which credits exist, and which usage is billable.

Neither side derives its state from the other. A provider payment status is
never an implicit access decision, and an entitlement in this library is never
proof that money moved. Crossing that line is how billing systems end up
granting access for a payment that later reverses.

## The host owns I/O

Constructors take repositories and a clock. They open no connections, run no
migrations and start no goroutines. `postgres.New` does no I/O; `Migrate` is a
separate explicit call.

The host owns HTTP routing, authentication, membership, worker scheduling and
secret storage. The library exposes work to do (`ProcessDue`, `Reconcile`) and
the host decides when and how often to run it. There is no hidden scheduler,
so there is nothing to shut down and nothing that keeps running after a test.

Account IDs are not credentials. The host authorizes the caller before it calls
the library; the library does not re-check identity it was never given.

## Provider identity

Provider identity is `billing.Scope` plus `billing.Reference`. Scope separates
provider, merchant and environment. Merchant is not a tenant key: two tenants
can share a merchant, and one tenant can exist in both sandbox and production.
Keys and cursors carry the scope so a sandbox object can never alias a
production one.

## Unknown outcomes are a state, not an error

A request that may have mutated provider state and then timed out is neither
success nor failure. It is recorded as unknown and resolved by lookup. It is
never retried blindly, because a blind retry on a payment mutation charges
twice. Adapters must resolve an operation's capability before dispatch — the
existence of a Go method is not evidence the provider supports the operation.

`billing.RequireSupported` rejects every capability result other than
supported, so an unresolved capability fails closed rather than falling through
to an attempt.

## Money arithmetic

Currency amounts are integer minor units. Credit amounts are integer subunits
of a named unit whose scale is fixed when the unit is published and never
changes afterwards. Intermediate rating arithmetic is exact rational; rounding
is applied once, to the complete result, never per component. There is no
implicit currency conversion.

## Time

All financial timestamps come from the injected clock, in UTC microseconds, to
match what PostgreSQL `timestamptz` can actually store. Periods are absolute
half-open intervals. Calendar recurrence is a separate concern: a monthly
schedule preserves its anchor rather than adding a fixed duration, so month
lengths and leap days stay correct.

Named-zone wall-clock recurrence is not supported. Schedules convert to UTC.

## Credit consumption

Credits live in lots with their own expiry and provenance. Consumption is
first-expiring-first-out across eligible lots, so a lot that expires sooner is
spent before one that expires later regardless of when either was granted.
Purchased, plan-included and promotional credits keep distinct provenance even
when they are spent in the same operation, because a refund has to know which
kind it is reversing.

Reservations hold balance before work runs. An expired lot is unusable
immediately, without waiting for a cleanup pass.

## Transaction boundaries

Cross-domain work goes through `Atomic`. Inside the callback, use the matching
`session.*()` port so every effect shares the host transaction. Repository
callbacks commit only on a nil error, and no handle may escape the callback.

Recorded business outcomes are distinct from infrastructure failures. A
settlement the provider rejected is a `*SettlementRejection` the host may
commit; a storage failure must abort. Match with `errors.AsType` before
deciding which one you have.

## Packages

`internal/` is not importable by a host, and the public package set is frozen
by test. SQL lives in `internal/pg`; the `postgres.Store` facade is lifecycle
plus domain accessors and deliberately does not implement the domain
repositories. Do not type-assert it back to the implementation type — that
assertion is the thing the facade exists to prevent.

`testdata/host` and `testdata/adapter` are separate Go modules. They compile
against the public API only, which is what makes them a real external-consumer
check rather than an internal test.
