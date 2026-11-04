package alert

import (
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler/reconcilertest"
)

// provider-runtime v1.8.0's Ready contract: every Observe that succeeds sets Ready, and an error it
// returns after setting Ready is wrapped with reconciler.ReadyAssessed. One check per path.

// observeReady runs Observe once on the Alert through the contract's own helper and returns the
// Ready condition it left.
func (h *harness) observeReady(name string) (prv1.Condition, error) {
	h.t.Helper()
	a := &v1alpha1.Alert{}
	if err := h.kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, a); err != nil {
		h.t.Fatal(err)
	}
	ext, err := h.conn.Connect(ctx, a)
	if err != nil {
		h.t.Fatal(err)
	}
	_, oerr := reconcilertest.AssertObserveSetsReady(h.t, ctx, ext, a)
	return a.GetCondition(prv1.TypeReady), oerr
}

func TestReadyWhenTheHyperDXAlertIsYetToBeCreated(t *testing.T) {
	h := newHarness(t, alertCR("fresh", "SeverityText = 'error'", v1alpha1.AlertStatus{}))
	c, _ := h.observeReady("fresh")
	eq(t, "reason", c.Reason, prv1.ReasonCreating)
}

func TestReadyWhenUpToDate(t *testing.T) {
	h := newHarness(t, alertCR("synced", "SeverityText = 'error'", v1alpha1.AlertStatus{}))
	h.pass(2)
	c, _ := h.observeReady("synced")
	eq(t, "reason", c.Reason, prv1.ReasonAvailable)
}

func TestReadyWhenTheSpecDrifted(t *testing.T) {
	h := newHarness(t, alertCR("drifted", "SeverityText = 'error'", v1alpha1.AlertStatus{}))
	h.pass(2)
	h.edit("drifted", func(a *v1alpha1.Alert) { a.Spec.Threshold = "9" })
	c, _ := h.observeReady("drifted")
	eq(t, "reason", c.Reason, prv1.ReasonAvailable)
}

func TestReadyWhenATautologyIsRefused(t *testing.T) {
	a := alertCR("taut", "SeverityText = 'error'", v1alpha1.AlertStatus{})
	a.Spec.Threshold, a.Spec.ThresholdType = "0", "above"
	h := newHarness(t, a)
	c, _ := h.observeReady("taut")
	eq(t, "reason", c.Reason, prv1.ReasonUnavailable)
	eq(t, "says why", c.Message != "", true)
}

func TestReadyWhenTheTileIsRefused(t *testing.T) {
	h := newHarness(t, metricAlertCR(budget, "no-such-metric", "increase", nil))
	c, _ := h.observeReady(budget)
	eq(t, "reason", c.Reason, prv1.ReasonUnavailable)
	eq(t, "says why", c.Message != "", true)
}

func TestReadyWhenBeingDeleted(t *testing.T) {
	h := newHarness(t, alertCR("gone", "SeverityText = 'error'", v1alpha1.AlertStatus{}))
	h.pass(2) // adds the finalizer, so the delete below leaves a deletionTimestamp
	a := h.get("gone")
	if err := h.kube.Delete(ctx, a); err != nil {
		t.Fatal(err)
	}
	c, _ := h.observeReady("gone")
	eq(t, "reason", c.Reason, prv1.ReasonDeleting)
}

// The tile read fails after Ready=Available mirrored HyperDX's live state: the error is
// ReadyAssessed, so the runtime keeps that Ready instead of replacing it with NotObserved.
func TestReadyStandsWhenTheTileReadFailsAfterIt(t *testing.T) {
	h := newHarness(t, alertCR("tileerr", "SeverityText = 'error'", v1alpha1.AlertStatus{}))
	h.pass(2)
	h.api.Fail = func(method, kind, _ string) int {
		if method == http.MethodGet && kind == "dashboards" {
			return http.StatusBadGateway
		}
		return 0
	}
	c, err := h.observeReady("tileerr")
	if err == nil || !reconciler.IsReadyAssessed(err) {
		t.Fatalf("want a ReadyAssessed error from the failed tile read, got %v", err)
	}
	eq(t, "reason", c.Reason, prv1.ReasonAvailable)
}
