package incident

import (
	"context"
	"sync"
	"testing"

	"github.com/krateo-platformops/alert-provider/internal/compare"
	"github.com/krateo-platformops/alert-provider/internal/tracing"
	"github.com/krateo-platformops/alert-provider/internal/tracing/tracingtest"
)

// A firing that opens an incident logs under the span each line runs in: the opening under
// incident.fire, the analysis under incident.analyze. Neither is the caller's span (here the
// alert.observe that hands the firing over), though both are in its trace when it is passed on
// undetached.
// MUTATION: binding Fire's or RunAnalysis' lines to the context they are given, before
// tracing.Start, fails this; so does logging either on w.log, unbound.
func TestAFiringsLinesNameTheirOwnSpans(t *testing.T) {
	rec := tracingtest.Install(t)
	logs := tracingtest.CaptureLogs(t)
	c := newCase(t)
	c.w.log = logs.Logger

	pctx, parent := tracing.Start(context.Background(), "alert.observe")
	c.w.Fire(pctx, Alert{Namespace: ns, Alert: compare.Alert{Name: alertName, Where: "Body LIKE '%x%'", Threshold: "1"}},
		c.recordsOf)
	parent.End()

	obs, fire, an := rec.One("alert.observe"), rec.One("incident.fire"), rec.One("incident.analyze")
	opened, analyzed := logs.One(": opened"), logs.One("analysis written")
	opened.In(t, fire)
	analyzed.In(t, an)
	logs.One(" chars, ").In(t, an) // analysis(), called by RunAnalysis
	for _, l := range []tracingtest.Line{opened, analyzed} {
		if l.SpanID == obs.SpanContext.SpanID().String() {
			t.Errorf("log %q names the caller's alert.observe", l.Msg)
		}
	}
}

// A reanalysis' RCA runs off the reconcile, in a new trace: its lines name that trace's
// incident.analyze, and the request's line names incident.reanalyze.
// MUTATION: binding RunAnalysis' lines to the context it is given, before tracing.Start, fails
// this (the line then has no span at all).
func TestAReanalysisLinesNameReanalyzeThenItsOwnAnalyze(t *testing.T) {
	rec := tracingtest.Install(t)
	logs := tracingtest.CaptureLogs(t)
	c := newCase(t)
	c.w.log = logs.Logger
	var wg sync.WaitGroup
	c.w.spawn = func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	c.analyzedIncident("Open")
	c.kube.writeSpec(ns, incName, map[string]any{"reanalysis": map[string]any{"feedback": "f"}})
	if err := c.w.Reanalyze(context.Background(), context.Background(), ns, incName); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	re, an := rec.One("incident.reanalyze"), rec.One("incident.analyze")
	logs.One("requested, analyzing").In(t, re)
	written := logs.One("analysis written")
	written.In(t, an)
	if written.TraceID == re.SpanContext.TraceID().String() {
		t.Errorf("the reanalysis' RCA line %q is in the reconcile's trace", written.Msg)
	}
}

// NEGATIVE CONTROL. RecoverInterrupted runs at startup, under no span: its lines name none, though
// tracing is on and another span is open while it runs.
func TestAStartupLineNamesNoSpan(t *testing.T) {
	rec := tracingtest.Install(t)
	logs := tracingtest.CaptureLogs(t)
	c := newCase(t)
	c.w.log = logs.Logger
	c.kube.put(ns, "inc-left", alertName, seed{state: "Analyzing", created: "2026-09-25T10:00:00Z"})

	_, other := tracing.Start(context.Background(), "unrelated")
	c.w.RecoverInterrupted(context.Background())
	other.End()

	rec.One("unrelated")
	logs.One("interrupted, opened").Unbound(t)
}
