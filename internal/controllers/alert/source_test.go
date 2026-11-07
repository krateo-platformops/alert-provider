package alert

import (
	"context"
	"strings"
	"testing"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
	"github.com/krateo-platformops/alert-provider/internal/compare"
	"github.com/krateo-platformops/alert-provider/internal/hyperdx/hyperdxtest"
	"github.com/krateo-platformops/alert-provider/internal/incident"
)

func tracesCR(name, where string) *v1alpha1.Alert {
	a := alertCR(name, where, v1alpha1.AlertStatus{})
	a.Spec.Source = v1alpha1.SourceTraces
	return a
}

// --- spec.source: the rows a row Alert reads ---

func TestATracesAlertCountsSpansOverTheTracesSource(t *testing.T) {
	h := newHarness(t, tracesCR(k, "StatusCode = 'Error'"))
	h.pass(1)
	st := h.status(k)
	eq(t, "phase", st.Phase, "Synced")
	tile := h.api.TileOf(h.api.Get("alerts", st.HyperdxAlertID))
	eq(t, "source", tile["sourceId"], "src-t")
	eq(t, "where", h.api.WhereOf(h.api.Get("alerts", st.HyperdxAlertID)), "StatusCode = 'Error'")
}

func TestALogAlertStillCountsTheLogsSource(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	h.pass(1)
	eq(t, "source", h.api.TileOf(h.api.Get("alerts", h.status(k).HyperdxAlertID))["sourceId"], "src-1")
}

// The logs source is found by kind: an install listing its traces first does not move a log
// Alert onto spans.
func TestALogAlertReadsTheLogSourceWhereverItIsListed(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	h.api.Sources = []any{map[string]any{"id": "src-t", "kind": "trace"}, map[string]any{"id": "src-1", "kind": "log"}}
	h.pass(1)
	eq(t, "source", h.api.TileOf(h.api.Get("alerts", h.status(k).HyperdxAlertID))["sourceId"], "src-1")
}

func TestATracesAlertAggregatesAValueReadFromItsSpans(t *testing.T) {
	a := tracesCR(k, "ServiceName = 'snowplow'")
	a.Spec.Value, a.Spec.Aggregation = "Duration / 1e6", v1alpha1.AggregationP95
	h := newHarness(t, a)
	h.pass(1)
	eq(t, "phase", h.status(k).Phase, "Synced")
	tile := h.api.TileOf(h.api.Get("alerts", h.status(k).HyperdxAlertID))
	eq(t, "source", tile["sourceId"], "src-t")
	sel := tile["select"].([]any)[0].(map[string]any)
	eq(t, "value", sel["valueExpression"], "Duration / 1e6")
	eq(t, "aggFn", sel["aggFn"], "quantile")
}

// A switch between logs and traces keeps the `where` but reads other rows: the tile is rewritten,
// the alert kept.
func TestSwitchingTheSourceRewritesTheTile(t *testing.T) {
	h := newHarness(t, alertCR(k, "StatusCode = 'Error'", v1alpha1.AlertStatus{}))
	h.pass(1)
	st := h.status(k)
	h.edit(k, func(a *v1alpha1.Alert) { a.Spec.Source = v1alpha1.SourceTraces })
	h.api.ClearWrites()
	h.pass(1)
	eq(t, "writes", nonWebhookWrites(h.api), []hyperdxtest.Write{hyperdxtest.W("PUT", "dashboards", st.HyperdxDashboardID)})
	eq(t, "alert kept", h.status(k).HyperdxAlertID, st.HyperdxAlertID)
	eq(t, "source", h.api.TileOf(h.api.Get("alerts", st.HyperdxAlertID))["sourceId"], "src-t")
	// and converged: the next pass writes nothing
	h.api.ClearWrites()
	h.pass(1)
	eq(t, "writes", nonWebhookWrites(h.api), []hyperdxtest.Write{})
}

// An install without a traces source cannot evaluate a traces Alert. The spec is valid, so it is
// an error of the pass (phase Error), not a refusal of the spec (phase Invalid), and nothing is
// created.
func TestATracesAlertWithoutATracesSourceIsAnError(t *testing.T) {
	h := newHarness(t, tracesCR(k, "StatusCode = 'Error'"))
	h.api.Sources = []any{map[string]any{"id": "src-1", "kind": "log"}, map[string]any{"id": "src-m", "kind": "metric"}}
	if err := h.reconcile(k); err == nil || !strings.Contains(err.Error(), "no HyperDX trace source configured") {
		t.Fatalf("err %v", err)
	}
	st := h.status(k)
	eq(t, "phase", st.Phase, "Error")
	eq(t, "no alert", st.HyperdxAlertID, "")
	eq(t, "no HyperDX alert", len(h.api.All("alerts")), 0)
}

// A traces Alert's firing carries its source, and its records are its spans: read from the traces
// source, one line per distinct span.
func TestATracesFiringReadsItsSpans(t *testing.T) {
	h := newHarness(t, tracesCR(k, "StatusCode = 'Error'"))
	var got incident.Alert
	var records incident.Records
	h.conn.firings.fire = func(_ context.Context, a incident.Alert, r incident.Records) { got, records = a, r }
	h.pass(1)
	h.api.Set("alerts", h.status(k).HyperdxAlertID, map[string]any{"state": "ALERT"})
	h.pass(1)
	want := compareAlert(k, "StatusCode = 'Error'")
	want.Source = v1alpha1.SourceTraces
	eq(t, "alert", got, incident.Alert{Namespace: ns, Alert: want})
	if _, err := records(ctx, got.Where, 900); err != nil {
		t.Fatal(err)
	}
	reqs := h.api.SeriesRequests()
	series := reqs[len(reqs)-1]["series"].([]any)[0].(map[string]any)
	eq(t, "source", series["sourceId"], "src-t")
	eq(t, "group", series["groupBy"], []any{compare.TraceRowGroup})
}

func TestALogFiringReadsItsLogRecords(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	var records incident.Records
	h.conn.firings.fire = func(_ context.Context, _ incident.Alert, r incident.Records) { records = r }
	h.pass(1)
	h.api.Set("alerts", h.status(k).HyperdxAlertID, map[string]any{"state": "ALERT"})
	h.pass(1)
	if _, err := records(ctx, "w", 900); err != nil {
		t.Fatal(err)
	}
	reqs := h.api.SeriesRequests()
	series := reqs[len(reqs)-1]["series"].([]any)[0].(map[string]any)
	eq(t, "source", series["sourceId"], "src-1")
	eq(t, "group", series["groupBy"], []any{compare.RowGroup})
}

// The incident writer names the traces source by its own constant: the two must be one value.
func TestTheWriterAndTheAPISpellTracesAlike(t *testing.T) {
	eq(t, "traces", compare.Traces, v1alpha1.SourceTraces)
}
