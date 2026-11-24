package incident

import (
	"context"
	"testing"

	"github.com/krateo-platformops/alert-provider/internal/compare"
)

// newCaseNoAgent is a writer without an analysis agent. Any model call fails the test at the end.
func newCaseNoAgent(t *testing.T) *writerCase {
	c := newCase(t)
	c.w.cfg.NoAgent = true
	c.w.spawn = func(f func()) { f() }
	t.Cleanup(func() {
		eq(t, "rca calls", len(c.rca), 0)
		eq(t, "comparisons", len(c.compared), 0)
		eq(t, "records read", len(c.reads), 0)
	})
	return c
}

func (c *writerCase) firePlain() {
	c.w.Fire(context.Background(), Alert{Namespace: ns, Alert: compare.Alert{
		Name: alertName, Where: "match(Body, \x27x\x27)", Interval: "15m", Threshold: "1"}}, c.recordsOf)
}

func TestWithNoAgentAFiringOpensOneIncidentForAPerson(t *testing.T) {
	c := newCaseNoAgent(t)
	c.firePlain()
	inc := c.kube.only(ns)
	s := st(inc)
	eq(t, "status", []any{s["state"], s["error"]}, []any{"Open", NoAgent})
	// Unlabelled, so an agent installed later supersedes it.
	eq(t, "labels", inc["metadata"].(map[string]any)["labels"], map[string]any{LabelAlert: alertName})
	writes := len(c.kube.statusWrites())
	c.firePlain()
	c.firePlain()
	c.kube.only(ns) // still one
	eq(t, "status writes", len(c.kube.statusWrites()), writes)
}

// What an unreachable agent left behind neither hides a firing nor is superseded by an incident
// that has no analysis either.
func TestWithNoAgentAFailedAnalysisDoesNotHoldTheFiring(t *testing.T) {
	c := newCaseNoAgent(t)
	c.kube.put(ns, "f-1", alertName, seed{state: "Closed", created: "2026-09-25T10:00:00Z"})
	c.kube.mu.Lock()
	meta := c.kube.incidents[ns+"/f-1"]["metadata"].(map[string]any)
	meta["annotations"] = map[string]any{AnnotationClosedReason: ClosedAnalysisFailed}
	c.kube.mu.Unlock()
	c.firePlain()
	eq(t, "incidents", len(c.kube.incidents), 2)
	eq(t, "superseded", annotations(c.kube.get(ns, "f-1"))[AnnotationSupersededBy], nil)
}

func TestWithNoAgentAnAlertLabelledForAPersonKeepsItsOwnReason(t *testing.T) {
	c := newCaseNoAgent(t)
	c.fireHuman()
	eq(t, "error", st(c.kube.only(ns))["error"], HumanOnly)
}

// Turning the agent on: the incident opened without one holds the alert like a failed analysis,
// then gives way to an analyzed one, which closes it.
func TestAnAgentInstalledLaterSupersedesTheIncidentOpenedWithoutOne(t *testing.T) {
	c := newCase(t)
	c.kube.put(ns, "n-1", alertName, seed{state: "Open", created: "2026-09-25T10:00:00Z"})
	c.kube.writeStatus(ns, "n-1", map[string]any{"error": NoAgent, "completedAt": "2026-09-25T10:00:00Z"})
	c.firePlain()
	eq(t, "rca calls", len(c.rca), 1)
	eq(t, "incidents", len(c.kube.incidents), 2)
	eq(t, "superseded", annotations(c.kube.get(ns, "n-1"))[AnnotationSupersededBy] != nil, true)
	eq(t, "closed", closedInSpec(c.kube.get(ns, "n-1")), true)
}

func TestWithNoAgentAnInterruptedAnalysisIsOpenedForAPerson(t *testing.T) {
	c := newCaseNoAgent(t)
	c.interrupted("a-1", "a", seed{state: "Analyzing"}, "p")
	c.w.RecoverInterrupted(context.Background())
	s := st(c.kube.get(ns, "a-1"))
	eq(t, "status", []any{s["state"], s["error"]}, []any{"Open", NoAgent})
}

// A reanalysis nobody can answer must not clear the analysis the incident has.
func TestWithNoAgentAReanalysisIsDroppedAndTheAnalysisStays(t *testing.T) {
	c := newCaseNoAgent(t)
	c.analyzed("a-1", "", "")
	before := st(c.kube.get(ns, "a-1"))
	c.kube.writeSpec(ns, "a-1", map[string]any{"reanalysis": map[string]any{"feedback": "again", "cause": CausePerson}})
	if err := c.w.Reanalyze(context.Background(), context.Background(), ns, "a-1"); err != nil {
		t.Fatal(err)
	}
	inc := c.kube.get(ns, "a-1")
	eq(t, "request", spec(inc)["reanalysis"], nil)
	eq(t, "status", st(inc), before)
}
