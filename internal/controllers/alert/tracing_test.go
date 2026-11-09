package alert

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
	"github.com/krateo-platformops/alert-provider/internal/incident"
	"github.com/krateo-platformops/alert-provider/internal/tracing/tracingtest"
)

// tracedReconcile is one reconcile of the home-namespace Alert name through traced, the wrapper
// Setup puts around provider-runtime's reconciler, under ctx.
func (h *harness) tracedReconcile(ctx context.Context, name string) {
	h.t.Helper()
	r := traced{reconcile.Func(func(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
		return ctrl.Result{}, h.reconcileCtx(ctx, req.Namespace, req.Name)
	})}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
		h.t.Fatal(err)
	}
}

func attr(s tracetest.SpanStub, key string) string {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.Emit()
		}
	}
	return ""
}

// clientChildren are the Client spans under parent, failing the test when there is none or when a
// child is not a Client span.
func clientChildren(t *testing.T, rec *tracingtest.Recorder, parent tracetest.SpanStub) []tracetest.SpanStub {
	t.Helper()
	kids := rec.ChildrenOf(parent.SpanContext)
	if len(kids) == 0 {
		t.Fatalf("%s has no HyperDX client span under it", parent.Name)
	}
	for _, k := range kids {
		if k.SpanKind != trace.SpanKindClient {
			t.Errorf("%s child %q is %v, want a Client span", parent.Name, k.Name, k.SpanKind)
		}
	}
	return kids
}

// A pass that pushes a drifted spec is one trace: alert.reconcile, then alert.observe and
// alert.update under it, each with its HyperDX calls as client spans under it.
func TestAnObserveUpdatePassIsOneTraceWithTheHyperDXCallsUnderItsActions(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	h.pass(1) // creates the HyperDX alert, untraced
	id := h.status(k).HyperdxAlertID
	h.api.Set("alerts", id, map[string]any{"threshold": 0})

	rec := tracingtest.Install(t)
	h.tracedReconcile(context.Background(), k)

	root, obs, upd := rec.One("alert.reconcile"), rec.One("alert.observe"), rec.One("alert.update")
	if n := len(rec.Named("alert.create")); n != 0 {
		t.Errorf("%d alert.create spans on a push pass", n)
	}
	if root.Parent.IsValid() {
		t.Error("alert.reconcile has a parent")
	}
	for _, s := range []tracetest.SpanStub{obs, upd} {
		if s.Parent.SpanID() != root.SpanContext.SpanID() || s.SpanContext.TraceID() != root.SpanContext.TraceID() {
			t.Errorf("%s is not under alert.reconcile", s.Name)
		}
		if attr(s, "alert.namespace") != ns || attr(s, "alert.name") != k {
			t.Errorf("%s names %s/%s", s.Name, attr(s, "alert.namespace"), attr(s, "alert.name"))
		}
		if attr(s, "alert.hyperdx_id") != id || attr(s, "alert.state") != "OK" {
			t.Errorf("%s: hyperdx id %q state %q", s.Name, attr(s, "alert.hyperdx_id"), attr(s, "alert.state"))
		}
	}
	clientChildren(t, rec, obs)
	put := false
	for _, c := range clientChildren(t, rec, upd) {
		put = put || attr(c, "http.request.method") == "PUT"
	}
	if !put {
		t.Error("the threshold PUT is not under alert.update")
	}
}

// The pass that creates the HyperDX objects names its action alert.create.
func TestTheCreatingPassIsAlertCreate(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	rec := tracingtest.Install(t)
	h.tracedReconcile(context.Background(), k)
	rec.One("alert.observe")
	c := rec.One("alert.create")
	if n := len(rec.Named("alert.update")); n != 0 {
		t.Errorf("%d alert.update spans on the creating pass", n)
	}
	clientChildren(t, rec, c)
	if attr(c, "alert.hyperdx_id") != h.status(k).HyperdxAlertID {
		t.Errorf("alert.create id %q", attr(c, "alert.hyperdx_id"))
	}
}

// A firing runs in its own goroutine under the process's context: incident.fire (started by the
// real Writer.Fire) is a new trace linked to the alert.observe that fired it, and the reconcile's
// cancellation does not reach it.
// MUTATION: handing the firing f.ctx instead of tracing.Detach(f.ctx, ctx) fails this.
func TestIncidentFireIsLinkedToTheReconcileAcrossTheGoroutine(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	// A writer homed elsewhere refuses the firing right after opening its span: nothing else runs.
	writer := incident.NewWriter(incident.Config{Namespace: "elsewhere"}, nil, &incident.A2A{}, &incident.Comparer{}, logging.NewNopLogger())
	var wg sync.WaitGroup
	var fired context.Context
	h.conn.firings.run = func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	h.conn.firings.fire = func(c context.Context, a incident.Alert, r incident.Records) {
		fired = c
		writer.Fire(c, a, r)
	}
	h.alerting(nil)

	rec := tracingtest.Install(t)
	rctx, cancel := context.WithCancel(context.Background())
	h.tracedReconcile(rctx, k)
	cancel() // the reconcile is over
	wg.Wait()

	obs, fire := rec.One("alert.observe"), rec.One("incident.fire")
	if fire.Parent.IsValid() {
		t.Errorf("incident.fire has parent %v; want a new root", fire.Parent.SpanID())
	}
	if fire.SpanContext.TraceID() == obs.SpanContext.TraceID() {
		t.Error("incident.fire is in the reconcile's trace")
	}
	if len(fire.Links) != 1 || fire.Links[0].SpanContext.SpanID() != obs.SpanContext.SpanID() ||
		fire.Links[0].SpanContext.TraceID() != obs.SpanContext.TraceID() {
		t.Fatalf("incident.fire links %+v, want alert.observe %v", fire.Links, obs.SpanContext.SpanID())
	}
	if attr(fire, "alert.name") != k {
		t.Errorf("incident.fire alert.name %q", attr(fire, "alert.name"))
	}
	if fired == nil || fired.Err() != nil {
		t.Errorf("the firing's context ended with the reconcile: %v", fired)
	}
}
