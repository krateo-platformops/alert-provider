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

// Sources of an Alert's rows (spec.source): what its where, and its value, read.
const (
	// SourceLogs is the HyperDX logs source; an Alert without a source reads it.
	SourceLogs = "logs"
	// SourceTraces is the HyperDX traces source: one row per span.
	SourceTraces = "traces"
)

// MetricRef names the Metric an Alert aggregates. It carries only a name, resolved in the Alert's
// own namespace, so it cannot reach another tenant's Metric.
type MetricRef struct {
	// Name of the Metric.
	Name string `json:"name"`
}

// Ratio makes a metric Alert compare a ratio instead of one aggregated value: the points its
// Labels keep (the numerator) over the points these Labels keep (the denominator), both from the
// same Metric, each aggregated by the Alert's aggregation over the interval.
//
// It exists for objectives stated as a share, such as "under 1% of API requests fail": the
// numerator is the failing requests, the denominator all of them. An error COUNT is not the same
// alert: at high traffic it fires on a healthy share, at low traffic it stays silent on a bad one.
type Ratio struct {
	// Labels keep the denominator's metric points, ANDed; empty keeps every point of the metric.
	Labels map[string]string `json:"labels,omitempty"`
}

// AlertSpec is what to alert on. The CRD (helm/alert-provider-crds) carries the schema.
//
// An Alert reads rows (Where, over the logs or the traces source) or a metric (MetricRef), never
// both. Rows are counted, or, with Value and Aggregation, a value read from each row is aggregated.
type AlertSpec struct {
	// DisplayName is shown in the portal and in the RCA prompt; metadata.name when empty.
	DisplayName string `json:"displayName,omitempty"`

	// Source is which rows Where and Value read: SourceLogs (when empty) or SourceTraces. Row
	// alerts only.
	Source string `json:"source,omitempty"`

	// Where is the ClickHouse SQL boolean expression over the rows of Source the alert counts.
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

	// Labels keep only the metric points that carry these values, on the point or on its
	// resource, e.g. {tenant: acme}. Metric alerts only.
	Labels map[string]string `json:"labels,omitempty"`

	// Ratio, when set, divides the aggregation of the points Labels keep by the aggregation of
	// the points Ratio.Labels keep. Metric alerts with aggregation sum or increase only.
	Ratio *Ratio `json:"ratio,omitempty"`

	// Interval is the evaluation window.
	Interval string `json:"interval,omitempty"`

	// Threshold is the value the count is compared with, as the JSON number it was written as.
	Threshold json.Number `json:"threshold,omitempty"`

	// ThresholdType is how the count is compared with the threshold.
	ThresholdType string `json:"thresholdType,omitempty"`

	// ThresholdMax is the upper bound of a range, required by thresholdType between and
	// not_between (value in [threshold, thresholdMax], or outside it) and refused with any other.
	ThresholdMax json.Number `json:"thresholdMax,omitempty"`

	// Description is one plain sentence saying what the Alert catches. The portal shows it, the
	// investigation reads it as the Alert's intent, and HyperDX sends it as the notification text.
	Description string `json:"description,omitempty"`

	// Runbook is the operator guidance for when the Alert fires (markdown): what to check, and
	// what never to do. HyperDX sends it after the description, a blank line between them, and
	// the investigation reads it as operator guidance before it proposes a fix. The portal does
	// not show it. At most 3072 characters, and with the description at most HyperDX's 4096 for
	// a notification text.
	Runbook string `json:"runbook,omitempty"`

	// GroupBy splits a metric Alert into one series per distinct value of these attribute keys,
	// each read off the point, or off its resource when the point lacks it (as Labels match).
	// HyperDX evaluates each series on its own, so one unhealthy series fires however many
	// healthy ones there are. Metric alerts only, never with Ratio.
	GroupBy []string `json:"groupBy,omitempty"`

	// ConsecutiveWindows is how many consecutive intervals the condition must hold before the
	// alert fires; HyperDX counts them per group. 1 when unset.
	ConsecutiveWindows int32 `json:"consecutiveWindows,omitempty"`
}

// MaxGroups caps status.groups: the groups past it are counted in status.groupsTruncated.
const MaxGroups = 50

// AlertGroup is one series of a grouped Alert that is ALERT: its condition held on each of the
// last consecutiveWindows completed intervals.
type AlertGroup struct {
	// Group is the series' value of each groupBy key.
	Group map[string]string `json:"group"`
	// Value is the series' value in the last completed interval.
	Value json.Number `json:"value"`
	// Since is when the series started breaching (RFC 3339): the start of the earliest breaching
	// interval in the window, kept while the series stays listed.
	Since string `json:"since"`
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

	// Groups are the series of a grouped Alert (spec.groupBy) that are ALERT, computed by the
	// provider each pass: HyperDX's API carries only the rolled-up state. They are judged one
	// interval late, since the newest completed interval may not be ingested yet, so they may trail
	// HyperDX's state by one interval. Sorted by group; at most MaxGroups.
	Groups []AlertGroup `json:"groups,omitempty"`

	// GroupsTruncated counts the ALERT groups left out of Groups past MaxGroups.
	GroupsTruncated int32 `json:"groupsTruncated,omitempty"`

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

// RowSource is the source the Alert's rows are read from: spec.source, SourceLogs when empty.
func (mg *Alert) RowSource() string {
	if mg.Spec.Source == "" {
		return SourceLogs
	}
	return mg.Spec.Source
}

// Grouped says the Alert evaluates one series per group (spec.groupBy).
func (mg *Alert) Grouped() bool {
	return len(mg.Spec.GroupBy) > 0
}

// Windows is spec.consecutiveWindows, 1 when unset.
func (mg *Alert) Windows() int {
	if mg.Spec.ConsecutiveWindows < 1 {
		return 1
	}
	return int(mg.Spec.ConsecutiveWindows)
}

// Aggregated says the Alert compares an aggregated value (a metric, or a value read from log
// rows) rather than a count of log rows.
func (mg *Alert) Aggregated() bool {
	return mg.Spec.Aggregation != ""
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
