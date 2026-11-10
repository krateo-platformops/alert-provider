package alert

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/krateo-platformops/provider-runtime/pkg/resource"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
)

// The attributes the Alert controller sets on provider-runtime's spans. They name the Alert and its
// HyperDX alert and carry its state; never its spec, its `where` or anything HyperDX returned beyond
// the id.
const (
	attrNamespace = attribute.Key("alert.namespace")
	attrName      = attribute.Key("alert.name")
	attrHyperDXID = attribute.Key("alert.hyperdx_id")
	attrState     = attribute.Key("alert.state")
	// attrAction is what an update span did: "create" on the pass that creates the HyperDX dashboard
	// and alert, "update" otherwise. provider-runtime names the span update either way: Observe
	// never reports the HyperDX alert missing, so Create is never called (see Observe).
	attrAction = attribute.Key("alert.action")
	// attrListCached says whether observe read the shared alerts list without a call of its own
	// (alertsList); attrThrottled that HyperDX rate-limited the pass, which was deferred.
	attrListCached = attribute.Key("hyperdx.alerts_list.cached")
	attrThrottled  = attribute.Key("hyperdx.throttled")
)

// annotate sets the Alert's attributes on the span in ctx: provider-runtime's observe, update or
// delete, which it opens around each ExternalClient call (provider-runtime docs/design-tracing.md
// §4.4, §11.3). It runs when the call returns, so the HyperDX id and state are the ones the call
// left on the Alert. provider-runtime marks the span Error when the call returns an error. With
// tracing off the span is the no-op one and nothing is recorded.
func annotate(ctx context.Context, mg resource.Managed, extra ...attribute.KeyValue) {
	a, ok := mg.(*v1alpha1.Alert)
	if !ok {
		return
	}
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	attrs := append([]attribute.KeyValue{attrNamespace.String(a.Namespace), attrName.String(a.Name)}, extra...)
	if id := a.Status.HyperdxAlertID; id != "" {
		attrs = append(attrs, attrHyperDXID.String(id))
	}
	if st := a.Status.State; st != "" {
		attrs = append(attrs, attrState.String(st))
	}
	span.SetAttributes(attrs...)
}
