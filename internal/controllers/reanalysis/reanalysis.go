// Package reanalysis is the controller of Incident reanalysis requests: it hands every Incident
// whose spec may carry a reanalysis request to the incident writer, which reads it past any
// cache, consumes the request and runs the root-cause analysis again.
//
// It watches Incidents as metadata only (PartialObjectMetadata), never whole: an Incident carries
// its analysis prompt (about 20 KB), so a cluster with a few thousand of them, nearly all Closed,
// holds tens of megabytes of JSON, and a cache of them as unstructured maps outgrew the
// controller's memory (OOMKilled on startup with 1957 Incidents under a 256Mi limit). A
// reanalysis request is a spec write, so it changes metadata.generation; a status write does not.
package reanalysis

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"

	"github.com/krateo-platformops/alert-provider/internal/incident"
)

// Reanalyzer consumes an Incident's reanalysis request (incident.Writer).
type Reanalyzer interface {
	Reanalyze(ctx, run context.Context, ns, name string) error
}

var _ Reanalyzer = (*incident.Writer)(nil)

// Options configure the reanalysis controller.
type Options struct {
	Writer Reanalyzer
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
	m := &metav1.PartialObjectMetadata{}
	m.SetGroupVersionKind(gvk)
	return ctrl.NewControllerManagedBy(mgr).
		Named("incident-reanalysis").
		For(m, builder.OnlyMetadata, builder.WithPredicates(specWrites)).
		Complete(&reconciler{o: o})
}

// specWrites passes what may carry a reanalysis request: every Incident the cache first lists
// (a request filed while the controller was down; the writer answers one without a request with
// nothing), and every update that changed the spec. Status writes (the bulk of an Incident's
// updates) and deletions pass nothing.
var specWrites = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return true },
	UpdateFunc:  func(e event.UpdateEvent) bool { return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() },
	DeleteFunc:  func(event.DeleteEvent) bool { return false },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

type reconciler struct {
	o Options
}

// Reconcile hands the Incident to the writer, which reads it past the cache and does nothing
// without a spec.reanalysis.
func (r *reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, r.o.Writer.Reanalyze(ctx, r.o.AnalysisContext, req.Namespace, req.Name)
}
