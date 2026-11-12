// Package incident turns a firing alert into Incidents, each with a root-cause analysis.
//
// The Alert reconcile mirrors every Alert's HyperDX state about every 60 s and calls Fire for each
// one that is ALERT. A firing an incident covers (see pick) writes at most its cover time (see
// recordCover); any other opens a new incident: it creates the Incident in state Analyzing, runs
// the incident-agent RCA over A2A, and writes the analysis with its howToFix scripts in state Open.
// An analysis that gives no scripts that can run is closed instead (see RunAnalysis). An alert
// therefore has any number of incidents, one per problem. From Open on, the incident controller
// runs the scripts and moves the Incident.
package incident

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"

	"github.com/krateo-platformops/alert-provider/internal/compare"
	"github.com/krateo-platformops/alert-provider/internal/pyfmt"
	"github.com/krateo-platformops/alert-provider/internal/report"
	"github.com/krateo-platformops/alert-provider/internal/scriptcheck"
	"github.com/krateo-platformops/alert-provider/internal/tracing"
)

const (
	// LabelAlert carries the Alert's metadata.name, so it is at most 63 characters.
	LabelAlert = "observability.krateo.io/alert"
	// LabelAnalysis on an Alert, copied to its incidents, says who analyses them. AnalysisHuman
	// means a person: no comparison, no RCA and no reanalysis, so no model reads what the alert
	// matched. For an alert whose records are the leak, such as a token in telemetry.
	LabelAnalysis = "observability.krateo.io/analysis"
	AnalysisHuman = "human"
	// AnnotationLastCoveredAt, on an open incident, is the RFC 3339 time it last covered a firing
	// of its alert (see recordCover). With its creation it says whether the incident is still fresh
	// (see stale).
	AnnotationLastCoveredAt = "observability.krateo.io/last-covered-at"
	// AnnotationSupersedesStale, on a new incident, names the stale open incidents of its alert the
	// opening firing was not compared with, newest first, comma-separated, at most maxSupersedes.
	// They are left as they are: their lifecycle is the incident controller's.
	AnnotationSupersedesStale = "observability.krateo.io/supersedes-stale"
	maxSupersedes             = 5
	// AnnotationClosedReason, on an incident the writer closed, says why; the close itself is
	// spec.closed, which the incident controller turns into state Closed. Both are set in ONE merge
	// patch (see giveUp, supersede): incident-provider (from the release carrying its #19) records
	// resolution.by analysis-failed only when one patch from one field manager set both, and user
	// otherwise. The writer never writes status.resolution. ClosedAnalysisFailed: its
	// RCA gave no scripts that can run, after the retries and the correction (see RunAnalysis), and
	// status.error says why. Open, it would have nothing to check and nobody would move it on. It
	// still takes its alert's firings for FailedAnalysisHold (see pick).
	AnnotationClosedReason = "observability.krateo.io/closed-reason"
	ClosedAnalysisFailed   = "analysis-failed"
	// AnnotationSupersedesFailed, on a new incident, names the failed analyses of its alert it
	// supersedes, newest first, comma-separated, at most maxSupersedes: the closed ones not yet
	// superseded, and any still Open without scripts (from before failed analyses were closed, or
	// whose close was refused), which it closes. Each gets AnnotationSupersededBy naming it.
	AnnotationSupersedesFailed = "observability.krateo.io/supersedes-failed"
	AnnotationSupersededBy     = "observability.krateo.io/superseded-by"
	// AnnotationRecoveries, on an incident a restart left Analyzing, counts the times its RCA ran
	// again at startup. Past maxRecoveries it is closed as a failed analysis instead: an RCA that
	// takes the process down with it must not run at every start.
	AnnotationRecoveries = "observability.krateo.io/analysis-recoveries"
	maxRecoveries        = 1
	// correctionTurns is how many times an answer without scripts that can run goes back to the
	// agent, in the same kagent thread, with what is wrong with it.
	correctionTurns = 1
	// coverStampsPerWindow bounds the cover-time writes: an incident's cover time is rewritten only
	// once it is older than CoverWindow/coverStampsPerWindow, so an alert that fires every pass
	// patches its incident about 24 times per window, not every pass.
	coverStampsPerWindow = 24
	// writeAttempts is how often a conditioned status write is retried on a 409.
	writeAttempts = 5
)

// Interrupted is the error of an incident a restart left without its analysis.
const Interrupted = "The analysis was interrupted: the alert-provider restarted before it finished."

// AutomaticReanalysisFailed starts the error of an automatic reanalysis that got no answer, which
// leaves its incident Open (see giveUp).
const AutomaticReanalysisFailed = "The automatic reanalysis failed: "

// AutomaticReanalysisNoFix starts the error of an automatic reanalysis whose answer had no scripts
// that can run, which leaves its incident Open too.
const AutomaticReanalysisNoFix = "The automatic reanalysis produced no usable fix: "

// intervalSeconds maps Alert spec.interval to the lookback window HyperDX counts the `where` rows
// over.
var intervalSeconds = map[string]int{"1m": 60, "5m": 300, "15m": 900, "30m": 1800, "1h": 3600,
	"6h": 21600, "12h": 43200, "1d": 86400}

// IntervalSeconds is spec.interval in seconds; 5m when unset or unknown.
func IntervalSeconds(interval string) int {
	if s, ok := intervalSeconds[interval]; ok {
		return s
	}
	return 300
}

// Alert is the firing Alert CR.
type Alert struct {
	compare.Alert
	Namespace string
	// HumanAnalysis is the Alert's LabelAnalysis saying AnalysisHuman.
	HumanAnalysis bool
}

// HumanOnly is status.error on an incident no model analyses: it says why it has no scripts.
const HumanOnly = "This alert is analysed by a person: no model reads what it matched."

// Records returns the alert's current records: the rows matching where over the last seconds.
type Records func(ctx context.Context, where string, seconds int) ([]compare.Row, error)

