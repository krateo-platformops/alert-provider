package alert

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
	"github.com/krateo-platformops/alert-provider/internal/hyperdx"
	"github.com/krateo-platformops/alert-provider/internal/hyperdx/hyperdxtest"
)

// The observability fixture: a container not ready for 5 consecutive minutes is ALERT, one series
// per container.
const notReady = "sre-pod-not-ready"

var (
	readyMetric = v1alpha1.MetricSpec{MetricName: "k8s.container.ready", Type: v1alpha1.MetricTypeGauge}
	podKeys     = []string{"k8s.namespace.name", "k8s.pod.name", "k8s.container.name"}
	// podGroupExprs are the group expressions of podKeys, unaliased: what charts/series is sent,
	// and what a 0.2.64 tile held.
	podGroupExprs = []string{
		"if(notEmpty(arrayElement(Attributes, 'k8s.namespace.name')), arrayElement(Attributes, 'k8s.namespace.name'), arrayElement(ResourceAttributes, 'k8s.namespace.name'))",
		"if(notEmpty(arrayElement(Attributes, 'k8s.pod.name')), arrayElement(Attributes, 'k8s.pod.name'), arrayElement(ResourceAttributes, 'k8s.pod.name'))",
		"if(notEmpty(arrayElement(Attributes, 'k8s.container.name')), arrayElement(Attributes, 'k8s.container.name'), arrayElement(ResourceAttributes, 'k8s.container.name'))",
	}
	// podGroupBy is the tile's groupBy for podKeys: each expression aliased as its key, so
	// HyperDX's notification reads "k8s.pod.name:<pod>".
	podGroupBy = "if(notEmpty(arrayElement(Attributes, 'k8s.namespace.name')), arrayElement(Attributes, 'k8s.namespace.name'), arrayElement(ResourceAttributes, 'k8s.namespace.name')) AS `k8s.namespace.name`, " +
		"if(notEmpty(arrayElement(Attributes, 'k8s.pod.name')), arrayElement(Attributes, 'k8s.pod.name'), arrayElement(ResourceAttributes, 'k8s.pod.name')) AS `k8s.pod.name`, " +
		"if(notEmpty(arrayElement(Attributes, 'k8s.container.name')), arrayElement(Attributes, 'k8s.container.name'), arrayElement(ResourceAttributes, 'k8s.container.name')) AS `k8s.container.name`"
	// podGroupBy0264 is the tile's groupBy as 0.2.64 wrote it: the expressions unaliased.
	podGroupBy0264 = strings.Join(podGroupExprs, ", ")
	// t0 is a pass's time: 30s into the minute 15:40.
	t0 = time.Date(2026, 11, 8, 15, 40, 30, 0, time.UTC)
)

func readinessCR() *v1alpha1.Alert {
	return &v1alpha1.Alert{
		ObjectMeta: metav1.ObjectMeta{Name: notReady, Namespace: ns, UID: types.UID("uid-" + notReady)},
		Spec: v1alpha1.AlertSpec{MetricRef: &v1alpha1.MetricRef{Name: "k8s-container-ready"}, Aggregation: "max",
			GroupBy: podKeys, ConsecutiveWindows: 5, Interval: "1m", Threshold: "1", ThresholdType: "below",
			Description: "A container is not ready."},
	}
}

// readinessHarness runs the fixture at the pass time now.
func readinessHarness(t *testing.T, now time.Time, alerts ...*v1alpha1.Alert) *harness {
	t.Helper()
	at(t, now)
	if len(alerts) == 0 {
		alerts = []*v1alpha1.Alert{readinessCR()}
	}
	h := newHarness(t, alerts...)
	h.addMetric(metricCR("k8s-container-ready", readyMetric))
	return h
}

// at sets the pass time.
func at(t *testing.T, now time.Time) {
	saved := clock
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = saved })
}

