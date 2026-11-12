package incident

import (
	"testing"
)

// mirrorNote is what a failure here asks for: the rule has a copy in another repository.
const mirrorNote = "the reanalysis drop rule changed: update incident-provider's writerAnswers " +
	"(internal/controllers/incident/applytarget.go) too, which files ApplyActionStale requests only where Reanalyze answers them"

// TestTheReanalysisDropRuleIsWhatIncidentProviderMirrors pins the drop rule incident-provider's
// writerAnswers copies (as of alert-provider a469b1a): Reanalyze answers a request exactly on an
// Open or Verifying incident that is not labelled observability.krateo.io/analysis=human, and
// removes it unanswered everywhere else.
func TestTheReanalysisDropRuleIsWhatIncidentProviderMirrors(t *testing.T) {
	if LabelAnalysis != "observability.krateo.io/analysis" || AnalysisHuman != "human" {
		t.Fatalf("label %s=%s: %s", LabelAnalysis, AnalysisHuman, mirrorNote)
	}
	states := []string{"", "Analyzing", "Open", "Verifying", "Resolved", "Closed"}
	answered := map[string]bool{"Open": true, "Verifying": true}
	for _, label := range []any{nil, "", "autopilot", "Human", "human"} {
		for _, s := range states {
			want := answered[s] && label != "human"
			name := "label=" + pyfmtStr(label) + "/state=" + s

			c := newCase(t)
			c.kube.put(ns, incName, alertName, seed{state: s, rootCause: "chart 1.1.9 is missing"})
			if label != nil {
				c.kube.incidents[ns+"/"+incName]["metadata"].(map[string]any)["labels"].(map[string]any)[LabelAnalysis] = label
			}
			if got := answers(c.kube.get(ns, incName)); got != want {
				t.Errorf("%s: answers %v, want %v: %s", name, got, want, mirrorNote)
			}

			// Reanalyze follows it: an answered request runs the RCA, a dropped one runs nothing.
			c.kube.writeSpec(ns, incName, map[string]any{"reanalysis": map[string]any{"cause": CauseApplyActionStale}})
			if err := c.reanalyze(); err != nil {
				t.Fatal(err)
			}
			if ran := len(c.rca) == 1; ran != want {
				t.Errorf("%s: Reanalyze ran the RCA %v, want %v: %s", name, ran, want, mirrorNote)
			}
			if _, left := c.kube.get(ns, incName)["spec"].(map[string]any)["reanalysis"]; left {
				t.Errorf("%s: spec.reanalysis left in place", name)
			}
		}
	}
}
