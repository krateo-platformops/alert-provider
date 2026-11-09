// Package hyperdx is a client of HyperDX's external API v2 (Bearer auth, port 8000): the calls go
// to the Express backend (krateo-clickstack-api.krateo-system.svc:8000) with the user.accessKey
// that the bootstrap Job writes into the hyperdx-api-token Secret.
//
// API shape notes:
//   - every collection endpoint returns {"data": [...], "meta": {...}}; unwrap normalises it;
//   - resources carry "id" (not "_id"), across alerts, webhooks, sources and dashboards;
//   - a dashboard tile config is {sourceId, select: [{aggFn, where}], displayType, ...}.
package hyperdx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/krateo-platformops/alert-provider/internal/compare"
	"github.com/krateo-platformops/alert-provider/internal/httpx"
	"github.com/krateo-platformops/alert-provider/internal/pyfmt"
)

// DefaultWebhookBody is the generic webhook's body. A generic webhook body can use only {{title}},
// {{body}}, {{link}}, {{state}}, {{startTime}}, {{endTime}} and {{eventId}} (a hash); there is no
// alert id. {{title}} is a state emoji plus the HyperDX alert name: its Alert CR's metadata.name,
// <namespace>/<name> outside the provider's namespace (alert.HyperDXName). {{state}} is ALERT on a firing and OK on a resolve. The provider only logs a
// notification: the reconcile fires alerts.
//
// {{body}} is the rendered notification: for a grouped tile alert it opens with HyperDX's own
// `Group: "<group>"` line (template.ts: {{#if group}}Group: "{{{group}}}"{{/if}}), on a firing and
// a resolve alike, and the alert's message follows it. {{eventId}} is a hash of the
// alert, the channel and the group. The body is safe inside a JSON string: HyperDX 2.35 compiles a
// generic webhook body with noEscape but passes body, title and link through escapeJsonString
// (tasks/checkAlerts/template.ts).
const DefaultWebhookBody = `{"alertName":"{{title}}","state":"{{state}}","body":"{{body}}","eventId":"{{eventId}}","source":"hyperdx-alert"}`

// granularities are the charts/series bucket sizes, in seconds.
var granularities = []struct {
	name    string
	seconds int
}{{"30s", 30}, {"1m", 60}, {"5m", 300}, {"10m", 600}, {"15m", 900}, {"30m", 1800},
	{"1h", 3600}, {"2h", 7200}, {"6h", 21600}, {"12h", 43200}, {"1d", 86400},
	{"2d", 172800}, {"7d", 604800}, {"30d", 2592000}}

// Client calls one HyperDX. Its dashboard list is cached for its lifetime: a new Client serves
// each reconcile, so a `where` comparison never reads an answer from an earlier one.
type Client struct {
	url        string
	key        string
	http       *http.Client
	dashboards []map[string]any
	cached     bool
}

// New returns a client of the HyperDX API at url.
func New(url, accessKey string) *Client {
	return &Client{url: strings.TrimRight(url, "/"), key: accessKey, http: &http.Client{Timeout: 30 * time.Second}}
}

func unwrap(v any) any {
	if m, ok := v.(map[string]any); ok {
		if d, ok := m["data"]; ok {
			return d
		}
	}
	return v
}

func (c *Client) req(ctx context.Context, method, path string, body any) (any, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	url := c.url + path
	r, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+c.key)
	r.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := httpx.Check(resp); err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	v, err := pyfmt.Decode(data)
	if err != nil {
		return nil, err
	}
	return unwrap(v), nil
}

