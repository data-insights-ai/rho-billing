package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var frozenPublicPackages = []string{
	"github.com/data-insights-ai/rho-billing",
	"github.com/data-insights-ai/rho-billing/billingtest",
	"github.com/data-insights-ai/rho-billing/catalog",
	"github.com/data-insights-ai/rho-billing/credit",
	"github.com/data-insights-ai/rho-billing/integration",
	"github.com/data-insights-ai/rho-billing/postgres",
	"github.com/data-insights-ai/rho-billing/purchase",
	"github.com/data-insights-ai/rho-billing/query",
	"github.com/data-insights-ai/rho-billing/subscription",
	"github.com/data-insights-ai/rho-billing/usage",
}

func TestPublicPackageSetIsFrozen(t *testing.T) {
	root := moduleRoot(t)
	seen := map[string]struct{}{}
	for _, symbol := range collectSymbols(root) {
		if symbol.boundary() != "public-declaration" {
			continue
		}
		if strings.Contains(symbol.packagePath, "/internal/") || strings.HasSuffix(symbol.packagePath, "/internal") {
			continue
		}
		if strings.Contains(symbol.packagePath, "/testdata/") || strings.Contains(symbol.packagePath, "/examples/") {
			continue
		}
		seen[symbol.packagePath] = struct{}{}
	}
	got := make([]string, 0, len(seen))
	for pkg := range seen {
		got = append(got, pkg)
	}
	slices.Sort(got)
	t.Logf("public packages (%d): %s", len(got), strings.Join(got, " "))
	if !slices.Equal(got, frozenPublicPackages) {
		t.Fatalf("public packages = %q, want %q", got, frozenPublicPackages)
	}
}

func TestMemoryEngineTypesAreUnexported(t *testing.T) {
	root := moduleRoot(t)
	for _, symbol := range collectSymbols(root) {
		if symbol.boundary() != "public-declaration" || symbol.kind != "type" {
			continue
		}
		if symbol.name == "MemoryRepository" || strings.HasSuffix(symbol.name, "MemoryRepository") {
			t.Errorf("%s exports %s", symbol.packagePath, symbol.name)
		}
	}
}

func TestPublicSignaturesDoNotReferenceInternalTypes(t *testing.T) {
	root := moduleRoot(t)
	for _, symbol := range collectSymbols(root) {
		if symbol.boundary() != "public-declaration" {
			continue
		}
		if strings.Contains(symbol.declaration, "/internal/") || strings.Contains(symbol.declaration, "internal/") {
			t.Errorf("%s %s references an internal type: %s", symbol.packagePath, symbol.name, symbol.declaration)
		}
	}
}

func TestPublicTypeAliasesDoNotReferenceInternalPackages(t *testing.T) {
	root := moduleRoot(t)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == "internal" || name == "testdata" || name == "examples" || name == "vendor" || strings.HasPrefix(name, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		imports := map[string]string{}
		for _, spec := range file.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			local := filepath.Base(imp)
			if spec.Name != nil {
				local = spec.Name.Name
			}
			if local == "." || local == "_" {
				continue
			}
			imports[local] = imp
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok || typeSpec.Assign == token.NoPos || !typeSpec.Name.IsExported() {
					continue
				}
				sel, ok := typeSpec.Type.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				pkgIdent, ok := sel.X.(*ast.Ident)
				if !ok {
					continue
				}
				imp := imports[pkgIdent.Name]
				if strings.Contains(imp, "/internal/") || strings.HasSuffix(imp, "/internal") {
					t.Errorf("%s exports %s = %s.%s from internal package %s", path, typeSpec.Name.Name, pkgIdent.Name, sel.Sel.Name, imp)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLivingDocsCiteShippedPackages(t *testing.T) {
	root := moduleRoot(t)
	forbidden := []string{
		"payment.New(store.Payments()",
		"session.Payments()",
		"*settlement.Rejection",
		"errors.AsType[*settlement.Rejection]",
		"allowance.Service.PutSchedule",
		"allowance.Service.IssueDue",
		"allowance.ErrMemberScope",
		"entitlement.Resolve(",
		"entitlement.Input",
		"catalog.Resolve(catalog.Input",
		"Store.FinalizeUsagePeriod",
		"Store.BuildCreditBalanceProjection",
		"Store.StoredCreditBalance",
		"type Store = pg.Store",
	}
	files := []string{
		"README.md",
		"CONTRIBUTING.md",
		"SECURITY.md",
		"CHANGELOG.md",
		"docs/ARCHITECTURE.md",
		"docs/PROVIDER_CAPABILITIES.md",
	}
	for _, rel := range files {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		for _, token := range forbidden {
			if strings.Contains(text, token) {
				t.Errorf("%s cites removed API %q", rel, token)
			}
		}
	}
}

func TestTestdataHostAndAdapterImportOnlyFrozenPublicPackages(t *testing.T) {
	root := moduleRoot(t)
	allowed := make(map[string]struct{}, len(frozenPublicPackages)+2)
	for _, pkg := range frozenPublicPackages {
		allowed[pkg] = struct{}{}
	}
	allowed["example.com/billing-adapter-contract"] = struct{}{}
	allowed["example.com/billing-host-contract"] = struct{}{}
	for _, dir := range []string{"testdata/host", "testdata/adapter"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, spec := range file.Imports {
				imp, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Errorf("%s: %v", path, err)
					continue
				}
				if strings.Contains(imp, "/internal/") || strings.HasSuffix(imp, "/internal") {
					t.Errorf("%s imports internal package %s", path, imp)
					continue
				}
				if strings.HasPrefix(imp, "github.com/data-insights-ai/") {
					if _, ok := allowed[imp]; !ok {
						t.Errorf("%s imports %s; testdata may import only frozen public packages", path, imp)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestCoreModuleHasNoProviderSDK(t *testing.T) {
	root := moduleRoot(t)
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(mod)
	for _, forbidden := range []string{
		"github.com/stripe/stripe-go",
		"github.com/PaddleHQ/paddle-go-sdk",
		"github.com/data-insights-ai/rho-paddle",
		"github.com/stripe/stripe-go/v",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("core go.mod contains provider SDK %q", forbidden)
		}
	}
	if !strings.Contains(text, "github.com/jackc/pgx/v5") {
		t.Fatal("core go.mod lost the PostgreSQL driver")
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("missing caller path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
}

// A struct containing time.Time must never be compared with == or !=. Go's
// struct equality also compares the monotonic reading and the location pointer,
// so a value read back from Postgres compares unequal to the identical instant
// that was written. That is not hypothetical: it made SetLimit reject every
// update to an existing spend cap, and the memory repository hid it because it
// returns the very value it was handed.
func TestPeriodsAreNeverComparedWithStructEquality(t *testing.T) {
	root := moduleRoot(t)
	pattern := regexp.MustCompile(`(Period|Interval|Coverage|Effective)\s*(==|!=)\s*[a-zA-Z_]`)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if pattern.MatchString(line) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s:%d compares a period with struct equality; use Equal: %s", rel, i+1, trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
