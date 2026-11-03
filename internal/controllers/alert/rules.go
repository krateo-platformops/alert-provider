package alert

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
	"github.com/krateo-platformops/alert-provider/internal/hyperdx"
	"github.com/krateo-platformops/alert-provider/internal/pyfmt"
)

// Tautology is why the alert can never change state, or "" if it can.
//
// A count is never negative. Every alert here counts rows over a window, so the value compared is
// an integer >= 0, and some threshold/type pairs are decided before any data is read:
//
//	above 0            value >= 0   always true: fires forever, on an empty cluster too.
//	below 0            value <  0   never true: silently cannot fire, which is worse, since it
//	                                looks armed and is not.
//	below_or_equal -1  value <= -1  never true, same shape.
//
// This is not a heuristic about whether an alert is useful. It is arithmetic: the comparison has
// one possible outcome, so the alert carries no information either way. It has cost real money
// twice: a catalogue shipped 25 alerts at `above 0` and every one fired permanently, each firing
// launching an RCA, and an Autopilot-authored alert reached run 72 the same way.
//
// It returns a sentence for the status, since an operator reading phase: Invalid needs to know
// which field to change and to what.
func Tautology(threshold any, thresholdType string) string {
	value, ok := pyfmt.Float(threshold)
	if !ok {
		return "" // not a number: the CRD's own typing owns that, not this
	}
	shown := pyfmt.Str(threshold)
	kind := thresholdType
	if kind == "" {
		kind = "above"
	}
	switch strings.ToLower(kind) {
	case "above":
		if value <= 0 {
			return fmt.Sprintf("thresholdType 'above' means value >= %s, and a count is never negative — "+
				"this fires on every evaluation including an empty result. Use 1 to mean 'at least one'.", shown)
		}
	case "below":
		if value <= 0 {
			return fmt.Sprintf("thresholdType 'below' means value < %s, which a count can never satisfy — "+
				"this can never fire. Use 1 to mean 'none in this window'.", shown)
		}
	case "below_or_equal":
		if value < 0 {
			return fmt.Sprintf("thresholdType 'below_or_equal' means value <= %s, which a count can never "+
				"satisfy — this can never fire. Use 0 to mean 'none in this window'.", shown)
		}
	}
	return ""
}

// aggFn is the HyperDX aggregate function of an aggregation, or why it is refused: increase is a
// counter's and is the caller's to allow.
func aggFn(aggregation string) (string, float64, string) {
	switch aggregation {
	case v1alpha1.AggregationAvg, v1alpha1.AggregationMax, v1alpha1.AggregationMin, v1alpha1.AggregationSum:
		return aggregation, 0, ""
	case v1alpha1.AggregationP95:
		return "quantile", 0.95, ""
	case "":
		return "", 0, "an aggregated Alert needs an aggregation: avg, max, min, sum or p95 (or increase on a counter metric)."
	}
	return "", 0, fmt.Sprintf("aggregation %q is not one of avg, max, min, sum, p95, increase.", aggregation)
}

// ValueSelect is the tile select of a log Alert that aggregates spec.Value over the rows its
// where matches, or why it is refused.
func ValueSelect(spec v1alpha1.AlertSpec) (hyperdx.Select, string) {
	if spec.Aggregation == v1alpha1.AggregationIncrease {
		return hyperdx.Select{}, "aggregation 'increase' is a counter metric's; a log value takes avg, max, min, sum or p95."
	}
	fn, level, why := aggFn(spec.Aggregation)
	if why != "" {
		return hyperdx.Select{}, why
	}
	return hyperdx.Select{AggFn: fn, Level: level, Where: spec.Where, ValueExpression: spec.Value}, ""
}

