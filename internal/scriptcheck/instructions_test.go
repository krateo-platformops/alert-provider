package scriptcheck

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// placeholder is an example's <alert>, <namespace>, ...: bash would read it as redirections.
var placeholder = regexp.MustCompile(`<([a-z][a-z-]*)>`)

// examples are the scripts the RCA instructions show, by howToFix key: the lines indented under a
// "  <key>:" line, with each <placeholder> filled in with its own name.
func examples(t *testing.T) []Problem {
	t.Helper()
	b, err := os.ReadFile("../report/instructions.txt")
	if err != nil {
		t.Fatal(err)
	}
	var out []Problem
	lines := strings.Split(string(b), "\n")
	for i := 0; i < len(lines); i++ {
		key, ok := strings.CutSuffix(strings.TrimSpace(lines[i]), ":")
		if !ok || !strings.HasPrefix(lines[i], "  ") || !contains(Scripts, key) {
			continue
		}
		var src []string
		for i+1 < len(lines) && strings.HasPrefix(lines[i+1], "    ") {
			i++
			src = append(src, placeholder.ReplaceAllString(lines[i][4:], "$1"))
		}
		out = append(out, Problem{Script: key, Detail: strings.Join(src, "\n") + "\n"})
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// The instructions teach by example: every script they show passes the checks their scripts get,
// the false-positive example's guarded Alert reads among them.
func TestTheInstructionsExamplesPassTheChecks(t *testing.T) {
	ex := examples(t)
	if len(ex) < 9 {
		t.Fatalf("%d example scripts found", len(ex))
	}
	alertReads := 0
	for _, e := range ex {
		if checks[e.Script] && strings.Contains(e.Detail, "kubectl get alerts.observability.krateo.io") {
			alertReads++
		}
		if ps := Script(e.Script, e.Detail); len(ps) != 0 {
			t.Errorf("example %s:\n%s\n%v", e.Script, e.Detail, ps)
		}
	}
	if alertReads != 2 {
		t.Errorf("%d example checks read the Alert, want the false-positive precondition and verify", alertReads)
	}
}
