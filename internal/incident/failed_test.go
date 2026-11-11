package incident

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// --- failed analyses are closed, never left Open ---

// Every way an analysis can end without scripts, retries and the correction spent, closes the
// incident with the reason: never Open with nothing to run.
// MUTATION: writing a failed analysis with finish(open) instead of giveUp fails every case.
func TestAFailedAnalysisIsClosedNeverLeftOpen(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		err          error
		why          string
	}{
		{"failed call", "", errors.New("a2a: read timed out (read timeout=15m0s)"), "The analysis failed: a2a: read timed out"},
		{"empty answer", "", nil, "The analysis returned no output."},
		{"model error", "LLM error: 503 Service Unavailable", nil, "no structured block"},
		{"no structured block", "## Root cause\nprose only", nil, "no structured block"},
		{"no howToFix", answer(block(nil)), nil, "no usable howToFix"},
		{"broken scripts", answer(block(map[string]any{"precondition": "kubectl get nodes.core n1 || exit 2",
			"apply": "true", "verify": "exit 0"})), nil, "scripts that cannot run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCase(t)
			c.answer, c.rcaErr = tc.answer, tc.err
			c.fire(fireOpt{})
			inc := c.kube.only(ns)
			s := st(inc)
			eq(t, "state", s["state"], "Closed")
			eq(t, "closed in spec", spec(inc)["closed"], true)
			eq(t, "closed reason", annotation(inc, AnnotationClosedReason), ClosedAnalysisFailed)
			c.closedInOnePatch(t, str(metadata(inc), "name"))
			if !strings.Contains(pyfmtStr(s["error"]), tc.why) {
				t.Errorf("error %q, want %q", s["error"], tc.why)
			}
			if _, ok := s["completedAt"]; !ok {
				t.Error("no completedAt: the hold counts from it")
			}
			if _, ok := s["howToFix"]; ok {
				t.Errorf("howToFix written: %v", s["howToFix"])
			}
			for _, w := range c.kube.statusWrites() {
				if w["state"] == "Open" {
					t.Errorf("a status write opened it: %v", w)
				}
				if _, ok := w["resolution"]; ok {
					t.Errorf("the writer wrote resolution, incident-provider's: %v", w)
				}
			}
		})
	}
}

// later moves the writer's clock a minute on, so the next incident gets a name of its own.
func (c *writerCase) later() {
	now := c.w.now().Add(time.Minute)
	c.w.now = func() time.Time { return now }
}

// closedInOnePatch asserts that name was closed by exactly one object patch, which sets both
// spec.closed and the closed-reason annotation: incident-provider records resolution.by
// analysis-failed only for that one patch from one field manager.
func (c *writerCase) closedInOnePatch(t *testing.T, name string) {
	t.Helper()
	closing := 0
	for _, p := range c.kube.objectPatches(name) {
		closed := false
		if s, ok := p["spec"].(map[string]any); ok {
			closed, _ = s["closed"].(bool)
		}
		meta, _ := p["metadata"].(map[string]any)
		ann, _ := meta["annotations"].(map[string]any)
		reason := ann[AnnotationClosedReason] == ClosedAnalysisFailed
		if closed != reason {
			t.Errorf("%s: a patch sets spec.closed=%v but closed-reason=%v; one patch must set both: %v", name, closed, reason, p)
		}
		if closed {
			closing++
		}
	}
	if closing != 1 {
		t.Errorf("%s: closed by %d patches, want exactly one", name, closing)
	}
}

func pyfmtStr(v any) string {
	s, _ := v.(string)
	return s
}

// The close is ONE merge patch setting spec.closed and the closed-reason annotation together:
// incident-provider (from the release carrying its #19) records resolution.by analysis-failed only
// when one field manager set both in one patch.
// MUTATION: splitting giveUp's patch into an annotation patch and a spec patch fails this.
func TestTheFailedAnalysisCloseIsOnePatchSettingBoth(t *testing.T) {
	c := newCase(t)
	c.rcaErr = errors.New("503")
	c.fire(fireOpt{})
	name := str(metadata(c.kube.only(ns)), "name")
	patches := c.kube.objectPatches(name)
	if len(patches) != 1 {
		t.Fatalf("%d object patches, want the one close: %v", len(patches), patches)
	}
	eq(t, "the close", patches[0], map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{AnnotationClosedReason: ClosedAnalysisFailed}},
		"spec":     map[string]any{"closed": true}})
}