// Config configures a Writer.
type Config struct {
	// Namespace is the home namespace: its Alerts alone open Incidents, which it holds. A firing
	// of an Alert in any other namespace (a tenant's) is refused: the RCA reads the whole platform.
	Namespace string
	// MaxConcurrentAnalyses is how many RCAs run at once. One RCA reads up to ~1M input tokens a
	// minute, so alerts that fire together (a startup, one fault behind several alerts) would
	// otherwise exhaust the model's per-minute quota and fail every RCA at once. A new incident
	// waits, Analyzing, for a free slot.
	MaxConcurrentAnalyses int
	// FailedAnalysisHold is how long an incident whose RCA failed keeps taking its alert's
	// firings, from the failure (status.completedAt), though it is closed. Past it, a firing nothing
	// else covers opens a new incident and RCA, which supersedes it.
	FailedAnalysisHold time.Duration
	// MaxCandidates is how many of an alert's newest open, analyzed incidents one comparison weighs.
	MaxCandidates int
	// CoverWindow is how long an open, analyzed incident stays a comparison candidate after it
	// opened or last covered a firing (AnnotationLastCoveredAt). Past it the incident is stale: no
	// comparison weighs it, so a firing it would have covered opens a new incident and RCA, which
	// names it in AnnotationSupersedesStale. An incident nobody moves on would otherwise take every
	// later firing of its alert, and new problems would get no analysis. 0 disables it: every open
	// incident is a candidate and no cover time is written.
	CoverWindow time.Duration
	// AnalysisRetries is how many more times an RCA that failed transiently (see transient) runs,
	// AnalysisRetryDelay after the first failure and twice as long after each next one. The
	// incident stays Analyzing meanwhile.
	AnalysisRetries    int
	AnalysisRetryDelay time.Duration
}

// Writer writes the Incidents of firing alerts.
type Writer struct {
	cfg  Config
	kube Kube
	// analyze runs the RCA; compare is the comparison model.
	analyze func(ctx context.Context, prompt, contextID string) (string, []report.ToolResult, error)
	compare func(ctx context.Context, prompt string, names []string) (string, string, error)
	log     logging.Logger
	now     func() time.Time
	// sleep waits d, and is false when ctx ended first; spawn runs a reanalysis' RCA off its
	// reconcile.
	sleep func(ctx context.Context, d time.Duration) bool
	spawn func(func())

	slots    chan struct{}
	mu       sync.Mutex
	firing   map[string]bool    // namespace/alert pairs with an evaluation in progress
	verdicts map[string]verdict // namespace/alert: its last comparison the model answered
}

// verdict is a comparison the model answered: the incidents it weighed, the records it saw, and
// the incident it named ("" for none).
type verdict struct {
	incidents string
	records   map[string]bool
	match     string
	reason    string
}

// NewWriter returns a Writer.
func NewWriter(cfg Config, kube Kube, a2a *A2A, comparer *Comparer, log logging.Logger) *Writer {
	return newWriter(cfg, kube, a2a.Analyze, comparer.Compare, log)
}

func newWriter(cfg Config, kube Kube,
	analyze func(ctx context.Context, prompt, contextID string) (string, []report.ToolResult, error),
	cmp func(ctx context.Context, prompt string, names []string) (string, string, error),
	log logging.Logger) *Writer {
	if cfg.MaxConcurrentAnalyses < 1 {
		cfg.MaxConcurrentAnalyses = 1
	}
	if cfg.MaxCandidates < 1 {
		cfg.MaxCandidates = 1
	}
	if cfg.AnalysisRetries < 0 {
		cfg.AnalysisRetries = 0
	}
	return &Writer{cfg: cfg, kube: kube, analyze: analyze, compare: cmp, log: log,
		now:      func() time.Time { return time.Now().UTC() },
		sleep:    sleep,
		spawn:    func(f func()) { go f() },
		slots:    make(chan struct{}, cfg.MaxConcurrentAnalyses),
		firing:   map[string]bool{},
		verdicts: map[string]verdict{}}
}

// IncidentName is <alert>-<yyyymmdd-hhmmss> of the opening firing, in UTC.
func IncidentName(alert string, at time.Time) string {
	return alert + "-" + at.UTC().Format("20060102-150405")
}

