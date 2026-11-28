package provider

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// BIL-PCI-1: only internal/adapters/provider/stripe imports
// github.com/stripe/*, so no Stripe type can cross a port or reach the
// domain, the use cases or the events. Every Go file of the module outside
// that adapter (and the test/ suites that drive stripe-mock) is checked.
func TestBIL_PCI_1_OnlyTheStripeAdapterImportsStripe(t *testing.T) {
	// Split so the module path never appears as a quoted literal here:
	// check-stripe-imports.sh greps for exactly that.
	stripeModule := "github.com/" + "stripe/"
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	adapter := filepath.Join(root, "internal", "adapters", "provider", "stripe")
	allowed := []string{adapter + string(filepath.Separator), filepath.Join(root, "test") + string(filepath.Separator)}
	files, insideAdapter := 0, 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "bin") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		files++
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if !strings.HasPrefix(p, stripeModule) {
				continue
			}
			if strings.HasPrefix(path, allowed[0]) {
				insideAdapter++
				continue
			}
			if strings.HasPrefix(path, allowed[1]) {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s imports %s outside internal/adapters/provider/stripe", rel, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The walk really covered the module and found the adapter's own imports.
	if files < 100 || insideAdapter == 0 {
		t.Fatalf("walked %d files, %d stripe imports inside the adapter: wrong root %s?", files, insideAdapter, root)
	}
}