// minute is the start of the minute i minutes before the window's end at t0, judged one minute
// late: minute(1) is the newest minute judged, minute(0) the newest completed one, left out since
// it may not be ingested yet.
func minute(i int) time.Time {
	return t0.Truncate(time.Minute).Add(-time.Duration(i+1) * time.Minute)
}

// pod is a series of the fixture: a container of a pod in krateo-system.
type pod struct{ name, container string }

// readings are charts/series buckets of p, one per value, oldest first, ending in the newest
// minute judged (minute(1)); a nil value is a bucket whose value is null.
func (p pod) readings(values ...any) []any {
	var out []any
	for i, v := range values {
		out = append(out, map[string]any{"ts_bucket": minute(len(values) - i).UnixMilli(),
			"group": []any{"krateo-system", p.name, p.container}, "series_0.data": v})
	}
	return out
}

func (p pod) group() map[string]string {
	return map[string]string{"k8s.namespace.name": "krateo-system", "k8s.pod.name": p.name, "k8s.container.name": p.container}
}

func replies(parts ...[]any) []any {
	out := []any{}
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

var (
	seal     = pod{"krateo-audit-worm-seal-29902560-6nm4v", "seal"}
	snowplow = pod{"snowplow-5c4954d6fc-s5rvj", "snowplow"}
)

// --- the tile and the HyperDX alert ---

func TestAGroupedAlertTileGroupsEachKeyOnThePointOrItsResource(t *testing.T) {
	h := readinessHarness(t, t0)
	h.pass(2)
	live := h.byName()[notReady]
	if live == nil {
		t.Fatal("no HyperDX alert")
	}
	tile := h.api.TileOf(live)
	eq(t, "groupBy", tile["groupBy"], podGroupBy)
	if g := tile["groupBy"].(string); strings.ContainsAny(g, "[=") {
		t.Errorf("groupBy holds [ or =, which HyperDX nulls: %s", g)
	}
	eq(t, "displayType", tile["displayType"], "line")
	eq(t, "select", tile["select"], []any{map[string]any{"aggFn": "max", "metricName": "k8s.container.ready",
		"metricType": "gauge", "valueExpression": "Value", "where": "", "whereLanguage": "sql"}})
	eq(t, "numConsecutiveWindows", live["numConsecutiveWindows"], 5)
	// HyperDX's body opens with its own group line: the message is pushed as written.
	eq(t, "message", live["message"], "A container is not ready.")
	eq(t, "phase", h.status(notReady).Phase, "Synced")

	// It converges: the windows are no drift.
	h.api.ClearWrites()
	h.pass(3)
	eq(t, "writes", nonWebhookWrites(h.api), []hyperdxtest.Write{})
}

func TestAKeyIsEscapedInItsGroupExpression(t *testing.T) {
	eq(t, "quoted", GroupBy([]string{`a'b\`}), []hyperdx.GroupKey{{Alias: `a'b\`,
		Expr: `if(notEmpty(arrayElement(Attributes, 'a\'b\\')), arrayElement(Attributes, 'a\'b\\'), arrayElement(ResourceAttributes, 'a\'b\\'))`}})
	eq(t, "none", GroupBy(nil), []hyperdx.GroupKey(nil))
}

// Each key is aliased as itself, from the one expression charts/series is sent unaliased.
func TestEachGroupKeyIsAliasedAsItself(t *testing.T) {
	keys := GroupBy(podKeys)
	for i, g := range keys {
		eq(t, "alias", g.Alias, podKeys[i])
		eq(t, "expr", g.Expr, podGroupExprs[i])
	}
}

// A 0.2.64 tile grouped by the unaliased expressions is rewritten in place on upgrade: one PUT of
// the dashboard, the same HyperDX alert and tile, and then no drift.
func TestAnUnaliasedTileIsRewrittenInPlaceOnUpgrade(t *testing.T) {
	h := readinessHarness(t, t0)
	h.pass(2)
	before := h.byName()[notReady]
	st := h.status(notReady)
	d := h.api.Get("dashboards", before["dashboardId"].(string))
	d["tiles"].([]any)[0].(map[string]any)["config"].(map[string]any)["groupBy"] = podGroupBy0264
	h.api.Put("dashboards", d)
	eq(t, "0.2.64 tile", h.api.TileOf(before)["groupBy"], podGroupBy0264)

	h.api.ClearWrites()
	h.pass(1)
	eq(t, "writes", nonWebhookWrites(h.api), []hyperdxtest.Write{hyperdxtest.W("PUT", "dashboards", st.HyperdxDashboardID)})
	live := h.byName()[notReady]
	eq(t, "aliased", h.api.TileOf(live)["groupBy"], podGroupBy)
	eq(t, "same alert", live["id"], before["id"])
	eq(t, "same tile", live["tileId"], before["tileId"])
	eq(t, "same status alert", h.status(notReady).HyperdxAlertID, st.HyperdxAlertID)

	h.api.ClearWrites()
	h.pass(3)
	eq(t, "converged", nonWebhookWrites(h.api), []hyperdxtest.Write{})
}

func TestAChangedGroupByRewritesTheTileInPlace(t *testing.T) {
	h := readinessHarness(t, t0)
	h.pass(2)
	before := h.byName()[notReady]
	h.edit(notReady, func(a *v1alpha1.Alert) { a.Spec.GroupBy = []string{"k8s.pod.name"} })
	h.pass(1)
	live := h.byName()[notReady]
	eq(t, "groupBy", h.api.TileOf(live)["groupBy"], podGroupExprs[1]+" AS `k8s.pod.name`")
	eq(t, "same alert", live["id"], before["id"])
	eq(t, "same tile", live["tileId"], before["tileId"])

	// Ungrouped, the tile carries no groupBy; the message is the same.
	h.edit(notReady, func(a *v1alpha1.Alert) { a.Spec.GroupBy = nil })
	h.pass(1)
	live = h.byName()[notReady]
	if g, ok := h.api.TileOf(live)["groupBy"]; ok {
		t.Errorf("an ungrouped tile kept groupBy %v", g)
	}
	eq(t, "message", live["message"], "A container is not ready.")
	eq(t, "same alert", live["id"], before["id"])
	eq(t, "same tile", live["tileId"], before["tileId"])

	// Grouping again rewrites it back.
	h.edit(notReady, func(a *v1alpha1.Alert) { a.Spec.GroupBy = podKeys })
	h.pass(1)
	eq(t, "regrouped", h.api.TileOf(h.byName()[notReady])["groupBy"], podGroupBy)
}

// --- the grouped paths HyperDX 2.35 cannot render, refused until proven ---

var latencyHistogram = v1alpha1.MetricSpec{MetricName: "http.server.duration", Type: v1alpha1.MetricTypeHistogram}

func TestOnlyAGaugeOrANonIncreaseSumIsGrouped(t *testing.T) {
	grouped := v1alpha1.AlertSpec{GroupBy: podKeys}
	for _, c := range []struct {
		agg     string
		metric  v1alpha1.MetricSpec
		refused string // a substring of the refusal; empty when admitted
	}{
		{"max", readyMetric, ""},
		{"p95", readyMetric, ""},
		{"sum", costMetric, ""},
		{"avg", costMetric, ""},
		{"increase", costMetric, "groupBy does not support aggregation 'increase' yet"},
		{"p95", latencyHistogram, "groupBy on a histogram metric is not supported yet: http.server.duration is a histogram"},
	} {
		spec := grouped
		spec.Aggregation = c.agg
		sel, why := MetricSelect(spec, c.metric)
		if c.refused == "" {
			if why != "" {
				t.Errorf("%s on a %s: refused: %s", c.agg, c.metric.Type, why)
			}
			eq(t, c.agg+" groups", sel.GroupBy, GroupBy(podKeys))
			continue
		}
		if !strings.Contains(why, c.refused) {
			t.Errorf("%s on a %s: want a refusal containing %q, got %q", c.agg, c.metric.Type, c.refused, why)
		}
	}
	// Ungrouped, both are what they were.
	for _, c := range []struct {
		agg    string
		metric v1alpha1.MetricSpec
	}{{"increase", costMetric}, {"p95", latencyHistogram}} {
		if _, why := MetricSelect(v1alpha1.AlertSpec{Aggregation: c.agg}, c.metric); why != "" {
			t.Errorf("ungrouped %s on a %s: refused: %s", c.agg, c.metric.Type, why)
		}
	}
}

// A grouped histogram Alert is Invalid and creates nothing in HyperDX: no dashboard, no alert.
func TestAGroupedHistogramAlertIsInvalidAndPushesNothing(t *testing.T) {
	a := readinessCR()
	a.Spec.MetricRef.Name, a.Spec.Aggregation, a.Spec.ThresholdType = "latency", "p95", "above"
	h := newHarness(t, a)
	h.addMetric(metricCR("latency", latencyHistogram))
	h.pass(2)
	st := h.status(notReady)
	eq(t, "phase", st.Phase, "Invalid")
	if !strings.Contains(st.Error, "groupBy on a histogram metric is not supported yet") {
		t.Errorf("error %q", st.Error)
	}
	eq(t, "no alert", len(h.api.All("alerts")), 0)
	eq(t, "no dashboard", len(h.api.All("dashboards")), 0)

	// Ungrouped, the same histogram Alert is pushed as before.
	h.edit(notReady, func(a *v1alpha1.Alert) { a.Spec.GroupBy = nil })
	h.pass(2)
	eq(t, "phase", h.status(notReady).Phase, "Synced")
	live := h.byName()[notReady]
	if live == nil {
		t.Fatal("no HyperDX alert")
	}
	tile := h.api.TileOf(live)
	if g, ok := tile["groupBy"]; ok {
		t.Errorf("an ungrouped tile has groupBy %v", g)
	}
	eq(t, "select", tile["select"], []any{map[string]any{"aggFn": "quantile", "level": 0.95, "metricName": "http.server.duration",
		"metricType": "histogram", "valueExpression": "Value", "where": "", "whereLanguage": "sql"}})
}

// A groupBy a person changes on the tile in HyperDX is put back.
func TestALiveGroupByEditIsADrift(t *testing.T) {
	h := readinessHarness(t, t0)
	h.pass(2)
	live := h.byName()[notReady]
	d := h.api.Get("dashboards", live["dashboardId"].(string))
	d["tiles"].([]any)[0].(map[string]any)["config"].(map[string]any)["groupBy"] = "ServiceName"
	h.api.Put("dashboards", d)
	h.pass(1)
	eq(t, "groupBy", h.api.TileOf(h.byName()[notReady])["groupBy"], podGroupBy)
}

func TestConsecutiveWindowsIsPushedAndItsDriftDetected(t *testing.T) {
	a := readinessCR()
	a.Spec.ConsecutiveWindows = 0
	h := readinessHarness(t, t0, a)
	h.pass(2)
	if w, ok := h.byName()[notReady]["numConsecutiveWindows"]; ok {
		t.Fatalf("one window sent numConsecutiveWindows %v", w)
	}
	// HyperDX answers null for one window: no drift.
	h.api.Set("alerts", h.status(notReady).HyperdxAlertID, map[string]any{"numConsecutiveWindows": nil})
	h.api.ClearWrites()
	h.pass(2)
	eq(t, "null is one window", nonWebhookWrites(h.api), []hyperdxtest.Write{})

	h.edit(notReady, func(a *v1alpha1.Alert) { a.Spec.ConsecutiveWindows = 3 })
	h.pass(1)
	id := h.status(notReady).HyperdxAlertID
	eq(t, "pushed", h.api.Get("alerts", id)["numConsecutiveWindows"], 3)

	// Changed in HyperDX, it is put back.
	h.api.Set("alerts", id, map[string]any{"numConsecutiveWindows": 7})
	h.pass(1)
	eq(t, "put back", h.api.Get("alerts", id)["numConsecutiveWindows"], 3)

	// Back to one window, it is cleared.
	h.edit(notReady, func(a *v1alpha1.Alert) { a.Spec.ConsecutiveWindows = 1 })
	h.pass(1)
	if w, ok := h.api.Get("alerts", id)["numConsecutiveWindows"]; ok {
		t.Errorf("one window kept numConsecutiveWindows %v", w)
	}
}

func TestAlertDriftComparesConsecutiveWindows(t *testing.T) {
	f := hyperdx.AlertFields{Interval: "1m", Threshold: "1", ThresholdType: "below", Message: "m", ConsecutiveWindows: 5}
	live := map[string]any{"name": "a", "interval": "1m", "threshold": 1, "thresholdType": "below", "message": "m",
		"numConsecutiveWindows": 5}
	eq(t, "same", hyperdx.AlertDrift(live, "a", f), []string(nil))
	live["numConsecutiveWindows"] = 4
	eq(t, "changed", hyperdx.AlertDrift(live, "a", f), []string{"numConsecutiveWindows"})
	f.ConsecutiveWindows, live["numConsecutiveWindows"] = 0, 1
	eq(t, "1 is unset", hyperdx.AlertDrift(live, "a", f), []string(nil))
}

// --- status.groups ---

func TestTheWindowIsJudgedOneIntervalLate(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 7, 30, 0, time.UTC)
	start, end := Window(now, time.Minute, 5)
	eq(t, "1m x 5 at 12:07:30", []string{start.Format("15:04"), end.Format("15:04")}, []string{"12:01", "12:06"})
	start, end = Window(time.Date(2026, 11, 8, 12, 15, 0, 0, time.UTC), 5*time.Minute, 2)
	eq(t, "5m x 2 at 12:15:00", []string{start.Format("15:04"), end.Format("15:04")}, []string{"12:00", "12:10"})
}

