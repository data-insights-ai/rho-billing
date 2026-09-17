# Examples

## Postpaid settlement and recovery

Run the example with a PostgreSQL connection in `BILLING_DATABASE_URL`:

```sh
BILLING_DATABASE_URL='postgres://billing:billing-local-only@127.0.0.1:55438/billing?sslmode=disable' go run ./examples/postpaid
```

The command creates and drops a unique PostgreSQL schema for this invocation,
then creates a unique account inside it. It publishes a fixed USD rating
through `store.Ratings()`, records postpaid usage, and closes the period with
`usage.NewSettlement(store.Settlements(), …).StartClose` / `AdvanceClose` /
`PublishClose`. It then creates a durable submission attempt and sends the
batch to the in-process `billingtest.AcceptThenTimeout` simulator. The local
attempt is recorded as `unknown`; an authoritative simulator lookup supplies
the provider reference and reconciles the attempt to `confirmed`.

The JSON output is an audit summary containing the account, batch total and
line count, local state revisions, and provider reference. The simulator does
not call a payment provider or validate an SDK integration.

## Inbox and outbox restart recovery

Run the recovery example with a PostgreSQL connection in
`BILLING_DATABASE_URL`:

```sh
BILLING_DATABASE_URL='postgres://billing:billing-local-only@127.0.0.1:55438/billing?sslmode=disable' go run ./examples/recovery
```

The command creates and drops a unique PostgreSQL schema for this invocation.
It receives an inbound message, simulates a crashed worker by expiring its
lease with a clearly marked demo-only SQL update, and starts a fresh store.
The successor fence applies the credit effect once; the stale worker and a
replay cannot apply it. It then submits an outbound command to the local
`billingtest.AcceptThenTimeout` simulator. A fresh store observes the durable
`unknown` state until an authoritative simulator lookup resolves it to
`completed` with the provider reference.

## Prepaid allowances, top-ups, and restart recovery

Run the prepaid example with a PostgreSQL connection in
`BILLING_DATABASE_URL`:

```sh
BILLING_DATABASE_URL='postgres://billing:billing-local-only@127.0.0.1:55438/billing?sslmode=disable' go run ./examples/prepaid
```

The command creates and drops a unique PostgreSQL schema for this invocation.
It publishes a monthly credit plan, stores an actual subscription assignment,
records confirmed annual payment coverage, and issues the monthly allowance
from that evidence. A local paid payment fact grants a separate credit top-up;
the example reserves credits with FEFO ordering and settles prepaid usage with
`integration.SettlePrepaid`.

The injected clock then advances to the next monthly boundary. A new store
instance replays the allowance worker, which issues the next allowance and
replays the same request without minting a duplicate. The command verifies the
credit ledger and prints the final balance as JSON. All payment and
subscription facts are local fixtures; no provider SDK or network request is
used.
