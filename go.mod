module github.com/data-insights-ai/rho-billing

go 1.27.1

// Withdrawn before the first stable release. They remain fetchable from the
// module proxy, which is immutable, but nothing should select them.
retract (
	v0.1.0 // Superseded; entitlements could not be listed for an account.
	v0.2.0 // Superseded by a rewritten, squashed history.
)