// The newest completed minute may not be ingested yet: missing for every group, it changes nothing.
func TestTheNewestCompletedBucketMissingKeepsTheGroupsListed(t *testing.T) {
	h := readinessHarness(t, t0)
	h.api.SetSeries(replies(seal.readings(0, 0, 0, 0, 0), snowplow.readings(0, 0, 0, 0, 0)))
	h.pass(2)
	want := []v1alpha1.AlertGroup{
		{Group: seal.group(), Value: "0", Since: minute(5).Format(time.RFC3339)},
		{Group: snowplow.group(), Value: "0", Since: minute(5).Format(time.RFC3339)}}
	eq(t, "listed", h.status(notReady).Groups, want)
	// A pass later, in the same minute: still no bucket for minute(0) from either group.
	at(t, t0.Add(20*time.Second))
	h.pass(1)
	eq(t, "still listed", h.status(notReady).Groups, want)
}

func TestTheGroupsQueryIsTheTileSelectOverTheLastCompletedWindows(t *testing.T) {
	h := readinessHarness(t, t0)
	h.pass(2)
	reqs := h.api.SeriesRequests()
	if len(reqs) == 0 {
		t.Fatal("no charts/series query")
	}
	body := reqs[len(reqs)-1]
	eq(t, "granularity", body["granularity"], "1m")
	eq(t, "start", body["startTime"], float64(minute(5).UnixMilli()))
	eq(t, "end", body["endTime"], float64(minute(0).UnixMilli()))
	eq(t, "series", body["series"], []any{map[string]any{"sourceId": "src-m", "aggFn": "max", "where": "",
		"whereLanguage": "sql", "metricName": "k8s.container.ready", "metricDataType": "gauge",
		"groupBy": []any{podGroupExprs[0], podGroupExprs[1], podGroupExprs[2]}}})
	// charts/series looks each group up by its expression's text: an alias would answer it null.
	for _, g := range body["series"].([]any)[0].(map[string]any)["groupBy"].([]any) {
		if strings.Contains(g.(string), " AS ") || strings.ContainsAny(g.(string), "[=`") {
			t.Errorf("a charts/series group is aliased or holds [ or =: %s", g)
		}
	}
}

