module github.com/data-insights-ai/rho-billing

go 1.26.0

require github.com/jackc/pgx/v5 v5.11.0

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/text v0.39.0 // indirect
)

// Withdrawn before the first stable release. They remain fetchable from the
// module proxy, which is immutable, but nothing should select them.
retract (
	v0.1.0 // Superseded; entitlements could not be listed for an account.
	v0.2.0 // Superseded by a rewritten, squashed history.
)
