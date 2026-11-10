package incident

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	prmeta "github.com/krateo-platformops/provider-runtime/pkg/meta"
	prtracing "github.com/krateo-platformops/provider-runtime/pkg/tracing"
)

// The names of the incident spans. They are a contract with the platform's tail sampler
// (krateo-platformops/observability, tailsampling): it keeps whole every trace holding a span whose
// name starts with "incident.", and samples the routine Alert loop at a few percent. A rename, or
// a name that loses the prefix, silently drops incident traces: coordinate it with observability.
const (
	spanFire      = "incident.fire"
	spanCompare   = "incident.compare"
	spanAnalyze   = "incident.analyze"
	spanReanalyze = "incident.reanalyze"
)

// The attributes of the incident spans: the Alert and Incident names, counts and outcomes. Never
// a prompt, a record, a model answer or its reason.
const (
	attrAlertNamespace    = attribute.Key("alert.namespace")
	attrAlertName         = attribute.Key("alert.name")
	attrIncidentNamespace = attribute.Key("incident.namespace")
	attrIncident          = attribute.Key("incident.name")
	attrOpened            = attribute.Key("incident.opened")
	attrStaleSkipped      = attribute.Key("incident.stale_skipped")
	attrAttempts          = attribute.Key("incident.analysis.attempts")
	attrCorrections       = attribute.Key("incident.analysis.corrections")
	attrReanalysis        = attribute.Key("incident.reanalysis.count")
	attrCandidates        = attribute.Key("incident.compare.candidates")
	attrCached            = attribute.Key("incident.compare.cached")
	attrMatch             = attribute.Key("incident.compare.match")
)

func alertAttrs(ns, name string) []attribute.KeyValue {
	return []attribute.KeyValue{attrAlertNamespace.String(ns), attrAlertName.String(name)}
}

// PROPAGATION ONTO INCIDENTS (provider-runtime docs/design-tracing.md §6.2, §6.5, §11.3 step 5).
// incident-provider reconciles the Incidents this package writes, with provider-runtime's
// reconciler: its reconcile root is a child of the Incident's krateo.io/traceparent while that value
// is new, and the pass that first acts on a value (its Create, Update or Delete) marks it used, so
// later passes only link to it. The writer stamps a value at two points of an Incident's life:
//
//   - at create, in Analyzing, from incident.fire (traceContext, in openOrCover): incident-provider's
//     first pass is a child of the firing;
//   - before the status write that sets Open and the howToFix scripts, a fresh value from the
//     current incident.analyze (stampTrace, in finish): the pass that starts the check Pod, which
//     acts on the scripts, is then a child of the analysis that wrote them. A reanalysis writes its
//     own the same way, from its incident.analyze.
//
// The second is a separate metadata patch, before the status write: the status subresource drops
// metadata, and a downstream pass that saw the scripts before the new value would act on the old
// one, already used, and only link.

// traceContext is the krateo.io/traceparent (and krateo.io/tracestate) of ctx's span, as
// provider-runtime writes them, or nil when ctx has none (tracing off).
func traceContext(ctx context.Context) map[string]string {
	ann := map[string]string{}
	if !prtracing.InjectAnnotations(ctx, ann) {
		return nil
	}
	return ann
}

// stampTrace writes a fresh krateo.io/traceparent from ctx's span onto the Incident, in a merge
// patch of its annotations alone. A tracestate the context has not is removed (null), so a stale
// one never travels with the new traceparent. With tracing off it writes nothing. A failed patch is
// logged and the analysis is still written: the trace is worth less than the Incident.
func (w *Writer) stampTrace(ctx context.Context, ns, name string) {
	tc := traceContext(ctx)
	if tc == nil {
		return
	}
	ann := map[string]any{prmeta.AnnotationKeyTraceparent: tc[prmeta.AnnotationKeyTraceparent], prmeta.AnnotationKeyTracestate: nil}
	if ts, ok := tc[prmeta.AnnotationKeyTracestate]; ok {
		ann[prmeta.AnnotationKeyTracestate] = ts
	}
	if err := w.kube.Patch(ctx, ns, name, map[string]any{"metadata": map[string]any{"annotations": ann}}); err != nil {
		logging.WithContext(w.log, ctx).Info(fmt.Sprintf("[incident] %s/%s: trace context not written (%s)", ns, name, err))
	}
}
