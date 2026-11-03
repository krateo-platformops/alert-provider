package alert

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
	"github.com/krateo-platformops/alert-provider/internal/hyperdx"
	"github.com/krateo-platformops/alert-provider/internal/hyperdx/hyperdxtest"
)

// The failing share of API requests: the CMP's X5 error-ratio SLO, from the collector's span
// metrics (krateo-observability otel-collector-daemonset 0.1.15).
const sloAlert = "slo-api-error-ratio"

var callsMetric = v1alpha1.MetricSpec{MetricName: "traces.span.metrics.calls", Type: v1alpha1.MetricTypeSum}

func ratioAlertCR(aggregation string, labels, denominator map[string]string) *v1alpha1.Alert {
	a := metricAlertCR(sloAlert, "span-calls", aggregation, labels)
	a.Spec.Interval, a.Spec.Threshold = "1h", "0.01"
	a.Spec.Ratio = &v1alpha1.Ratio{Labels: denominator}
	return a
}

var (
	failing = map[string]string{"service.name": "snowplow", "krateo.sli.error": "true"}
	all     = map[string]string{"service.name": "snowplow"}
)

// --- the select a ratio Alert evaluates ---

func TestMetricSelectBuildsTheDenominator(t *testing.T) {
	sel, why := MetricSelect(v1alpha1.AlertSpec{Aggregation: "sum", Labels: failing, Ratio: &v1alpha1.Ratio{Labels: all}}, callsMetric)
	if why != "" {
		t.Fatalf("refused: %s", why)
	}
	num := hyperdx.Select{AggFn: "sum", MetricName: callsMetric.MetricName, MetricType: "sum",
		Where: "Attributes['krateo.sli.error'] = 'true' AND Attributes['service.name'] = 'snowplow'"}
	den := num
	den.Where = "Attributes['service.name'] = 'snowplow'"
	num.Ratio = &den
	eq(t, "select", sel, num)
}

func TestMetricSelectRatioOverEveryPoint(t *testing.T) {
	sel, why := MetricSelect(v1alpha1.AlertSpec{Aggregation: "increase", Labels: failing, Ratio: &v1alpha1.Ratio{}}, callsMetric)
	if why != "" {
		t.Fatalf("refused: %s", why)
	}
	eq(t, "denominator keeps every point", sel.Ratio.Where, "")
	eq(t, "denominator aggregates the same way", sel.Ratio.AggFn, "increase")
}

func TestARatioOfAnythingButTotalsIsRefused(t *testing.T) {
	for _, agg := range []string{"avg", "max", "min", "p95"} {
		_, why := MetricSelect(v1alpha1.AlertSpec{Aggregation: agg, Labels: failing, Ratio: &v1alpha1.Ratio{Labels: all}}, callsMetric)
		if !strings.Contains(why, "sum or increase") {
			t.Errorf("%s: want a refusal naming sum or increase, got %q", agg, why)
		}
	}
}

func TestARatioOfAPointSetToItselfIsRefused(t *testing.T) {
	_, why := MetricSelect(v1alpha1.AlertSpec{Aggregation: "sum", Labels: all, Ratio: &v1alpha1.Ratio{Labels: all}}, callsMetric)
	if !strings.Contains(why, "always 1") {
		t.Fatalf("want 'always 1', got %q", why)
	}
}

// --- what lands on HyperDX ---

func TestARatioAlertCarriesBothSelectsAsARatio(t *testing.T) {
	h := newHarness(t, ratioAlertCR("sum", failing, all))
	h.addMetric(metricCR("span-calls", callsMetric))
	h.pass(2)
	live := h.byName()[sloAlert]
	if live == nil {
		t.Fatal("no HyperDX alert")
	}
	tile := h.api.TileOf(live)
	eq(t, "asRatio", tile["asRatio"], true)
	eq(t, "select", tile["select"], []any{
		map[string]any{"aggFn": "sum", "metricName": callsMetric.MetricName, "metricType": "sum", "valueExpression": "Value",
			"where": "Attributes['krateo.sli.error'] = 'true' AND Attributes['service.name'] = 'snowplow'", "whereLanguage": "sql"},
		map[string]any{"aggFn": "sum", "metricName": callsMetric.MetricName, "metricType": "sum", "valueExpression": "Value",
			"where": "Attributes['service.name'] = 'snowplow'", "whereLanguage": "sql"},
	})
	eq(t, "threshold", live["threshold"], 0.01)
	eq(t, "phase", h.status(sloAlert).Phase, "Synced")

	// It converges: a further pass writes nothing.
	h.api.ClearWrites()
	h.pass(2)
	eq(t, "writes", nonWebhookWrites(h.api), []hyperdxtest.Write{})
}

