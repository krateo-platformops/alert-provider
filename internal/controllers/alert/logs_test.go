package alert

import (
	"context"
	"sync"
	"testing"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
	"github.com/krateo-platformops/alert-provider/internal/incident"
	"github.com/krateo-platformops/alert-provider/internal/tracing/tracingtest"
)

// captureLogs hands the connector, its firings and its webhook the production log pipeline,
// writing into memory.
func (h *harness) captureLogs() *tracingtest.Logs {
	logs := tracingtest.CaptureLogs(h.t)
	h.conn.log, h.conn.firings.log, h.conn.webhook.log = logs.Logger, logs.Logger, logs.Logger
	return logs
}

// A line logged inside an ExternalClient call names provider-runtime's span around that call, not
// the reconcile root: the firing skipped in Observe names observe, the push names update.
// MUTATION: logging the push on e.log, unbound, fails this; so does binding firings.start's lines
// to the reconcile's context.
func TestALineInsideAnActionNamesTheActionsSpan(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	h.alerting(map[string]string{"krateo.io/paused": "true"}) // ALERT, and paused: Observe skips the firing
	h.api.Set("alerts", h.status(k).HyperdxAlertID, map[string]any{"threshold": 0})

	logs := h.captureLogs()
	rec := tracingtest.Install(t)
	h.tracedReconcile(context.Background(), k)

	root, obs, upd := rec.One("reconcile"), rec.One("observe"), rec.One("update")
	skipped, pushed := logs.One("is paused; firing skipped"), logs.One(": pushed ")
	skipped.In(t, obs)
	pushed.In(t, upd)
	for _, l := range []tracingtest.Line{skipped, pushed} {
		if l.SpanID == root.SpanContext.SpanID().String() {
			t.Errorf("log %q names the reconcile root, not its call's span", l.Msg)
		}
	}
}

// The pass that creates the HyperDX objects logs it under provider-runtime's update span.
// MUTATION: logging create's line on e.log, unbound, fails this.
func TestTheCreateLineNamesTheUpdateSpan(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	logs := h.captureLogs()
	rec := tracingtest.Install(t)
	h.tracedReconcile(context.Background(), k)
	logs.One("synced Alert").In(t, rec.One("update"))
}

// The finalizing pass logs it under provider-runtime's delete span.
// MUTATION: logging Delete's line on e.log, unbound, fails this.
func TestTheDeleteLineNamesTheDeleteSpan(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	h.pass(1)
	if err := h.kube.Delete(ctx, h.get(k)); err != nil {
		t.Fatal(err)
	}
	logs := h.captureLogs()
	rec := tracingtest.Install(t)
	h.tracedReconcile(context.Background(), k)
	logs.One("finalized Alert").In(t, rec.One("delete"))
}

// Connect runs under provider-runtime's connect span: a recreated webhook's line names it.
// MUTATION: logging the reset on w.log, unbound, fails this.
func TestTheWebhookResetLineNamesTheConnectSpan(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	h.pass(1)
	for _, w := range h.api.All("webhooks") {
		h.api.Delete("webhooks", w["id"].(string))
	}
	logs := h.captureLogs()
	rec := tracingtest.Install(t)
	h.tracedReconcile(context.Background(), k)
	logs.One("was recreated").In(t, rec.One("connect"))
}

// A firing's lines are its own incident.fire's, across the goroutine it runs in: a new trace, not
// the reconcile's that handed the firing over.
// MUTATION: binding Writer.Fire's lines before tracing.Start fails this.
func TestAFiringsLinesNameIncidentFireNotTheReconcile(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	logs := h.captureLogs()
	// A writer homed elsewhere refuses the firing right after opening its span, with one line.
	writer := incident.NewWriter(incident.Config{Namespace: "elsewhere"}, nil, &incident.A2A{}, &incident.Comparer{}, logs.Logger)
	var wg sync.WaitGroup
	h.conn.firings.run = func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	h.conn.firings.fire = writer.Fire
	h.alerting(nil)

	rec := tracingtest.Install(t)
	h.tracedReconcile(context.Background(), k)
	wg.Wait()

	root, fire := rec.One("reconcile"), rec.One("incident.fire")
	line := logs.One("no incident and no RCA")
	line.In(t, fire)
	if line.TraceID == root.SpanContext.TraceID().String() {
		t.Errorf("the firing's line %q is in the reconcile's trace", line.Msg)
	}
}
