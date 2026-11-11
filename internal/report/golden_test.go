package report

import (
	"strings"
	"testing"

	"github.com/krateo-platformops/alert-provider/internal/golden"
)

// TestParseMatchesPython holds Parse to the Python implementation's output on the inputs its test
// suite passed and on seeded random ones (hack/golden/generate.py).
func TestParseMatchesPython(t *testing.T) {
	for i, c := range golden.Load(t, "testdata/parse.json.gz") {
		text, _ := c["text"].(string)
		var ledger []ToolResult
		if l, ok := c["ledger"].([]any); ok {
			for _, e := range l {
				m := e.(map[string]any)
				name, _ := m["name"].(string)
				payload, _ := m["payload"].(string)
				failed, _ := m["failed"].(bool)
				ledger = append(ledger, ToolResult{Name: name, Payload: payload, Failed: failed})
			}
		}
		prose, v2 := Parse(text, ledger)
		if prose != c["prose"] {
			t.Errorf("case %d: prose\n got %q\nwant %q\ntext %q", i, prose, c["prose"], text)
		}
		if d := golden.Diff(fittedToTheCRD(c["v2"]), v2); d != "" {
			t.Errorf("case %d: fields (-python +go):\n%s\ntext %q", i, d, text)
		}
	}
}

// fittedToTheCRD is the Python implementation's fields with the one deliberate departure from it:
// the root cause's category is fitted to incident-provider 0.10.0's Incident CRD, an enum (see
// rootCause). The corpus inputs carry no owner, so no root cause gets one (not assessed). It edits
// the case in place: each test loads its own corpus.
func fittedToTheCRD(v any) any {
	v2, _ := v.(map[string]any)
	rc, ok := v2["rootCause"].(map[string]any)
	if !ok {
		return v
	}
	if c, ok := rc["category"].(string); ok {
		if contains(categories, strings.ToLower(c)) {
			rc["category"] = strings.ToLower(c)
		} else {
			rc["category"] = categoryOther
		}
	}
	return v2
}
