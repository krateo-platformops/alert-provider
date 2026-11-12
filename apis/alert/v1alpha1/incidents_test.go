package v1alpha1

import (
	"os"
	"reflect"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
)

const home = "krateo-system"

func specAlert(namespace string, spec AlertSpec, annotations map[string]string) *Alert {
	return &Alert{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: namespace, Annotations: annotations}, Spec: spec}
}

var (
	logCount   = AlertSpec{Where: "SeverityText = 'error'"}
	traceCount = AlertSpec{Source: SourceTraces, Where: "StatusCode = 'Error'"}
	metric     = AlertSpec{MetricRef: &MetricRef{Name: "k8s-volume-available"}, Aggregation: AggregationMin}
	logValue   = AlertSpec{Value: "toFloat64OrZero(Body)", Aggregation: AggregationSum}
	traceValue = AlertSpec{Source: SourceTraces, Where: "ServiceName = 'snowplow'", Value: "Duration / 1e6", Aggregation: AggregationP95}
	paused     = map[string]string{AnnotationPaused: "true"}
)

func TestTheTypeFollowsTheSpec(t *testing.T) {
	for want, spec := range map[string]AlertSpec{
		AlertTypeLogs: logCount, AlertTypeTraces: traceCount, AlertTypeMetric: metric,
		AlertTypeLogValue: logValue, AlertTypeTraceValue: traceValue,
		// an explicit logs source is the default one
		AlertTypeLogs + " (source logs)": {Source: SourceLogs, Where: "x"},
	} {
		want, _, _ = strings.Cut(want, " ")
		if got := specAlert(home, spec, nil).AlertType(); got != want {
			t.Errorf("%+v: type %s, want %s", spec, got, want)
		}
	}
}

// The reason is the first one that holds, the ones a person cannot lift first: resuming a paused
// metric Alert would still open no incident.
func TestOpensIncidentsNamesTheFirstReasonThatHolds(t *testing.T) {
	for _, c := range []struct {
		what        string
		namespace   string
		spec        AlertSpec
		annotations map[string]string
		status      metav1.ConditionStatus
		reason      prv1.ConditionReason
		message     string
	}{
		{"log count", home, logCount, nil, metav1.ConditionTrue, ReasonCountsRecords, "from the log lines"},
		{"trace count", home, traceCount, nil, metav1.ConditionTrue, ReasonCountsRecords, "from the spans"},
		{"metric", home, metric, nil, metav1.ConditionFalse, ReasonAggregatedValue, "aggregated value"},
		{"log value", home, logValue, nil, metav1.ConditionFalse, ReasonAggregatedValue, "aggregated value"},
		{"trace value", home, traceValue, nil, metav1.ConditionFalse, ReasonAggregatedValue, "aggregated value"},
		{"paused log count", home, logCount, paused, metav1.ConditionFalse, ReasonPaused, "krateo.io/paused"},
		{"paused metric", home, metric, paused, metav1.ConditionFalse, ReasonAggregatedValue, "aggregated value"},
		{"tenant log count", "team-a", logCount, nil, metav1.ConditionFalse, ReasonOutsideHomeNamespace, "Only the Alerts in krateo-system"},
		{"paused tenant metric", "team-a", metric, paused, metav1.ConditionFalse, ReasonOutsideHomeNamespace, "Only the Alerts in krateo-system"},
		{"not paused by another value", home, logCount, map[string]string{AnnotationPaused: "false"}, metav1.ConditionTrue, ReasonCountsRecords, ""},
	} {
		got := specAlert(c.namespace, c.spec, c.annotations).OpensIncidents(home)
		if got.Type != TypeOpensIncidents || got.Status != c.status || got.Reason != c.reason || !strings.Contains(got.Message, c.message) {
			t.Errorf("%s: %s %s %s %q, want %s %s containing %q", c.what, got.Type, got.Status, got.Reason, got.Message, c.status, c.reason, c.message)
		}
	}
}

// The status.type enum is the AlertType constants: a type the enum lacks is refused by the
// apiserver, and the whole status write with it. The Incidents column reads the condition by its
// type.
func TestTheCRDAdmitsEveryTypeAndShowsTheCondition(t *testing.T) {
	data, err := os.ReadFile("../../../helm/alert-provider-crds/templates/crd.alert.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	v := crd.Spec.Versions[0]
	var enum []string
	for _, e := range v.Schema.OpenAPIV3Schema.Properties["status"].Properties["type"].Enum {
		enum = append(enum, strings.Trim(string(e.Raw), `"`))
	}
	if want := []string{AlertTypeLogs, AlertTypeTraces, AlertTypeMetric, AlertTypeLogValue, AlertTypeTraceValue}; !reflect.DeepEqual(enum, want) {
		t.Errorf("status.type enum %v, want %v", enum, want)
	}
	cols := map[string]string{}
	for _, c := range v.AdditionalPrinterColumns {
		cols[c.Name] = c.JSONPath
	}
	if got, want := cols["Type"], ".status.type"; got != want {
		t.Errorf("Type column %q, want %q", got, want)
	}
	if got, want := cols["Incidents"], `.status.conditions[?(@.type=="`+string(TypeOpensIncidents)+`")].status`; got != want {
		t.Errorf("Incidents column %q, want %q", got, want)
	}
	if got, want := cols["Observed"], ".status.stateObservedAt"; got != want {
		t.Errorf("Observed column %q, want %q", got, want)
	}
	if _, ok := v.Schema.OpenAPIV3Schema.Properties["status"].Properties["stateObservedAt"]; !ok {
		t.Error("status.stateObservedAt is not in the schema: an apiserver would prune it")
	}
}
