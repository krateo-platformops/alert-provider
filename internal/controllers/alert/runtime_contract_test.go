package alert

import (
	"context"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler/reconcilertest"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
)

// errorLog is a logging.Logger that keeps every Error, with the message and error.
type errorLog struct {
	mu     *sync.Mutex
	errors *[]string
}

func newErrorLog() errorLog { return errorLog{mu: &sync.Mutex{}, errors: &[]string{}} }

func (l errorLog) Error(err error, msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	*l.errors = append(*l.errors, msg+": "+err.Error())
}
func (l errorLog) Info(string, ...any)                        {}
func (l errorLog) Debug(string, ...any)                       {}
func (l errorLog) Warn(string, ...any)                        {}
func (l errorLog) WithValues(...any) logging.Logger           { return l }
func (l errorLog) WithName(string) logging.Logger             { return l }
func (l errorLog) WithContext(context.Context) logging.Logger { return l }

func (l errorLog) got() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), *l.errors...)
}

// A normal deletion ends with a pass whose Get finds the Alert gone: its finalizer was released
// and the apiserver removed it. That pass is no error, so nothing is logged at ERROR (it inflated
// error counts and could trip log-based alerts on krateo-057), and it neither fails nor requeues.
// provider-runtime v1.17.1 logs it at Debug; v1.15.0 logged "Cannot get managed resource" at ERROR.
// MUTATION: provider-runtime v1.15.0 fails this.
func TestAReconcileAfterAFinalizedDeleteLogsNoError(t *testing.T) {
	h := newHarness(t, alertCR("cd-not-ready", "Body LIKE '%x%'", v1alpha1.AlertStatus{}))
	log := newErrorLog()
	r := h.runtime(reconciler.WithLogger(log))
	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "cd-not-ready"}}

	// Created: the finalizer is on and the HyperDX alert exists.
	for range 2 {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	eq(t, "finalizer", h.get("cd-not-ready").Finalizers, []string{Finalizer})
	eq(t, "hyperdx alerts", len(h.api.All("alerts")), 1)

	// Deleted: the pass deletes the HyperDX alert and releases the finalizer, and the Alert goes.
	if err := h.kube.Delete(ctx, h.get("cd-not-ready")); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		if err := h.kube.Get(ctx, req.NamespacedName, &v1alpha1.Alert{}); err != nil {
			break
		}
		if i == 3 {
			t.Fatal("the Alert was never removed")
		}
	}
	eq(t, "hyperdx alerts", len(h.api.All("alerts")), 0)
	before := len(log.got())

	// The requeue after it: the Get is NotFound.
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Errorf("the pass after the delete failed: %v", err)
	}
	eq(t, "result", res, reconcile.Result{})
	if got := log.got()[before:]; len(got) != 0 {
		t.Errorf("the pass after a finalized delete logged at ERROR: %q", got)
	}
	if got := log.got(); len(got) != 0 {
		t.Errorf("a normal create and delete logged at ERROR: %q", got)
	}
}

// The ExternalClient contract (provider-runtime v1.16.0): a method that writes the managed object
// must leave mg describing what is stored. The Alert controller's methods never write mg: Observe
// and Update set its status in memory for the runtime to write, and the one API write a pass can
// make, a recreated webhook's reset, patches the other Alerts and resets mg in memory. Checked on
// the pass that creates the webhook, so the reset runs, and creates the HyperDX alert.
// MUTATION: resetting mg in webhook.reset with a status patch through a copy, as it patches the
// others, fails this (the runtime's write then conflicts).
func TestTheAlertPassLeavesTheManagedAlertCurrent(t *testing.T) {
	other := alertCR("other", "Body LIKE '%y%'", v1alpha1.AlertStatus{HyperdxAlertID: "gone", Phase: v1alpha1.PhaseSynced})
	h := newHarness(t, alertCR("cd-not-ready", "Body LIKE '%x%'", v1alpha1.AlertStatus{}), other)
	mg := h.get("cd-not-ready")
	err := reconcilertest.AssertLeavesManagedCurrent(t, ctx, h.kube, mg, func(ctx context.Context, m resource.Managed) error {
		ext, err := h.conn.Connect(ctx, m)
		if err != nil {
			return err
		}
		if _, err := ext.Observe(ctx, m); err != nil {
			return err
		}
		return ext.Update(ctx, m)
	})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "the other Alert was reset", h.get("other").Status.Phase, v1alpha1.PhasePending)
	if h.get("cd-not-ready").Status.HyperdxAlertID == "" {
		t.Error("the pass created no HyperDX alert")
	}
}
