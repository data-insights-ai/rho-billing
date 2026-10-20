# Contributing

Go 1.27. `make check` runs vet and the race-enabled tests; run `make fuzz`
when you change how credit moves.

Keep the library storage-free: a change that needs a database, a clock of its
own or a goroutine belongs in the host. Amounts are integer subunits; rating
uses exact rational arithmetic and rounds once.
