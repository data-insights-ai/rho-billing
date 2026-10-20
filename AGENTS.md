# rho-billing

Contributor notes for the storage-free billing library.

- Go module: `github.com/data-insights-ai/rho-billing`; standard library only.
- No storage, schema, transactions, goroutines or network calls: the host
  loads, the library decides, the host stores.
- No payment-provider SDK in this module; providers come as plugins.
- Never put credentials or customer data in source, commits or fixtures.
- Tests and exported APIs are the contract: `make check`.

## Hosts

| Host | Pinned | Notes |
|---|---|---|
| account-platform | v0.4+ | the central wallet of Data Insights products |
| rho-paddle | v0.3.7 | uses the removed storage-backed API; stays pinned until it is rebuilt on the provider abstraction (account platform stage 6) |
| sigma-scg-platform | v0.3.7 | uses the removed catalog, subscription, purchase and Postgres API; stays pinned until migrated |

A behaviour change is finished when it is tagged and the hosts that need it
have bumped and passed their checks.
