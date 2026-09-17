// Command apiinventory emits an exhaustive exported-symbol inventory for a Go
// module. It intentionally reads the scan root supplied with -root so a clean
// historical tree can be audited without changing the working checkout.
package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

type packageJSON struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	CgoFiles   []string
}

type symbol struct {
	packagePath string
	file        string
	kind        string
	name        string
	receiver    string
	owner       string
	declaration string
	line        int
}

// ownerName unwraps the receiver syntax without relying on parameter names or
// formatting. Classification describes declarations, not all inferred reachability.
func ownerName(expr ast.Expr) string {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr.Name
	case *ast.StarExpr:
		return ownerName(expr.X)
	case *ast.IndexExpr:
		return ownerName(expr.X)
	case *ast.IndexListExpr:
		return ownerName(expr.X)
	case *ast.ParenExpr:
		return ownerName(expr.X)
	default:
		return ""
	}
}

func (s symbol) boundary() string {
	if slices.Contains(strings.Split(s.packagePath, "/"), "internal") {
		return "internal-package"
	}
	if s.owner != "" && !ast.IsExported(s.owner) {
		return "private-receiver"
	}
	return "public-declaration"
}

func isExported(name string) bool {
	return ast.IsExported(name)
}

func nodeText(fileSet *token.FileSet, node any) string {
	var text strings.Builder
	if err := format.Node(&text, fileSet, node); err != nil {
		return "<format error>"
	}
	return strings.Join(strings.Fields(text.String()), " ")
}

func receiverText(fileSet *token.FileSet, fields *ast.FieldList) string {
	if fields == nil || len(fields.List) == 0 {
		return ""
	}
	field := fields.List[0]
	typeText := nodeText(fileSet, field.Type)
	if len(field.Names) == 0 {
		return typeText
	}
	return field.Names[0].Name + " " + typeText
}

func functionDeclaration(fileSet *token.FileSet, declaration *ast.FuncDecl) string {
	withoutBody := *declaration
	withoutBody.Body = nil
	withoutBody.Doc = nil
	return nodeText(fileSet, &withoutBody)
}

func relativeFile(root, directory, base string) string {
	relative, err := filepath.Rel(root, filepath.Join(directory, base))
	if err != nil {
		panic(err)
	}
	return filepath.ToSlash(relative)
}

func addInterfaceMethods(symbols *[]symbol, packagePath, root, directory, base string, fileSet *token.FileSet, file *ast.File) {
	for _, declaration := range file.Decls {
		gen, ok := declaration.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, specification := range gen.Specs {
			typeSpec, ok := specification.(*ast.TypeSpec)
			if !ok {
				continue
			}
			interfaceType, ok := typeSpec.Type.(*ast.InterfaceType)
			if !ok {
				continue
			}
			for _, field := range interfaceType.Methods.List {
				functionType, ok := field.Type.(*ast.FuncType)
				if !ok {
					continue
				}
				for _, name := range field.Names {
					if !isExported(name.Name) {
						continue
					}
					method := &ast.FuncDecl{Name: name, Type: functionType}
					position := fileSet.Position(name.Pos())
					*symbols = append(*symbols, symbol{
						packagePath: packagePath,
						file:        relativeFile(root, directory, base),
						kind:        "method",
						name:        name.Name,
						receiver:    "interface " + typeSpec.Name.Name,
						owner:       typeSpec.Name.Name,
						declaration: functionDeclaration(fileSet, method),
						line:        position.Line,
					})
				}
			}
		}
	}
}

func collectSymbols(root string) []symbol {
	command := exec.Command("go", "list", "-json", "./...")
	command.Dir = root
	command.Env = append(command.Environ(), "GOWORK=off")
	command.Stderr = os.Stderr
	output, err := command.Output()
	if err != nil {
		panic(err)
	}

	decoder := json.NewDecoder(bytes.NewReader(output))
	var symbols []symbol
	for {
		var packageInfo packageJSON
		err := decoder.Decode(&packageInfo)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			panic(err)
		}

		fileSet := token.NewFileSet()
		files := append(slices.Clone(packageInfo.GoFiles), packageInfo.CgoFiles...)
		slices.Sort(files)
		for _, base := range files {
			path := filepath.Join(packageInfo.Dir, base)
			file, err := parser.ParseFile(fileSet, path, nil, parser.ParseComments)
			if err != nil {
				panic(err)
			}
			for _, declaration := range file.Decls {
				switch declaration := declaration.(type) {
				case *ast.GenDecl:
					if declaration.Tok != token.TYPE && declaration.Tok != token.VAR && declaration.Tok != token.CONST {
						continue
					}
					for _, specification := range declaration.Specs {
						var names []*ast.Ident
						switch specification := specification.(type) {
						case *ast.TypeSpec:
							names = []*ast.Ident{specification.Name}
						case *ast.ValueSpec:
							names = specification.Names
						}
						for _, name := range names {
							if !isExported(name.Name) {
								continue
							}
							position := fileSet.Position(name.Pos())
							symbols = append(symbols, symbol{
								packagePath: packageInfo.ImportPath,
								file:        relativeFile(root, packageInfo.Dir, base),
								kind:        declaration.Tok.String(),
								name:        name.Name,
								declaration: nodeText(fileSet, specification),
								line:        position.Line,
							})
						}
					}
				case *ast.FuncDecl:
					if !isExported(declaration.Name.Name) {
						continue
					}
					kind := "func"
					receiver := ""
					owner := ""
					if declaration.Recv != nil {
						kind = "method"
						receiver = receiverText(fileSet, declaration.Recv)
						owner = ownerName(declaration.Recv.List[0].Type)
					}
					position := fileSet.Position(declaration.Name.Pos())
					symbols = append(symbols, symbol{
						packagePath: packageInfo.ImportPath,
						file:        relativeFile(root, packageInfo.Dir, base),
						kind:        kind,
						name:        declaration.Name.Name,
						receiver:    receiver,
						owner:       owner,
						declaration: functionDeclaration(fileSet, declaration),
						line:        position.Line,
					})
				}
			}
			addInterfaceMethods(&symbols, packageInfo.ImportPath, root, packageInfo.Dir, base, fileSet, file)
		}
	}

	slices.SortFunc(symbols, func(left, right symbol) int {
		if result := cmp.Compare(left.packagePath, right.packagePath); result != 0 {
			return result
		}
		if result := cmp.Compare(left.file, right.file); result != 0 {
			return result
		}
		return cmp.Compare(left.line, right.line)
	})
	return symbols
}

func main() {
	rootFlag := flag.String("root", ".", "module root to inventory")
	classifyFlag := flag.Bool("classify", false, "append declaration boundary classification")
	flag.Parse()
	root, err := filepath.Abs(*rootFlag)
	if err != nil {
		panic(err)
	}
	for _, symbol := range collectSymbols(root) {
		fmt.Printf("%s|%s|%s|%s|%s:%d|%s", symbol.packagePath, symbol.kind, symbol.name, symbol.receiver, symbol.file, symbol.line, strings.ReplaceAll(symbol.declaration, "|", "\\|"))
		if *classifyFlag {
			fmt.Printf("|%s", symbol.boundary())
		}
		fmt.Println()
	}
}