// The close is spec.closed: the incident controller's own transition. While that controller
// lags, the incident is still Analyzing, but closed in spec, so it is over: it neither holds the
// alert's firings forever nor counts as interrupted.
func TestAClosedInSpecIncidentIsOverBeforeTheControllerMovesIt(t *testing.T) {
	c := newCase(t)
	c.kube.noController = true
	c.rcaErr = errors.New("503")
	c.fire(fireOpt{})
	inc := c.kube.only(ns)
	eq(t, "state", st(inc)["state"], "Analyzing")
	eq(t, "closed in spec", spec(inc)["closed"], true)
	if !ended(inc) {
		t.Error("not ended")
	}
	name := str(metadata(inc), "name")
	c.w.RecoverInterrupted(context.Background())
	eq(t, "rca after recovery", len(c.rca), 1)
	// Past the hold, a firing opens a new incident instead of being held by an "Analyzing" one.
	c.kube.writeStatus(ns, name, map[string]any{"completedAt": ago(hold + time.Minute)})
	c.rcaErr = nil
	c.later()
	c.fire(fireOpt{})
	eq(t, "incidents", c.kube.count(), 2)
}

// A person who closed or moved on the incident while it was analyzing keeps it: no close, no
// reason, the analysis written without a state.
func TestAFailedAnalysisOfAnIncidentAPersonMovedOnIsNotClosedAgain(t *testing.T) {
	for _, moved := range []map[string]any{{"state": "Closed", "resolution": map[string]any{"by": "user", "at": "t"}}, {"state": "Verifying"}} {
		c := newCase(t)
		c.rcaErr = errors.New("503")
		c.duringRCA = func() { c.kube.writeStatus(ns, str(metadata(c.kube.only(ns)), "name"), moved) }
		c.fire(fireOpt{})
		inc := c.kube.only(ns)
		eq(t, "state", st(inc)["state"], moved["state"])
		eq(t, "closed reason", annotation(inc, AnnotationClosedReason), "")
		if !strings.Contains(pyfmtStr(st(inc)["error"]), "503") {
			t.Errorf("error %v", st(inc)["error"])
		}
	}
	c := newCase(t)
	c.kube.noController = true
	c.rcaErr = errors.New("503")
	c.duringRCA = func() { c.kube.writeSpec(ns, str(metadata(c.kube.only(ns)), "name"), map[string]any{"closed": true}) }
	c.fire(fireOpt{})
	eq(t, "closed reason", annotation(c.kube.only(ns), AnnotationClosedReason), "")
}

// When the close is refused, the failed analysis is written Open, as before failed analyses were
// closed, so it still says why it has no scripts; the next incident of its alert closes it.
func TestARefusedCloseFallsBackToOpenWithTheError(t *testing.T) {
	c := newCase(t)
	c.rcaErr = errors.New("503")
	c.kube.failPatch = apierrors.NewForbidden(incidentsGR, "x", errors.New("no"))
	c.fire(fireOpt{})
	s := st(c.kube.only(ns))
	eq(t, "state", s["state"], "Open")
	if !strings.Contains(pyfmtStr(s["error"]), "503") {
		t.Errorf("error %v", s["error"])
	}
}

// --- the hold and the supersede ---

// closedFailed seeds an incident the writer closed as a failed analysis, failed d ago.
func (c *writerCase) closedFailed(name string, d time.Duration) {
	c.kube.put(ns, name, alertName, seed{state: "Closed", completed: ago(d), created: ago(d + time.Minute)})
	c.kube.writeStatus(ns, name, map[string]any{"error": "The analysis failed: 503",
		"resolution": map[string]any{"by": "user", "at": ago(d)}})
	c.kube.writeSpec(ns, name, map[string]any{"closed": true})
	c.kube.annotate(ns, name, AnnotationClosedReason, ClosedAnalysisFailed)
}

