package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/meta"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"
)

// AnnotationPaused, "true" on an Alert, skips its firings: no incident opens and none is counted.
// The spec is still pushed to HyperDX and the state still mirrored. It is provider-runtime's pause
// annotation, which stops a whole reconcile, so the Alert keeps it out of provider-runtime's view
// (see GetAnnotations).
const AnnotationPaused = meta.AnnotationKeyReconciliationPaused

// Phases of an Alert's reconcile (status.phase).
const (
	// PhasePending: no HyperDX alert yet.
	PhasePending = "Pending"
	// PhaseInvalid: the threshold can only have one outcome.
	PhaseInvalid = "Invalid"
	// PhaseSynced: the spec is the live HyperDX alert.
	PhaseSynced = "Synced"
	// PhaseSpecDrift: the spec could not be pushed, so the live alert does not match it.
	PhaseSpecDrift = "SpecDrift"
	// PhaseError: the reconcile failed.
	PhaseError = "Error"
)

// Aggregations of a metric Alert: how its metric's points over the interval become the one value
// compared with the threshold.
const (
	AggregationAvg = "avg"
	AggregationMax = "max"
	AggregationMin = "min"
	AggregationSum = "sum"
	AggregationP95 = "p95"
	// AggregationIncrease is a counter's (a sum metric's) increase over the interval, with resets
	// clamped: a restarted counter neither spikes nor goes negative.
	AggregationIncrease = "increase"
)

// MetricRef names the Metric an Alert aggregates, in the Alert's namespace: the controller reads
// only its own namespace.
type MetricRef struct {
	// Name of the Metric.
	Name string `json:"name"`
}

// AlertSpec is what to alert on. The CRD (helm/alert-provider-crds) carries the schema.
//
// An Alert has one source: log rows (Where) or a metric (MetricRef), never both. Log rows are
// counted, or, with Value and Aggregation, a value read from each row is aggregated.
type AlertSpec struct {
	// DisplayName is shown in the portal and in the RCA prompt; metadata.name when empty.
	DisplayName string `json:"displayName,omitempty"`

	// Where is the ClickHouse SQL boolean expression over the logs source the alert counts.
	Where string `json:"where,omitempty"`

	// MetricRef is the Metric the alert aggregates, instead of counting log rows.
	MetricRef *MetricRef `json:"metricRef,omitempty"`

	// Aggregation is how the points over the interval (the metric's, or Value's per log row)
	// become the compared value.
	Aggregation string `json:"aggregation,omitempty"`

	// Value is a ClickHouse SQL expression read from each log row Where matches, aggregated by
	// Aggregation instead of counting the rows, e.g. a cost extracted from the row's Body. Log
	// alerts only.
	Value string `json:"value,omitempty"`

	// Labels keep only the metric points whose attributes carry these values, e.g.
	// {tenant: acme}. Metric alerts only.
	Labels map[string]string `json:"labels,omitempty"`

	// Interval is the evaluation window.
	Interval string `json:"interval,omitempty"`

	// Threshold is the value the count is compared with, as the JSON number it was written as.
	Threshold json.Number `json:"threshold,omitempty"`

	// ThresholdType is how the count is compared with the threshold.
	ThresholdType string `json:"thresholdType,omitempty"`

	// Message is the notification message.
	Message string `json:"message,omitempty"`

	// Incidents is whether this Alert's firings open Incidents.
	//
	// Auto opens one per firing that no open incident covers, which is what a counting Alert has
	// always done. Never mirrors the ALERT state on the Alert and notifies through HyperDX's
	// webhook, and opens nothing.
	//
	// UNSET IS NOT "Auto": it keeps the behaviour each kind of Alert already had -- Auto for a
	// counting Alert, Never for an aggregated one -- so adding this field changes nothing for
	// any Alert that exists. Read it through OpensIncidents rather than comparing the string.
	//
	// Auto on an aggregated Alert is refused by the CRD, not silently ignored: the incident
	// writer compares and root-causes the LOG RECORDS an alert counts, and an aggregated value is
	// not a count, so the comparison prompt would describe a threshold that is not the one that
	// fired. Lifting that refusal needs an evidence source for an aggregated firing (a metric's
	// series around the firing, or the rows contributing most to a log value), which is the open
	// half of #66.
	Incidents string `json:"incidents,omitempty"`
}

