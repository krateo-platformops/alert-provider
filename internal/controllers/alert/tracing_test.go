package alert

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/resource/fake"
	prtracing "github.com/krateo-platformops/provider-runtime/pkg/tracing"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
	"github.com/krateo-platformops/alert-provider/internal/incident"
	"github.com/krateo-platformops/alert-provider/internal/tracing/tracingtest"
)

// runtime is the Alert controller's reconciler as Setup builds it (newReconciler) over the
// harness's fake apiserver and connector, with opts.
func (h *harness) runtime(opts ...reconciler.ReconcilerOption) reconcile.Reconciler {
	return newReconciler(&fake.Manager{Client: h.kube, Scheme: h.kube.Scheme()}, h.conn, h.conn.interval, opts...)
}

// tracedReconcile is one reconcile of the home-namespace Alert name by that reconciler, under ctx.
// With tracingtest.Install, provider-runtime spans it through the production pipeline.
func (h *harness) tracedReconcile(ctx context.Context, name string) {
	h.t.Helper()
	if _, err := h.runtime().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
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

// noAlertSpans fails the test when any span is named alert.*: the Alert controller starts none of
// its own since provider-runtime spans the reconcile (design-tracing §11.3).
func noAlertSpans(t *testing.T, rec *tracingtest.Recorder) {
	t.Helper()
	for _, s := range rec.Spans() {
		if strings.HasPrefix(s.Name, "alert.") {
			t.Errorf("span %q: the Alert controller's own spans would double provider-runtime's; exported %v",
				s.Name, tracingtest.Names(rec.Spans()))
		}
	}
}

// A pass that pushes a drifted spec is one trace: provider-runtime's reconcile root, with connect,
// observe and update under it, the Alert's attributes on observe and update, and each one's HyperDX
// calls as client spans under it. No alert.* span doubles them.
// MUTATION: keeping the traced wrapper (alert.reconcile around provider-runtime's reconciler) fails
// this; so does keeping the action spans (alert.observe inside observe).
func TestAPassIsOneTraceUnderProviderRuntimesReconcile(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	h.pass(1) // creates the HyperDX alert, untraced
	id := h.status(k).HyperdxAlertID
	h.api.Set("alerts", id, map[string]any{"threshold": 0})

	rec := tracingtest.Install(t)
	h.tracedReconcile(context.Background(), k)

	noAlertSpans(t, rec)
	root, conn, obs, upd := rec.One("reconcile"), rec.One("connect"), rec.One("observe"), rec.One("update")
	if n := len(rec.Named("create")); n != 0 {
		t.Errorf("%d create spans on a push pass", n)
	}
	if root.Parent.IsValid() {
		t.Error("reconcile has a parent")
	}
	if attr(root, "k8s.object.kind") != "Alert" || attr(root, "k8s.object.name") != k || attr(root, "k8s.object.namespace") != ns {
		t.Errorf("reconcile attributes %v", root.Attributes)
	}
	for _, s := range rec.Spans() {
		if s.SpanContext.TraceID() != root.SpanContext.TraceID() {
			t.Errorf("span %q is in another trace", s.Name)
		}
	}
	for _, s := range []tracetest.SpanStub{conn, obs, upd} {
		tracingtest.Under(t, s, root)
	}
	for _, s := range []tracetest.SpanStub{obs, upd} {
		if attr(s, "alert.namespace") != ns || attr(s, "alert.name") != k {
			t.Errorf("%s names %s/%s", s.Name, attr(s, "alert.namespace"), attr(s, "alert.name"))
		}
		if attr(s, "alert.hyperdx_id") != id || attr(s, "alert.state") != "OK" {
			t.Errorf("%s: hyperdx id %q state %q", s.Name, attr(s, "alert.hyperdx_id"), attr(s, "alert.state"))
		}
	}
	if attr(obs, "hyperdx.alerts_list.cached") != "false" {
		t.Errorf("observe hyperdx.alerts_list.cached %q", attr(obs, "hyperdx.alerts_list.cached"))
	}
	if attr(upd, "alert.action") != "update" {
		t.Errorf("update alert.action %q", attr(upd, "alert.action"))
	}
	clientChildren(t, rec, obs)
	put := false
	for _, c := range clientChildren(t, rec, upd) {
		put = put || attr(c, "http.request.method") == "PUT"
	}
	if !put {
		t.Error("the threshold PUT is not under update")
	}
}

// The pass that creates the HyperDX objects does it in provider-runtime's update span (Observe
// never reports the alert missing, so Create is never called), marked alert.action=create.
func TestTheCreatingPassIsAnUpdateSpanMarkedCreate(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	rec := tracingtest.Install(t)
	h.tracedReconcile(context.Background(), k)
	noAlertSpans(t, rec)
	root := rec.One("reconcile")
	tracingtest.Under(t, rec.One("observe"), root)
	u := rec.One("update")
	tracingtest.Under(t, u, root)
	if n := len(rec.Named("create")); n != 0 {
		t.Errorf("%d create spans: Create is never called", n)
	}
	clientChildren(t, rec, u)
	if attr(u, "alert.action") != "create" || attr(u, "alert.hyperdx_id") != h.status(k).HyperdxAlertID {
		t.Errorf("update attributes %v", u.Attributes)
	}
}

// The finalizing pass carries the Alert on provider-runtime's delete span.
func TestTheFinalizingPassAnnotatesTheDeleteSpan(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	h.pass(1)
	if err := h.kube.Delete(ctx, h.get(k)); err != nil {
		t.Fatal(err)
	}
	rec := tracingtest.Install(t)
	h.tracedReconcile(context.Background(), k)
	noAlertSpans(t, rec)
	d := rec.One("delete")
	tracingtest.Under(t, d, rec.One("reconcile"))
	if attr(d, "alert.name") != k || attr(d, "alert.namespace") != ns {
		t.Errorf("delete attributes %v", d.Attributes)
	}
}

// A firing runs in its own goroutine under the process's context: incident.fire (started by the
// real Writer.Fire) is a new trace linked to provider-runtime's observe that fired it, and the
// reconcile's cancellation does not reach it.
// MUTATION: handing the firing f.ctx instead of tracing.Detach(f.ctx, ctx) fails this.
func TestIncidentFireIsLinkedToTheRuntimesObserve(t *testing.T) {
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

	noAlertSpans(t, rec)
	obs, fire := rec.One("observe"), rec.One("incident.fire")
	tracingtest.Under(t, obs, rec.One("reconcile"))
	if fire.Parent.IsValid() {
		t.Errorf("incident.fire has parent %v; want a new root", fire.Parent.SpanID())
	}
	if fire.SpanContext.TraceID() == obs.SpanContext.TraceID() {
		t.Error("incident.fire is in the reconcile's trace")
	}
	if len(fire.Links) != 1 || fire.Links[0].SpanContext.SpanID() != obs.SpanContext.SpanID() ||
		fire.Links[0].SpanContext.TraceID() != obs.SpanContext.TraceID() {
		t.Fatalf("incident.fire links %+v, want observe %v", fire.Links, obs.SpanContext.SpanID())
	}
	if attr(fire, "alert.name") != k {
		t.Errorf("incident.fire alert.name %q", attr(fire, "alert.name"))
	}
	if fired == nil || fired.Err() != nil {
		t.Errorf("the firing's context ended with the reconcile: %v", fired)
	}
}

// upstream is a krateo.io/traceparent a stamper (the composition-dynamic-controller, on every
// apply) writes on an Alert: a sampled span of another trace, which no test exports.
const upstream = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// stampUpstream writes upstream on the Alert name, as the stamper does.
func (h *harness) stampUpstream(name string) trace.SpanContext {
	h.t.Helper()
	h.edit(name, func(a *v1alpha1.Alert) {
		ann := a.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann["krateo.io/traceparent"] = upstream
		a.SetAnnotations(ann)
	})
	d := prtracing.Decide(map[string]string{"krateo.io/traceparent": upstream}, "", time.Time{}, time.Now(), 10*time.Minute)
	if d.Mode != prtracing.Parent || !d.Remote.IsValid() {
		h.t.Fatalf("provider-runtime decides %v on a fresh stamp, want Parent", d.Mode)
	}
	return d.Remote
}

// linksTo reports whether s has a link to sc.
func linksTo(s tracetest.SpanStub, sc trace.SpanContext) bool {
	for _, l := range s.Links {
		if l.SpanContext.TraceID() == sc.TraceID() && l.SpanContext.SpanID() == sc.SpanID() {
			return true
		}
	}
	return false
}

// A pass that only observes an Alert stamped with a fresh krateo.io/traceparent is a trace of its
// own: provider-runtime v1.14.1's reconcile root has no parent and exactly one link, to the stamp,
// and no span of the pass is in the stamper's trace. (Under v1.14.0, every pass inside the stamp's
// 10-minute window was its child, so a chart upgrade pulled every Alert's polls into its trace.)
// MUTATION: a provider-runtime that parents the root on a new value (v1.14.0) fails this.
func TestAnObserveOnlyPassOnAFreshStampIsANewRootLinkedToIt(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	h.pass(1) // creates the HyperDX alert, untraced: the next pass has nothing to do
	remote := h.stampUpstream(k)

	rec := tracingtest.Install(t)
	h.tracedReconcile(context.Background(), k)

	noAlertSpans(t, rec)
	root := rec.One("reconcile")
	for _, act := range []string{"create", "update", "delete"} {
		if n := len(rec.Named(act)); n != 0 {
			t.Fatalf("%d %s spans on an observe-only pass", n, act)
		}
	}
	if root.Parent.IsValid() {
		t.Errorf("reconcile has parent %v; want a new root", root.Parent.SpanID())
	}
	if root.SpanContext.TraceID() == remote.TraceID() {
		t.Error("reconcile is in the stamper's trace")
	}
	if len(root.Links) != 1 || !linksTo(root, remote) {
		t.Errorf("reconcile links %+v, want exactly one, to the stamp %v", root.Links, remote.SpanID())
	}
	for _, s := range rec.Spans() {
		if s.SpanContext.TraceID() != root.SpanContext.TraceID() {
			t.Errorf("span %q is not in the reconcile's trace", s.Name)
		}
	}
	tracingtest.Under(t, rec.One("observe"), root)
	if _, ok := h.get(k).GetAnnotations()["krateo.io/traceparent-continued"]; ok {
		t.Error("an observe-only pass inside the window wrote the marker")
	}
}

// The first pass that acts on a fresh stamp joins only its act to the stamper's trace: the update
// span is the stamp's child, carries the reconcile's k8s.object.* attributes and the Alert's own
// (alert.action included), links back to the root, which links to it, and the HyperDX calls the
// update makes are under it. The root itself stays a new root, and the marker records the stamp.
// MUTATION: a provider-runtime whose act steps are always the root's children fails this; so does
// annotate writing to a span other than the one in Update's ctx.
func TestTheFirstActOnAFreshStampJoinsTheStampersTrace(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	h.pass(1)
	id := h.status(k).HyperdxAlertID
	h.api.Set("alerts", id, map[string]any{"threshold": 0})
	remote := h.stampUpstream(k)

	rec := tracingtest.Install(t)
	h.tracedReconcile(context.Background(), k)

	noAlertSpans(t, rec)
	root, upd := rec.One("reconcile"), rec.One("update")
	if root.Parent.IsValid() || root.SpanContext.TraceID() == remote.TraceID() {
		t.Errorf("reconcile parent %v trace %v; want a new root", root.Parent.SpanID(), root.SpanContext.TraceID())
	}
	if upd.Parent.SpanID() != remote.SpanID() || upd.SpanContext.TraceID() != remote.TraceID() {
		t.Errorf("update parent %v in trace %v; want the stamp %v", upd.Parent.SpanID(), upd.SpanContext.TraceID(), remote.SpanID())
	}
	if len(upd.Links) != 1 || !linksTo(upd, root.SpanContext) {
		t.Errorf("update links %+v, want one, to reconcile", upd.Links)
	}
	if len(root.Links) != 2 || !linksTo(root, remote) || !linksTo(root, upd.SpanContext) {
		t.Errorf("reconcile links %+v, want the stamp and its update", root.Links)
	}
	for key, want := range map[string]string{
		"k8s.object.kind": "Alert", "k8s.object.name": k, "k8s.object.namespace": ns,
		"alert.namespace": ns, "alert.name": k, "alert.hyperdx_id": id, "alert.action": "update",
	} {
		if got := attr(upd, key); got != want {
			t.Errorf("update %s = %q, want %q", key, got, want)
		}
	}
	tracingtest.Under(t, rec.One("observe"), root)
	put := false
	for _, c := range clientChildren(t, rec, upd) {
		put = put || attr(c, "http.request.method") == "PUT"
	}
	if !put {
		t.Error("the threshold PUT is not under the joined update")
	}
	if got := h.get(k).GetAnnotations()["krateo.io/traceparent-continued"]; got != upstream {
		t.Errorf("marker %q, want the stamp %q", got, upstream)
	}
}