// Closed, a failed analysis still takes its alert's firings for the hold: while the agent or its
// model is down, a new incident every pass would fail the same way.
// MUTATION: dropping the closed-failed rule from pick fails this.
func TestAClosedFailedAnalysisTakesFiringsInsideTheHold(t *testing.T) {
	c := newCase(t)
	c.closedFailed(alertName+"-x", hold-time.Minute)
	c.fire(fireOpt{})
	eq(t, "covered", c.took(alertName+"-x"), 1)
	eq(t, "incidents", c.kube.count(), 1)
	eq(t, "rca", len(c.rca), 0)
	eq(t, "state", st(c.kube.get(ns, alertName+"-x"))["state"], "Closed")
}

// Past the hold, the firing opens a new incident with a fresh RCA, which names the failed one;
// the failed one names it back. Neither is reopened or closed again.
func TestPastTheHoldANewIncidentSupersedesTheClosedFailedOne(t *testing.T) {
	c := newCase(t)
	old := alertName + "-x"
	c.closedFailed(old, hold+time.Minute)
	c.fire(fireOpt{})
	eq(t, "rca", len(c.rca), 1)
	inc := c.opened(old)
	name := str(metadata(inc), "name")
	eq(t, "supersedes", annotation(inc, AnnotationSupersedesFailed), old)
	x := c.kube.get(ns, old)
	eq(t, "superseded by", annotation(x, AnnotationSupersededBy), name)
	eq(t, "state", st(x)["state"], "Closed")
	for _, p := range c.kube.objectPatches(old) {
		if _, ok := p["spec"]; ok {
			t.Errorf("a closed incident was closed again: %v", p)
		}
	}
	// Superseded, it holds nothing and is not superseded twice.
	c.kube.writeStatus(ns, name, map[string]any{"completedAt": ago(hold + time.Minute)})
	c.kube.writeSpec(ns, name, map[string]any{"closed": true})
	c.kube.annotate(ns, name, AnnotationClosedReason, ClosedAnalysisFailed)
	c.later()
	c.fire(fireOpt{})
	third := c.opened(old, name)
	eq(t, "supersedes", annotation(third, AnnotationSupersedesFailed), name)
}

// A failed analysis still Open from before failed analyses were closed is superseded and closed
// by the alert's next incident, newest first, at most five; an analyzed incident and a person's
// incident are not.
func TestANewIncidentClosesTheOpenFailedAnalysesItSupersedes(t *testing.T) {
	c := newCase(t)
	var failed []string
	for i := range 6 {
		name := alertName + "-f" + string(rune('0'+i))
		c.kube.put(ns, name, alertName, seed{state: "Open", completed: ago(hold + time.Hour), created: ago(time.Duration(48-i) * time.Hour)})
		c.kube.writeStatus(ns, name, map[string]any{"error": Interrupted})
		failed = append(failed, name)
	}
	c.analyzed(alertName+"-ok", ago(time.Hour), "")
	c.match = ""
	c.fire(fireOpt{})
	inc := c.opened(append(failed, alertName+"-ok")...)
	name := str(metadata(inc), "name")
	eq(t, "supersedes", annotation(inc, AnnotationSupersedesFailed), strings.Join([]string{failed[5], failed[4], failed[3], failed[2], failed[1]}, ","))
	for _, f := range failed[1:] {
		x := c.kube.get(ns, f)
		eq(t, f+" state", st(x)["state"], "Closed")
		eq(t, f+" reason", annotation(x, AnnotationClosedReason), ClosedAnalysisFailed)
		eq(t, f+" by", annotation(x, AnnotationSupersededBy), name)
		c.closedInOnePatch(t, f)
	}
	eq(t, "the sixth", st(c.kube.get(ns, failed[0]))["state"], "Open")
	eq(t, "analyzed", st(c.kube.get(ns, alertName+"-ok"))["state"], "Open")
}

