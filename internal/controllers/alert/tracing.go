package alert

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/krateo-platformops/provider-runtime/pkg/resource"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
	"github.com/krateo-platformops/alert-provider/internal/tracing"
)

// The attributes of the Alert controller's spans. They name the Alert and its HyperDX alert and
// carry its state; never its spec, its `where` or anything HyperDX returned beyond the id.
const (
	attrNamespace = attribute.Key("alert.namespace")
	attrName      = attribute.Key("alert.name")
	attrHyperDXID = attribute.Key("alert.hyperdx_id")
	attrState     = attribute.Key("alert.state")
	// attrListCached says whether alert.observe read the shared alerts list without a call of its
	// own (alertsList); attrThrottled that HyperDX rate-limited the pass, which was deferred.
	attrListCached = attribute.Key("hyperdx.alerts_list.cached")
	attrThrottled  = attribute.Key("hyperdx.throttled")
)

// traced opens alert.reconcile around one provider-runtime reconcile, so the pass's action spans
// (alert.observe, alert.create or alert.update, alert.delete) and the HyperDX calls Connect makes
// are one trace. provider-runtime passes the context on to every ExternalClient call.
type traced struct {
	reconcile.Reconciler
}

func (t traced) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, span := tracing.Start(ctx, "alert.reconcile",
		trace.WithAttributes(attrNamespace.String(req.Namespace), attrName.String(req.Name)))
	defer span.End()
	res, err := t.Reconciler.Reconcile(ctx, req)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

// action opens the span of one ExternalClient action on mg. The returned func ends it with the
// HyperDX id and state the action left on the Alert, and status Error for a non-nil error.
func action(ctx context.Context, name string, mg resource.Managed) (context.Context, func(error)) {
	a, _ := mg.(*v1alpha1.Alert)
	var attrs []attribute.KeyValue
	if a != nil {
		attrs = append(attrs, attrNamespace.String(a.Namespace), attrName.String(a.Name))
	}
	ctx, span := tracing.Start(ctx, name, trace.WithAttributes(attrs...))
	return ctx, func(err error) {
		if a != nil {
			if id := a.Status.HyperdxAlertID; id != "" {
				span.SetAttributes(attrHyperDXID.String(id))
			}
			if st := a.Status.State; st != "" {
				span.SetAttributes(attrState.String(st))
			}
		}
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}
