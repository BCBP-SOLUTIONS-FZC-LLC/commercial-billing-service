package http

import (
	"os"
	"regexp"
	"testing"
)

// The chart's operator HTTPRoute list (deploy/helm/values.yaml) is the
// route catalogue's operator routes, B-O1 to B-O17 (§13.1, BL-OQ-9).
func TestHelmOperatorRoutes(t *testing.T) {
	b, err := os.ReadFile("../../../deploy/helm/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\{ id: (B-O\d+), method: ([A-Z]+), path: "([^"]+)" \}`)
	got := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		got[m[1]] = m[2] + " " + m[3]
	}
	want := map[string]string{}
	for _, r := range operatorRoutes {
		want[r.ID] = r.Method + " " + r.Path
	}
	if len(got) != len(want) {
		t.Fatalf("chart lists %d operator routes, catalogue has %d", len(got), len(want))
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: chart %q, catalogue %q", id, got[id], w)
		}
	}
}
