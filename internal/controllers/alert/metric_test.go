package alert

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
	"github.com/krateo-platformops/alert-provider/internal/hyperdx"
	"github.com/krateo-platformops/alert-provider/internal/hyperdx/hyperdxtest"
)

const budget = "llm-budget-acme"

var costMetric = v1alpha1.MetricSpec{MetricName: "agentgateway_gen_ai_client_cost_usd_total", Type: v1alpha1.MetricTypeSum, Unit: "USD"}

func metricCR(name string, spec v1alpha1.MetricSpec) *v1alpha1.Metric {
	return &v1alpha1.Metric{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec}
}

func metricAlertCR(name, metric, aggregation string, labels map[string]string) *v1alpha1.Alert {
	return &v1alpha1.Alert{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name)},
		Spec: v1alpha1.AlertSpec{MetricRef: &v1alpha1.MetricRef{Name: metric}, Aggregation: aggregation,
			Labels: labels, Interval: "1d", Threshold: "6", ThresholdType: "above", Message: "m"},
	}
}

func (h *harness) addMetric(m *v1alpha1.Metric) {
	h.t.Helper()
	if err := h.kube.Create(ctx, m); err != nil {
		h.t.Fatal(err)
	}
}

// --- the select a metric Alert evaluates ---

func TestMetricSelectMapsEachAggregation(t *testing.T) {
	gauge := v1alpha1.MetricSpec{MetricName: "container.cpu.usage", Type: v1alpha1.MetricTypeGauge}
	for _, c := range []struct {
		agg, aggFn string
		metric     v1alpha1.MetricSpec
		level      float64
	}{
		{"avg", "avg", gauge, 0}, {"max", "max", gauge, 0}, {"min", "min", gauge, 0},
		{"sum", "sum", gauge, 0}, {"p95", "quantile", gauge, 0.95}, {"increase", "increase", costMetric, 0},
	} {
		sel, why := MetricSelect(v1alpha1.AlertSpec{Aggregation: c.agg}, c.metric)
		if why != "" {
			t.Errorf("%s: refused: %s", c.agg, why)
			continue
		}
		eq(t, c.agg, sel, hyperdx.Select{AggFn: c.aggFn, MetricName: c.metric.MetricName, MetricType: c.metric.Type, Level: c.level})
	}
}

func TestMetricSelectRefusesIncreaseOnAnythingButACounter(t *testing.T) {
	for _, typ := range []string{v1alpha1.MetricTypeGauge, v1alpha1.MetricTypeHistogram} {
		_, why := MetricSelect(v1alpha1.AlertSpec{Aggregation: "increase"}, v1alpha1.MetricSpec{MetricName: "m", Type: typ})
		if !strings.Contains(why, "needs a sum (counter) metric") {
			t.Errorf("%s: %q", typ, why)
		}
	}
	if _, why := MetricSelect(v1alpha1.AlertSpec{}, costMetric); why == "" {
		t.Error("a metric Alert with no aggregation was accepted")
	}
	if _, why := MetricSelect(v1alpha1.AlertSpec{Aggregation: "rate"}, costMetric); why == "" {
		t.Error("an unknown aggregation was accepted")
	}
}

