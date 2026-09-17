# rho-billing

Contributor notes for the provider-independent billing core.

Read `README.md`, `CONTRIBUTING.md`, `docs/ARCHITECTURE.md` and
`docs/PROVIDER_CAPABILITIES.md` before changing behaviour.

- Go module: `github.com/data-insights-ai/rho-billing`.
- No payment-provider SDK in this module.
- Constructors do no I/O and start no workers.
- Hosts authorize before calling the library.
- Never put credentials, customer payloads, or secret-bearing logs in source,
  commits, documentation, or fixtures.
- Tests and exported APIs are the contract. `sh scripts/check-local.sh` is
  the local gate.
- Do not import `internal/` from a host.
