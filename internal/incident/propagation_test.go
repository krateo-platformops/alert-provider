package incident

import (
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	prtracing "github.com/krateo-platformops/provider-runtime/pkg/tracing"

	"github.com/krateo-platformops/alert-provider/internal/tracing/tracingtest"
)

// The annotations incident-provider's reconciler reads (provider-runtime pkg/meta), spelled out:
// they are the contract with it.
const (
	annTraceparent = "krateo.io/traceparent"
	annTracestate  = "krateo.io/tracestate"
)

// callsOf are the apiserver calls on the Incident name, in order.
func (f *fakeKube) callsOf(name string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

// traceparentOf is the krateo.io/traceparent a call's body writes, and whether it writes one.
func traceparentOf(c call) (string, bool) {
	meta, _ := c.body["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	v, ok := ann[annTraceparent].(string)
	return v, ok
}

// canonical is the traceparent of span s, as provider-runtime writes it.
func canonical(s tracetest.SpanStub) string { return prtracing.Canonical(s.SpanContext) }

// stampBeforeOpen fails the test unless the calls on name hold an object patch writing want as
// krateo.io/traceparent (and removing any tracestate), followed by the status write that sets Open
// with the howToFix scripts. It returns the patch's index.
func stampBeforeOpen(t *testing.T, calls []call, want string) int {
	t.Helper()
	stamp, open := -1, -1
	for i, c := range calls {
		if v, ok := traceparentOf(c); ok && c.method == "PATCH-OBJECT" && v == want && stamp < 0 {
			stamp = i
			ann := c.body["metadata"].(map[string]any)["annotations"].(map[string]any)
			if ts, ok := ann[annTracestate]; !ok || ts != nil {
				t.Errorf("the stamp leaves krateo.io/tracestate as it was (%v, present %v); want null", ts, ok)
			}
			if len(c.body) != 1 || len(c.body["metadata"].(map[string]any)) != 1 {
				t.Errorf("the stamp patch writes more than the annotations: %v", c.body)
			}
		}
		if st, _ := c.body["status"].(map[string]any); c.method == "PATCH" && st["state"] == "Open" && st["howToFix"] != nil && open < 0 {
			open = i
		}
	}
	if stamp < 0 {
		t.Fatalf("no object patch writes krateo.io/traceparent %s; calls %v", want, calls)
	}
	if open < 0 {
		t.Fatalf("no status write sets Open with the scripts; calls %v", calls)
	}
	if stamp > open {
		t.Errorf("the traceparent patch (call %d) follows the Open status write (call %d): incident-provider's acting pass would see the old value", stamp, open)
	}
	return stamp
}

// The Incident a firing creates carries incident.fire's context, so incident-provider's first pass
// on it is a child of the firing.
// MUTATION: dropping the traceContext merge in openOrCover fails this.
func TestTheCreatedIncidentCarriesIncidentFiresTraceparent(t *testing.T) {
	rec := tracingtest.Install(t)
	c := newCase(t)
	c.fire(fireOpt{})
	name := str(metadata(c.kube.only(ns)), "name")
	fire := rec.One("incident.fire")
	calls := c.kube.callsOf(name)
	var post *call
	for i := range calls {
		if calls[i].method == "POST" {
			post = &calls[i]
			break
		}
	}
	if post == nil {
		t.Fatalf("no create; calls %v", calls)
	}
	got, ok := traceparentOf(*post)
	if !ok || got != canonical(fire) {
		t.Fatalf("create krateo.io/traceparent %q (present %v), want incident.fire's %q", got, ok, canonical(fire))
	}
	if _, ok := post.body["metadata"].(map[string]any)["annotations"].(map[string]any)[annTracestate]; ok {
		t.Error("create writes a krateo.io/tracestate the context does not have")
	}
}

// Before the status write that sets Open and the scripts, a metadata patch stamps a fresh
// traceparent from incident.analyze, so incident-provider's pass that starts the check Pod is a
// child of the analysis, not a link to the create's value it already used.
// MUTATION: dropping stampTrace from finish (inject at create only) fails this; so does stamping
// after the status write instead of before it.
func TestBeforeOpenAFreshTraceparentFromIncidentAnalyzeIsPatchedFirst(t *testing.T) {
	rec := tracingtest.Install(t)
	c := newCase(t)
	c.fire(fireOpt{})
	inc := c.kube.only(ns)
	name := str(metadata(inc), "name")
	fire, analyze := rec.One("incident.fire"), rec.One("incident.analyze")
	tracingtest.Under(t, analyze, fire)
	want := canonical(analyze)
	if want == canonical(fire) {
		t.Fatal("incident.analyze and incident.fire have one context")
	}
	stampBeforeOpen(t, c.kube.callsOf(name), want)
	if got := annotations(inc)[annTraceparent]; got != want {
		t.Errorf("the Incident ends with krateo.io/traceparent %v, want incident.analyze's %s", got, want)
	}
	if s, _ := state(inc); s != "Open" {
		t.Errorf("state %q, want Open", s)
	}
	// incident-provider used the create's value on its first acting pass (its marker); the fresh
	// value is new to provider-runtime's rule, so the pass that sees Open is a Parent pass.
	d := prtracing.Decide(map[string]string{annTraceparent: want, "krateo.io/traceparent-continued": canonical(fire)},
		"", time.Time{}, time.Now(), 10*time.Minute)
	if d.Mode != prtracing.Parent {
		t.Errorf("provider-runtime decides %v on the stamped Incident, want Parent", d.Mode)
	}
}

// A reanalysis stamps a fresh value from its own incident.analyze before its Open write, replacing
// the earlier one and the tracestate that came with it.
// MUTATION: dropping stampTrace from finish fails this; so does stamping only first analyses (none
// when status.reanalysis is set).
func TestAReanalysisStampsAFreshTraceparentBeforeItsOpen(t *testing.T) {
	const old = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	rec := tracingtest.Install(t)
	c := newCase(t)
	c.analyzedIncident("Open")
	c.kube.annotate(ns, incName, annTraceparent, old)
	c.kube.annotate(ns, incName, annTracestate, "vendor=stale")
	c.kube.writeSpec(ns, incName, map[string]any{"reanalysis": map[string]any{"feedback": "f"}})
	if err := c.reanalyze(); err != nil {
		t.Fatal(err)
	}
	an := rec.One("incident.analyze")
	want := canonical(an)
	if want == old {
		t.Fatal("the reanalysis' context is the old one")
	}
	calls := c.kube.callsOf(incName)
	stamp := stampBeforeOpen(t, calls, want)
	analyzing := -1
	for i, cl := range calls {
		if st, _ := cl.body["status"].(map[string]any); cl.method == "PATCH" && st["state"] == "Analyzing" {
			analyzing = i
		}
	}
	if analyzing < 0 || analyzing > stamp {
		t.Errorf("the reanalysis' Analyzing write (call %d) is not before its stamp (call %d)", analyzing, stamp)
	}
	ann := annotations(c.kube.get(ns, incName))
	if ann[annTraceparent] != want {
		t.Errorf("krateo.io/traceparent %v, want the reanalysis' %s", ann[annTraceparent], want)
	}
	if ts, ok := ann[annTracestate]; ok {
		t.Errorf("the stale krateo.io/tracestate %v survived the new traceparent", ts)
	}
}

// With tracing off nothing is stamped: no annotation at create, no extra patch before Open.
func TestWithTracingOffNoTraceContextIsWritten(t *testing.T) {
	c := newCase(t)
	c.fire(fireOpt{})
	name := str(metadata(c.kube.only(ns)), "name")
	for _, cl := range c.kube.callsOf(name) {
		if v, ok := traceparentOf(cl); ok {
			t.Errorf("%s writes krateo.io/traceparent %q with tracing off", cl.method, v)
		}
	}
	if _, ok := annotations(c.kube.only(ns))[annTraceparent]; ok {
		t.Error("the Incident carries a traceparent with tracing off")
	}
}
