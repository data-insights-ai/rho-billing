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

## Releasing, and what it does to the hosts

`rho-paddle` and `sigma-scg-platform` each keep a gitignored `go.work` that
substitutes this checkout. Their local test runs therefore see whatever is
in this working tree, while their CI and production see the version their
`go.mod` pins. A change here is invisible to them until it is tagged and
they bump.

So a behaviour change here is not finished when it is merged:

1. Tag and push a version.
2. In each host that depends on the new behaviour:
   `GOWORK=off go get github.com/data-insights-ai/rho-billing@vX.Y.Z`,
   and in `rho-paddle` also `(cd testdata/host && GOWORK=off go mod tidy)`.
3. Let that host's CI run. Its gate sets `GOWORK=off`, so it is the first
   thing that actually exercises the released pair.

Skipping step 2 does not fail loudly. The host's tests fail as if they
were wrong, which sends you reading the test rather than the pin.
