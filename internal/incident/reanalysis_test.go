package incident

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/krateo-platformops/alert-provider/internal/report"
)

// retrying is a case whose RCA is retried twice, 2 and 4 minutes after its failures; slept are the
// waits.
func retrying(t *testing.T) (*writerCase, *[]time.Duration) {
	c := newCase(t)
	c.w.cfg.AnalysisRetries, c.w.cfg.AnalysisRetryDelay = 2, 2*time.Minute
	var slept []time.Duration
	c.w.sleep = func(_ context.Context, d time.Duration) bool { slept = append(slept, d); return true }
	return c, &slept
}

func TestAFailedCallIsRetriedThenWritesTheError(t *testing.T) {
	c, slept := retrying(t)
	c.rcaErr = errors.New("503 Server Error: Service Unavailable")
	c.fire(fireOpt{})
	inc := c.kube.only(ns)
	name := inc["metadata"].(map[string]any)["name"].(string)
	eq(t, "waits", *slept, []time.Duration{2 * time.Minute, 4 * time.Minute})
	eq(t, "threads", []string{c.rca[0][1], c.rca[1][1], c.rca[2][1]}, []string{
		uuid.NewSHA1(uuid.NameSpaceDNS, []byte(name)).String(),
		uuid.NewSHA1(uuid.NameSpaceDNS, []byte(name+"#retry-1")).String(),
		uuid.NewSHA1(uuid.NameSpaceDNS, []byte(name+"#retry-2")).String(),
	})
	eq(t, "state", st(inc)["state"], "Open")
	eq(t, "error", st(inc)["error"], "The analysis failed: 503 Server Error: Service Unavailable")
}

func TestARetryThatSucceedsWritesTheAnalysis(t *testing.T) {
	c, slept := retrying(t)
	c.rcaRun = func(n int) (string, error) {
		if n == 0 {
			return "", nil // an empty answer
		}
		return answer(block(how)), nil
	}
	c.fire(fireOpt{})
	inc := c.kube.only(ns)
	eq(t, "waits", len(*slept), 1)
	eq(t, "howToFix", st(inc)["howToFix"], how)
	if _, ok := st(inc)["error"]; ok {
		t.Errorf("error set: %v", st(inc)["error"])
	}
	// Only the final result is written: one Analyzing write, one analysis write.
	eq(t, "status writes", len(c.kube.statusWrites()), 2)
}

func TestAModelErrorAnswerIsRetried(t *testing.T) {
	c, _ := retrying(t)
	c.answer = `LLM error: STREAM_ERROR POST "http://gw/llm/v1/chat/completions": 429 Too Many Requests`
	c.fire(fireOpt{})
	eq(t, "rca calls", len(c.rca), 3)
}

func TestAnAnswerWithoutScriptsIsNotRetried(t *testing.T) {
	c, slept := retrying(t)
	c.answer = "## Root cause\nno structured block here"
	c.fire(fireOpt{})
	eq(t, "rca calls", len(c.rca), 1)
	eq(t, "waits", len(*slept), 0)
}

func TestARetryStopsWhenTheContextEnds(t *testing.T) {
	c, _ := retrying(t)
	c.w.sleep = func(context.Context, time.Duration) bool { return false }
	c.rcaErr = errors.New("read timed out")
	c.fire(fireOpt{})
	eq(t, "rca calls", len(c.rca), 1)
	eq(t, "state", st(c.kube.only(ns))["state"], "Open")
}

const incName = "cd-not-ready-20260925-100000"

// analyzedIncident is an Open incident with a full analysis, a flagged precondition that answered
// unknown, and its original prompt.
func (c *writerCase) analyzedIncident(state string) {
	c.kube.put(ns, incName, alertName, seed{state: state, rootCause: "chart 1.1.9 is missing"})
	c.kube.writeSpec(ns, incName, map[string]any{"prompt": BuildPrompt("CD not ready", "ALERT", "Body LIKE '%x%'", "")})
	c.kube.writeStatus(ns, incName, map[string]any{
		"howToFix": how, "report": "## old report", "sources": []any{map[string]any{"type": "logs"}},
		"lastChecks": map[string]any{"precondition": map[string]any{"exit": 2, "since": "2026-10-19T15:41:19Z",
			"output": "jq: error: Invalid escape at line 1, column 4"}},
		"conditions": []any{
			map[string]any{"type": "Ready", "status": "True"},
			map[string]any{"type": "Reproduced", "status": "False"},
		},
	})
}

// reanalyze runs a reanalysis, its RCA inline.
func (c *writerCase) reanalyze() error {
	c.w.spawn = func(f func()) { f() }
	return c.w.Reanalyze(context.Background(), context.Background(), ns, incName)
}

