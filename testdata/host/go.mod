module example.com/billing-host-contract

go 1.26.0

require (
	github.com/data-insights-ai/rho-billing v0.1.0
	example.com/billing-adapter-contract v0.0.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.11.0 // indirect
	golang.org/x/text v0.39.0 // indirect
)

replace github.com/data-insights-ai/rho-billing => ../..

replace example.com/billing-adapter-contract => ../adapter
