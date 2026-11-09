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
	sel := hyperdx.Select{Where: LabelsWhere(spec.Labels), MetricName: m.MetricName, MetricType: m.Type,
		GroupBy: GroupBy(spec.GroupBy)}
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
	if why := groupedRefusal(spec, m); why != "" {
		return hyperdx.Select{}, why
	}
	if spec.Ratio == nil {
		return sel, ""
	}
	// A grouped ratio needs both totals grouped alike, and HyperDX pairs a ratio's two selects
	// only ungrouped. The CRD refuses the pair too.
	if len(spec.GroupBy) > 0 {
		return hyperdx.Select{}, "groupBy and ratio cannot be combined: a grouped ratio would need both its totals grouped alike."
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
	if why := IdleFires(threshold(spec), spec.ThresholdType, spec.ThresholdMax); why != "" {
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
func IdleFires(threshold any, thresholdType string, thresholdMax any) string {
	value, ok := pyfmt.Float(threshold)
	if !ok {
		return ""
	}
	kind := strings.ToLower(thresholdType)
	if kind == "" {
		kind = "above"
	}
	if fires, ok := Exceeds(0, kind, value, thresholdMax); !ok || !fires {
		return ""
	}
	return fmt.Sprintf("a ratio with thresholdType '%s' %s holds at 0, and HyperDX evaluates a window with no traffic as 0, "+
		"so this fires whenever the service is idle. Alert on the failing share instead: labels select the failures, "+
		"thresholdType 'above' a positive threshold (e.g. above 0.01 rather than a success share below 0.99).", kind, pyfmt.Str(threshold))
}

// Exceeds is HyperDX's doesExceedThreshold: whether value breaches threshold under thresholdType
// ("" is above), with thresholdMax the upper bound of a range. ok is false when the comparison
// cannot be made: a threshold that is not a number, an unknown type, or a range without its bound.
func Exceeds(value float64, thresholdType string, threshold, thresholdMax any) (exceeds, ok bool) {
	t, ok := pyfmt.Float(threshold)
	if !ok {
		return false, false
	}
	upper, hasUpper := pyfmt.Float(thresholdMax)
	switch strings.ToLower(thresholdType) {
	case "above", "":
		return value >= t, true
	case "above_exclusive":
		return value > t, true
	case "below":
		return value < t, true
	case "below_or_equal":
		return value <= t, true
	case "equal":
		return value == t, true
	case "not_equal":
		return value != t, true
	case "between":
		return value >= t && value <= upper, hasUpper
	case "not_between":
		return value < t || value > upper, hasUpper
	}
	return false, false
}

// groupedRefusal is why a grouped metric Alert cannot be evaluated, or "" if it can. HyperDX 2.35
// renders a tile's groupBy as a plain select list only for a gauge and for a sum without
// increase; the other two paths are refused until each is proven live:
//
//   - increase (a sum): the group is pasted, as one string, into the TopGroups ranking's
//     tuple(...) and a toString(...) null filter (common-utils/src/core/renderChartConfig.ts
//     :1949-2007), where HyperDX's own comment calls an alias a syntax error and several keys
//     do not render. The CRD refuses the pair too.
//   - a histogram (p95): the group is packed into one array literal, [groupBy] AS group
//     (common-utils/src/core/histogram.ts:191), so the notification names a single "group"
//     column. The metric type is the Metric's, which admission cannot see.
func groupedRefusal(spec v1alpha1.AlertSpec, m v1alpha1.MetricSpec) string {
	if len(spec.GroupBy) == 0 {
		return ""
	}
	if spec.Aggregation == v1alpha1.AggregationIncrease {
		return "groupBy does not support aggregation 'increase' yet: HyperDX 2.35 renders the group into " +
			"tuple() and toString() (renderChartConfig.ts:1949-2007), where an alias is a syntax error and " +
			"several keys do not render. On a counter, aggregation 'sum' totals the same per-interval " +
			"deltas (renderChartConfig.ts:2017-2030) and groups."
	}
	if m.Type == v1alpha1.MetricTypeHistogram {
		return fmt.Sprintf("groupBy on a histogram metric is not supported yet: %s is a histogram, and HyperDX 2.35 "+
			"packs a histogram's groups into one array column (histogram.ts:191). Group a gauge or a sum, "+
			"or drop groupBy.", m.MetricName)
	}
	return ""
}

// GroupBy are the keys a tile groups by, each aliased as itself: the key's value on the point,
// else on its resource, as LabelsWhere matches a label. kubeletstats and k8scluster put every
// identity on the resource and leave the point empty, so a point-only group would put every
// series in one group named "".
//
// The tile names each column after its key, "<expr> AS <key in backticks>", so HyperDX's
// notification reads "k8s.pod.name:<pod>"; charts/series is sent the same expressions unaliased
// (see hyperdx.Select).
func GroupBy(keys []string) []hyperdx.GroupKey {
	if len(keys) == 0 {
		return nil
	}
	out := make([]hyperdx.GroupKey, len(keys))
	for i, k := range keys {
		out[i] = hyperdx.GroupKey{Alias: k, Expr: groupExpr(k)}
	}
	return out
}

// groupExpr is the SQL expression of key's value: on the point, else on its resource.
//
// It holds no `[` and no `=`: charts/series names a group's column after its expression and looks
// the value up by that name, and ClickHouse names a column `m['k']` "arrayElement(m, 'k')" and
// `a = b` "equals(a, b)", so a group written with either comes back null (see compare.RowGroup).
// Hence arrayElement, notEmpty and if.
func groupExpr(key string) string {
	k := sqlString(key)
	return fmt.Sprintf("if(notEmpty(arrayElement(Attributes, %s)), arrayElement(Attributes, %s), arrayElement(ResourceAttributes, %s))", k, k, k)
}

// LabelsWhere is the SQL filter keeping the metric points that carry labels, on the point or on its
// resource: kubeletstats puts every identity on the resource and leaves the point empty, so a
// filter on the point alone matches nothing, and a 'below' alert then fires on the empty window.
// Keys are in order so the same labels always render the same filter (a reordered filter would be
// a drift pushed on every pass). Empty labels keep every point.
func LabelsWhere(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	terms := make([]string, 0, len(keys))
	for _, k := range keys {
		key, value := sqlString(k), sqlString(labels[k])
		terms = append(terms, fmt.Sprintf("(Attributes[%s] = %s OR ResourceAttributes[%s] = %s)", key, value, key, value))
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