// AlertStatus mirrors the live HyperDX alert.
type AlertStatus struct {
	prv1.ConditionedStatus `json:",inline"`

	HyperdxAlertID     string `json:"hyperdxAlertId,omitempty"`
	HyperdxDashboardID string `json:"hyperdxDashboardId,omitempty"`

	// State is the live HyperDX alert state (OK / ALERT / PENDING).
	State string `json:"state,omitempty"`

	// OkSince is when the alert last turned OK; empty while state is anything but OK.
	OkSince string `json:"okSince,omitempty"`

	// Phase is the reconcile phase: Pending, Invalid, Synced, SpecDrift or Error.
	Phase string `json:"phase,omitempty"`

	Error string `json:"error,omitempty"`
}

// +kubebuilder:object:root=true

// An Alert is a desired HyperDX alert; its status mirrors the live one.
type Alert struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AlertSpec   `json:"spec"`
	Status AlertStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AlertList contains a list of Alert.
type AlertList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Alert `json:"items"`
}

var (
	_ resource.Managed     = &Alert{}
	_ resource.ManagedList = &AlertList{}
)

// IsMetric says the Alert aggregates a metric rather than reading log rows.
func (mg *Alert) IsMetric() bool {
	return mg.Spec.MetricRef != nil
}

// Aggregated says the Alert compares an aggregated value (a metric, or a value read from log
// rows) rather than a count of log rows.
func (mg *Alert) Aggregated() bool {
	return mg.Spec.Aggregation != ""
}

// IncidentsAuto and IncidentsNever are the values of Alert.Spec.Incidents. The zero value is
// neither: see OpensIncidents.
const (
	IncidentsAuto  = "Auto"
	IncidentsNever = "Never"
)

// OpensIncidents says whether a firing of this Alert should reach the incident writer.
//
// It is the ONE place the unset default is decided, so that "unset" cannot mean different things
// in different callers:
//
//	Never            -> false, always.
//	Auto             -> true. The CRD refuses Auto on an aggregated Alert, so this does not
//	                    silently enable an incident the writer has no evidence for.
//	unset ("")       -> !Aggregated(): true for a counting Alert, false for an aggregated one.
//	                    That is exactly the behaviour both kinds had before this field existed.
//
// It deliberately says nothing about whether the incident path CAN run -- a missing kagent
// ModelConfig makes the comparison fail, which is a different problem from an Alert that is not
// meant to open incidents. Never is how an install without the agent tier says so declaratively
// instead of relying on that failure.
func (mg *Alert) OpensIncidents() bool {
	switch mg.Spec.Incidents {
	case IncidentsNever:
		return false
	case IncidentsAuto:
		return true
	default:
		return !mg.Aggregated()
	}
}

// Paused says the Alert's firings are skipped.
func (mg *Alert) Paused() bool {
	return mg.Annotations[AnnotationPaused] == "true"
}

// GetAnnotations returns the annotations without AnnotationPaused. provider-runtime reads the
// pause through this method and would skip the Alert's whole reconcile; an Alert's pause skips
// only its firings, which read it through Paused.
func (mg *Alert) GetAnnotations() map[string]string {
	if _, ok := mg.Annotations[AnnotationPaused]; !ok {
		return mg.Annotations
	}
	out := make(map[string]string, len(mg.Annotations)-1)
	for k, v := range mg.Annotations {
		if k != AnnotationPaused {
			out[k] = v
		}
	}
	return out
}

// SetAnnotations sets the annotations and keeps AnnotationPaused, which GetAnnotations hides from
// the callers that set them back.
func (mg *Alert) SetAnnotations(a map[string]string) {
	paused, ok := mg.Annotations[AnnotationPaused]
	mg.Annotations = a
	if ok {
		if mg.Annotations == nil {
			mg.Annotations = map[string]string{}
		}
		mg.Annotations[AnnotationPaused] = paused
	}
}

// GetCondition of this Alert.
func (mg *Alert) GetCondition(ct prv1.ConditionType) prv1.Condition {
	return mg.Status.GetCondition(ct)
}

// SetConditions of this Alert.
func (mg *Alert) SetConditions(c ...prv1.Condition) {
	mg.Status.SetConditions(c...)
}

// GetItems of this AlertList.
func (l *AlertList) GetItems() []resource.Managed {
	items := make([]resource.Managed, len(l.Items))
	for i := range l.Items {
		items[i] = &l.Items[i]
	}
	return items
}
