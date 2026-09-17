.PHONY: test race vet check integration release-check db-up db-down

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

check: test race vet

integration:
	@test -n "$$BILLING_TEST_DATABASE_URL" || (echo 'Set BILLING_TEST_DATABASE_URL to an isolated Postgres test database'; exit 1)
	go test -race -count=1 ./postgres/...

db-up:
	docker compose up -d --wait

db-down:
	docker compose down

release-check:
	sh scripts/check-local.sh