func TestAGroupBreachingEveryWindowIsListed(t *testing.T) {
	h := readinessHarness(t, t0)
	h.api.SetSeries(replies(seal.readings(0, 0, 0, 0, 0)))
	h.pass(2)
	eq(t, "groups", h.status(notReady).Groups, []v1alpha1.AlertGroup{
		{Group: seal.group(), Value: "0", Since: minute(5).Format(time.RFC3339)}})
}

func TestAGroupOKInOneWindowIsNotListed(t *testing.T) {
	h := readinessHarness(t, t0)
	for _, values := range [][]any{{0, 0, 0, 0, 1}, {1, 0, 0, 0, 0}, {0, 0, 1, 0, 0}} {
		h.api.SetSeries(replies(seal.readings(values...)))
		h.pass(1)
		eq(t, fmt.Sprint(values), h.status(notReady).Groups, []v1alpha1.AlertGroup(nil))
	}
}

// A gap is no breach: HyperDX skips it, which restarts the count.
func TestAGroupWithAMissingWindowIsNotListed(t *testing.T) {
	h := readinessHarness(t, t0)
	missing := seal.readings(0, 0, 0, 0, 0)
	h.api.SetSeries(append(append([]any{}, missing[:2]...), missing[3:]...))
	h.pass(2)
	eq(t, "a missing bucket", h.status(notReady).Groups, []v1alpha1.AlertGroup(nil))
	h.api.SetSeries(replies(seal.readings(0, 0, nil, 0, 0)))
	h.pass(1)
	eq(t, "a null bucket", h.status(notReady).Groups, []v1alpha1.AlertGroup(nil))
}

