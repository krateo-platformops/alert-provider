package alert

import (
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
)

// incidentsOf is an Alert's OpensIncidents condition as the apiserver holds it.
func incidentsOf(t *testing.T, a *v1alpha1.Alert) prv1.Condition {
	t.Helper()
	c := a.GetCondition(v1alpha1.TypeOpensIncidents)
	if c.Reason == "" {
		t.Fatalf("%s/%s: no OpensIncidents condition in %v", a.Namespace, a.Name, a.Status.Conditions)
	}
	return c
}

// provider-runtime adds the finalizer to a new Alert between Observe and Update, which reloads it
// and drops what Observe set: the first pass's status must still carry both.
// MUTATION: dropping describe from Update fails this.
func TestANewAlertCarriesItsTypeAndConditionAfterItsFirstPass(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	h.reconcileRuntime(k)
	a := h.get(k)
	c := incidentsOf(t, a)
	eq(t, "type", a.Status.Type, v1alpha1.AlertTypeLogs)
	eq(t, "condition", []any{c.Status, c.Reason}, []any{metav1.ConditionTrue, v1alpha1.ReasonCountsRecords})
}

// Every kind of Alert says what it evaluates and whether it opens incidents, and exactly the ones
// whose condition is True fire into the incident writer.
// MUTATION: a gate that reads Paused, the namespace or Aggregated on its own, in any order, still
// passes; one that skips any of them fails.
func TestEveryAlertSaysWhetherItOpensIncidentsAndTheGateAgrees(t *testing.T) {
	logValue := alertCR("log-value", "ServiceName = 'agentgateway'", v1alpha1.AlertStatus{})
	logValue.Spec.Value, logValue.Spec.Aggregation = "toFloat64OrZero(Body)", v1alpha1.AggregationSum
	traceValue := tracesCR("trace-value", "ServiceName = 'snowplow'")
	traceValue.Spec.Value, traceValue.Spec.Aggregation = "Duration / 1e6", v1alpha1.AggregationP95
	pausedLogs := alertCR("paused", "w", v1alpha1.AlertStatus{})
	pausedLogs.Annotations = map[string]string{v1alpha1.AnnotationPaused: "true"}
	pausedMetric := metricAlertCR("paused-metric", "llm-cost-usd", v1alpha1.AggregationIncrease, nil)
	pausedMetric.Annotations = map[string]string{v1alpha1.AnnotationPaused: "true"}

	h := newHarness(t,
		alertCR("logs", "w", v1alpha1.AlertStatus{}),
		tracesCR("traces", "StatusCode = 'Error'"),
		metricAlertCR("metric", "llm-cost-usd", v1alpha1.AggregationIncrease, nil),
		logValue, traceValue, pausedLogs, pausedMetric,
		tenantCR(teamA, "tenant", "w"),
	)
	h.addMetric(metricCR("llm-cost-usd", costMetric))
	got := h.recordFirings()
	h.pass(1)
	var l v1alpha1.AlertList
	if err := h.kube.List(ctx, &l); err != nil {
		t.Fatal(err)
	}
	for _, a := range l.Items {
		h.api.Set("alerts", a.Status.HyperdxAlertID, map[string]any{"state": "ALERT"})
	}
	h.pass(1)

	type row struct {
		Type   string
		Status metav1.ConditionStatus
		Reason prv1.ConditionReason
	}
	want := map[string]row{
		ns + "/logs":          {v1alpha1.AlertTypeLogs, metav1.ConditionTrue, v1alpha1.ReasonCountsRecords},
		ns + "/traces":        {v1alpha1.AlertTypeTraces, metav1.ConditionTrue, v1alpha1.ReasonCountsRecords},
		ns + "/metric":        {v1alpha1.AlertTypeMetric, metav1.ConditionFalse, v1alpha1.ReasonAggregatedValue},
		ns + "/log-value":     {v1alpha1.AlertTypeLogValue, metav1.ConditionFalse, v1alpha1.ReasonAggregatedValue},
		ns + "/trace-value":   {v1alpha1.AlertTypeTraceValue, metav1.ConditionFalse, v1alpha1.ReasonAggregatedValue},
		ns + "/paused":        {v1alpha1.AlertTypeLogs, metav1.ConditionFalse, v1alpha1.ReasonPaused},
		ns + "/paused-metric": {v1alpha1.AlertTypeMetric, metav1.ConditionFalse, v1alpha1.ReasonAggregatedValue},
		teamA + "/tenant":     {v1alpha1.AlertTypeLogs, metav1.ConditionFalse, v1alpha1.ReasonOutsideHomeNamespace},
	}
	rows, opens := map[string]row{}, []firing{}
	for n, w := range want {
		namespace, name, _ := strings.Cut(n, "/")
		a := h.getIn(namespace, name)
		eq(t, n+" state", a.Status.State, "ALERT")
		c := incidentsOf(t, a)
		rows[n] = row{a.Status.Type, c.Status, c.Reason}
		if w.Status == metav1.ConditionTrue {
			opens = append(opens, firing{namespace, name})
		}
	}
	eq(t, "status", rows, want)
	sortFirings(opens)
	sortFirings(*got)
	eq(t, "fired", *got, opens)
}

// A refused Alert evaluates nothing, but still says what it is: the refusal is about its
// threshold or its Metric, not its type.
func TestARefusedAlertStillCarriesItsTypeAndCondition(t *testing.T) {
	h := newHarness(t, metricAlertCR(budget, "no-such-metric", v1alpha1.AggregationIncrease, nil))
	h.reconcileRuntime(budget)
	a := h.get(budget)
	eq(t, "phase", a.Status.Phase, v1alpha1.PhaseInvalid)
	eq(t, "type", a.Status.Type, v1alpha1.AlertTypeMetric)
	eq(t, "reason", incidentsOf(t, a).Reason, v1alpha1.ReasonAggregatedValue)
}

// Pausing flips the condition on the next pass and resuming flips it back; an unchanged condition
// keeps its lastTransitionTime, so a pass rewrites nothing.
func TestPausingAndResumingFlipTheCondition(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	h.pass(1)
	first := incidentsOf(t, h.get(k))
	h.pass(1)
	eq(t, "unchanged lastTransitionTime", incidentsOf(t, h.get(k)).LastTransitionTime, first.LastTransitionTime)

	h.edit(k, func(a *v1alpha1.Alert) { a.Annotations = map[string]string{v1alpha1.AnnotationPaused: "true"} })
	h.pass(1)
	c := incidentsOf(t, h.get(k))
	eq(t, "paused", []any{c.Status, c.Reason}, []any{metav1.ConditionFalse, v1alpha1.ReasonPaused})

	h.edit(k, func(a *v1alpha1.Alert) { a.Annotations = nil })
	h.pass(1)
	c = incidentsOf(t, h.get(k))
	eq(t, "resumed", []any{c.Status, c.Reason}, []any{metav1.ConditionTrue, v1alpha1.ReasonCountsRecords})
}

func sortFirings(f []firing) {
	sort.Slice(f, func(i, j int) bool {
		if f[i].namespace != f[j].namespace {
			return f[i].namespace < f[j].namespace
		}
		return f[i].name < f[j].name
	})
}