func TestAddingAndRemovingARatioRewritesTheTile(t *testing.T) {
	h := newHarness(t, metricAlertCR(sloAlert, "span-calls", "sum", failing))
	h.addMetric(metricCR("span-calls", callsMetric))
	h.pass(2)
	id := h.status(sloAlert).HyperdxAlertID
	eq(t, "count first", h.api.TileOf(h.byName()[sloAlert])["asRatio"], false)

	h.edit(sloAlert, func(a *v1alpha1.Alert) { a.Spec.Ratio = &v1alpha1.Ratio{Labels: all} })
	h.pass(1)
	tile := h.api.TileOf(h.byName()[sloAlert])
	eq(t, "now a ratio", tile["asRatio"], true)
	eq(t, "two selects", len(tile["select"].([]any)), 2)

	// A changed denominator alone is a drift too.
	h.edit(sloAlert, func(a *v1alpha1.Alert) { a.Spec.Ratio = &v1alpha1.Ratio{} })
	h.pass(1)
	tile = h.api.TileOf(h.byName()[sloAlert])
	eq(t, "denominator rewritten", tile["select"].([]any)[1].(map[string]any)["where"], "")

	h.edit(sloAlert, func(a *v1alpha1.Alert) { a.Spec.Ratio = nil })
	h.pass(1)
	tile = h.api.TileOf(h.byName()[sloAlert])
	eq(t, "back to one total", tile["asRatio"], false)
	eq(t, "one select", len(tile["select"].([]any)), 1)
	eq(t, "same alert throughout", h.status(sloAlert).HyperdxAlertID, id)
}

func TestARatioOfPercentilesIsRefusedAndNothingIsPushed(t *testing.T) {
	h := newHarness(t, ratioAlertCR("p95", failing, all))
	h.addMetric(metricCR("span-calls", callsMetric))
	h.pass(2)
	eq(t, "phase", h.status(sloAlert).Phase, "Invalid")
	eq(t, "nothing pushed", len(h.api.All("alerts")), 0)
}

// --- an idle window reads 0 in HyperDX: a ratio must not fire at 0 ---

func TestARatioThatFiresAtZeroIsRefused(t *testing.T) {
	for _, c := range []struct{ threshold, kind string }{
		{"0.99", "below"}, {"0", "below_or_equal"}, {"0.5", "below_or_equal"}, {"0", "equal"},
		{"0.5", "not_equal"}, {"0", "above"}, {"-0.1", "above_exclusive"}, {"0", ""},
	} {
		_, why := MetricSelect(v1alpha1.AlertSpec{Aggregation: "sum", Labels: failing, Ratio: &v1alpha1.Ratio{Labels: all},
			Threshold: json.Number(c.threshold), ThresholdType: c.kind}, callsMetric)
		if !strings.Contains(why, "no traffic as 0") {
			t.Errorf("%s %s: want the idle refusal, got %q", c.kind, c.threshold, why)
		}
	}
}

func TestARatioThatStaysQuietAtZeroIsAccepted(t *testing.T) {
	for _, c := range []struct{ threshold, kind string }{
		{"0.01", "above"}, {"0", "above_exclusive"}, {"0.5", "equal"}, {"0", "not_equal"}, {"0", "below"}, {"-1", "below_or_equal"},
	} {
		if _, why := MetricSelect(v1alpha1.AlertSpec{Aggregation: "sum", Labels: failing, Ratio: &v1alpha1.Ratio{Labels: all},
			Threshold: json.Number(c.threshold), ThresholdType: c.kind}, callsMetric); why != "" {
			t.Errorf("%s %s: refused: %s", c.kind, c.threshold, why)
		}
	}
}

func TestIdleFiresIsForRatiosOnly(t *testing.T) {
	// A plain metric alert keeps its own semantics: 'below 0.99' on a gauge is a real question.
	if _, why := MetricSelect(v1alpha1.AlertSpec{Aggregation: "avg", Threshold: "0.99", ThresholdType: "below"},
		v1alpha1.MetricSpec{MetricName: "x", Type: v1alpha1.MetricTypeGauge}); why != "" {
		t.Fatalf("a non-ratio metric alert was refused: %s", why)
	}
}
