package wire_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// A new typed error must not ship unmapped: it would reach a caller as an unclassified INTERNAL, and a
// host acts differently on each error. This enumerates the exported Err* of domain/errors.go and fails on
// any that the transports' condition table does not name.
func TestEveryExportedDomainErrorHasAStatusRow(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../../../domain/errors.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	ast.Inspect(f, func(n ast.Node) bool {
		if vs, ok := n.(*ast.ValueSpec); ok {
			for _, name := range vs.Names {
				if strings.HasPrefix(name.Name, "Err") && name.IsExported() {
					declared = append(declared, name.Name)
				}
			}
		}
		return true
	})
	sort.Strings(declared)
	if len(declared) < 10 {
		t.Fatalf("found only %v: is the scan reading the right file?", declared)
	}

	mapped := mappedSentinels() // by name: the wire package's table, keyed by the domain variable's name
	var missing []string
	for _, name := range declared {
		if !mapped[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("domain/errors.go exports %v, which no row of wire.Table names — add a row (code, reason, HTTP status) and a row to the status tests", missing)
	}
	// And the reverse: a row naming an error that no longer exists is a stale row.
	known := map[string]bool{}
	for _, n := range declared {
		known[n] = true
	}
	for name := range mapped {
		if !known[name] {
			t.Errorf("wire.Table names %s, which domain/errors.go does not export", name)
		}
	}
}
