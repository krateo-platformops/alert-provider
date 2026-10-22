// Package reanalysis is the controller of Incident reanalyses: it hands every Incident that carries
// spec.reanalysis to the incident writer, which consumes the request and runs the root-cause
// analysis again, and every Open one whose precondition answers only unknown, which the writer
// reanalyzes once that has lasted long enough.
package reanalysis

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"

	"github.com/krateo-platformops/alert-provider/internal/incident"
)

// Options configure the reanalysis controller.
type Options struct {
	Writer *incident.Writer
	// AnalysisContext bounds the analyses: one outlives its reconcile, since an RCA takes minutes.
	AnalysisContext context.Context
	Log             logging.Logger
}

// Setup adds the reanalysis controller to the manager. Without the Incident CRD there is nothing
// to watch, and it is skipped.
func Setup(mgr ctrl.Manager, o Options) error {
	gvk := incident.IncidentGVK
	if _, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		o.Log.Info(fmt.Sprintf("[reanalysis] disabled: no %s (%s)", gvk.GroupKind(), err))
		return nil
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return ctrl.NewControllerManagedBy(mgr).
		Named("incident-reanalysis").
		For(u, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return requested(obj) || unanswered(obj)
		}))).
		Complete(&reconciler{o: o})
}

// requested says the Incident carries spec.reanalysis.
func requested(obj client.Object) bool {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return false
	}
	_, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "reanalysis")
	return found
}

// unanswered says the Incident's precondition has answered only unknown (see
// incident.UnansweredSince).
func unanswered(obj client.Object) bool {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return false
	}
	_, ok = incident.UnansweredSince(u.Object)
	return ok
}

type reconciler struct {
	o Options
}

func (r *reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if err := r.o.Writer.Reanalyze(ctx, r.o.AnalysisContext, req.Namespace, req.Name); err != nil {
		return ctrl.Result{}, err
	}
	wait, err := r.o.Writer.RepairCheck(ctx, r.o.AnalysisContext, req.Namespace, req.Name)
	return ctrl.Result{RequeueAfter: wait}, err
}
