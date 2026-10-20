package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Metric types, as the collector emits them (the otel_metrics_* table they land in).
const (
	MetricTypeGauge     = "gauge"
	MetricTypeSum       = "sum"
	MetricTypeHistogram = "histogram"
)

// MetricSpec names one collected metric, so an Alert can reference it instead of a free-text
// name: a reference to a Metric that does not exist is refused, where a mistyped name would
// silently never match. The CRD (helm/alert-provider-crds) carries the schema.
type MetricSpec struct {
	// MetricName is the name the metric is emitted under (the MetricName column).
	MetricName string `json:"metricName"`

	// Type is how it is collected: gauge, sum (a counter) or histogram.
	Type string `json:"type"`

	// Unit is the metric's unit, as emitted.
	Unit string `json:"unit,omitempty"`

	// Description says what the metric measures.
	Description string `json:"description,omitempty"`

	// CollectedBy is how it is collected (the ScopeName: a receiver or an instrumentation scope).
	CollectedBy string `json:"collectedBy,omitempty"`
}

// +kubebuilder:object:root=true

// A Metric is a collected metric an Alert can reference. It has no controller: it is a catalogue
// entry, curated by whoever owns the collector configuration.
type Metric struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MetricSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// MetricList contains a list of Metric.
type MetricList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Metric `json:"items"`
}
