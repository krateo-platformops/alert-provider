// Package incident turns a firing alert into Incidents, each with a root-cause analysis.
//
// The Alert reconcile mirrors every Alert's HyperDX state about every 60 s and calls Fire for each
// one that is ALERT. A firing an incident covers (see pick) writes at most its cover time (see
// recordCover); any other opens a new incident: it creates the Incident in state Analyzing, runs
// the incident-agent RCA over A2A, and writes the analysis with its howToFix scripts in state Open.
// An alert therefore has any number of incidents, one per problem. From Open on, the incident
// controller runs the scripts and moves the Incident.
package incident

import (
	"context"
	"fmt"
	"sort"
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
	// coverStampsPerWindow bounds the cover-time writes: an incident's cover time is rewritten only
	// once it is older than CoverWindow/coverStampsPerWindow, so an alert that fires every pass
	// patches its incident about 24 times per window, not every pass.
	coverStampsPerWindow = 24
	// writeAttempts is how often a conditioned status write is retried on a 409.
	writeAttempts = 5
)

// Interrupted is the error of an incident a restart left without its analysis.
const Interrupted = "The analysis was interrupted: the alert-provider restarted before it finished."

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
	// firings, from the failure (status.completedAt). Past it, a firing nothing else covers opens a
	// new incident and RCA.
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