// MetricSelect is the tile select of a metric Alert whose metric is m, or why the pairing is
// refused. Like Tautology, a refusal is a sentence an operator can act on.
func MetricSelect(spec v1alpha1.AlertSpec, m v1alpha1.MetricSpec) (hyperdx.Select, string) {
	sel := hyperdx.Select{Where: LabelsWhere(spec.Labels), MetricName: m.MetricName, MetricType: m.Type}
	switch spec.Aggregation {
	case v1alpha1.AggregationAvg, v1alpha1.AggregationMax, v1alpha1.AggregationMin, v1alpha1.AggregationSum:
		sel.AggFn = spec.Aggregation
	case v1alpha1.AggregationP95:
		sel.AggFn, sel.Level = "quantile", 0.95
	case v1alpha1.AggregationIncrease:
		// HyperDX evaluates 'increase' only on a sum: it is the counter's per-row delta, summed.
		if m.Type != v1alpha1.MetricTypeSum {
			return hyperdx.Select{}, fmt.Sprintf("aggregation 'increase' needs a sum (counter) metric, and %s is a %s. "+
				"Use avg, max, min, sum or p95.", m.MetricName, m.Type)
		}
		sel.AggFn = "increase"
	case "":
		return hyperdx.Select{}, "a metric Alert needs an aggregation: avg, max, min, sum, p95 or increase."
	default:
		return hyperdx.Select{}, fmt.Sprintf("aggregation %q is not one of avg, max, min, sum, p95, increase.", spec.Aggregation)
	}
	if spec.Ratio == nil {
		return sel, ""
	}
	// A ratio divides two totals of the same metric. Only a total divides meaningfully: an
	// average, extreme or percentile over a ratio's denominator is not a share of anything.
	if spec.Aggregation != v1alpha1.AggregationSum && spec.Aggregation != v1alpha1.AggregationIncrease {
		return hyperdx.Select{}, fmt.Sprintf("a ratio divides two totals: aggregation must be sum or increase, not %s.", spec.Aggregation)
	}
	den := LabelsWhere(spec.Ratio.Labels)
	// The same points over and under: the ratio is 1 wherever there is data, so the alert carries
	// no information, the arithmetic Tautology refuses for counts.
	if den == sel.Where {
		return hyperdx.Select{}, "ratio.labels keep the same points as labels, so the ratio is always 1. " +
			"labels select the numerator (e.g. the failing requests), ratio.labels the denominator (all of them)."
	}
	if why := IdleFires(threshold(spec), spec.ThresholdType); why != "" {
		return hyperdx.Select{}, why
	}
	d := sel
	d.Where = den
	sel.Ratio = &d
	return sel, ""
}

// IdleFires is why a ratio Alert would fire on every window with no traffic, or "" if it would not.
//
// HyperDX evaluates a window whose query returns no rows as the value 0 (checkAlerts: an empty
// bucket is "zeroValueIsAlert = doesExceedThreshold(alert, 0)"), and a ratio over an idle service
// returns no rows. So a ratio whose condition holds at 0 fires on every quiet window: 'below 0.99'
// on a success share pages whenever nobody calls the service. Measured on HyperDX 2.35.0: a
// 'below 1' ratio over a series with no rows went ALERT. The comparisons are HyperDX's
// (doesExceedThreshold), evaluated at 0.
func IdleFires(threshold any, thresholdType string) string {
	value, ok := pyfmt.Float(threshold)
	if !ok {
		return ""
	}
	kind := strings.ToLower(thresholdType)
	if kind == "" {
		kind = "above"
	}
	var fires bool
	switch kind {
	case "above":
		fires = 0 >= value
	case "above_exclusive":
		fires = 0 > value
	case "below":
		fires = 0 < value
	case "below_or_equal":
		fires = 0 <= value
	case "equal":
		fires = value == 0
	case "not_equal":
		fires = value != 0
	default:
		return ""
	}
	if !fires {
		return ""
	}
	return fmt.Sprintf("a ratio with thresholdType '%s' %s holds at 0, and HyperDX evaluates a window with no traffic as 0, "+
		"so this fires whenever the service is idle. Alert on the failing share instead: labels select the failures, "+
		"thresholdType 'above' a positive threshold (e.g. above 0.01 rather than a success share below 0.99).", kind, pyfmt.Str(threshold))
}

// LabelsWhere is the SQL filter keeping the metric points whose attributes carry labels, keys in
// order so the same labels always render the same filter (a reordered filter would be a drift
// pushed on every pass). Empty labels keep every point.
func LabelsWhere(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	terms := make([]string, 0, len(keys))
	for _, k := range keys {
		terms = append(terms, fmt.Sprintf("Attributes[%s] = %s", sqlString(k), sqlString(labels[k])))
	}
	return strings.Join(terms, " AND ")
}

// sqlString is s as a ClickHouse string literal: single-quoted, with backslash and quote escaped,
// so a label value can never close the literal and extend the filter.
func sqlString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// OkSince is status.okSince for an Alert whose live state is now state: the time the alert last
// turned OK, kept while it stays OK; an OK alert with none gets now. Empty in every other state.
// Display-only: nothing reads it to close or resolve anything.
func OkSince(st v1alpha1.AlertStatus, state, now string) string {
	if state != "OK" {
		return ""
	}
	if st.State == "OK" && st.OkSince != "" {
		return st.OkSince
	}
	return now
}

// threshold is spec.threshold, 1 when unset.
func threshold(spec v1alpha1.AlertSpec) json.Number {
	if spec.Threshold == "" {
		return "1"
	}
	return spec.Threshold
}
