package incident

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// staleReanalysisRequest is the request incident-provider files when an applyAction goes stale.
var staleReanalysisRequest = map[string]any{"cause": CauseApplyActionStale,
	"feedback": "applyAction targets a version this cluster no longer serves"}

// wantKeptOpen says an automatic reanalysis' failure left the incident Open with the error and its
// status.reanalysis, and did not close it.
func wantKeptOpen(t *testing.T, c *writerCase, name, why string) {
	t.Helper()
	inc := c.kube.get(ns, name)
	s := st(inc)
	eq(t, "state", s["state"], "Open")
	if closed, _ := inc["spec"].(map[string]any)["closed"].(bool); closed {
		t.Error("spec.closed set: an automatic reanalysis closed an open incident")
	}
	eq(t, "closed reason", annotation(inc, AnnotationClosedReason), "")
	if e := pyfmtStr(s["error"]); !strings.HasPrefix(e, AutomaticReanalysisFailed) || !strings.Contains(e, why) {
		t.Errorf("error %q, want %q…%q", e, AutomaticReanalysisFailed, why)
	}
	re, _ := s["reanalysis"].(map[string]any)
	eq(t, "reanalysis cause", re["cause"], CauseApplyActionStale)
	if s["completedAt"] == nil {
		t.Error("no completedAt")
	}
	if s["howToFix"] != nil {
		t.Errorf("howToFix %v", s["howToFix"])
	}
}

// wantClosedAsFailed says the incident was closed as a failed analysis, as before.
func wantClosedAsFailed(t *testing.T, c *writerCase, name string) {
	t.Helper()
	inc := c.kube.get(ns, name)
	eq(t, "state", st(inc)["state"], "Closed")
	eq(t, "closed reason", annotation(inc, AnnotationClosedReason), ClosedAnalysisFailed)
	if strings.HasPrefix(pyfmtStr(st(inc)["error"]), AutomaticReanalysisFailed) {
		t.Errorf("error %q", st(inc)["error"])
	}
}

// An automatic reanalysis whose agent call keeps failing (a 429 burst) through its retries goes
// back to Open with the error: an automatic action never ends an open incident.
// MUTATION: dropping the cause check in giveUp (closing it) fails this.
func TestAnAutomaticReanalysisThatFailsTransientlyGoesBackToOpen(t *testing.T) {
	for _, fail := range []struct {
		name   string
		rcaErr error
		answer string
		why    string
	}{
		{"call failed", errors.New("429 Too Many Requests"), "", "429 Too Many Requests"},
		{"model error", nil, "LLM error: 429 rate limited", "no structured block"},
		{"empty answer", nil, "", "no output"},
	} {
		t.Run(fail.name, func(t *testing.T) {
			c := newCase(t)
			c.w.cfg.AnalysisRetries = 2
			c.w.sleep = func(context.Context, time.Duration) bool { return true }
			c.analyzedIncident("Open")
			c.kube.writeSpec(ns, incName, map[string]any{"reanalysis": staleReanalysisRequest})
			c.rcaErr, c.answer = fail.rcaErr, fail.answer
			if err := c.reanalyze(); err != nil {
				t.Fatal(err)
			}
			eq(t, "rca calls", len(c.rca), 3) // the first and its two retries
			wantKeptOpen(t, c, incName, fail.why)
		})
	}
}

// A person's reanalysis that fails the same way is closed as a failed analysis, as before.
func TestAPersonsReanalysisThatFailsTransientlyIsClosed(t *testing.T) {
	for _, req := range []map[string]any{{}, {"cause": CausePerson, "feedback": "wrong pod"}} {
		c := newCase(t)
		c.analyzedIncident("Open")
		c.kube.writeSpec(ns, incName, map[string]any{"reanalysis": req})
		c.rcaErr = errors.New("429 Too Many Requests")
		if err := c.reanalyze(); err != nil {
			t.Fatal(err)
		}
		wantClosedAsFailed(t, c, incName)
	}
}

// interruptedReanalysis is an incident a restart left Analyzing in a reanalysis of cause, its RCA
// already rerun recoveries times.
func (c *writerCase) interruptedReanalysis(name, cause string, recoveries string) {
	c.interrupted(name, "a", seed{state: "Analyzing"}, BuildPrompt("CD not ready", "ALERT", "Body LIKE '%x%'", "", "", ""))
	c.kube.writeStatus(ns, name, map[string]any{"reanalysis": map[string]any{"at": "2026-11-12T10:00:00Z",
		"feedback": "f", "count": 1, "cause": cause}})
	if recoveries != "" {
		c.kube.annotate(ns, name, AnnotationRecoveries, recoveries)
	}
}

// After a restart, the final give-up follows the same rule: an automatic reanalysis interrupted
// past its recoveries goes back to Open with the error, a person's is closed.
func TestARestartGivesUpOnAnInterruptedReanalysisByItsCause(t *testing.T) {
	c := newCase(t)
	c.w.spawn = func(f func()) { f() }
	c.interruptedReanalysis("stale-1", CauseApplyActionStale, "1")
	c.interruptedReanalysis("person-1", CausePerson, "1")
	c.w.RecoverInterrupted(context.Background())
	eq(t, "rca calls", len(c.rca), 0)
	wantKeptOpen(t, c, "stale-1", "the alert-provider restarted before it finished")
	wantClosedAsFailed(t, c, "person-1")
}

// A restart's rerun of an interrupted reanalysis that then fails transiently follows it too.
func TestARestartsRerunOfAReanalysisThatFailsFollowsItsCause(t *testing.T) {
	c := newCase(t)
	c.w.spawn = func(f func()) { f() }
	c.rcaErr = errors.New("503")
	c.interruptedReanalysis("stale-1", CauseApplyActionStale, "")
	c.interruptedReanalysis("person-1", CausePerson, "")
	c.w.RecoverInterrupted(context.Background())
	eq(t, "rca calls", len(c.rca), 2)
	wantKeptOpen(t, c, "stale-1", "503")
	wantClosedAsFailed(t, c, "person-1")
}