func TestAVanishedGroupDropsOut(t *testing.T) {
	h := readinessHarness(t, t0)
	h.api.SetSeries(replies(seal.readings(0, 0, 0, 0, 0), snowplow.readings(0, 0, 0, 0, 0)))
	h.pass(2)
	eq(t, "both", len(h.status(notReady).Groups), 2)
	// The seal pod is deleted: its series stops, and HyperDX would never resolve it.
	h.api.SetSeries(replies(snowplow.readings(0, 0, 0, 0, 0)))
	h.pass(1)
	eq(t, "groups", h.status(notReady).Groups, []v1alpha1.AlertGroup{
		{Group: snowplow.group(), Value: "0", Since: minute(5).Format(time.RFC3339)}})
	h.api.SetSeries(nil)
	h.pass(1)
	eq(t, "none", h.status(notReady).Groups, []v1alpha1.AlertGroup(nil))
}

func TestSinceIsKeptAcrossPasses(t *testing.T) {
	h := readinessHarness(t, t0)
	h.api.SetSeries(replies(seal.readings(0, 0, 0, 0, 0)))
	h.pass(2)
	since := minute(5).Format(time.RFC3339)
	eq(t, "first", h.status(notReady).Groups[0].Since, since)
	// Three minutes later the window has moved on; the group is still the one that started then.
	at(t, t0.Add(3*time.Minute))
	later := []any{}
	for i := 5; i >= 1; i-- {
		later = append(later, map[string]any{"ts_bucket": minute(i - 3).UnixMilli(),
			"group": []any{"krateo-system", seal.name, seal.container}, "series_0.data": "0"})
	}
	h.api.SetSeries(later)
	h.pass(1)
	eq(t, "kept", h.status(notReady).Groups, []v1alpha1.AlertGroup{{Group: seal.group(), Value: "0", Since: since}})
	// Once it recovers and breaches again, it is a new breach.
	h.api.SetSeries(nil)
	h.pass(1)
	h.api.SetSeries(later)
	h.pass(1)
	eq(t, "anew", h.status(notReady).Groups[0].Since, minute(2).Format(time.RFC3339))
}

