package main

import (
	"go/parser"
	"testing"
)

func TestReceiverBoundaryClassification(t *testing.T) {
	for _, tc := range []struct{ expression, owner, boundary string }{
		{"Store", "Store", "public-declaration"},
		{"*Store", "Store", "public-declaration"},
		{"*memoryRepository", "memoryRepository", "private-receiver"},
		{"*Cache[K, V]", "Cache", "public-declaration"},
		{"*cache[T]", "cache", "private-receiver"},
	} {
		t.Run(tc.expression, func(t *testing.T) {
			expr, err := parser.ParseExpr(tc.expression)
			if err != nil {
				t.Fatal(err)
			}
			owner := ownerName(expr)
			if owner != tc.owner {
				t.Fatalf("owner=%q, want %q", owner, tc.owner)
			}
			if got := (symbol{packagePath: "example.com/billing", owner: owner}).boundary(); got != tc.boundary {
				t.Fatalf("boundary=%q, want %q", got, tc.boundary)
			}
		})
	}
	if got := (symbol{packagePath: "example.com/billing/internal/codec", owner: "Codec"}).boundary(); got != "internal-package" {
		t.Fatalf("internal package=%q", got)
	}
	if got := (symbol{packagePath: "example.com/billing/internalized"}).boundary(); got != "public-declaration" {
		t.Fatalf("non-internal segment=%q", got)
	}
}
