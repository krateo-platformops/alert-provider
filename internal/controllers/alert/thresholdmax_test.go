package alert

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
	"github.com/krateo-platformops/alert-provider/internal/hyperdx/hyperdxtest"
)

// between and not_between are ranges: HyperDX needs thresholdMax, the upper bound, and throws
// ("thresholdMax is required for threshold type ...") without it.

func betweenCR(kind, lo, hi string) *v1alpha1.Alert {
	a := alertCR("latency-degraded", "SeverityText = 'error'", v1alpha1.AlertStatus{})
	a.Spec.ThresholdType, a.Spec.Threshold, a.Spec.ThresholdMax = kind, json.Number(lo), json.Number(hi)
	return a
}

func TestARangeAlertCarriesThresholdMaxToHyperDX(t *testing.T) {
	h := newHarness(t, betweenCR("between", "800", "1200"))
	h.pass(2)
	live := h.byName()["latency-degraded"]
	if live == nil {
		t.Fatal("no HyperDX alert")
	}
	eq(t, "thresholdType", live["thresholdType"], "between")
	eq(t, "threshold", live["threshold"], 800)
	eq(t, "thresholdMax", live["thresholdMax"], 1200)

	// It converges: a further pass writes nothing.
	h.api.ClearWrites()
	h.pass(2)
	eq(t, "writes", nonWebhookWrites(h.api), []hyperdxtest.Write{})
}

func TestAChangedThresholdMaxIsPushed(t *testing.T) {
	h := newHarness(t, betweenCR("not_between", "800", "1200"))
	h.pass(2)
	h.edit("latency-degraded", func(a *v1alpha1.Alert) { a.Spec.ThresholdMax = "1500" })
	h.pass(1)
	eq(t, "thresholdMax", h.api.Get("alerts", h.status("latency-degraded").HyperdxAlertID)["thresholdMax"], 1500)
}

func TestAPlainAlertSendsNoThresholdMax(t *testing.T) {
	h := newHarness(t, alertCR("plain", "SeverityText = 'error'", v1alpha1.AlertStatus{}))
	h.pass(2)
	if _, ok := h.byName()["plain"]["thresholdMax"]; ok {
		t.Fatal("thresholdMax sent for an 'above' alert")
	}
}

func TestARatioRangeThatContainsZeroIsRefused(t *testing.T) {
	for _, c := range []struct{ kind, lo, hi string }{
		{"between", "0", "0.5"}, {"between", "-1", "1"}, {"not_between", "0.01", "0.5"}, {"not_between", "-1", "-0.5"},
	} {
		_, why := MetricSelect(v1alpha1.AlertSpec{Aggregation: "sum", Labels: failing, Ratio: &v1alpha1.Ratio{Labels: all},
			Threshold: json.Number(c.lo), ThresholdMax: json.Number(c.hi), ThresholdType: c.kind}, callsMetric)
		if !strings.Contains(why, "no traffic as 0") {
			t.Errorf("%s [%s, %s]: want the idle refusal, got %q", c.kind, c.lo, c.hi, why)
		}
	}
	for _, c := range []struct{ kind, lo, hi string }{{"between", "0.01", "0.5"}, {"not_between", "-1", "0.5"}} {
		if _, why := MetricSelect(v1alpha1.AlertSpec{Aggregation: "sum", Labels: failing, Ratio: &v1alpha1.Ratio{Labels: all},
			Threshold: json.Number(c.lo), ThresholdMax: json.Number(c.hi), ThresholdType: c.kind}, callsMetric); why != "" {
			t.Errorf("%s [%s, %s]: refused: %s", c.kind, c.lo, c.hi, why)
		}
	}
}