func TestLabelsWhereIsOrderedAndQuoted(t *testing.T) {
	eq(t, "empty", LabelsWhere(nil), "")
	eq(t, "ordered", LabelsWhere(map[string]string{"tenant": "acme", "env": "prod"}),
		"Attributes['env'] = 'prod' AND Attributes['tenant'] = 'acme'")
	// A value can never close its literal and extend the filter.
	eq(t, "quoted", LabelsWhere(map[string]string{"tenant": `a' OR 1=1 --\`}),
		`Attributes['tenant'] = 'a\' OR 1=1 --\\'`)
}

// --- a metric Alert is a metric tile on the metrics source ---

func TestAMetricAlertEvaluatesItsMetricOnTheMetricsSource(t *testing.T) {
	h := newHarness(t, metricAlertCR(budget, "llm-cost-usd", "increase", map[string]string{"tenant": "acme"}))
	h.addMetric(metricCR("llm-cost-usd", costMetric))
	h.pass(2)
	live := h.byName()[budget]
	if live == nil {
		t.Fatal("no HyperDX alert")
	}
	tile := h.api.TileOf(live)
	eq(t, "source", tile["sourceId"], "src-m")
	eq(t, "select", tile["select"], []any{map[string]any{
		"aggFn": "increase", "metricName": costMetric.MetricName, "metricType": "sum",
		"valueExpression": "Value", "where": "Attributes['tenant'] = 'acme'", "whereLanguage": "sql"}})
	eq(t, "interval", live["interval"], "1d")
	eq(t, "phase", h.status(budget).Phase, "Synced")

	// It converges: a further pass writes nothing.
	h.api.ClearWrites()
	h.pass(2)
	eq(t, "writes", nonWebhookWrites(h.api), []hyperdxtest.Write{})
}

func TestAChangedAggregationIsPushedOntoTheTile(t *testing.T) {
	h := newHarness(t, metricAlertCR(budget, "llm-cost-usd", "increase", map[string]string{"tenant": "acme"}))
	h.addMetric(metricCR("llm-cost-usd", costMetric))
	h.pass(2)
	id := h.status(budget).HyperdxAlertID
	h.edit(budget, func(a *v1alpha1.Alert) {
		a.Spec.Aggregation = "max"
		a.Spec.Labels = map[string]string{"tenant": "beta"}
	})
	h.pass(1)
	live := h.byName()[budget]
	sel := h.api.TileOf(live)["select"].([]any)[0].(map[string]any)
	eq(t, "aggFn", sel["aggFn"], "max")
	eq(t, "where", sel["where"], "Attributes['tenant'] = 'beta'")
	eq(t, "same alert", h.status(budget).HyperdxAlertID, id)
}

func TestAMissingMetricIsRefused(t *testing.T) {
	h := newHarness(t, metricAlertCR(budget, "nope", "increase", nil))
	h.pass(2)
	st := h.status(budget)
	eq(t, "phase", st.Phase, "Invalid")
	if !strings.Contains(st.Error, "no such Metric") {
		t.Errorf("error %q", st.Error)
	}
	eq(t, "nothing pushed", len(h.api.All("alerts")), 0)
}

func TestIncreaseOnAGaugeIsRefused(t *testing.T) {
	h := newHarness(t, metricAlertCR(budget, "cpu", "increase", nil))
	h.addMetric(metricCR("cpu", v1alpha1.MetricSpec{MetricName: "container.cpu.usage", Type: v1alpha1.MetricTypeGauge}))
	h.pass(2)
	eq(t, "phase", h.status(budget).Phase, "Invalid")
	eq(t, "nothing pushed", len(h.api.All("alerts")), 0)
}

// A count is never negative, a metric can be: 'above 0' is a real question on a metric.
func TestTheCountTautologyDoesNotApplyToAMetric(t *testing.T) {
	a := metricAlertCR(budget, "llm-cost-usd", "increase", nil)
	a.Spec.Threshold = "0"
	h := newHarness(t, a)
	h.addMetric(metricCR("llm-cost-usd", costMetric))
	h.pass(2)
	eq(t, "phase", h.status(budget).Phase, "Synced")
}

// The incident writer reads log records; a metric has none, so its firing opens no incident.
func TestAMetricFiringOpensNoIncident(t *testing.T) {
	h := newHarness(t, metricAlertCR(budget, "llm-cost-usd", "increase", nil))
	h.addMetric(metricCR("llm-cost-usd", costMetric))
	h.pass(1)
	h.api.Set("alerts", h.status(budget).HyperdxAlertID, map[string]any{"state": "ALERT"})
	h.pass(1)
	eq(t, "state", h.status(budget).State, "ALERT")
	eq(t, "fired", h.fired, []string(nil))
}

// An Alert switched from a metric back to a log count gets a log tile again.
func TestASwitchBackToWhereRewritesTheTile(t *testing.T) {
	h := newHarness(t, metricAlertCR(budget, "llm-cost-usd", "increase", nil))
	h.addMetric(metricCR("llm-cost-usd", costMetric))
	h.pass(2)
	h.edit(budget, func(a *v1alpha1.Alert) {
		a.Spec.MetricRef, a.Spec.Aggregation, a.Spec.Labels, a.Spec.Where, a.Spec.Threshold = nil, "", nil, "", "1"
	})
	h.pass(1)
	tile := h.api.TileOf(h.byName()[budget])
	eq(t, "source", tile["sourceId"], "src-1")
	eq(t, "select", tile["select"], []any{map[string]any{"aggFn": "count", "where": "", "whereLanguage": "sql"}})
}