// One healthy container does not hide an unhealthy one, as an ungrouped max would.
func TestAHealthyGroupDoesNotMaskAnUnhealthyOne(t *testing.T) {
	h := readinessHarness(t, t0)
	h.api.SetSeries(replies(snowplow.readings(1, 1, 1, 1, 1), seal.readings(0, 0, 0, 0, 0)))
	h.pass(2)
	eq(t, "groups", h.status(notReady).Groups, []v1alpha1.AlertGroup{
		{Group: seal.group(), Value: "0", Since: minute(5).Format(time.RFC3339)}})
}

// A row of the same group and bucket that is OK does not hide one that breaches, as HyperDX keeps
// a bucket's worst case; and a bucket past the window is not counted.
func TestABucketKeepsItsWorstRowAndTheCurrentMinuteIsIgnored(t *testing.T) {
	start := minute(5)
	var buckets []hyperdx.Bucket
	for i := range 5 {
		ts := start.Add(time.Duration(i) * time.Minute)
		buckets = append(buckets, hyperdx.Bucket{Start: ts, Group: []string{"p"}, Value: 0, Has: true},
			hyperdx.Bucket{Start: ts, Group: []string{"p"}, Value: 1, Has: true})
	}
	buckets = append(buckets, hyperdx.Bucket{Start: minute(0), Group: []string{"p"}, Value: 1, Has: true})
	spec := v1alpha1.AlertSpec{GroupBy: []string{"pod"}, Threshold: "1", ThresholdType: "below"}
	got, _ := Groups(spec, nil, buckets, start, time.Minute, 5)
	eq(t, "groups", got, []v1alpha1.AlertGroup{{Group: map[string]string{"pod": "p"}, Value: "0",
		Since: start.Format(time.RFC3339)}})
}

