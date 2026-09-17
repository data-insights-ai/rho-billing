# Provider capabilities

What a payment provider can and cannot do is not derivable from this
repository's types. A Go method exists for every operation the core models; it
does not follow that a given provider supports that operation.

The core expresses this with `billing.Capability`, `billing.Support` and
`billing.CapabilityError`. Adapters resolve an operation's capability before
dispatch. `billing.RequireSupported` rejects every result other than supported,
so unknown, unsupported and action-required all fail closed.

## Core capabilities that are off by default

| Operation | Result | Reason |
|---|---|---|
| account allowance issuance | supported | — |
| member allowance issuance | `credit.ErrMemberScope`, unsupported (`member_allowance_issuance`) | Allowances are issued per account; member-scoped issuance has no defined grant ceiling. |
| named-zone wall-clock recurrence | unsupported (`named_zone_recurrence`) | Schedules convert to UTC. A named zone would make an anchor ambiguous across a DST transition. |
| automatic top-up | unsupported (`automatic_top_up`) | Requires recorded consent and a provider unattended-collection flow. Off until both exist. |

An unsupported optional operation exposes a typed result and a safe
operator-visible path. It never silently succeeds.

## Provider-specific limits

What a given provider supports, and which operations it forces to fail closed,
belongs with that provider's adapter. For Paddle see `docs/CAPABILITIES.md` in
`rho-paddle`.

## Adding a provider

Before marking an operation supported for a new adapter, record the endpoint
and API version, the request identity, the timeout behaviour, the lookup
strategy that resolves an unknown outcome, the account permissions required,
and the actual result observed against that provider's sandbox.

Three things are distinct evidence and must not be substituted for each other:
reading the provider's documentation, a simulated event against a fixture, and
a real sandbox call. Never treat an uncertain create as rejected merely because
an eventually consistent list call does not show it yet.
