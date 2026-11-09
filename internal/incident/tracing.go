package incident

import "go.opentelemetry.io/otel/attribute"

// The attributes of the incident spans: the Alert and Incident names, counts and outcomes. Never
// a prompt, a record, a model answer or its reason.
const (
	attrAlertNamespace    = attribute.Key("alert.namespace")
	attrAlertName         = attribute.Key("alert.name")
	attrIncidentNamespace = attribute.Key("incident.namespace")
	attrIncident          = attribute.Key("incident.name")
	attrOpened            = attribute.Key("incident.opened")
	attrAttempts          = attribute.Key("incident.analysis.attempts")
	attrReanalysis        = attribute.Key("incident.reanalysis.count")
	attrCandidates        = attribute.Key("incident.compare.candidates")
	attrCached            = attribute.Key("incident.compare.cached")
	attrMatch             = attribute.Key("incident.compare.match")
)

func alertAttrs(ns, name string) []attribute.KeyValue {
	return []attribute.KeyValue{attrAlertNamespace.String(ns), attrAlertName.String(name)}
}