func maps(v any) []map[string]any {
	l, _ := v.([]any)
	out := make([]map[string]any, 0, len(l))
	for _, x := range l {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// ID is a resource's "id" as a string.
func ID(m map[string]any) string {
	s, _ := m["id"].(string)
	return s
}

// Kinds of HyperDX source, as /api/v2/sources names them.
const (
	KindLog    = "log"
	KindTrace  = "trace"
	KindMetric = "metric"
)

// Sources are the HyperDX sources: one of each kind on a ClickStack install (logs, traces,
// metrics, sessions), in the order HyperDX lists them.
func (c *Client) Sources(ctx context.Context) ([]map[string]any, error) {
	v, err := c.req(ctx, http.MethodGet, "/api/v2/sources", nil)
	if err != nil {
		return nil, err
	}
	return maps(v), nil
}

// OfKind is the first of srcs whose kind is kind, or an error naming the missing kind. A source is
// chosen by kind, never by position: the first source is a log source only by install order.
func OfKind(srcs []map[string]any, kind string) (map[string]any, error) {
	for _, s := range srcs {
		if s["kind"] == kind {
			return s, nil
		}
	}
	return nil, fmt.Errorf("no HyperDX %s source configured", kind)
}

// MetricSource is the first HyperDX source of kind "metric": the metrics a metric alert
// aggregates.
func (c *Client) MetricSource(ctx context.Context) (map[string]any, error) {
	srcs, err := c.Sources(ctx)
	if err != nil {
		return nil, err
	}
	return OfKind(srcs, KindMetric)
}

// Select is what a tile aggregates: the count of the rows (log records or spans) matching Where,
// a value read from them, or a metric. It is the tile's single select item.
type Select struct {
	// AggFn is HyperDX's aggregate function: "count" for a counting alert.
	AggFn string
	// Where is the SQL filter applied before aggregating.
	Where string
	// MetricName and MetricType name the metric; empty for a row alert.
	MetricName, MetricType string
	// ValueExpression is what a row alert aggregates from each row; empty when it counts rows.
	// A metric is always aggregated over its Value column.
	ValueExpression string
	// Level is the quantile level when AggFn is "quantile".
	Level float64
	// Ratio, when set, is the denominator: the tile carries both select items with asRatio, and
	// HyperDX evaluates this select divided by Ratio (renderChartConfig: divide(a, b)).
	Ratio *Select
	// GroupBy are the group keys, in order: the tile evaluates one series per distinct tuple of
	// their values, and HyperDX alerts on each series on its own. Empty for one series.
	GroupBy []GroupKey
}

// GroupKey is one key a select groups by: Expr is the SQL expression of its value, Alias the
// column name the tile gives it.
type GroupKey struct {
	Alias, Expr string
}

// groupBy is the tile's config.groupBy: each key's expression AS its alias, as one comma-separated
// SQL list. HyperDX's alert evaluation (checkAlerts) names a firing group after the result's
// column names, "<column>:<value>, ...", so unaliased a notification would repeat each whole
// expression. The tile renders this string raw into SELECT and GROUP BY, which ClickHouse takes.
func (s Select) groupBy() string {
	items := make([]string, len(s.GroupBy))
	for i, g := range s.GroupBy {
		items[i] = g.Expr + " AS " + identifier(g.Alias)
	}
	return strings.Join(items, ", ")
}

// groupExprs are the charts/series groupBy: the keys' expressions, unaliased. charts/series takes
// an alias but looks each group's value up by the expression's text, so aliased it answers every
// group null and the series collapse into one.
func (s Select) groupExprs() []string {
	exprs := make([]string, len(s.GroupBy))
	for i, g := range s.GroupBy {
		exprs[i] = g.Expr
	}
	return exprs
}

// identifier is name as a ClickHouse quoted identifier: backtick-quoted, so a key's dots are part
// of the name, with backslash and backtick escaped, so a name can never close the quote. The CRD
// admits neither in a group key.
func identifier(name string) string {
	return "`" + strings.NewReplacer(`\`, `\\`, "`", "\\`").Replace(name) + "`"
}

// CountSelect counts the rows matching where.
func CountSelect(where string) Select {
	return Select{AggFn: "count", Where: where}
}

// item is the select item a tile carries. A metric is aggregated over its Value column.
func (s Select) item() map[string]any {
	// whereLanguage "sql" is load-bearing: omitted, HyperDX maps the series to lucene and the SQL
	// filter becomes a full-text search that matches its own text.
	m := map[string]any{"aggFn": s.AggFn, "where": s.Where, "whereLanguage": "sql"}
	if s.MetricName != "" {
		m["metricName"], m["metricType"], m["valueExpression"] = s.MetricName, s.MetricType, "Value"
	} else if s.ValueExpression != "" {
		m["valueExpression"] = s.ValueExpression
	}
	if s.AggFn == "quantile" {
		m["level"] = s.Level
	}
	return m
}

// items are the tile's select items: sel's, then its denominator's when it is a ratio.
func (s Select) items() []map[string]any {
	if s.Ratio == nil {
		return []map[string]any{s.item()}
	}
	return []map[string]any{s.item(), s.Ratio.item()}
}

// LiveTile is what a live tile evaluates: its select items in order and whether it divides them.
type LiveTile struct {
	Selects []map[string]any
	AsRatio bool
	// GroupBy is the tile's config.groupBy, empty when it has none.
	GroupBy string
}

// First is the live tile's first select item, empty when it has none.
func (l LiveTile) First() map[string]any {
	if len(l.Selects) == 0 {
		return map[string]any{}
	}
	return l.Selects[0]
}

// SelectDrift says whether a live tile's select items, asRatio, groupBy and source differ from
// what sel and source would write. Only the keys sel writes are compared, as strings, so a field
// HyperDX adds to its answer is never a drift; a different number of items, a flipped asRatio, or
// another groupBy, is.
func SelectDrift(live LiveTile, liveSourceID string, source map[string]any, sel Select) bool {
	if liveSourceID != ID(source) || live.GroupBy != sel.groupBy() {
		return true
	}
	want := sel.items()
	if len(live.Selects) != len(want) || live.AsRatio != (sel.Ratio != nil) {
		return true
	}
	for i, item := range want {
		for k, v := range item {
			if pyfmt.Str(live.Selects[i][k]) != pyfmt.Str(v) {
				return true
			}
		}
	}
	return false
}

// RecordCounts are the records matching where (SQL) over the last seconds, grouped by the groupBy
// expression, the most frequent first. The buckets are the coarsest that cover the window, so each
// group comes back in at most two.
func (c *Client) RecordCounts(ctx context.Context, sourceID, where string, seconds int, groupBy string) ([]compare.Row, error) {
	end := time.Now().UnixMilli()
	granularity := granularities[len(granularities)-1].name
	for _, g := range granularities {
		if g.seconds >= seconds {
			granularity = g.name
			break
		}
	}
	v, err := c.req(ctx, http.MethodPost, "/api/v2/charts/series", map[string]any{
		"startTime": end - int64(seconds)*1000, "endTime": end, "granularity": granularity,
		"series": []any{map[string]any{"sourceId": sourceID, "aggFn": "count", "where": where,
			"whereLanguage": "sql", "groupBy": []string{groupBy}}}})
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, b := range maps(v) { // one per (time bucket, group)
		group := ""
		if g, ok := b["group"].([]any); ok && len(g) > 0 {
			if s, ok := g[0].(string); ok {
				group = s
			} else if g[0] != nil {
				group = pyfmt.Str(g[0])
			}
		}
		n := 0
		if pyfmt.Truthy(b["series_0.data"]) {
			if f, ok := pyfmt.Float(b["series_0.data"]); ok {
				n = int(f)
			}
		}
		counts[group] += n
	}
	rows := make([]compare.Row, 0, len(counts))
	for g, n := range counts {
		rows = append(rows, compare.Row{Record: g, Count: n})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Record < rows[j].Record
	})
	return rows, nil
}

// Bucket is one interval of one series of a grouped select, as /api/v2/charts/series answers it.
type Bucket struct {
	// Start is the interval's start.
	Start time.Time
	// Group are the series' groupBy values, in the select's GroupBy order; "" for a null.
	Group []string
	// Value is the select's value over the interval; false when the series has none there.
	Value float64
	Has   bool
}

// GroupBuckets are sel's buckets over source in [start, end), one per interval of granularity
// that a series of sel's groupBy has data in: the same select, groupBy and buckets HyperDX alerts
// on (a granularity is an interval's name, its buckets aligned as toStartOfInterval aligns them).
// A bucket the reply places outside the range is dropped, so an end HyperDX takes as inclusive
// never adds an interval that has only begun.
func (c *Client) GroupBuckets(ctx context.Context, source map[string]any, sel Select, granularity string, start, end time.Time) ([]Bucket, error) {
	series := map[string]any{"sourceId": ID(source), "aggFn": sel.AggFn, "where": sel.Where,
		"whereLanguage": "sql", "groupBy": sel.groupExprs()}
	if sel.AggFn == "quantile" {
		series["level"] = sel.Level
	}
	if sel.MetricName != "" {
		series["metricName"], series["metricDataType"] = sel.MetricName, sel.MetricType
	} else if sel.ValueExpression != "" {
		series["field"] = sel.ValueExpression
	}
	v, err := c.req(ctx, http.MethodPost, "/api/v2/charts/series", map[string]any{
		"startTime": start.UnixMilli(), "endTime": end.UnixMilli(), "granularity": granularity,
		"series": []any{series}})
	if err != nil {
		return nil, err
	}
	var out []Bucket
	for _, b := range maps(v) {
		ms, ok := pyfmt.Float(b["ts_bucket"])
		if !ok {
			return nil, fmt.Errorf("charts/series: a bucket without a ts_bucket: %s", pyfmt.Dumps(b))
		}
		t := time.UnixMilli(int64(ms)).UTC()
		if t.Before(start) || !t.Before(end) {
			continue
		}
		g, _ := b["group"].([]any)
		group := make([]string, len(sel.GroupBy))
		for i := range group {
			if i < len(g) && g[i] != nil {
				group[i] = pyfmt.Str(g[i])
			}
		}
		bucket := Bucket{Start: t, Group: group}
		// A null is no data, never 0: HyperDX skips it rather than evaluate a gap. A NaN (an
		// average over no points) is no data too.
		if x := b["series_0.data"]; x != nil {
			bucket.Value, bucket.Has = pyfmt.Float(x)
			if !bucket.Has || math.IsNaN(bucket.Value) || math.IsInf(bucket.Value, 0) {
				bucket.Value, bucket.Has = 0, false
			}
		}
		out = append(out, bucket)
	}
	return out, nil
}

// EnsureWebhook ensures a generic webhook named name exists and sends DefaultWebhookBody. It
// returns the webhook's id and whether it was created. An existing webhook whose body differs is
// updated in place (PUT keeps its id, so its alerts keep notifying it); only a new webhook, whose
// alerts must be re-pointed, is created. The URL is not compared: the API redacts it.
func (c *Client) EnsureWebhook(ctx context.Context, name, targetURL, description string) (string, bool, error) {
	if description == "" {
		description = name
	}
	doc := map[string]any{"name": name, "service": "generic", "url": targetURL,
		"description": description, "body": DefaultWebhookBody}
	v, err := c.req(ctx, http.MethodGet, "/api/v2/webhooks", nil)
	if err != nil {
		return "", false, err
	}
	for _, w := range maps(v) {
		if w["name"] == name {
			if w["body"] != DefaultWebhookBody {
				if _, err := c.req(ctx, http.MethodPut, "/api/v2/webhooks/"+ID(w), doc); err != nil {
					return "", false, err
				}
			}
			return ID(w), false, nil
		}
	}
	created, err := c.req(ctx, http.MethodPost, "/api/v2/webhooks", doc)
	if err != nil {
		return "", false, err
	}
	m, _ := created.(map[string]any)
	return ID(m), true, nil
}

// ListDashboards are the dashboards, read once per Client (see Client).
func (c *Client) ListDashboards(ctx context.Context) ([]map[string]any, error) {
	if !c.cached {
		v, err := c.req(ctx, http.MethodGet, "/api/v2/dashboards", nil)
		if err != nil {
			return nil, err
		}
		c.dashboards, c.cached = maps(v), true
	}
	return c.dashboards, nil
}

func (c *Client) invalidate() {
	c.dashboards, c.cached = nil, false
}

// tileFor is the single over-time tile, as both create and update send it. A create mints a new
// tile id whatever is sent. An update must send the live tile's id: an unknown id makes HyperDX
// mint a new tile, drop the old one and delete every alert on it.
//
// It stays a line even when grouped: the v2 API takes groupBy on line and stacked-bar tiles only.
func tileFor(name string, source map[string]any, sel Select, tileID string) map[string]any {
	config := map[string]any{
		"displayType": "line",
		"sourceId":    source["id"],
		"asRatio":     sel.Ratio != nil,
		"fillNulls":   true,
		"select":      anys(sel.items()),
	}
	if len(sel.GroupBy) > 0 {
		config["groupBy"] = sel.groupBy()
	}
	return map[string]any{
		"id": tileID, "x": 0, "y": 0, "w": 6, "h": 3,
		"name":   name,
		"config": config,
	}
}

// TileSelect is the live tile's select items, asRatio and groupBy, and its source id; false when
// the dashboard is gone.
func (c *Client) TileSelect(ctx context.Context, dashboardID string) (LiveTile, string, bool, error) {
	ds, err := c.ListDashboards(ctx)
	if err != nil {
		return LiveTile{}, "", false, err
	}
	for _, d := range ds {
		if ID(d) != dashboardID {
			continue
		}
		t := tiles(d)
		if len(t) == 0 {
			return LiveTile{}, "", false, nil
		}
		cfg, _ := t[0]["config"].(map[string]any)
		src, _ := cfg["sourceId"].(string)
		ratio, _ := cfg["asRatio"].(bool)
		groupBy, _ := cfg["groupBy"].(string)
		return LiveTile{Selects: maps(cfg["select"]), AsRatio: ratio, GroupBy: groupBy}, src, true, nil
	}
	return LiveTile{}, "", false, nil
}

func anys(ms []map[string]any) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

func tiles(d map[string]any) []map[string]any {
	return maps(d["tiles"])
}

// TileWhere is the `where` the dashboard's tile evaluates, and false when the dashboard is gone.
// spec.where lands on the dashboard tile, not the alert, which is why a where edit needs its own
// comparison and its own push.
func (c *Client) TileWhere(ctx context.Context, dashboardID string) (string, bool, error) {
	ds, err := c.ListDashboards(ctx)
	if err != nil {
		return "", false, err
	}
	for _, d := range ds {
		if ID(d) != dashboardID {
			continue
		}
		t := tiles(d)
		if len(t) == 0 {
			return "", false, nil
		}
		cfg, _ := t[0]["config"].(map[string]any)
		sel := maps(cfg["select"])
		if len(sel) == 0 {
			return "", true, nil
		}
		w, ok := sel[0]["where"]
		if !ok {
			return "", true, nil
		}
		if w == nil {
			return "", false, nil
		}
		return pyfmt.Str(w), true, nil
	}
	return "", false, nil
}

// UpdateDashboardTile PUTs the dashboard so its tile tileID evaluates where. tileID is the id the
// alert evaluates: the API keeps a tile id only if it already exists, and any other id replaces the
// tile, which deletes the alerts on the replaced one.
func (c *Client) UpdateDashboardTile(ctx context.Context, dashboardID, name string, source map[string]any, where, tileID string) error {
	return c.UpdateTile(ctx, dashboardID, name, source, CountSelect(where), tileID)
}

// UpdateTile PUTs the dashboard so its tile tileID aggregates sel over source (see
// UpdateDashboardTile for tileID).
func (c *Client) UpdateTile(ctx context.Context, dashboardID, name string, source map[string]any, sel Select, tileID string) error {
	_, err := c.req(ctx, http.MethodPut, "/api/v2/dashboards/"+dashboardID,
		map[string]any{"name": name, "tags": []any{}, "tiles": []any{tileFor(name, source, sel, tileID)}})
	c.invalidate()
	return err
}

// EnsureDashboardTile ensures a single-tile dashboard named name with a count-over-time line chart
// and returns its id and its tile's. A dashboard whose tile id is in taken (a tile another alert
// already evaluates) is not reused, so two alerts never share one `where`.
func (c *Client) EnsureDashboardTile(ctx context.Context, name string, source map[string]any, where string, taken map[string]bool) (string, string, error) {
	return c.EnsureTile(ctx, name, source, CountSelect(where), taken)
}

// EnsureTile is EnsureDashboardTile for any select: a log count or a metric aggregation.
func (c *Client) EnsureTile(ctx context.Context, name string, source map[string]any, sel Select, taken map[string]bool) (string, string, error) {
	ds, err := c.ListDashboards(ctx)
	if err != nil {
		return "", "", err
	}
	for _, d := range ds {
		if t := tiles(d); d["name"] == name && len(t) > 0 && !taken[ID(t[0])] {
			return ID(d), ID(t[0]), nil
		}
	}
	v, err := c.req(ctx, http.MethodPost, "/api/v2/dashboards",
		map[string]any{"name": name, "tags": []any{}, "tiles": []any{tileFor(name, source, sel, "count")}})
	c.invalidate()
	if err != nil {
		return "", "", err
	}
	d, _ := v.(map[string]any)
	t := tiles(d)
	if len(t) == 0 {
		return "", "", fmt.Errorf("the created dashboard has no tile")
	}
	return ID(d), ID(t[0]), nil
}

// ListAlerts are the live alerts.
func (c *Client) ListAlerts(ctx context.Context) ([]map[string]any, error) {
	v, err := c.req(ctx, http.MethodGet, "/api/v2/alerts", nil)
	if err != nil {
		return nil, err
	}
	return maps(v), nil
}

// AlertFields are the fields an Alert CR owns on its HyperDX alert. where is not one: it lives on
// the dashboard tile.
type AlertFields struct {
	Interval      string
	Threshold     any
	ThresholdType string
	// ThresholdMax is the range's upper bound for between and not_between; nil otherwise.
	ThresholdMax any
	Message      string
	// ConsecutiveWindows is HyperDX's numConsecutiveWindows, counted per group; 0 or 1 is one
	// window, HyperDX's default, and is not sent.
	ConsecutiveWindows int
}

// mutable are the alert fields a CR may change after creation.
var mutable = []string{"interval", "threshold", "thresholdType", "thresholdMax", "message", "numConsecutiveWindows"}

// alertBody is the alert document as both POST and PUT take it: PUT is a full replace validated
// against the same schema as POST, so one builder keeps the two from drifting.
func alertBody(name, dashboardID, tileID, webhookID string, f AlertFields) map[string]any {
	message := f.Message
	if message == "" {
		message = name + " threshold crossed — incident-agent will auto-triage."
	}
	b := map[string]any{
		"name":          name,
		"source":        "tile",
		"dashboardId":   dashboardID,
		"tileId":        tileID,
		"interval":      f.Interval,
		"threshold":     f.Threshold,
		"thresholdType": f.ThresholdType,
		"channel":       map[string]any{"type": "webhook", "webhookId": webhookID},
		"message":       message,
	}
	if f.ThresholdMax != nil {
		b["thresholdMax"] = f.ThresholdMax
	}
	// PUT is a full replace, and HyperDX clears a numConsecutiveWindows it is not sent, so leaving
	// it out sets one window.
	if f.ConsecutiveWindows > 1 {
		b["numConsecutiveWindows"] = f.ConsecutiveWindows
	}
	return b
}

// windows is a numConsecutiveWindows as compared: HyperDX answers null when it is unset, which is
// one window, as 1 is.
func windows(v any) string {
	if f, ok := pyfmt.Float(v); ok && f > 1 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return "1"
}

// AlertDrift are the mutable fields and the name that differ between the live alert and the CR,
// sorted. They are compared as strings: the API returns threshold as a number and a CR may carry
// it as either, and 1 != "1" would be a drift corrected on every pass, forever. The name also
// seeds the default message, so an empty message compares equal to the live one.
func AlertDrift(live map[string]any, name string, f AlertFields) []string {
	body := alertBody(name, "", "", "", f)
	var out []string
	for _, field := range append(append([]string{}, mutable...), "name") {
		if field == "numConsecutiveWindows" {
			if windows(live[field]) != windows(body[field]) {
				out = append(out, field)
			}
			continue
		}
		if pyfmt.Str(live[field]) != pyfmt.Str(body[field]) {
			out = append(out, field)
		}
	}
	sort.Strings(out)
	return out
}

func idState(v any, fallbackID string) map[string]any {
	m, _ := v.(map[string]any)
	id, ok := m["id"]
	if !ok {
		id = fallbackID
	}
	state, ok := m["state"]
	if !ok {
		state = "OK"
	}
	return map[string]any{"id": id, "state": state}
}

// UpdateAlert PUTs the alert. It returns {id, state}.
func (c *Client) UpdateAlert(ctx context.Context, alertID, name, dashboardID, tileID, webhookID string, f AlertFields) (map[string]any, error) {
	v, err := c.req(ctx, http.MethodPut, "/api/v2/alerts/"+alertID, alertBody(name, dashboardID, tileID, webhookID, f))
	if err != nil {
		return nil, err
	}
	return idState(v, alertID), nil
}

// EnsureAlert creates the tile alert, or reconciles the one already carrying this name. name is
// the Alert CR's HyperDX name (alert.HyperDXName), unique across namespaces, so one name is one CR. It returns {id, state}.
func (c *Client) EnsureAlert(ctx context.Context, name, dashboardID, tileID, webhookID string, f AlertFields) (map[string]any, error) {
	alerts, err := c.ListAlerts(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range alerts {
		if a["name"] == name {
			if len(AlertDrift(a, name, f)) == 0 {
				return idState(a, ID(a)), nil
			}
			return c.UpdateAlert(ctx, ID(a), name, dashboardID, tileID, webhookID, f)
		}
	}
	v, err := c.req(ctx, http.MethodPost, "/api/v2/alerts", alertBody(name, dashboardID, tileID, webhookID, f))
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	return idState(m, ID(m)), nil
}

// DeleteAlert deletes the alert.
func (c *Client) DeleteAlert(ctx context.Context, id string) error {
	_, err := c.req(ctx, http.MethodDelete, "/api/v2/alerts/"+id, nil)
	return err
}

// DeleteDashboard deletes the dashboard.
func (c *Client) DeleteDashboard(ctx context.Context, id string) error {
	_, err := c.req(ctx, http.MethodDelete, "/api/v2/dashboards/"+id, nil)
	return err
}
