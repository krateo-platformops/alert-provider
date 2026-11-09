package incident

import "go.opentelemetry.io/otel/attribute"

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
	attrReanalysis        = attribute.Key("incident.reanalysis.count")
	attrCandidates        = attribute.Key("incident.compare.candidates")
	attrCached            = attribute.Key("incident.compare.cached")
	attrMatch             = attribute.Key("incident.compare.match")
)

func alertAttrs(ns, name string) []attribute.KeyValue {
	return []attribute.KeyValue{attrAlertNamespace.String(ns), attrAlertName.String(name)}
}
