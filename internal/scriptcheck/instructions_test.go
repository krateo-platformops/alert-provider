package scriptcheck

import (
	"os"
	"strings"
	"testing"
)

// examples are the scripts the RCA instructions show, by howToFix key: the lines indented under a
// "  <key>:" line.
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
			src = append(src, lines[i][4:])
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

// The instructions teach by example: every script they show passes the checks their scripts get.
func TestTheInstructionsExamplesPassTheChecks(t *testing.T) {
	ex := examples(t)
	if len(ex) < 5 {
		t.Fatalf("%d example scripts found", len(ex))
	}
	for _, e := range ex {
		if ps := Script(e.Script, e.Detail); len(ps) != 0 {
			t.Errorf("example %s:\n%s\n%v", e.Script, e.Detail, ps)
		}
	}
}
