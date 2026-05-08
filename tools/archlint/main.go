package main

import (
	"fmt"
	"os"

	"golang.org/x/tools/go/packages"
)

type Violation struct {
	From string
	To   string
	Msg  string
}

type Rule struct {
	Module string
	Allow  []string
	Deny   []string
}

type Package struct {
	Name    string
	Imports []string
}

var Rules = []Rule{
	{
		Module: "internal/order",
		Deny: []string{
			"internal/inventory",
			"internal/shipment",
		},
	},
	{
		Module: "internal/inventory",
		Deny: []string{
			"internal/order",
			"internal/shipment",
		},
	},
	{
		Module: "internal/shipment",
		Deny: []string{
			"internal/order",
			"internal/inventory",
		},
	},
}

func (v Violation) String() string {
	return fmt.Sprintf(
		"❌ %s imports forbidden package %s",
		v.From,
		v.To,
	)
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}

	violations, err := Run(root)
	if err != nil {
		fmt.Println("archlint error:", err)
		os.Exit(1)
	}

	if len(violations) > 0 {
		for _, v := range violations {
			fmt.Println(v.String())
		}
		os.Exit(1)
	}

	fmt.Println("architecture clean")
}

func Scan(root string) ([]Package, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedImports,
		Dir:  root,
	}

	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, err
	}

	var result []Package

	for _, p := range pkgs {
		result = append(result, Package{
			Name:    p.PkgPath,
			Imports: extractImports(p),
		})
	}

	return result, nil
}

func extractImports(p *packages.Package) []string {
	imports := make([]string, 0, len(p.Imports))

	for path := range p.Imports {
		imports = append(imports, path)
	}
	return imports
}

func checkPackages(pkgs []Package) []Violation {
	var violations []Violation
	for _, pkg := range pkgs {
		for _, imp := range pkg.Imports {
			if isForbidden(pkg.Name, imp) {
				violations = append(violations, Violation{
					From: pkg.Name,
					To:   imp,
				})
			}
		}
	}
	return violations
}

func Run(root string) ([]Violation, error) {
	pkgs, err := Scan(root)
	if err != nil {
		return nil, err
	}
	return checkPackages(pkgs), nil
}

func isForbidden(fromPkg, toImport string) bool {
	// Only lint imports that are within this module
	// Standard library and third-party packages are always allowed
	moduleRoot := "github.com/nickemma/chainpulse"

	if !hasPrefix(toImport, moduleRoot) {
		return false
	}

	for _, r := range Rules {
		if hasPrefix(fromPkg, moduleRoot+"/"+r.Module) {
			for _, d := range r.Deny {
				if hasPrefix(toImport, moduleRoot+"/"+d) {
					return true
				}
			}
		}
	}
	return false
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