func TestGroupsAreSortedAndCapped(t *testing.T) {
	start := minute(1)
	var buckets []hyperdx.Bucket
	for i := v1alpha1.MaxGroups + 2; i > 0; i-- {
		buckets = append(buckets, hyperdx.Bucket{Start: start, Group: []string{"ns", fmt.Sprintf("p%03d", i)}, Value: 0, Has: true})
	}
	spec := v1alpha1.AlertSpec{GroupBy: []string{"ns", "pod"}, Threshold: "1", ThresholdType: "below"}
	got, truncated := Groups(spec, nil, buckets, start, time.Minute, 1)
	eq(t, "listed", len(got), v1alpha1.MaxGroups)
	eq(t, "truncated", truncated, int32(2))
	eq(t, "first", got[0].Group["pod"], "p001")
	eq(t, "last", got[len(got)-1].Group["pod"], fmt.Sprintf("p%03d", v1alpha1.MaxGroups))
}

func TestGroupsCompareEachThresholdTypeAsHyperDX(t *testing.T) {
	start := minute(1)
	for _, c := range []struct {
		kind, lo, hi string
		value        float64
		listed       bool
	}{
		{"above", "5", "", 5, true}, {"above", "5", "", 4.9, false},
		{"above_exclusive", "5", "", 5, false}, {"above_exclusive", "5", "", 5.1, true},
		{"below", "1", "", 0, true}, {"below", "1", "", 1, false},
		{"below_or_equal", "1", "", 1, true}, {"below_or_equal", "1", "", 1.1, false},
		{"equal", "0", "", 0, true}, {"equal", "0", "", 1, false},
		{"not_equal", "0", "", 1, true}, {"not_equal", "0", "", 0, false},
		{"between", "800", "1200", 800, true}, {"between", "800", "1200", 1200, true},
		{"between", "800", "1200", 1000, true}, {"between", "800", "1200", 799, false}, {"between", "800", "1200", 1201, false},
		{"not_between", "800", "1200", 799, true}, {"not_between", "800", "1200", 1201, true},
		{"not_between", "800", "1200", 800, false}, {"not_between", "800", "1200", 1200, false}, {"not_between", "800", "1200", 1000, false},
	} {
		spec := v1alpha1.AlertSpec{GroupBy: []string{"g"}, Threshold: jsonNum(c.lo), ThresholdType: c.kind, ThresholdMax: jsonNum(c.hi)}
		got, _ := Groups(spec, nil, []hyperdx.Bucket{{Start: start, Group: []string{"x"}, Value: c.value, Has: true}}, start, time.Minute, 1)
		if (len(got) == 1) != c.listed {
			t.Errorf("%s [%s, %s] at %v: listed %v, want %v", c.kind, c.lo, c.hi, c.value, len(got) == 1, c.listed)
		}
	}
}