// contextID is the kagent thread of one RCA run, named by key: every run starts from an empty
// conversation.
func contextID(key string) string {
	return uuid.NewSHA1(uuid.NameSpaceDNS, []byte(key)).String()
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func metadata(obj map[string]any) map[string]any {
	m, _ := obj["metadata"].(map[string]any)
	return m
}

func status(obj map[string]any) map[string]any {
	m, _ := obj["status"].(map[string]any)
	return m
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// state is status.state, and false when it is unset (Python's None).
func state(obj map[string]any) (string, bool) {
	v, ok := status(obj)["state"]
	if !ok || v == nil {
		return "", false
	}
	return pyfmt.Str(v), true
}

// ended says the incident is over: Resolved or Closed, or spec.closed set and not yet turned into
// Closed by the incident controller.
func ended(obj map[string]any) bool {
	s, _ := state(obj)
	return s == "Resolved" || s == "Closed" || closedInSpec(obj)
}

func closedInSpec(obj map[string]any) bool {
	b, _ := spec(obj)["closed"].(bool)
	return b
}

func annotations(obj map[string]any) map[string]any {
	a, _ := metadata(obj)["annotations"].(map[string]any)
	return a
}

func humanIncident(obj map[string]any) bool {
	labels, _ := metadata(obj)["labels"].(map[string]any)
	return labels[LabelAnalysis] == AnalysisHuman
}

// closedFailed says the writer closed the incident as a failed analysis.
func closedFailed(obj map[string]any) bool {
	return annotations(obj)[AnnotationClosedReason] == ClosedAnalysisFailed
}

// openFailed says the incident is Open with an error and no scripts: an RCA that failed before
// failed analyses were closed, or one whose close was refused. A person's incident is not one.
func openFailed(obj map[string]any) bool {
	s, _ := state(obj)
	st := status(obj)
	return s == "Open" && !closedInSpec(obj) && !humanIncident(obj) && st["howToFix"] == nil && pyfmt.Truthy(st["error"])
}

func analyzingOrUnset(obj map[string]any) bool {
	s, ok := state(obj)
	return !ok || s == "Analyzing"
}

// sortKey orders incidents by (creationTimestamp, name).
func sortKey(obj map[string]any) [2]string {
	m := metadata(obj)
	return [2]string{str(m, "creationTimestamp"), str(m, "name")}
}

func less(a, b [2]string) bool {
	if a[0] != b[0] {
		return a[0] < b[0]
	}
	return a[1] < b[1]
}

// newest is the incident with the greatest (creationTimestamp, name), the first of equals.
func newest(items []map[string]any) map[string]any {
	var out map[string]any
	for _, i := range items {
		if out == nil || less(sortKey(out), sortKey(i)) {
			out = i
		}
	}
	return out
}

func timestamp(v any) (time.Time, bool) {
	if !pyfmt.Truthy(v) {
		return time.Time{}, false
	}
	return pyfmt.ParseTime(pyfmt.Str(v))
}

// resolvedWithin says incident is Resolved less than grace before at.
func resolvedWithin(incident map[string]any, at time.Time, grace time.Duration) bool {
	if s, _ := state(incident); s != "Resolved" {
		return false
	}
	res, _ := status(incident)["resolution"].(map[string]any)
	resolved, ok := timestamp(res["at"])
	return ok && at.Sub(resolved) < grace
}

// holdsFirings says an open incident without an analysis takes its alert's firings at at: always
// while its RCA runs, and for hold after the RCA failed, counted from status.completedAt, else
// from its creation.
func holdsFirings(incident map[string]any, at time.Time, hold time.Duration) bool {
	return analyzingOrUnset(incident) || withinHold(incident, at, hold)
}

// withinHold says the incident's RCA failed less than hold before at, counted from
// status.completedAt, else from its creation.
func withinHold(incident map[string]any, at time.Time, hold time.Duration) bool {
	since, ok := timestamp(status(incident)["completedAt"])
	if !ok {
		since, ok = timestamp(metadata(incident)["creationTimestamp"])
	}
	return !ok || at.Sub(since) < hold
}

// lastActive is when incident opened or last covered a firing (AnnotationLastCoveredAt), whichever
// is later; false when it carries neither.
func lastActive(incident map[string]any) (time.Time, bool) {
	m := metadata(incident)
	since, ok := timestamp(m["creationTimestamp"])
	annotations, _ := m["annotations"].(map[string]any)
	if covered, cok := timestamp(annotations[AnnotationLastCoveredAt]); cok && (!ok || covered.After(since)) {
		return covered, true
	}
	return since, ok
}

// stale says an open incident neither opened nor covered a firing in the CoverWindow before at.
// An incident without either time is never stale.
func (w *Writer) stale(incident map[string]any, at time.Time) bool {
	if w.cfg.CoverWindow <= 0 {
		return false
	}
	since, ok := lastActive(incident)
	return ok && at.Sub(since) >= w.cfg.CoverWindow
}

func (w *Writer) alertIncidents(ctx context.Context, ns, alert string) ([]map[string]any, error) {
	return w.kube.List(ctx, ns, LabelAlert+"="+alert)
}

// pick is the incident a firing at at counts on, or nil to open a new one, and the stale open
// incidents (see stale) it did not compare, newest first. In order:
//  1. the open incident (any state but Resolved and Closed) with an analysis that the comparison
//     (see verdict) names as the cause of the alert's current records, among the newest
//     MaxCandidates that are not stale;
//  2. the newest open incident without an analysis: one still Analyzing, or one whose RCA failed
//     less than FailedAnalysisHold ago; else the newest incident closed as a failed analysis
//     (closedFailed), not yet superseded, less than FailedAnalysisHold after its failure. There is
//     nothing to compare with, and a new incident would rerun the analysis, which while the agent
//     or its model is down fails the same way. Past the hold, a failed incident takes no more
//     firings, so a new incident gets a fresh RCA and supersedes it;
//  3. the alert's latest incident when it is Resolved less than grace (one spec.interval) ago: a
//     `where` alert keeps counting rows from before the fix for its lookback window, and those
//     belong to the incident the fix resolved. A Closed incident takes no firing past rule 2.
//
// rows returns the alert's current records; it is called only when there is an incident to
// compare with. It returns a compare.NoVerdict when the comparison gave no verdict and nothing
// above covers the firing: whether it is a new problem is then unknown, so nothing opens.
func (w *Writer) pick(ctx context.Context, alert Alert, ns string, items []map[string]any, at time.Time,
	grace time.Duration, rows func() ([]compare.Row, error)) (map[string]any, []string, error) {
	var open []map[string]any
	for _, i := range items {
		if !ended(i) {
			open = append(open, i)
		}
	}
	sort.SliceStable(open, func(a, b int) bool { return less(sortKey(open[b]), sortKey(open[a])) })
	if alert.HumanAnalysis {
		// Nothing to compare, and its records must not reach a model: the newest open incident
		// takes every firing until a person closes it, however long ago it opened.
		if len(open) > 0 {
			return open[0], nil, nil
		}
		if latest := newest(items); latest != nil && resolvedWithin(latest, at, grace) {
			return latest, nil, nil
		}
		return nil, nil, nil
	}
	var noVerdict error
	var candidates []map[string]any
	var stale []string
	for _, i := range open {
		switch {
		case !compare.Comparable(i):
		case w.stale(i, at):
			stale = append(stale, str(metadata(i), "name"))
		case len(candidates) < w.cfg.MaxCandidates:
			candidates = append(candidates, i)
		}
	}
	if len(stale) > 0 {
		logging.WithContext(w.log, ctx).Info(fmt.Sprintf("[compare] %s/%s: %d stale open incidents not compared (no firing covered in %s)",
			ns, alert.Name, len(stale), w.cfg.CoverWindow))
	}
	if len(candidates) > 0 {
		names := make([]string, len(candidates))
		byName := map[string]map[string]any{}
		for i, c := range candidates {
			names[i] = str(metadata(c), "name")
			byName[names[i]] = c
		}
		match, reason, err := w.verdict(ctx, ns+"/"+alert.Name, alert, candidates, names, rows)
		if err != nil {
			logging.WithContext(w.log, ctx).Info(fmt.Sprintf("[compare] %s/%s vs %d incidents: no verdict (%s)", ns, alert.Name, len(candidates), err))
			noVerdict = err
		} else {
			shown := match
			if shown == "" {
				shown = "none"
			}
			logging.WithContext(w.log, ctx).Info(fmt.Sprintf("[compare] %s/%s vs %d incidents: %s (%s)", ns, alert.Name, len(candidates), shown, reason))
			if match != "" {
				return byName[match], stale, nil
			}
		}
	}
	for _, i := range open {
		if !compare.Comparable(i) && holdsFirings(i, at, w.cfg.FailedAnalysisHold) {
			return i, stale, nil
		}
	}
	var failed []map[string]any
	for _, i := range items {
		if closedFailed(i) && annotations(i)[AnnotationSupersededBy] == nil {
			failed = append(failed, i)
		}
	}
	if f := newest(failed); f != nil && withinHold(f, at, w.cfg.FailedAnalysisHold) {
		return f, stale, nil
	}
	if latest := newest(items); latest != nil && resolvedWithin(latest, at, grace) {
		return latest, stale, nil
	}
	return nil, stale, noVerdict
}

// supersedable are the failed analyses of an alert a new incident supersedes, newest first, at most
// maxSupersedes: the ones closed as failed analyses and the ones still Open without scripts (see
// openFailed), not yet superseded.
func supersedable(items []map[string]any) []map[string]any {
	var out []map[string]any
	for _, i := range items {
		if annotations(i)[AnnotationSupersededBy] == nil && (closedFailed(i) || openFailed(i)) {
			out = append(out, i)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return less(sortKey(out[b]), sortKey(out[a])) })
	return out[:min(len(out), maxSupersedes)]
}

// supersede links each failed analysis to the incident name that supersedes it, and closes the
// ones still Open. A failure is logged: the new incident names them either way.
func (w *Writer) supersede(ctx context.Context, ns, name string, failed []map[string]any) {
	for _, f := range failed {
		old := str(metadata(f), "name")
		ann := map[string]any{AnnotationSupersededBy: name}
		patch := map[string]any{"metadata": map[string]any{"annotations": ann}}
		closing := openFailed(f)
		if closing {
			ann[AnnotationClosedReason] = ClosedAnalysisFailed
			patch["spec"] = map[string]any{"closed": true}
		}
		if err := w.kube.Patch(ctx, ns, old, patch); err != nil {
			logging.WithContext(w.log, ctx).Info(fmt.Sprintf("[incident] %s/%s: not marked superseded by %s (%s)", ns, old, name, err))
			continue
		}
		done := "superseded"
		if closing {
			done = "closed, superseded"
		}
		logging.WithContext(w.log, ctx).Info(fmt.Sprintf("[incident] %s/%s: %s by %s", ns, old, done, name))
	}
}

// verdict is the comparison of a firing with candidates, named names: the alert's last answered
// one while it still holds, else one LLM call. It holds while the candidates are the same and every
// current record was among the ones it saw; a record that left the window since does not void it.
// Only an answered call is remembered, so a failed one is retried on the next firing.
//
// Its span is incident.compare: the records read (a HyperDX client span) and, unless the last
// verdict holds, the model call (a client span). Neither the prompt nor the reason is recorded.
func (w *Writer) verdict(ctx context.Context, key string, alert Alert, candidates []map[string]any, names []string,
	rows func() ([]compare.Row, error)) (match, reason string, err error) {
	ctx, span := tracing.Start(ctx, spanCompare, trace.WithAttributes(
		append(alertAttrs(alert.Namespace, alert.Name), attrCandidates.Int(len(candidates)))...))
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		} else if match != "" { // none: no attribute
			span.SetAttributes(attrMatch.String(match))
		}
		span.End()
	}()
	return w.verdictIn(ctx, key, alert, candidates, names, rows, span)
}

