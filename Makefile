.PHONY: test race vet fuzz check

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fuzz:
	go test -run x -fuzz FuzzCreditIsConserved -fuzztime 30s ./credit/

check: vet race