// A person's incident has no scripts by design: it is never superseded or closed.
func TestAPersonsIncidentIsNeverSuperseded(t *testing.T) {
	c := newCase(t)
	c.fireHuman()
	inc := c.kube.only(ns)
	name := str(metadata(inc), "name")
	c.kube.writeStatus(ns, name, map[string]any{"completedAt": ago(hold + time.Hour)})
	c.fire(fireOpt{}) // the alert lost its label
	eq(t, "state", st(c.kube.get(ns, name))["state"], "Open")
	eq(t, "superseded by", annotation(c.kube.get(ns, name), AnnotationSupersededBy), "")
}

// --- recovery after a restart ---

func (c *writerCase) interrupted(name, alert string, s seed, prompt string) {
	c.kube.put(ns, name, alert, s)
	if prompt != "" {
		c.kube.writeSpec(ns, name, map[string]any{"prompt": prompt})
	}
}

// An analysis a restart interrupted runs again at startup, once, on a thread of its own, and is
// written like a first one.
// MUTATION: opening (or closing) an interrupted incident instead of rerunning it fails this.
func TestARestartRerunsAnInterruptedAnalysis(t *testing.T) {
	c := newCase(t)
	c.w.spawn = func(f func()) { f() }
	c.interrupted("a-1", "a", seed{state: "Analyzing"}, "prompt a")
	c.interrupted("b-1", "b", seed{}, "prompt b") // its first status write never landed
	c.kube.put(ns, "c-1", "c", seed{state: "Open"})
	c.w.RecoverInterrupted(context.Background())
	eq(t, "rca", len(c.rca), 2)
	for i, n := range []string{"a-1", "b-1"} {
		inc := c.kube.get(ns, n)
		eq(t, n+" state", st(inc)["state"], "Open")
		eq(t, n+" howToFix", st(inc)["howToFix"], how)
		eq(t, n+" recoveries", annotation(inc, AnnotationRecoveries), "1")
		eq(t, n+" thread", c.rca[i][1], uuid.NewSHA1(uuid.NameSpaceDNS, []byte(n+"#recovery-1")).String())
	}
	eq(t, "prompt", c.rca[0][0], "prompt a")
	if _, ok := st(c.kube.get(ns, "c-1"))["error"]; ok {
		t.Error("an Open incident was touched")
	}
}

// Interrupted again, or with no prompt to run, it is closed as a failed analysis with the reason.
func TestAnAnalysisInterruptedAgainIsClosed(t *testing.T) {
	c := newCase(t)
	c.w.spawn = func(f func()) { f() }
	c.interrupted("a-1", "a", seed{state: "Analyzing"}, "prompt a")
	c.kube.annotate(ns, "a-1", AnnotationRecoveries, "1")
	c.interrupted("b-1", "b", seed{state: "Analyzing"}, "")
	c.w.RecoverInterrupted(context.Background())
	eq(t, "rca", len(c.rca), 0)
	for _, n := range []string{"a-1", "b-1"} {
		inc := c.kube.get(ns, n)
		eq(t, n, []any{st(inc)["state"], st(inc)["error"], annotation(inc, AnnotationClosedReason)},
			[]any{"Closed", Interrupted, ClosedAnalysisFailed})
		c.closedInOnePatch(t, n)
		if _, ok := st(inc)["completedAt"]; !ok {
			t.Errorf("%s: no completedAt", n)
		}
	}
}

// A rerun that fails is closed like any failed analysis.
func TestARerunThatFailsIsClosed(t *testing.T) {
	c := newCase(t)
	c.w.spawn = func(f func()) { f() }
	c.rcaErr = errors.New("503")
	c.interrupted("a-1", "a", seed{state: "Analyzing"}, "prompt a")
	c.w.RecoverInterrupted(context.Background())
	eq(t, "state", st(c.kube.get(ns, "a-1"))["state"], "Closed")
}