func TestAReanalysisRunsTheRCAAgainAndWritesTheNewAnalysis(t *testing.T) {
	c := newCase(t)
	c.analyzedIncident("Open")
	c.kube.writeSpec(ns, incName, map[string]any{"reanalysis": map[string]any{"feedback": "the jq in verify does not parse"}})
	if err := c.reanalyze(); err != nil {
		t.Fatal(err)
	}
	inc := c.kube.get(ns, incName)
	if _, ok := inc["spec"].(map[string]any)["reanalysis"]; ok {
		t.Error("spec.reanalysis not removed")
	}
	s := st(inc)
	eq(t, "state", s["state"], "Open")
	eq(t, "howToFix", s["howToFix"], how)
	re := s["reanalysis"].(map[string]any)
	eq(t, "feedback", re["feedback"], "the jq in verify does not parse")
	eq(t, "count", re["count"].(interface{ String() string }).String(), "1")
	for _, k := range []string{"lastChecks", "error"} {
		if _, ok := s[k]; ok {
			t.Errorf("%s left: %v", k, s[k])
		}
	}
	eq(t, "conditions", s["conditions"], []any{map[string]any{"type": "Ready", "status": "True"}})
	eq(t, "thread", c.rca[0][1], uuid.NewSHA1(uuid.NameSpaceDNS, []byte(incName+"#reanalysis-1")).String())

	// The analysis was cleared, with the request recorded, before the RCA ran.
	first := c.kube.statusWrites()[0]
	eq(t, "first state", first["state"], "Analyzing")
	for _, k := range analysisKeys {
		if v, ok := first[k]; !ok || v != nil {
			t.Errorf("%s not cleared: %v", k, v)
		}
	}
}

func TestTheReanalysisPromptCarriesThePreviousAnalysisItsChecksAndTheFeedback(t *testing.T) {
	c := newCase(t)
	c.analyzedIncident("Open")
	c.kube.writeSpec(ns, incName, map[string]any{"reanalysis": map[string]any{"feedback": "the jq does not parse"}})
	if err := c.reanalyze(); err != nil {
		t.Fatal(err)
	}
	p := c.rca[0][0]
	for _, want := range []string{
		"The HyperDX alert \"CD not ready\" has fired",
		"## This is a reanalysis",
		"```text\nthe jq does not parse\n```",
		"Previous root cause: chart 1.1.9 is missing",
		"The incident was Open. Its precondition exited 0 on its first run",
		"Previous precondition script:\n\n```bash\n#!/usr/bin/env bash\n# holds while fireworksapp is not Ready\nexit 1\n```",
		"Its last run exited 2 (since 2026-10-19T15:41:19Z) and printed:\n\n```text\njq: error: Invalid escape at line 1, column 4\n```",
		"Previous verify script:",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if !strings.HasSuffix(p, report.Instructions) || strings.Count(p, report.Instructions) != 1 {
		t.Error("the output instructions do not end the prompt exactly once")
	}
}

func TestASecondReanalysisCountsAndDropsTheOldFeedback(t *testing.T) {
	c := newCase(t)
	c.analyzedIncident("Verifying")
	c.kube.writeSpec(ns, incName, map[string]any{"reanalysis": map[string]any{"feedback": "first"}})
	if err := c.reanalyze(); err != nil {
		t.Fatal(err)
	}
	c.kube.writeSpec(ns, incName, map[string]any{"reanalysis": map[string]any{}})
	if err := c.reanalyze(); err != nil {
		t.Fatal(err)
	}
	re := st(c.kube.get(ns, incName))["reanalysis"].(map[string]any)
	eq(t, "count", re["count"].(interface{ String() string }).String(), "2")
	if _, ok := re["feedback"]; ok {
		t.Errorf("feedback left: %v", re["feedback"])
	}
	if strings.Contains(c.rca[1][0], "What the person found wrong") {
		t.Error("a request without feedback carries a feedback section")
	}
}

func TestAReanalysisOfAnEndedOrAnalyzingIncidentIsDropped(t *testing.T) {
	for _, state := range []string{"Analyzing", "Resolved", "Closed"} {
		t.Run(state, func(t *testing.T) {
			c := newCase(t)
			c.analyzedIncident(state)
			c.kube.writeSpec(ns, incName, map[string]any{"reanalysis": map[string]any{}})
			if err := c.reanalyze(); err != nil {
				t.Fatal(err)
			}
			inc := c.kube.get(ns, incName)
			if _, ok := inc["spec"].(map[string]any)["reanalysis"]; ok {
				t.Error("spec.reanalysis not removed")
			}
			eq(t, "state", st(inc)["state"], state)
			eq(t, "rca calls", len(c.rca), 0)
			eq(t, "status writes", len(c.kube.statusWrites()), 0)
		})
	}
}

func TestNoRequestNoReanalysis(t *testing.T) {
	c := newCase(t)
	c.analyzedIncident("Open")
	if err := c.reanalyze(); err != nil {
		t.Fatal(err)
	}
	eq(t, "rca calls", len(c.rca), 0)
	eq(t, "status writes", len(c.kube.statusWrites()), 0)
}

func TestAReanalysisRereadsAfterAConflict(t *testing.T) {
	c := newCase(t)
	c.analyzedIncident("Open")
	c.kube.writeSpec(ns, incName, map[string]any{"reanalysis": map[string]any{}})
	once := false
	c.kube.beforePatch = func(string, string) {
		if !once {
			once = true
			c.kube.writeStatus(ns, incName, map[string]any{"lastChecks": map[string]any{"precondition": map[string]any{"exit": 1, "since": "x"}}})
		}
	}
	if err := c.reanalyze(); err != nil {
		t.Fatal(err)
	}
	eq(t, "state", st(c.kube.get(ns, incName))["state"], "Open")
	eq(t, "rca calls", len(c.rca), 1)
	if !strings.Contains(c.rca[0][0], "Its last run exited 1") {
		t.Error("the prompt is not from the re-read incident")
	}
}