func (w *Writer) verdictIn(ctx context.Context, key string, alert Alert, candidates []map[string]any, names []string,
	rows func() ([]compare.Row, error), span trace.Span) (string, string, error) {
	r, err := rows()
	if err != nil {
		return "", "", err
	}
	incidents := strings.Join(names, "\n") + "\n" + compare.Incidents(candidates)
	w.mu.Lock()
	last, ok := w.verdicts[key]
	w.mu.Unlock()
	cached := ok && last.incidents == incidents && seen(last.records, r)
	span.SetAttributes(attrCached.Bool(cached))
	if cached {
		return last.match, "no new record since the last verdict: " + last.reason, nil
	}
	match, reason, err := w.compare(ctx, compare.Prompt(alert.Alert, candidates, r), names)
	if err != nil {
		return "", "", err
	}
	records := make(map[string]bool, len(r))
	for _, row := range r {
		records[row.Record] = true
	}
	w.mu.Lock()
	w.verdicts[key] = verdict{incidents: incidents, records: records, match: match, reason: reason}
	w.mu.Unlock()
	return match, reason, nil
}

// seen says every row's record is in records.
func seen(records map[string]bool, rows []compare.Row) bool {
	for _, r := range rows {
		if !records[r.Record] {
			return false
		}
	}
	return true
}

// patchStatus merges status into the Incident's status, conditioned on the resourceVersion it was
// read at: the controller writes the same status, so a 409 means re-read and retry.
func (w *Writer) patchStatus(ctx context.Context, ns string, incident map[string]any, st map[string]any) error {
	m := metadata(incident)
	return w.kube.PatchStatus(ctx, ns, str(m, "name"), map[string]any{
		"metadata": map[string]any{"resourceVersion": m["resourceVersion"]}, "status": st})
}

// covered logs the incident that covers a firing at at, and records the cover on it (see
// recordCover).
func (w *Writer) covered(ctx context.Context, ns string, incident map[string]any, at time.Time) {
	shown, ok := state(incident)
	if !ok || shown == "" {
		shown = "new"
	}
	logging.WithContext(w.log, ctx).Info(fmt.Sprintf("[incident] %s/%s: covers the firing (%s)", ns, str(metadata(incident), "name"), shown))
	w.recordCover(ctx, ns, incident, at)
}

// recordCover stamps AnnotationLastCoveredAt with at on an open incident that covered a firing, so
// an incident whose problem keeps firing stays fresh (see stale). It is a merge patch of that one
// annotation, so it never touches status, and it is skipped while the incident's last activity is
// younger than CoverWindow/coverStampsPerWindow: a failed or skipped stamp costs at most that much
// freshness. A failure is logged; the firing stays covered.
func (w *Writer) recordCover(ctx context.Context, ns string, incident map[string]any, at time.Time) {
	if w.cfg.CoverWindow <= 0 || ended(incident) {
		return
	}
	if since, ok := lastActive(incident); ok && at.Sub(since) < w.cfg.CoverWindow/coverStampsPerWindow {
		return
	}
	name := str(metadata(incident), "name")
	if err := w.kube.Patch(ctx, ns, name, map[string]any{"metadata": map[string]any{
		"annotations": map[string]any{AnnotationLastCoveredAt: at.UTC().Format(time.RFC3339)}}}); err != nil {
		logging.WithContext(w.log, ctx).Info(fmt.Sprintf("[incident] %s/%s: cover time not recorded (%s)", ns, name, err))
	}
}