// An interrupted reanalysis reruns with its feedback; the previous analysis was cleared.
func TestAnInterruptedReanalysisRerunsWithItsFeedback(t *testing.T) {
	c := newCase(t)
	c.w.spawn = func(f func()) { f() }
	original := BuildPrompt("CD not ready", "ALERT", "Body LIKE '%x%'", "", "", "")
	c.interrupted("a-1", "a", seed{state: "Analyzing"}, original)
	c.kube.writeStatus(ns, "a-1", map[string]any{"reanalysis": map[string]any{"at": "t", "feedback": "wrong pod", "count": 1}})
	c.w.RecoverInterrupted(context.Background())
	eq(t, "rca", len(c.rca), 1)
	eq(t, "prompt", c.rca[0][0], ReanalysisPrompt(original, map[string]any{}, "wrong pod", CausePerson))
}

// An interrupted reanalysis that a stale applyAction requested reruns with that cause's intro.
func TestAnInterruptedStaleReanalysisRerunsWithItsCause(t *testing.T) {
	c := newCase(t)
	c.w.spawn = func(f func()) { f() }
	original := BuildPrompt("CD not ready", "ALERT", "Body LIKE '%x%'", "", "", "")
	c.interrupted("a-1", "a", seed{state: "Analyzing"}, original)
	c.kube.writeStatus(ns, "a-1", map[string]any{"reanalysis": map[string]any{"at": "t", "feedback": "stale",
		"count": 1, "cause": CauseApplyActionStale}})
	c.w.RecoverInterrupted(context.Background())
	eq(t, "rca", len(c.rca), 1)
	eq(t, "prompt", c.rca[0][0], ReanalysisPrompt(original, map[string]any{}, "stale", CauseApplyActionStale))
}

// A person's incident whose first write never landed gets it: Open, with HumanOnly. No model runs.
func TestAnInterruptedPersonsIncidentIsOpened(t *testing.T) {
	c := newCase(t)
	c.w.spawn = func(f func()) { f() }
	c.kube.put(ns, "h-1", alertName, seed{})
	c.kube.writeSpec(ns, "h-1", map[string]any{"prompt": "p"})
	c.kube.mu.Lock()
	c.kube.incidents[ns+"/h-1"]["metadata"].(map[string]any)["labels"] = map[string]any{LabelAlert: alertName, LabelAnalysis: AnalysisHuman}
	c.kube.mu.Unlock()
	c.w.RecoverInterrupted(context.Background())
	eq(t, "rca", len(c.rca), 0)
	s := st(c.kube.get(ns, "h-1"))
	eq(t, "status", []any{s["state"], s["error"]}, []any{"Open", HumanOnly})
}

// --- scripts are checked before they are written ---

// A real precondition, apply and verify, in the form the RCA instructions teach.
var realFix = map[string]any{
	"precondition": "#!/usr/bin/env bash\n# Holds while web still runs the image tag that does not exist.\n" +
		"d=$(kubectl get deployments.apps web -n shop -o json --ignore-not-found) || exit 2\n[[ -n $d ]] || exit 0\n" +
		"jq -e --arg c web --arg i reg/web:9.9 'any(.spec.template.spec.containers[]; .name == $c and .image == $i)' <<<\"$d\" >/dev/null\n" +
		"case $? in 0) exit 1 ;; 1) exit 0 ;; *) exit 2 ;; esac\n",
	"apply": "#!/usr/bin/env bash\n# Put web back on the tag that exists.\nset -euo pipefail\n" +
		"kubectl set image deployments.apps/web -n shop web=reg/web:1.2\n",
	"verify": "#!/usr/bin/env bash\n# Fixed once every replica runs a pullable image.\n" +
		"d=$(kubectl get deployments.apps web -n shop -o json) || exit 2\n" +
		"jq -e '.status.observedGeneration >= .metadata.generation and .status.availableReplicas == .spec.replicas' <<<\"$d\" >/dev/null\n" +
		"case $? in 0) exit 0 ;; 1) exit 1 ;; *) exit 2 ;; esac\n",
}

func withScript(name, src string) map[string]any {
	out := map[string]any{}
	for k, v := range realFix {
		out[k] = v
	}
	out[name] = src
	return out
}

