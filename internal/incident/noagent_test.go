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

var synced = map[string]any{"type": "Synced", "status": "True", "reason": "ReconcileSuccess",
	"lastTransitionTime": "2026-09-25T10:00:00Z"}

func wantUnavailable(t *testing.T, inc map[string]any, want, reason string) {
	t.Helper()
	c, _ := condition(st(inc), ConditionAnalysisUnavailable)
	eq(t, "AnalysisUnavailable", []any{c["status"], c["reason"], c["message"] != nil, c["lastTransitionTime"] != nil},
		[]any{want, reason, true, true})
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
	eq(t, "status", []any{s["state"], s["error"], s["completedAt"] != nil}, []any{"Open", nil, true})
	wantUnavailable(t, inc, "True", ReasonNoAgent)
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
	for k, inc := range c.kube.incidents {
		if v := annotations(inc)[AnnotationSupersedesFailed]; v != nil {
			t.Fatalf("%s names the failed analyses %v", k, v)
		}
	}
}

// The incident controller writes between the read and the write: the write is retried on what it
// wrote, and a person's close in between is left alone.
func TestOpeningForAPersonSurvivesAConcurrentWrite(t *testing.T) {
	c := newCaseNoAgent(t)
	once := true
	c.kube.beforePatch = func(ns, name string) {
		if once {
			once = false
			c.kube.writeStatus(ns, name, map[string]any{"conditions": []any{synced}})
		}
	}
	c.firePlain()
	inc := c.kube.only(ns)
	eq(t, "state", st(inc)["state"], "Open")
	if _, ok := condition(st(inc), "Synced"); !ok {
		t.Fatal("Synced is gone")
	}
	wantUnavailable(t, inc, "True", ReasonNoAgent)

	// Closed is final in the CRD; from Verifying the apiserver would accept the write.
	for _, moved := range []string{"Closed", "Verifying"} {
		c := newCaseNoAgent(t)
		c.kube.beforePatch = func(ns, name string) { c.kube.writeStatus(ns, name, map[string]any{"state": moved}) }
		c.firePlain()
		eq(t, "state", st(c.kube.only(ns))["state"], moved)
	}
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
	c.kube.writeStatus(ns, "n-1", map[string]any{"completedAt": "2026-09-25T10:00:00Z",
		"conditions": []any{unavailable(true, "2026-09-25T10:00:00Z")}})
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
	inc := c.kube.get(ns, "a-1")
	eq(t, "state", st(inc)["state"], "Open")
	wantUnavailable(t, inc, "True", ReasonNoAgent)
}

// The controller's conditions survive the write: a merge patch replaces the whole list.
func TestOpeningForAPersonKeepsTheOtherConditions(t *testing.T) {
	c := newCaseNoAgent(t)
	c.interrupted("a-1", "a", seed{state: "Analyzing"}, "p")
	c.kube.writeStatus(ns, "a-1", map[string]any{"conditions": []any{synced}})
	c.w.RecoverInterrupted(context.Background())
	inc := c.kube.get(ns, "a-1")
	if _, ok := condition(st(inc), "Synced"); !ok {
		t.Fatalf("Synced is gone: %v", st(inc)["conditions"])
	}
	wantUnavailable(t, inc, "True", ReasonNoAgent)
}

// With an agent at the next start the condition turns False, the rest of the list stays, and a
// closed incident is not written.
func TestAnAgentInstalledLaterTurnsTheConditionFalse(t *testing.T) {
	c := newCase(t)
	for name, state := range map[string]string{"n-1": "Open", "n-2": "Closed"} {
		c.kube.put(ns, name, alertName, seed{state: state, created: "2026-09-25T10:00:00Z"})
		c.kube.writeStatus(ns, name, map[string]any{"conditions": []any{synced, unavailable(true, "2026-09-25T10:00:00Z")}})
	}
	c.w.RecoverInterrupted(context.Background())
	wantUnavailable(t, c.kube.get(ns, "n-1"), "False", ReasonAgentAvailable)
	wantUnavailable(t, c.kube.get(ns, "n-2"), "True", ReasonNoAgent)
	if _, ok := condition(st(c.kube.get(ns, "n-1")), "Synced"); !ok {
		t.Fatal("Synced is gone")
	}
	writes := len(c.kube.statusWrites())
	c.w.RecoverInterrupted(context.Background())
	eq(t, "writes of a second start", len(c.kube.statusWrites()), writes)
}

// A person's re-run with an agent: the new analysis starts without the condition.
func TestAReanalysisClearsTheCondition(t *testing.T) {
	c := newCase(t)
	c.w.spawn = func(func()) {}
	c.kube.put(ns, "n-1", alertName, seed{state: "Open", created: "2026-09-25T10:00:00Z"})
	c.kube.writeStatus(ns, "n-1", map[string]any{"conditions": []any{synced, unavailable(false, "2026-09-25T10:00:00Z")}})
	c.kube.writeSpec(ns, "n-1", map[string]any{"reanalysis": map[string]any{"cause": CausePerson}})
	if err := c.w.Reanalyze(context.Background(), context.Background(), ns, "n-1"); err != nil {
		t.Fatal(err)
	}
	inc := c.kube.get(ns, "n-1")
	eq(t, "state", st(inc)["state"], "Analyzing")
	if _, ok := condition(st(inc), ConditionAnalysisUnavailable); ok {
		t.Fatal("the condition is still there")
	}
	if _, ok := condition(st(inc), "Synced"); !ok {
		t.Fatal("Synced is gone")
	}
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