// openOrCover is one firing: an incident pick chooses covers it, or it creates one. It returns
// the new Incident, or nil when an existing one covers the firing, and how many stale open
// incidents the firing was not compared with.
func (w *Writer) openOrCover(ctx context.Context, ns string, alert Alert, prompt string, at time.Time,
	grace time.Duration, rows func() ([]compare.Row, error)) (map[string]any, int, error) {
	now := pyfmt.Isoformat(at)
	items, err := w.alertIncidents(ctx, ns, alert.Name)
	if err != nil {
		return nil, 0, err
	}
	target, stale, err := w.pick(ctx, alert, ns, items, at, grace, rows)
	if err != nil {
		return nil, len(stale), err
	}
	if target != nil {
		w.covered(ctx, ns, target, at)
		return nil, len(stale), nil
	}
	name := IncidentName(alert.Name, at)
	labels := map[string]any{LabelAlert: alert.Name}
	if alert.HumanAnalysis {
		labels[LabelAnalysis] = AnalysisHuman
	}
	meta := map[string]any{"name": name, "namespace": ns, "labels": labels}
	ann := map[string]any{}
	if len(stale) > 0 {
		ann[AnnotationSupersedesStale] = strings.Join(stale[:min(len(stale), maxSupersedes)], ",")
	}
	var failed []map[string]any
	if !alert.HumanAnalysis {
		failed = supersedable(items)
	}
	if len(failed) > 0 {
		names := make([]string, len(failed))
		for i, f := range failed {
			names[i] = str(metadata(f), "name")
		}
		ann[AnnotationSupersedesFailed] = strings.Join(names, ",")
	}
	// incident.fire's context: incident-provider's first act on the new Incident is its child.
	for k, v := range traceContext(ctx) {
		ann[k] = v
	}
	if len(ann) > 0 {
		meta["annotations"] = ann
	}
	created, err := w.kube.Create(ctx, map[string]any{
		"apiVersion": IncidentGVK.GroupVersion().String(), "kind": IncidentGVK.Kind,
		"metadata": meta,
		"spec": map[string]any{"alertRef": map[string]any{"name": alert.Name, "namespace": ns}, "trigger": "alert",
			"prompt": prompt, "triggeredAt": now}})
	if err != nil {
		if code(err) != 409 {
			return nil, len(stale), err
		}
		// A concurrent firing created it this second.
		logging.WithContext(w.log, ctx).Info(fmt.Sprintf("[incident] %s/%s: covers the firing (created concurrently)", ns, name))
		return nil, len(stale), nil
	}
	// Unconditioned: the controller may already have touched the new object, and no other writer
	// sets these fields yet.
	first := map[string]any{"state": "Analyzing"}
	if alert.HumanAnalysis {
		first = map[string]any{"state": "Open", "error": HumanOnly, "completedAt": now}
	}
	if err := w.kube.PatchStatus(ctx, ns, name, map[string]any{"status": first}); err != nil {
		return nil, len(stale), err
	}
	logging.WithContext(w.log, ctx).Info(fmt.Sprintf("[incident] %s/%s: opened", ns, name))
	w.supersede(ctx, ns, name, failed)
	return created, len(stale), nil
}

