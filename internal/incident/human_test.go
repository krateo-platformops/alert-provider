package incident

import (
	"context"
	"testing"

	"github.com/krateo-platformops/alert-provider/internal/compare"
)

// fireHuman fires an alert a person analyses.
func (c *writerCase) fireHuman() {
	c.w.Fire(context.Background(), Alert{Namespace: ns, HumanAnalysis: true, Alert: compare.Alert{
		Name: alertName, Where: "match(Body, \x27x\x27)", Interval: "15m", Threshold: "1"}}, c.recordsOf)
}

func TestAHumanAlertOpensAnIncidentNoModelReads(t *testing.T) {
	c := newCase(t)
	c.fireHuman()
	inc := c.kube.only(ns)
	meta := inc["metadata"].(map[string]any)
	eq(t, "labels", meta["labels"], map[string]any{LabelAlert: alertName, LabelAnalysis: AnalysisHuman})
	s := st(inc)
	eq(t, "state", s["state"], "Open")
	eq(t, "error", s["error"], HumanOnly)
	eq(t, "rca calls", len(c.rca), 0)
	eq(t, "comparisons", len(c.compared), 0)
	eq(t, "records read", len(c.reads), 0)
}

func TestTheOpenIncidentTakesEveryLaterFiringOfAHumanAlert(t *testing.T) {
	c := newCase(t)
	c.fireHuman()
	writes := len(c.kube.statusWrites())
	c.fireHuman()
	c.fireHuman()
	c.kube.only(ns) // still one
	eq(t, "status writes", len(c.kube.statusWrites()), writes)
	eq(t, "rca calls", len(c.rca), 0)
	eq(t, "comparisons", len(c.compared), 0)
	eq(t, "records read", len(c.reads), 0)
}

// An open incident that has an analysis, from before the label, takes the firing uncompared.
func TestAHumanAlertNeverComparesWithAnAnalyzedIncident(t *testing.T) {
	c := newCase(t)
	c.analyzed("cd-not-ready-20260925-100000", "", "")
	c.fireHuman()
	eq(t, "comparisons", len(c.compared), 0)
	eq(t, "records read", len(c.reads), 0)
	c.kube.only(ns)
}

func TestAClosedIncidentGivesAHumanAlertANewOne(t *testing.T) {
	c := newCase(t)
	c.kube.put(ns, "cd-not-ready-20260925-100000", alertName, seed{state: "Closed", created: "2026-09-25T10:00:00Z"})
	c.fireHuman()
	eq(t, "incidents", len(c.kube.incidents), 2)
	eq(t, "rca calls", len(c.rca), 0)
}

func TestAReanalysisOfAHumanIncidentIsDropped(t *testing.T) {
	c := newCase(t)
	c.analyzedIncident("Open")
	labels := c.kube.incidents[ns+"/"+incName]["metadata"].(map[string]any)["labels"].(map[string]any)
	labels[LabelAnalysis] = AnalysisHuman
	c.kube.writeSpec(ns, incName, map[string]any{"reanalysis": map[string]any{}})
	if err := c.reanalyze(); err != nil {
		t.Fatal(err)
	}
	inc := c.kube.get(ns, incName)
	if _, ok := inc["spec"].(map[string]any)["reanalysis"]; ok {
		t.Error("spec.reanalysis not removed")
	}
	eq(t, "state", st(inc)["state"], "Open")
	eq(t, "rca calls", len(c.rca), 0)
}
