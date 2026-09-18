# Changelog

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html);
pre-1.0, the exported API may change between minor versions.

## [0.3.1] - 2026-09-18

### Fixed

- `query.Entitlements` returned `ErrNotFound` for an account the store had
  never seen. An account nothing was ever sold to holds nothing; it is not a
  lookup failure, and a host that treated it as one fell back to a cache for a
  customer who never had one.

## [0.3.0] - 2026-09-17

Same code as 0.2.0, republished from a squashed history. 0.1.0 and 0.2.0 are
retracted.

## [0.2.0] - 2026-09-17

### Added

- `query`: `Entitlements`, the read model answering "what may this account do
  right now?" in one call. It resolves from catalog assignments minus
  revocations, needs no provider reference, and reports an account that never
  subscribed as empty rather than as an error.
- `catalog`: `EntitlementService.Holdings` with `EntitlementTx.AssignmentsPage`
  and `EntitlementTx.RevocationsPage`.

### Changed

- `catalog.EntitlementTx` gained two paging methods. Implementations outside
  this module must add them.

### Removed

- `subscription.Lifecycle.Assignments`. It was declared, validated, cloned and
  persisted, and written by nothing, so reading it returned an empty
  entitlement set that looked like a legitimate answer. Entitlements come from
  catalog assignments; use `query.Entitlements`.

### Fixed

- Listing an account's entitlement assignments was impossible: a single
  assignment could only be fetched by an id the caller already knew, so nothing
  could answer what an account held.

## [0.1.0] - 2026-09-17

First release.

### Added

- `billing`: IDs, errors, clock precision, `Scope`, `Reference` and typed
  provider capability results.
- `catalog`: plans, entitlements and billing accounts.
- `credit`: lots, reservations, first-expiring-first-out consumption, spend
  limits, allowances, projection rebuild and repair.
- `usage`: ingestion, rating with exact arithmetic, internal cost and
  settlement batches.
- `purchase`: immutable quotes, payments, refunds and fulfillment with explicit
  allocations.
- `subscription`: provider-observation snapshots and seats.
- `integration`: inbox, transactional outbox and host-run `ProcessDue` /
  `Reconcile`.
- `postgres`: `New`, `Migrate` and domain accessors over a single baseline
  schema.
- `query`: customer and operator read models with snapshot-bound cursors.
- `billingtest`: conformance helpers for repository implementations.
- `testdata/host` and `testdata/adapter` as separate modules that compile
  against the public API only.
- Examples for prepaid, postpaid and recovery workflows.
- `integration.ReleaseOutbox` returns an unsent outbound message to the queue
  when the provider refused it before it could take effect, so a rate limit or
  a rejected credential no longer terminally rejects a queued financial
  operation.

[0.1.0]: https://github.com/data-insights-ai/rho-billing/releases/tag/v0.1.0
