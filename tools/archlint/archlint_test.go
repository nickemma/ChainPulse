package main

import "testing"

func TestForbiddenImportProducesViolation(t *testing.T) {
	pkgs := []Package{
		{
			Name: "github.com/nickemma/chainpulse/internal/order/application",
			Imports: []string{
				"github.com/nickemma/chainpulse/internal/inventory/domain",
			},
		},
	}
	violations := checkPackages(pkgs)
	if len(violations) == 0 {
		t.Fatal("expected violation, got none")
	}
}

func TestAllowedImportProducesNoViolations(t *testing.T) {
	pkg := Package{
		Name: "github.com/nickemma/chainpulse/internal/order/application",
		Imports: []string{
			"github.com/nickemma/chainpulse/internal/order/domain",
			"github.com/nickemma/chainpulse/internal/shared/testutil",
		},
	}

	var violations []Violation

	for _, imp := range pkg.Imports {
		if isForbidden(pkg.Name, imp) {
			violations = append(violations, Violation{
				From: pkg.Name,
				To:   imp,
			})
		}
	}

	if len(violations) != 0 {
		t.Fatalf("expected no violations, got %d", len(violations))
	}
}

func TestStandardLibraryIsNeverFlagged(t *testing.T) {
	pkg := Package{
		Name: "github.com/nickemma/chainpulse/internal/order/application",
		Imports: []string{
			"fmt",
			"os",
			"time",
		},
	}

	var violations []Violation

	for _, imp := range pkg.Imports {
		if isForbidden(pkg.Name, imp) {
			violations = append(violations, Violation{
				From: pkg.Name,
				To:   imp,
			})
		}
	}

	if len(violations) != 0 {
		t.Fatalf("standard library imports should never be flagged, got %d", len(violations))
	}
}
