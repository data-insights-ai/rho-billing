# rho-billing

Storage-free billing building blocks for Go services. The host keeps every
record in its own database and transactions; the library only decides.

Module: `github.com/data-insights-ai/rho-billing`. License: Apache 2.0.
No dependencies beyond the standard library.

| You need | Package |
|---|---|
| Prepaid credit: lots spent soonest-expiring first, reservations (hold, settle with overage, cancel), expiry and revocation | `credit` |
| Exact usage rating: fixed, weighted, included quota, graduated tiers, rounded once | `rating` |
| Subscriptions in provider-neutral terms: newest snapshot wins whatever the event order; status to access | `subscription` |
| The interface a payment-provider plugin offers (checkout, customer portal, catalog, charges, decoded events); Paddle: `rho-paddle` | `provider` |

## Credits

```go
book, err := credit.Open(lotsOfTheAccount) // loaded in the host's transaction, account locked
book.Expire(now)
r, err := book.Reserve("job-42", "soc", 500, now)   // all or nothing
s, err := book.Settle(r, 640, now)                   // s.Uncovered: overage beyond all credit
store(book.Changed(), book.Changes())                // same transaction: lots and journal
```

Every lot keeps `Initial = Available + Held + Consumed + Expired + Revoked`;
`Changes` are the movements as counter deltas for the host's journal. The
host serializes operations per account, stores reservations with their
state and deadline, and makes operations idempotent.

## Rating

`rating.NewRule` builds an immutable, versioned rule; `Rule.Rate` prices
metric quantities with exact rational arithmetic and one rounding step.

## History

Up to v0.3 this module also stored billing state itself (Postgres schema,
outbox, purchases, subscriptions). From v0.4 it holds no state; hosts on
v0.3 keep working on that version until they move.

## Subscriptions

```go
current, changed, err := subscription.Apply(stored, event.Subscription) // nil when unseen
if changed {
	store(current)                             // with the host's own consequences
	access := subscription.AccessOf(current.Status)
}
```