// finish writes the analysis. With open, an incident still Analyzing becomes Open; in any other
// state (a human closed it, or applied a fix, while it was analyzing) the analysis is written
// without a state, since Closed is final and the controller owns the rest. Without open no state
// is written at all: the incident is being closed (see giveUp).
//
// With open, a fresh trace context from ctx's span (incident.analyze) is stamped on the Incident
// first, in a metadata patch of its own (stampTrace): the status write that follows is what makes
// incident-provider act, so the act step of its acting pass is then a child of this analysis.
func (w *Writer) finish(ctx context.Context, ns, name string, st map[string]any, open bool) error {
	if open {
		w.stampTrace(ctx, ns, name)
	}
	for range writeAttempts {
		incident, err := w.kube.Get(ctx, ns, name)
		if err != nil {
			return err
		}
		body := make(map[string]any, len(st)+1)
		for k, v := range st {
			body[k] = v
		}
		if open && analyzingOrUnset(incident) && !closedInSpec(incident) {
			body["state"] = "Open"
		}
		if err := w.patchStatus(ctx, ns, incident, body); err != nil {
			if code(err) != 409 {
				return err
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("incident %s/%s: no status write succeeded in %d attempts", ns, name, writeAttempts)
}

// giveUp ends an analysis that gave no scripts that can run: it closes the incident, with
// spec.closed and AnnotationClosedReason ClosedAnalysisFailed in one patch, then writes the
// analysis (status.error says why it failed) without a state; the incident controller moves it to
// Closed. Open, it would have nothing to check and nobody would move it on. An incident a human
// closed or moved on meanwhile is left to them, with the analysis written. It reports whether it
// closed the incident. A refused close falls back to Open with the error, as before failed analyses
// were closed: the next incident of the alert supersedes and closes it.
//
// An automatic reanalysis (status.reanalysis.cause ApplyActionStale, which incident-provider filed
// on an Open incident) is never closed, whatever its failure: an automatic action never ends an
// open incident. It goes back to Open with the error and status.reanalysis as written, so a person
// can ask for a reanalysis again. transient picks the error: the analysis never got an answer (the
// agent call kept failing through its retries, or a restart interrupted it for good), else the
// answer had no scripts that can run (see automaticFailure). A person's reanalysis, and a first
// analysis, are closed as above.
func (w *Writer) giveUp(ctx context.Context, ns, name string, st map[string]any, transient bool) (bool, error) {
	incident, err := w.kube.Get(ctx, ns, name)
	if err != nil {
		return false, err
	}
	if !analyzingOrUnset(incident) || closedInSpec(incident) {
		return false, w.finish(ctx, ns, name, st, false)
	}
	if automaticReanalysis(incident) {
		st["error"] = automaticFailure(pyfmt.Str(st["error"]), transient)
		logging.WithContext(w.log, ctx).Info(fmt.Sprintf(
			"[incident] %s/%s: the automatic reanalysis failed; back to Open with the error, not closed", ns, name))
		return false, w.finish(ctx, ns, name, st, true)
	}
	if err := w.kube.Patch(ctx, ns, name, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{AnnotationClosedReason: ClosedAnalysisFailed}},
		"spec":     map[string]any{"closed": true}}); err != nil {
		logging.WithContext(w.log, ctx).Info(fmt.Sprintf("[err] incident %s/%s: not closed (%s); opened with the error", ns, name, err))
		return false, w.finish(ctx, ns, name, st, true)
	}
	return true, w.finish(ctx, ns, name, st, false)
}

// RecoverInterrupted handles every incident a restart left Analyzing (or before its first status
// write), before any firing can open one. An Analyzing incident takes every firing of its alert that
// no analyzed incident covers and is never checked, so none may stay so:
//   - one whose RCA has not run again yet runs it again, off the caller, on a thread of its own
//     (<name>#recovery-<n>), with its spec.prompt (a reanalysis' feedback too), counted in
//     AnnotationRecoveries;
//   - one it already ran again maxRecoveries times, or without a prompt, is closed as a failed
//     analysis with Interrupted in error (see giveUp);
//   - a person's incident gets the first write it missed: Open, with HumanOnly.
//
// An incident closed in spec is left to the incident controller.
func (w *Writer) RecoverInterrupted(ctx context.Context) {
	ns := w.cfg.Namespace
	items, err := w.kube.List(ctx, ns, LabelAlert)
	if err != nil { // no Incident CRD yet, or no apiserver: nothing to recover
		w.log.Info(fmt.Sprintf("[incident] recovery skipped (%s)", err))
		return
	}
	for _, incident := range items {
		if !analyzingOrUnset(incident) || closedInSpec(incident) {
			continue
		}
		name := str(metadata(incident), "name")
		if humanIncident(incident) {
			if err := w.patchStatus(ctx, ns, incident, map[string]any{"state": "Open", "error": HumanOnly,
				"completedAt": pyfmt.Isoformat(w.now())}); err != nil {
				w.log.Info(fmt.Sprintf("[incident] %s/%s: recovery skipped (%s)", ns, name, err))
			}
			continue
		}
		w.recoverAnalysis(ctx, ns, incident)
	}
}

// automaticReanalysis says the incident's current analysis answers a reanalysis incident-provider
// filed itself (status.reanalysis.cause ApplyActionStale), not a person.
func automaticReanalysis(incident map[string]any) bool {
	re, ok := status(incident)["reanalysis"].(map[string]any)
	if !ok {
		return false
	}
	cause, _ := reanalysisCause(re)
	return cause == CauseApplyActionStale
}

// automaticFailure is the status.error of an automatic reanalysis that failed: why, after
// AutomaticReanalysisFailed when it got no answer (transient), else after AutomaticReanalysisNoFix.
func automaticFailure(why string, transient bool) string {
	if !transient {
		return AutomaticReanalysisNoFix + why
	}
	for _, p := range []string{"The analysis failed: ", "The analysis was interrupted: "} {
		why = strings.TrimPrefix(why, p)
	}
	return AutomaticReanalysisFailed + why
}

func (w *Writer) recoverAnalysis(ctx context.Context, ns string, listed map[string]any) {
	// Listed Incidents carry no prompt: read this one whole, which is also its latest version.
	name := str(metadata(listed), "name")
	incident, err := w.kube.Get(ctx, ns, name)
	if err != nil {
		if code(err) != 404 {
			w.log.Info(fmt.Sprintf("[incident] %s/%s: recovery skipped (%s)", ns, name, err))
		}
		return
	}
	if !analyzingOrUnset(incident) || closedInSpec(incident) {
		return // moved on since it was listed
	}
	m := metadata(incident)
	n, _ := strconv.Atoi(pyfmt.Str(annotations(incident)[AnnotationRecoveries]))
	prompt := str(spec(incident), "prompt")
	if n >= maxRecoveries || prompt == "" {
		closed, err := w.giveUp(ctx, ns, name, map[string]any{"error": Interrupted, "completedAt": pyfmt.Isoformat(w.now())}, true)
		switch {
		case err != nil:
			w.log.Info(fmt.Sprintf("[incident] %s/%s: recovery skipped (%s)", ns, name, err))
		case closed:
			w.log.Info(fmt.Sprintf("[incident] %s/%s: interrupted after %d recoveries, closed as a failed analysis", ns, name, n))
		}
		return
	}
	// Conditioned on the resourceVersion read: another writer that moved it on owns it now.
	if err := w.kube.Patch(ctx, ns, name, map[string]any{"metadata": map[string]any{"resourceVersion": m["resourceVersion"],
		"annotations": map[string]any{AnnotationRecoveries: strconv.Itoa(n + 1)}}}); err != nil {
		w.log.Info(fmt.Sprintf("[incident] %s/%s: recovery skipped (%s)", ns, name, err))
		return
	}
	if _, ok := state(incident); !ok {
		if err := w.kube.PatchStatus(ctx, ns, name, map[string]any{"status": map[string]any{"state": "Analyzing"}}); err != nil {
			w.log.Info(fmt.Sprintf("[incident] %s/%s: recovery skipped (%s)", ns, name, err))
			return
		}
	}
	if req, ok := status(incident)["reanalysis"].(map[string]any); ok {
		// The reanalysis cleared the previous analysis; its feedback is what is left of it.
		cause, _ := reanalysisCause(req)
		prompt = ReanalysisPrompt(prompt, map[string]any{}, str(req, "feedback"), cause)
	}
	key := fmt.Sprintf("%s#recovery-%d", name, n+1)
	w.log.Info(fmt.Sprintf("[incident] %s/%s: interrupted, analyzing again (recovery %d of %d)", ns, name, n+1, maxRecoveries))
	w.spawn(func() { w.RunAnalysis(ctx, ns, name, prompt, key) })
}

// Fire is one evaluation of a firing Alert, from its reconcile (about every 60 s). The firing
// counts on the incident pick chooses, or opens a new one whose RCA then runs in this call. The
// alert's name keys the incidents and their label, the home namespace holds them, spec.interval is
// the Resolved grace window and the lookback of its records. One evaluation per alert runs at a
// time: while one is still comparing, the alert's next firing is skipped.
//
// An alert outside the home namespace is refused before anything is read, compared or written:
// the RCA agent reads the whole platform, so its analysis of a tenant's alert could quote other
// tenants' data. The Alert controller never hands one over; this is the writer's own guard.
//
// Its span is incident.fire, with incident.stale_skipped the stale open incidents the firing was
// not compared with (see stale). The Alert controller passes a context detached from its reconcile
// (tracing.Detach), so the span is a new trace linked to provider-runtime's observe of that
// reconcile. The Incident it creates carries its trace context (see stampTrace).
func (w *Writer) Fire(ctx context.Context, alert Alert, records Records) {
	ctx, span := tracing.Start(ctx, spanFire, trace.WithAttributes(alertAttrs(alert.Namespace, alert.Name)...))
	defer span.End()
	// Bound to incident.fire, not to the reconcile that handed the firing over: its trace is another.
	log := logging.WithContext(w.log, ctx)
	if pyfmt.Len(alert.Name) > 63 {
		log.Info(fmt.Sprintf("[incident] alert %s: a name over 63 characters cannot label an Incident; skipping", pyfmt.Repr(alert.Name)))
		return
	}
	ns := alert.Namespace
	if ns == "" {
		ns = w.cfg.Namespace
	}
	if ns != w.cfg.Namespace {
		log.Info(fmt.Sprintf("[incident] alert %s/%s: outside %s, no incident and no RCA", ns, alert.Name, w.cfg.Namespace))
		return
	}
	key := ns + "/" + alert.Name
	w.mu.Lock()
	if w.firing[key] {
		w.mu.Unlock()
		log.Info(fmt.Sprintf("[incident] alert %s: the previous evaluation is still running; skipping this firing", key))
		return
	}
	w.firing[key] = true
	w.mu.Unlock()

	name := alert.DisplayName
	if name == "" {
		name = alert.Name
	}
	prompt := BuildPrompt(name, "ALERT", alert.Where, alert.Description, alert.Runbook, alert.Source)
	grace := IntervalSeconds(alert.Interval)
	rows := func() ([]compare.Row, error) {
		found, err := records(ctx, alert.Where, grace)
		if err != nil { // without the records there is no comparison
			return nil, &compare.NoVerdict{Reason: "the alert's records are unreadable: " + pyfmt.Cut(err.Error(), 200)}
		}
		// HyperDX's ALERT is from its last evaluation; by now the window can hold no row, and a
		// comparison over no records names no incident, which would open a duplicate.
		if len(found) == 0 {
			return nil, &compare.NoVerdict{Reason: "no record matches the alert now"}
		}
		return found, nil
	}
	created, staleSkipped, err := w.openOrCover(ctx, ns, alert, prompt, w.now(), time.Duration(grace)*time.Second, rows)
	w.mu.Lock()
	delete(w.firing, key)
	w.mu.Unlock()
	span.SetAttributes(attrStaleSkipped.Int(staleSkipped))
	switch {
	case compare.IsNoVerdict(err):
		log.Info(fmt.Sprintf("[incident] alert %s: no comparison verdict (%s); the next pass retries", key, err))
		return
	case err != nil: // a failed write loses this firing, not the next one
		log.Info(fmt.Sprintf("[err] alert %s: firing not recorded (%s)", key, err))
		span.SetStatus(codes.Error, err.Error())
		return
	}
	span.SetAttributes(attrOpened.Bool(created != nil))
	if created != nil && !alert.HumanAnalysis {
		name := str(metadata(created), "name")
		span.SetAttributes(attrIncident.String(name))
		w.RunAnalysis(ctx, ns, name, prompt, name)
	}
}

// RunAnalysis is the RCA of an Analyzing incident, then its one status write. key names the run's
// kagent threads: the incident's name for its first analysis.
//
//   - A transient failure (see analysis) is retried AnalysisRetries times, AnalysisRetryDelay after
//     the first and twice as long after each next one, each in a thread of its own.
//   - An answer without scripts that can run (no structured block, an unusable howToFix, or scripts
//     scriptcheck refuses) goes back to the agent correctionTurns times, in the same thread, with
//     what is wrong with it.
//   - An analysis with scripts is written in state Open, with error cleared. One without is closed
//     as a failed analysis, with error saying why (see giveUp): never left Open with nothing to run.
//
// Its span is incident.analyze, with a client span per A2A call; neither carries the prompt or
// the answer.
func (w *Writer) RunAnalysis(ctx context.Context, ns, name, prompt, key string) {
	ctx, span := tracing.Start(ctx, spanAnalyze, trace.WithAttributes(
		attrIncidentNamespace.String(ns), attrIncident.String(name)))
	defer span.End()
	// Bound to incident.analyze, whichever span started the analysis.
	log := logging.WithContext(w.log, ctx)
	var r result
	for attempt := 0; ; attempt++ {
		span.SetAttributes(attrAttempts.Int(attempt + 1))
		id := key
		if attempt > 0 {
			id = fmt.Sprintf("%s#retry-%d", key, attempt)
		}
		r = w.analysis(ctx, ns, name, prompt, contextID(id))
		if r.transient == "" || attempt >= w.cfg.AnalysisRetries {
			break
		}
		delay := w.cfg.AnalysisRetryDelay << attempt
		log.Info(fmt.Sprintf("[a2a] %s/%s: %s; retry %d of %d in %s", ns, name, r.transient, attempt+1, w.cfg.AnalysisRetries, delay))
		if !w.sleep(ctx, delay) {
			break
		}
	}
	if r.transient != "" {
		span.SetStatus(codes.Error, r.transient) // e.g. "the call failed"; the A2A client span has the error
	}
	turns := 0
	for ; turns < correctionTurns && r.transient == "" && len(r.correct) > 0; turns++ {
		r = w.correction(ctx, ns, name, r)
	}
	span.SetAttributes(attrCorrections.Int(turns))
	r.st["completedAt"] = pyfmt.Isoformat(w.now())
	if r.st["howToFix"] == nil {
		closed, err := w.giveUp(ctx, ns, name, r.st, r.transient != "")
		if err != nil { // RecoverInterrupted handles it after a restart
			log.Info(fmt.Sprintf("[err] incident %s/%s: analysis not written (%s)", ns, name, err))
			span.SetStatus(codes.Error, err.Error())
			return
		}
		outcome := "written (howToFix=no)"
		if closed {
			outcome = "failed, closed"
		}
		log.Info(fmt.Sprintf("[incident] %s/%s: analysis %s: %s", ns, name, outcome, pyfmt.Cut(pyfmt.Str(r.st["error"]), 300)))
		return
	}
	if err := w.finish(ctx, ns, name, r.st, true); err != nil { // RecoverInterrupted handles it after a restart
		log.Info(fmt.Sprintf("[err] incident %s/%s: analysis not written (%s)", ns, name, err))
		span.SetStatus(codes.Error, err.Error())
		return
	}
	log.Info(fmt.Sprintf("[ok] incident %s/%s: analysis written (howToFix=yes)", ns, name))
}

// llmError starts the answer kagent streams when its model call failed.
const llmError = "LLM error:"

// result is one RCA answer: the status fields it writes; why it failed transiently, "" when it did
// not; and what a correction turn would fix, nil when it has scripts or a correction cannot help.
// contextID and ledger are its thread and its tool results. coerced says which rootCause values
// the parser fitted to the Incident CRD (logged, never written).
type result struct {
	st        map[string]any
	transient string
	correct   []string
	contextID string
	ledger    []report.ToolResult
	coerced   []string
}

// logCoerced logs, at info, the rootCause values an answer had coerced to fit the Incident CRD.
func logCoerced(log logging.Logger, ns, name string, r result) {
	if len(r.coerced) > 0 {
		log.Info(pyfmt.Cut(fmt.Sprintf("[a2a] %s/%s: rootCause fitted to the Incident CRD: %s", ns, name,
			strings.Join(r.coerced, "; ")), 2000))
	}
}

// analysis is one RCA run. A failed call, an empty answer and an answer that is only kagent's
// model error are transient: the gateway or the model provider failed, and the same prompt may
// succeed later. An answer without usable scripts is not: the agent answered (see evaluate).
func (w *Writer) analysis(ctx context.Context, ns, name, prompt, contextID string) result {
	w.slots <- struct{}{}
	raw, ledger, err := w.analyze(ctx, prompt, contextID)
	<-w.slots
	if err != nil { // a failed RCA still writes why
		return result{st: map[string]any{"error": "The analysis failed: " + pyfmt.Cut(err.Error(), 500)},
			transient: "the call failed", contextID: contextID}
	}
	log := logging.WithContext(w.log, ctx)
	log.Info(fmt.Sprintf("[a2a] %s/%s: %d chars, %d tool results", ns, name, pyfmt.Len(raw), len(ledger)))
	r := evaluate(raw, ledger)
	logCoerced(log, ns, name, r)
	r.contextID, r.ledger = contextID, ledger
	return r
}

// correction sends first's problems back to the agent in first's thread, and is the corrected
// answer when it has scripts. Otherwise first stands, its error saying the correction failed too.
func (w *Writer) correction(ctx context.Context, ns, name string, first result) result {
	log := logging.WithContext(w.log, ctx)
	log.Info(fmt.Sprintf("[a2a] %s/%s: no scripts that can run (%d problems); asking for a correction", ns, name, len(first.correct)))
	w.slots <- struct{}{}
	raw, ledger, err := w.analyze(ctx, CorrectionPrompt(first.correct), first.contextID)
	<-w.slots
	why := ""
	if err != nil {
		why = "its call failed: " + pyfmt.Cut(err.Error(), 300)
	} else {
		log.Info(fmt.Sprintf("[a2a] %s/%s: correction, %d chars, %d tool results", ns, name, pyfmt.Len(raw), len(ledger)))
		// The corrected block describes the same investigation: the first turn's reads count too.
		r := evaluate(raw, append(append([]report.ToolResult{}, first.ledger...), ledger...))
		logCoerced(log, ns, name, r)
		if r.st["howToFix"] != nil {
			r.contextID, r.ledger = first.contextID, append(first.ledger, ledger...)
			return r
		}
		why = pyfmt.Str(r.st["error"])
	}
	first.st["error"] = fmt.Sprintf("%s It was sent back once with these problems, and the correction gave none either: %s",
		pyfmt.Str(first.st["error"]), why)
	first.correct = nil
	return first
}

// evaluate is an RCA answer as status fields. It has scripts when report.Parse keeps a howToFix and
// scriptcheck finds no problem in its precondition, apply and verify; a refused rollback is left out
// alone, as the parser leaves out an unusable one. Otherwise error says why there are none, and
// correct holds what a correction turn would fix: not for an empty answer or a model error, which
// are transient, nor for a structured block without any howToFix, which the agent chose.
func evaluate(raw string, ledger []report.ToolResult) result {
	prose, v2, fixProblems, coerced := report.ParseFix(raw, ledger)
	r := parsed(prose, v2, fixProblems)
	r.coerced = coerced
	return r
}

// parsed is evaluate of an answer report.ParseFix has parsed.
func parsed(prose string, v2 map[string]any, fixProblems []string) result {
	st := map[string]any{}
	if pyfmt.Strip(prose) != "" {
		st["report"] = prose
	}
	for _, k := range report.StatusKeys {
		if v, ok := v2[k]; ok {
			st[k] = v
		}
	}
	fix, hasFix := v2["howToFix"].(map[string]any)
	switch {
	case hasFix:
		return refuse(st, fix, scriptcheck.Check(fix))
	case pyfmt.Strip(prose) == "" && len(v2) == 0:
		st["error"] = "The analysis returned no output."
		return result{st: st, transient: "the answer was empty"}
	case len(v2) == 0:
		st["error"] = "The analysis returned no structured block, so the incident has no scripts to check or fix it."
		if strings.HasPrefix(pyfmt.Strip(prose), llmError) {
			return result{st: st, transient: "the model call failed"}
		}
		return result{st: st, correct: []string{"the answer has no ```json block, so it gives no scripts."}}
	}
	st["error"] = "The analysis returned no usable howToFix, so the incident has no scripts to check or fix it."
	if len(fixProblems) == 1 && fixProblems[0] == report.FixNone {
		return result{st: st}
	}
	return result{st: st, correct: []string{"howToFix is unusable: " + strings.Join(fixProblems, "; ") + "."}}
}

// refuse applies scriptcheck's problems to an analysis with scripts: none, and it stands with
// error cleared; a rollback's alone, and the rollback is left out with a missingContext line; any
// other, and howToFix is dropped, error and missingContext say why, and every problem is one to
// correct. A script known to be broken is never written.
func refuse(st, fix map[string]any, problems []scriptcheck.Problem) result {
	var lines, others []string
	for _, p := range problems {
		lines = append(lines, p.String())
		if p.Script != "rollback" {
			others = append(others, p.String())
		}
	}
	switch {
	case len(problems) == 0:
		st["error"] = nil
		return result{st: st}
	case len(others) == 0:
		kept := make(map[string]any, len(fix))
		for k, v := range fix {
			if k != "rollback" {
				kept[k] = v
			}
		}
		st["howToFix"] = kept
		st["missingContext"] = withGap(st["missingContext"], fmt.Sprintf(
			"No usable rollback (%s): undoing apply is left to whoever runs it.", strings.Join(lines, "; ")))
		st["error"] = nil
		return result{st: st}
	}
	delete(st, "howToFix")
	why := strings.Join(others, "; ")
	st["error"] = pyfmt.Cut("The analysis returned scripts that cannot run ("+why+"), so the incident has no scripts to check or fix it.", 2000)
	st["missingContext"] = withGap(st["missingContext"], pyfmt.Cut(
		"No usable howToFix ("+why+"): the incident has no scripts to check or fix it.", 2000))
	return result{st: st, correct: lines}
}

// withGap is missingContext with line appended, at most 64 lines as the parser keeps.
func withGap(missing any, line string) []string {
	var out []string
	switch l := missing.(type) {
	case []string:
		out = append(out, l...)
	case []any:
		for _, x := range l {
			out = append(out, pyfmt.Str(x))
		}
	}
	if len(out) > 63 {
		out = out[:63]
	}
	return append(out, line)
}