// The four failures seen on krateo-057, one per rule.
var brokenFixes = []struct {
	name, script, src, says string
}{
	{"broken bash", "precondition", "#!/usr/bin/env bash\nif [[ -n $d ]]; then\n  exit 1\n", "bash cannot parse it"},
	{"bad jq", "verify", "#!/usr/bin/env bash\nd=$(kubectl get pods -n shop -o json) || exit 2\njq -e '.items[0].metadata.name | test(\"web\\-1\")' <<<\"$d\"\n", "does not parse"},
	{"Alert CR read", "precondition", "#!/usr/bin/env bash\nw=$(kubectl get alerts.observability.krateo.io a -n krateo-system -o jsonpath='{.spec.where}') || exit 2\n[[ -n $w ]] && exit 1\n", "reads the Alert CR"},
	{"nodes.core", "verify", "#!/usr/bin/env bash\nkubectl get nodes.core n1 -o json >/dev/null || exit 2\nexit 0\n", "core kind with a .core suffix"},
}

// Each broken script goes back to the agent once, in the same thread, with its problem; the
// corrected answer is written.
// MUTATION: skipping any one of scriptcheck's four rules fails its subtest; dropping the
// correction turn fails every subtest.
func TestABrokenScriptIsSentBackOnceAndTheCorrectionWritten(t *testing.T) {
	for _, b := range brokenFixes {
		t.Run(b.name, func(t *testing.T) {
			c := newCase(t)
			c.rcaRun = func(n int) (string, error) {
				if n == 0 {
					return answer(block(withScript(b.script, b.src))), nil
				}
				return answer(block(realFix)), nil
			}
			c.fire(fireOpt{})
			if len(c.rca) != 2 {
				t.Fatalf("rca calls %d, want 2 (the broken answer refused, then corrected)", len(c.rca))
			}
			eq(t, "thread", c.rca[1][1], c.rca[0][1])
			if !strings.Contains(c.rca[1][0], b.script) || !strings.Contains(c.rca[1][0], b.says) {
				t.Errorf("correction %q does not say %q of %s", c.rca[1][0], b.says, b.script)
			}
			s := st(c.kube.only(ns))
			eq(t, "state", s["state"], "Open")
			eq(t, "howToFix", s["howToFix"], realFix)
			for _, w := range c.kube.statusWrites() {
				if fix, ok := w["howToFix"].(map[string]any); ok && fix[b.script] == b.src {
					t.Error("the broken script was written")
				}
			}
		})
	}
}

// A script still broken after the correction is never written: the analysis fails, and is
// closed with the problem in error and missingContext.
func TestAScriptStillBrokenAfterTheCorrectionClosesTheIncident(t *testing.T) {
	for _, b := range brokenFixes {
		t.Run(b.name, func(t *testing.T) {
			c := newCase(t)
			c.answer = answer(block(withScript(b.script, b.src)))
			c.fire(fireOpt{})
			eq(t, "rca calls", len(c.rca), 2)
			inc := c.kube.only(ns)
			s := st(inc)
			eq(t, "state", s["state"], "Closed")
			eq(t, "closed reason", annotation(inc, AnnotationClosedReason), ClosedAnalysisFailed)
			if _, ok := s["howToFix"]; ok {
				t.Errorf("howToFix written: %v", s["howToFix"])
			}
			e := pyfmtStr(s["error"])
			if !strings.Contains(e, "scripts that cannot run") || !strings.Contains(e, b.says) || !strings.Contains(e, "sent back once") {
				t.Errorf("error %q", e)
			}
			mc, _ := s["missingContext"].([]any)
			if len(mc) == 0 || !strings.Contains(pyfmtStr(mc[len(mc)-1]), b.says) {
				t.Errorf("missingContext %v", mc)
			}
			eq(t, "root cause kept", s["rootCause"].(map[string]any)["statement"], "chart 1.1.9 is missing")
		})
	}
}