func ended(obj map[string]any) bool {
	s, _ := state(obj)
	return s == "Resolved" || s == "Closed"
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
	if analyzingOrUnset(incident) {
		return true
	}
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
//     less than FailedAnalysisHold ago. There is nothing to compare with, and a new incident would
//     rerun the analysis. Past the hold, a failed incident takes no more firings, so a new
//     incident gets a fresh RCA;
//  3. the alert's latest incident when it is Resolved less than grace (one spec.interval) ago: a
//     `where` alert keeps counting rows from before the fix for its lookback window, and those
//     belong to the incident the fix resolved. A Closed incident never takes a firing.
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
	if latest := newest(items); latest != nil && resolvedWithin(latest, at, grace) {
		return latest, stale, nil
	}
	return nil, stale, noVerdict
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
	if len(stale) > 0 {
		meta["annotations"] = map[string]any{AnnotationSupersedesStale: strings.Join(stale[:min(len(stale), maxSupersedes)], ",")}
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
	return created, len(stale), nil
}

// finish writes the analysis. An incident still Analyzing becomes Open; in any other state (a
// human closed it, or applied a fix, while it was analyzing) the analysis is written without a
// state, since Closed is final and the controller owns the rest.
func (w *Writer) finish(ctx context.Context, ns, name string, st map[string]any) error {
	for range writeAttempts {
		incident, err := w.kube.Get(ctx, ns, name)
		if err != nil {
			return err
		}
		body := make(map[string]any, len(st)+1)
		for k, v := range st {
			body[k] = v
		}
		if analyzingOrUnset(incident) {
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

// RecoverInterrupted opens every incident a restart left Analyzing (or before its first status
// write), with the reason in error. An Analyzing incident takes every firing of its alert that no
// analyzed incident covers, and is never checked. Open with error, it says why it has no scripts
// and can be closed, and the next firing then opens a fresh one. An analysis still running in
// another replica overwrites error when it finishes.
func (w *Writer) RecoverInterrupted(ctx context.Context) {
	ns := w.cfg.Namespace
	items, err := w.kube.List(ctx, ns, LabelAlert)
	if err != nil { // no Incident CRD yet, or no apiserver: nothing to recover
		w.log.Info(fmt.Sprintf("[incident] recovery skipped (%s)", err))
		return
	}
	for _, incident := range items {
		if !analyzingOrUnset(incident) {
			continue
		}
		name := str(metadata(incident), "name")
		if err := w.patchStatus(ctx, ns, incident, map[string]any{"state": "Open", "error": Interrupted,
			"completedAt": pyfmt.Isoformat(w.now())}); err != nil { // a 409 means another writer moved it
			w.log.Info(fmt.Sprintf("[incident] %s/%s: recovery skipped (%s)", ns, name, err))
			continue
		}
		w.log.Info(fmt.Sprintf("[incident] %s/%s: interrupted, opened", ns, name))
	}
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
// (tracing.Detach), so the span is a new trace linked to the reconcile's alert.observe.
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
	prompt := BuildPrompt(name, "ALERT", alert.Where, alert.Description, alert.Source)
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

// RunAnalysis is the RCA of an Analyzing incident, then its one status write: the analysis and
// state Open. key names the run's kagent threads: the incident's name for its first analysis.
// A transient failure is retried AnalysisRetries times, each in a thread of its own. error says why
// an incident has no howToFix: the call failed, the answer was empty or unstructured, or its
// scripts were unusable. It is cleared when howToFix is written.
//
// Its span is incident.analyze, with a client span per A2A call; neither carries the prompt or
// the answer.
func (w *Writer) RunAnalysis(ctx context.Context, ns, name, prompt, key string) {
	ctx, span := tracing.Start(ctx, spanAnalyze, trace.WithAttributes(
		attrIncidentNamespace.String(ns), attrIncident.String(name)))
	defer span.End()
	// Bound to incident.analyze, whichever span started the analysis.
	log := logging.WithContext(w.log, ctx)
	var st map[string]any
	var why string // the last attempt's transient failure, "" when it did not fail
	for attempt := 0; ; attempt++ {
		span.SetAttributes(attrAttempts.Int(attempt + 1))
		id := key
		if attempt > 0 {
			id = fmt.Sprintf("%s#retry-%d", key, attempt)
		}
		st, why = w.analysis(ctx, ns, name, prompt, contextID(id))
		if why == "" || attempt >= w.cfg.AnalysisRetries {
			break
		}
		delay := w.cfg.AnalysisRetryDelay << attempt
		log.Info(fmt.Sprintf("[a2a] %s/%s: %s; retry %d of %d in %s", ns, name, why, attempt+1, w.cfg.AnalysisRetries, delay))
		if !w.sleep(ctx, delay) {
			break
		}
	}
	if why != "" {
		span.SetStatus(codes.Error, why) // e.g. "the call failed"; the A2A client span has the error
	}
	st["completedAt"] = pyfmt.Isoformat(w.now())
	if err := w.finish(ctx, ns, name, st); err != nil { // RecoverInterrupted opens it after a restart
		log.Info(fmt.Sprintf("[err] incident %s/%s: analysis not written (%s)", ns, name, err))
		span.SetStatus(codes.Error, err.Error())
		return
	}
	fix := "no"
	if st["howToFix"] != nil {
		fix = "yes"
	}
	log.Info(fmt.Sprintf("[ok] incident %s/%s: analysis written (howToFix=%s)", ns, name, fix))
}

// llmError starts the answer kagent streams when its model call failed.
const llmError = "LLM error:"

// analysis is one RCA run: the status fields it writes, and why the run failed transiently, or ""
// when it did not. A failed call, an empty answer and an answer that is only kagent's model error
// are transient: the gateway or the model provider failed, and the same prompt may succeed later.
// An answer without usable scripts is not: the agent answered.
func (w *Writer) analysis(ctx context.Context, ns, name, prompt, contextID string) (map[string]any, string) {
	st := map[string]any{}
	w.slots <- struct{}{}
	raw, ledger, err := w.analyze(ctx, prompt, contextID)
	<-w.slots
	if err != nil { // a failed RCA still opens the incident
		st["error"] = "The analysis failed: " + pyfmt.Cut(err.Error(), 500)
		return st, "the call failed"
	}
	logging.WithContext(w.log, ctx).Info(fmt.Sprintf("[a2a] %s/%s: %d chars, %d tool results", ns, name, pyfmt.Len(raw), len(ledger)))
	prose, v2 := report.Parse(raw, ledger)
	if pyfmt.Strip(prose) != "" {
		st["report"] = prose
	}
	for _, k := range report.StatusKeys {
		if v, ok := v2[k]; ok {
			st[k] = v
		}
	}
	_, hasFix := v2["howToFix"]
	switch {
	case hasFix:
		st["error"] = nil
	case pyfmt.Strip(prose) == "" && len(v2) == 0:
		st["error"] = "The analysis returned no output."
		return st, "the answer was empty"
	case len(v2) == 0:
		st["error"] = "The analysis returned no structured block, so the incident has no scripts to check or fix it."
		if strings.HasPrefix(pyfmt.Strip(prose), llmError) {
			return st, "the model call failed"
		}
	default:
		st["error"] = "The analysis returned no usable howToFix, so the incident has no scripts to check or fix it."
	}
	return st, ""
}
