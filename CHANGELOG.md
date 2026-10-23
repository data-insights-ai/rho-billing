# Changelog

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html);
pre-1.0, the exported API may change between minor versions.

## [0.8.0] - 2026-10-23

### Added

- `provider.CustomItem.TrialDays`: a recurring custom item's first charge
  waits that many days (a contract that starts later); one-time items are
  still charged at once.
- `provider.Provider.SetNextBilling`: move a subscription's next renewal
  without billing the change (anchor a subscription to a contract's start or
  a contract year's boundary).

## [0.7.0] - 2026-10-23

### Added

- `provider.CheckoutRequest.Custom` (`CustomItem`): prices the host sets
  for one checkout instead of catalog prices, recurring or one-time
  (contracts with negotiated amounts, price tiers kept in the host's
  catalog). `CustomItem.Validate` checks one before it is sent.
- `provider.Provider.ReplaceItems` (`ReplaceRequest`): new custom items on
  a subscription from its next renewal, without billing the change (a
  contract's price for its next year).
- `provider.Provider.CancelAtPeriodEnd`: end a subscription when its
  current period ends (a contract's last year).

## [0.6.0] - 2026-10-22

### Added

- `provider.Provider.CreateCustomer` (`CustomerRequest`): a host creates
  the provider's customer for an account before its first checkout, so
  the checkout and every payment are bound to it.
- `provider.Provider.GetTransaction` (`Transaction`): a host reads a
  transaction it did not start (a renewal-recovery link) to see which
  customer and subscription it belongs to.

## [0.5.0] - 2026-10-20

### Added

- `subscription`: a provider's subscription in neutral terms. `Apply`
  keeps the newest snapshot by revision (provider time, then event id), so
  late, repeated and out-of-order events change nothing; `AccessOf` maps a
  status to full, grace or no access.
- `provider`: the interface a payment-provider plugin offers: checkout,
  customer portal, catalog, one-time charges, and decoded events
  (subscription changed, payment completed).

## [0.4.1] - 2026-10-20

### Added

- `credit.Book.Lots`: every lot as it is now.

## [0.4.0] - 2026-10-20

### Changed

- The library is storage-free. `credit` is rebuilt as pure functions over
  lots the host loads: soonest-expiring-first spending, all-or-nothing
  reservations, settlement with usage beyond the hold (overage reported as
  uncovered), cancel, expiry and revocation, journal changes as counter
  deltas. A reservation without scope no longer spends scoped credit.
- Rating moves from `usage` to `rating`, unchanged.

### Removed

- Postgres storage, migrations, outbox and inbox, `catalog`, `subscription`,
  `purchase`, `usage` settlement, `query`, `integration`, allowances, spend
  limits and `billingtest`. Hosts that use them stay on v0.3.7.

## [0.3.2] - 2026-09-18

### Added

- `purchase`: `Service.RekeyCollectionLines` and `CollectionTx.ReplaceCollectionBinding`.
  A provider may re-issue its line ids when it recomputes a transaction while
  the lines themselves, price, quantity and what they pay for, stay what the
  host bound. Re-keying replaces the ids under exactly that condition; any
  commercial change is refused as a conflict, and replaying the current ids is
  a no-op. The paid transaction thereby becomes the reference that refunds and
  adjustments resolve against.

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