// An alert-query false positive reads the Alert in its checks, guarded: written as given.
func TestAGuardedAlertReadIsWrittenUntouched(t *testing.T) {
	c := newCase(t)
	fp := map[string]any{
		"precondition": "#!/usr/bin/env bash\n# Holds while a's where does not exclude svc-a.\n" +
			"w=$(kubectl get alerts.observability.krateo.io a -n krateo-system -o jsonpath='{.spec.where}' --ignore-not-found) || exit 2\n" +
			"[[ -n $w ]] || exit 0\n[[ $w == *\"'svc-a'\"* ]] || exit 1\nexit 0\n",
		"apply": "#!/usr/bin/env bash\n# Exclude svc-a.\nset -euo pipefail\n" +
			"kubectl patch alerts.observability.krateo.io a -n krateo-system --type merge -p '{\"spec\":{\"where\":\"x\"}}'\n",
		"verify": "#!/usr/bin/env bash\n# Fixed once the where excludes svc-a, or a is gone.\n" +
			"o=$(kubectl get alerts.observability.krateo.io a -n krateo-system -o json --ignore-not-found) || exit 2\n" +
			"[[ -n $o ]] || exit 0\njq -e --arg a \"'svc-a'\" '.spec.where // \"\" | contains($a)' <<<\"$o\" >/dev/null\n" +
			"case $? in 0) exit 0 ;; 1) exit 1 ;; *) exit 2 ;; esac\n",
	}
	c.answer = answer(block(fp))
	c.fire(fireOpt{})
	eq(t, "rca calls", len(c.rca), 1)
	s := st(c.kube.only(ns))
	eq(t, "state", s["state"], "Open")
	eq(t, "howToFix", s["howToFix"], fp)
}

// Valid scripts pass untouched: one RCA call, written as given.
func TestValidScriptsAreWrittenUntouched(t *testing.T) {
	c := newCase(t)
	c.answer = answer(block(realFix))
	c.fire(fireOpt{})
	eq(t, "rca calls", len(c.rca), 1)
	s := st(c.kube.only(ns))
	eq(t, "howToFix", s["howToFix"], realFix)
	eq(t, "state", s["state"], "Open")
}

// A broken rollback alone is left out, as the parser leaves out an unusable one: the fix works
// without it. No correction is asked for.
func TestABrokenRollbackAloneIsLeftOut(t *testing.T) {
	c := newCase(t)
	c.answer = answer(block(withScript("rollback", "kubectl set image deployments.apps/web -n shop web=reg/web:9.9 && if true; then")))
	c.fire(fireOpt{})
	eq(t, "rca calls", len(c.rca), 1)
	s := st(c.kube.only(ns))
	eq(t, "howToFix", s["howToFix"], realFix)
	mc, _ := s["missingContext"].([]any)
	if len(mc) == 0 || !strings.HasPrefix(pyfmtStr(mc[len(mc)-1]), "No usable rollback (rollback, line 1: bash cannot parse it") {
		t.Errorf("missingContext %v", mc)
	}
}

// A structured block without any howToFix is the agent's choice: it is not sent back.
func TestAnAnswerWithoutAnyHowToFixIsNotSentBack(t *testing.T) {
	c := newCase(t)
	c.answer = answer(block(nil))
	c.fire(fireOpt{})
	eq(t, "rca calls", len(c.rca), 1)
	eq(t, "state", st(c.kube.only(ns))["state"], "Closed")
}

// A correction whose call fails leaves the first answer's analysis, closed with both reasons.
func TestAFailedCorrectionKeepsTheFirstAnswer(t *testing.T) {
	c := newCase(t)
	c.rcaRun = func(n int) (string, error) {
		if n == 0 {
			return answer(block(withScript("verify", "kubectl get nodes.core n1 || exit 2"))), nil
		}
		return "", errors.New("503")
	}
	c.fire(fireOpt{})
	s := st(c.kube.only(ns))
	eq(t, "state", s["state"], "Closed")
	e := pyfmtStr(s["error"])
	if !strings.Contains(e, "nodes.core") || !strings.Contains(e, "its call failed: 503") {
		t.Errorf("error %q", e)
	}
}