func TestAFailedGroupsQueryLeavesTheGroupsAsTheyWere(t *testing.T) {
	h := readinessHarness(t, t0)
	h.api.SetSeries(replies(seal.readings(0, 0, 0, 0, 0)))
	h.pass(2)
	want := h.status(notReady).Groups
	eq(t, "listed", len(want), 1)
	h.api.SetSeries(nil)
	h.api.Fail = func(_, kind, _ string) int {
		if kind == "charts" {
			return 502
		}
		return 0
	}
	if err := h.reconcile(notReady); err == nil || !strings.Contains(err.Error(), "status.groups") {
		t.Fatalf("err %v", err)
	}
	eq(t, "groups", h.status(notReady).Groups, want)
	eq(t, "state still mirrored", h.status(notReady).State, "OK")

	// A pass with a push still pushes; the failed read is its error.
	h.edit(notReady, func(a *v1alpha1.Alert) { a.Spec.ConsecutiveWindows = 3 })
	if err := h.reconcile(notReady); err == nil {
		t.Fatal("no error")
	}
	eq(t, "pushed", h.api.Get("alerts", h.status(notReady).HyperdxAlertID)["numConsecutiveWindows"], 3)
	eq(t, "groups after a push", h.status(notReady).Groups, want)
}

func TestAnUngroupedAlertHasNoGroupsAndNoQuery(t *testing.T) {
	h := newHarness(t, metricAlertCR(budget, "llm-cost-usd", "increase", nil))
	h.addMetric(metricCR("llm-cost-usd", costMetric))
	h.pass(2)
	eq(t, "queries", len(h.api.SeriesRequests()), 0)
	eq(t, "groups", h.status(budget).Groups, []v1alpha1.AlertGroup(nil))
	if m := h.byName()[budget]["message"].(string); strings.Contains(m, "{{{group}}}") {
		t.Errorf("an ungrouped message names a group: %q", m)
	}
}

// Ungrouped again, the groups go.
func TestDroppingGroupByClearsTheGroups(t *testing.T) {
	h := readinessHarness(t, t0)
	h.api.SetSeries(replies(seal.readings(0, 0, 0, 0, 0)))
	h.pass(2)
	eq(t, "listed", len(h.status(notReady).Groups), 1)
	h.edit(notReady, func(a *v1alpha1.Alert) { a.Spec.GroupBy = nil })
	h.pass(2)
	eq(t, "groups", h.status(notReady).Groups, []v1alpha1.AlertGroup(nil))
}

func TestAGroupedRatioIsRefused(t *testing.T) {
	_, why := MetricSelect(v1alpha1.AlertSpec{Aggregation: "sum", Labels: failing, Ratio: &v1alpha1.Ratio{Labels: all},
		GroupBy: []string{"service.name"}, Threshold: "0.01", ThresholdType: "above"}, callsMetric)
	if !strings.Contains(why, "groupBy and ratio") {
		t.Fatalf("why %q", why)
	}
}

// A metric Alert's firing opens no incident, grouped or not.
func TestAGroupedFiringOpensNoIncident(t *testing.T) {
	h := readinessHarness(t, t0)
	h.pass(1)
	h.api.Set("alerts", h.status(notReady).HyperdxAlertID, map[string]any{"state": "ALERT"})
	h.pass(1)
	eq(t, "state", h.status(notReady).State, "ALERT")
	eq(t, "fired", h.fired, []string(nil))
}

func jsonNum(s string) json.Number { return json.Number(s) }
