package alert

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
)

// reconcileRuntime is one reconcile of the home-namespace Alert name by provider-runtime's own
// Reconciler, wired as Setup wires it, so the status that reaches the fake apiserver is the one
// provider-runtime persists.
func (h *harness) reconcileRuntime(name string) {
	h.t.Helper()
	if _, err := h.runtime().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
}

// An Alert created in the same second as its Metric is refused on its first pass; the pass that
// then creates its HyperDX alert must clear the refusal in the same status write, not leave a
// healthy Alert carrying the old error until the next poll.
func TestThePassThatCreatesTheAlertClearsTheRefusal(t *testing.T) {
	h := newHarness(t, metricAlertCR(budget, "llm-cost-usd", "increase", nil))

	h.reconcileRuntime(budget)
	st := h.status(budget)
	eq(t, "refused phase", st.Phase, v1alpha1.PhaseInvalid)
	if !strings.Contains(st.Error, "no such Metric") {
		t.Fatalf("refusal not persisted: error %q", st.Error)
	}
	eq(t, "nothing pushed", len(h.api.All("alerts")), 0)

	h.addMetric(metricCR("llm-cost-usd", costMetric))
	h.reconcileRuntime(budget)

	if h.byName()[budget] == nil {
		t.Fatal("the pass created no HyperDX alert")
	}
	a := h.get(budget)
	eq(t, "phase", a.Status.Phase, v1alpha1.PhaseSynced)
	eq(t, "error", a.Status.Error, "")
	ready := a.GetCondition(prv1.TypeReady)
	eq(t, "ready", ready.Status, "True")
	eq(t, "ready reason", ready.Reason, prv1.ReasonAvailable)
}
