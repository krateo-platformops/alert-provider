// Package alert is the controller of Alert managed resources, in every namespace. The external
// resource of an Alert is its HyperDX alert and the single-tile dashboard the alert evaluates,
// both named after the Alert (see HyperDXName): its metadata.name in the home namespace,
// <namespace>/<name> in any other.
//
// Every poll interval each Alert is one pass: its spec is pushed to HyperDX, the live state
// (OK/ALERT/PENDING) is mirrored to its status, and a pass that finds it ALERT is one firing. A
// firing of a home-namespace Alert is handed to the incident writer off the reconcile, unless the
// Alert is paused (krateo.io/paused: "true"). An Alert in any other namespace (a tenant's) never
// reaches the writer: its state is mirrored and HyperDX notifies, with no Incident and no RCA,
// since the incident agent reads the whole platform and its analysis could quote other tenants'
// data. A finalizer removes the HyperDX alert and dashboard before the Alert is deleted.
package alert

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	prevent "github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/plumbing/kubeutil/eventrecorder"
	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/meta"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimiter"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
	"github.com/krateo-platformops/alert-provider/internal/compare"
	"github.com/krateo-platformops/alert-provider/internal/controllers/common/option"
	"github.com/krateo-platformops/alert-provider/internal/httpx"
	"github.com/krateo-platformops/alert-provider/internal/hyperdx"
	"github.com/krateo-platformops/alert-provider/internal/incident"
	"github.com/krateo-platformops/alert-provider/internal/pyfmt"
)

// Finalizer guards each Alert so its HyperDX alert and dashboard are removed before it is deleted.
const Finalizer = "observability.krateo.io/hyperdx-cleanup"

var errNotAlert = errors.New("managed resource is not an Alert")

// Options configure the Alert controller.
type Options struct {
	Controller option.ControllerOptions
	// Namespace is the provider's home namespace. Its Alerts keep their bare names in HyperDX and
	// alone open Incidents; the Alerts of every other namespace are reconciled and mirrored only.
	Namespace string
	// HyperDX is the HyperDX API the alerts are pushed to.
	HyperDX func() *hyperdx.Client
	// Webhook names the shared generic webhook, the HyperDX alerts' channel, and where it posts.
	WebhookName, WebhookTarget string
	// Writer writes the incidents of the firings, under FiringContext: a firing outlives its
	// reconcile, since a new incident's RCA takes minutes.
	Writer        *incident.Writer
	FiringContext context.Context
}

// Setup adds the Alert controller to the manager.
//
// One Alert reconciles at a time: a recreated webhook resets every Alert, and a HyperDX alert two
// Alerts claim is released by one of them, each of which assumes no other Alert is mid-push.
func Setup(mgr ctrl.Manager, o Options) error {
	if o.Namespace == "" {
		return errors.New("the Alert controller needs its home namespace")
	}
	name := reconciler.ControllerName(v1alpha1.AlertGroupKind)
	log := o.Controller.Logger.WithValues("controller", name)

	recorder, err := eventrecorder.Create(context.Background(), mgr.GetConfig(), name, nil)
	if err != nil {
		return fmt.Errorf("failed to create event recorder: %w", err)
	}

	f := &firings{home: o.Namespace, interval: o.Controller.PollInterval, fire: o.Writer.Fire, ctx: o.FiringContext,
		hdx: o.HyperDX, log: log, last: map[types.UID]time.Time{}, run: func(f func()) { go f() }}
	c := &connector{
		home:    o.Namespace,
		kube:    mgr.GetClient(),
		hdx:     o.HyperDX,
		webhook: &webhook{kube: mgr.GetClient(), name: o.WebhookName, target: o.WebhookTarget, ttl: o.Controller.PollInterval, log: log},
		firings: f,
		log:     log,
	}
	r := reconciler.NewReconciler(mgr,
		resource.ManagedKind(v1alpha1.AlertGroupVersionKind),
		reconciler.WithExternalConnecter(c),
		reconciler.WithFinalizer(resource.NewAPIFinalizer(mgr.GetClient(), Finalizer)),
		reconciler.WithPollInterval(o.Controller.PollInterval),
		reconciler.WithLogger(log),
		reconciler.WithRecorder(prevent.NewAPIRecorder(recorder)),
		// No warning Events: the seeded reconcile-error alert counts CannotObserveExternalResource
		// Events cluster-wide, so a HyperDX outage would make this controller fire on itself. The
		// error is in the Synced condition, status.phase and the logs.
		reconciler.WithThrottledRecorder(prevent.NewNopRecorder()),
		reconciler.WithTimeout(o.Controller.Timeout),
	)

	opts := o.Controller.ForControllerRuntime()
	opts.MaxConcurrentReconciles = 1
	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(opts).
		For(&v1alpha1.Alert{}, builder.WithPredicates(changed)).
		Complete(ratelimiter.New(name, r, o.Controller.GlobalRateLimiter))
}

// changed lets through what a person changes: the spec, the annotations and the deletion. The
// reconcile's own status writes would otherwise requeue it at once.
var changed = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		o, n := e.ObjectOld, e.ObjectNew
		if o.GetGeneration() != n.GetGeneration() || !o.GetDeletionTimestamp().Equal(n.GetDeletionTimestamp()) {
			return true
		}
		oa, na := o.(*v1alpha1.Alert), n.(*v1alpha1.Alert)
		if len(oa.Annotations) != len(na.Annotations) {
			return true
		}
		for k, v := range oa.Annotations {
			if na.Annotations[k] != v {
				return true
			}
		}
		return false
	},
}

type connector struct {
	home    string // the home namespace, see HyperDXName
	kube    client.Client
	hdx     func() *hyperdx.Client
	webhook *webhook
	firings *firings
	log     logging.Logger
}

// Connect opens a pass: a new HyperDX client, so the pass reads the dashboards afresh, and the
// HyperDX sources and shared webhook every alert uses.
func (c *connector) Connect(ctx context.Context, mg resource.Managed) (reconciler.ExternalClient, error) {
	a, ok := mg.(*v1alpha1.Alert)
	if !ok {
		return nil, errNotAlert
	}
	e := &external{home: c.home, kube: c.kube, hdx: c.hdx(), firings: c.firings, log: c.log.WithValues("alert", a.Namespace+"/"+a.Name)}
	if meta.WasDeleted(a) {
		return e, nil // removing the HyperDX objects needs neither
	}
	sources, webhookID, err := c.webhook.ensure(ctx, e.hdx, a)
	if err != nil {
		return nil, err
	}
	e.sources, e.webhookID = sources, webhookID
	return e, nil
}

// plan is what Observe found for Update to act on. A new external serves each reconcile.
type plan struct {
	// invalid is why the threshold is refused; nothing is pushed.
	invalid string
	// deleteShared is a HyperDX alert several Alerts claimed and none is named after: it is
	// deleted, so it stops evaluating one Alert's `where` for another.
	deleteShared string
	// create means no HyperDX alert is this Alert's yet.
	create bool
	taken  map[string]bool // tiles other alerts evaluate, never reused for a new one
	// alertID, dashboardID and tileID are the live alert's, which name the tile it evaluates.
	alertID, dashboardID, tileID string
	where                        bool     // the tile's where differs from spec.where
	drift                        []string // the alert fields that differ from the spec
}

func (p plan) String() string {
	var parts []string
	if p.deleteShared != "" {
		parts = append(parts, "delete shared HyperDX alert "+p.deleteShared)
	}
	if p.create {
		parts = append(parts, "create the HyperDX dashboard and alert")
	}
	if p.where {
		parts = append(parts, "push where")
	}
	if len(p.drift) > 0 {
		parts = append(parts, "push "+strings.Join(p.drift, ", "))
	}
	return strings.Join(parts, "; ")
}

type external struct {
	home      string
	kube      client.Client
	hdx       *hyperdx.Client
	firings   *firings
	log       logging.Logger
	sources   []map[string]any // HyperDX's, every kind
	webhookID string
	plan      plan
	// rows is the source a row Alert reads (its logs or traces source, see rowSource); nil for a
	// metric Alert. Its firings read their records from it.
	rows map[string]any
	// sel and tileSource are what the Alert's tile aggregates and over which source: the count of
	// its `where` rows (or a value read from them) over rows, or its metric over the metrics source.
	sel        hyperdx.Select
	tileSource map[string]any
}

// sourceKinds are the HyperDX source kinds spec.source names.
var sourceKinds = map[string]string{v1alpha1.SourceLogs: hyperdx.KindLog, v1alpha1.SourceTraces: hyperdx.KindTrace}

// rowSource is the HyperDX source of the Alert's rows: the one of the kind spec.source names. A
// source the install lacks is an error, not a refusal: the spec is valid, the install is not.
func (e *external) rowSource(a *v1alpha1.Alert) (map[string]any, error) {
	kind, ok := sourceKinds[a.RowSource()]
	if !ok { // the CRD's enum admits no other value
		return nil, fmt.Errorf("spec.source %q: not logs or traces", a.Spec.Source)
	}
	return hyperdx.OfKind(e.sources, kind)
}

// refuse is the observation of an Alert refused with why: nothing is pushed, and Update writes the
// refusal to the status, once.
func (e *external) refuse(a *v1alpha1.Alert, why string) reconciler.ExternalObservation {
	// Ready on every successful Observe (provider-runtime v1.8.0): a refused Alert is not
	// evaluating anything, and says why, until its spec is fixed.
	a.SetConditions(prv1.Unavailable().WithMessage(why))
	if a.Status.Phase == v1alpha1.PhaseInvalid && a.Status.Error == why {
		return reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}
	}
	e.plan.invalid = why
	return reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false, Diff: "refused: " + why}
}

// tile resolves what the Alert's tile aggregates. A row Alert reads the source spec.source names;
// a metric Alert reads its Metric and the metrics source. A missing Metric, or one its aggregation
// cannot apply to, is why it is refused.
func (e *external) tile(ctx context.Context, a *v1alpha1.Alert) (string, error) {
	if !a.IsMetric() {
		if a.Grouped() { // the CRD refuses it too
			return "groupBy splits a metric's series; it requires metricRef.", nil
		}
		rows, err := e.rowSource(a)
		if err != nil {
			return "", err
		}
		sel := hyperdx.CountSelect(a.Spec.Where)
		if a.Aggregated() {
			var why string
			if sel, why = ValueSelect(a.Spec); why != "" {
				return why, nil
			}
		}
		e.rows, e.sel, e.tileSource = rows, sel, rows
		return "", nil
	}
	ref, ns := a.Spec.MetricRef, a.Namespace
	var m v1alpha1.Metric
	if err := e.kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, &m); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("metricRef %s/%s: no such Metric.", ns, ref.Name), nil
		}
		return "", err
	}
	sel, why := MetricSelect(a.Spec, m.Spec)
	if why != "" {
		return why, nil
	}
	src, err := e.hdx.MetricSource(ctx)
	if err != nil {
		return "", err
	}
	e.sel, e.tileSource = sel, src
	return "", nil
}

// fields are the HyperDX alert fields of the Alert. An Alert outside the home namespace without a
// message gets one that does not promise the incident-agent triage it never gets.
func fields(home string, a *v1alpha1.Alert) hyperdx.AlertFields {
	spec := a.Spec
	f := hyperdx.AlertFields{Interval: spec.Interval, Threshold: threshold(spec),
		ThresholdType: spec.ThresholdType, Message: spec.Message,
		ConsecutiveWindows: a.Windows(), Grouped: a.Grouped()}
	if spec.ThresholdMax != "" {
		f.ThresholdMax = spec.ThresholdMax
	}
	if f.Message == "" && a.Namespace != home {
		f.Message = hyperdxName(home, a) + " threshold crossed."
	}
	if f.Interval == "" {
		f.Interval = "5m"
	}
	if f.ThresholdType == "" {
		f.ThresholdType = "above"
	}
	return f
}

func now() string { return pyfmt.Isoformat(time.Now()) }

// failed records a failed pass. A HyperDX HTTP error leaves the status as it was; any other error
// is phase Error.
func failed(a *v1alpha1.Alert, err error) error {
	if httpx.Code(err) == 0 {
		a.Status.Phase, a.Status.Error = v1alpha1.PhaseError, pyfmt.Cut(err.Error(), 300)
	}
	return err
}

// specDrift records a push that failed: the live alert does not match the Alert. The state is
// still mirrored, so a firing alert stays visible.
func (e *external) specDrift(a *v1alpha1.Alert, err error) error {
	a.Status.Phase, a.Status.Error = v1alpha1.PhaseSpecDrift, pyfmt.Cut(err.Error(), 300)
	e.log.Info(fmt.Sprintf("[reconciler] Alert %s: spec push failed, phase=SpecDrift (%s)", hyperdxName(e.home, a), err))
	return err
}

// Observe finds the Alert's HyperDX alert, mirrors its state to the status and plans the push.
// The HyperDX alert is found by the status id, else by its name (HyperDXName), so a status that lost its id, or
// a create cut short, adopts its own alert instead of creating a second one. Observe therefore
// never reports it missing: that would arm provider-runtime's create handshake, under which an
// interrupted create blocks the Alert until a person clears an annotation. A missing alert is
// created by Update.
//
// A pass that finds the alert ALERT fires it, here, since it is the one step every pass runs.
func (e *external) Observe(ctx context.Context, mg resource.Managed) (reconciler.ExternalObservation, error) {
	a, ok := mg.(*v1alpha1.Alert)
	if !ok {
		return reconciler.ExternalObservation{}, errNotAlert
	}
	if meta.WasDeleted(a) {
		a.SetConditions(prv1.Deleting())
		return reconciler.ExternalObservation{ResourceExists: a.Status.HyperdxAlertID != "" || a.Status.HyperdxDashboardID != ""}, nil
	}

	// A tautological threshold is refused, on an existing alert too: the comparison has one
	// possible outcome, so the alert cannot signal anything. The existing alert is not deleted:
	// refusing to keep pushing stops the damage.
	// It is written by Update: a status Observe sets is lost when provider-runtime then adds the
	// finalizer to a new Alert, which reloads the object.
	// Only a count is never negative, so the rule holds for counting alerts alone: an aggregated
	// value (a metric, or a value read from log rows) can be anything, and 'above 0' is a real
	// question.
	if !a.Aggregated() {
		if why := Tautology(threshold(a.Spec), a.Spec.ThresholdType); why != "" {
			return e.refuse(a, why), nil
		}
	}
	why, err := e.tile(ctx, a)
	if err != nil {
		return reconciler.ExternalObservation{}, failed(a, err)
	}
	if why != "" {
		return e.refuse(a, why), nil
	}

	live, err := e.hdx.ListAlerts(ctx)
	if err != nil {
		return reconciler.ExternalObservation{}, failed(a, err)
	}
	byID := map[string]map[string]any{}
	for _, l := range live {
		byID[hyperdx.ID(l)] = l
	}
	if err := e.release(ctx, a, byID); err != nil {
		return reconciler.ExternalObservation{}, err
	}

	var alert map[string]any
	if id := a.Status.HyperdxAlertID; id != "" {
		alert = byID[id]
	}
	if alert == nil {
		for _, l := range live {
			if l["name"] == hyperdxName(e.home, a) && hyperdx.ID(l) != e.plan.deleteShared {
				alert = l
				break
			}
		}
	}
	if alert == nil {
		e.plan.create = true
		e.plan.taken = map[string]bool{}
		for _, l := range live {
			if t, ok := l["tileId"].(string); ok && hyperdx.ID(l) != e.plan.deleteShared {
				e.plan.taken[t] = true
			}
		}
		// Ready on every successful Observe (provider-runtime v1.8.0): Update creates it next.
		a.SetConditions(prv1.Creating())
		return reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false, Diff: e.plan.String()}, nil
	}

	st := "OK"
	if v, ok := alert["state"]; ok {
		st = ""
		if v != nil {
			st = pyfmt.Str(v)
		}
	}
	// Both ids come from the live alert: it is authoritative about which tile it reads, and a
	// status dashboard id paired with the live tile id describes a tile that is not on that
	// dashboard. The status is the fallback for an alert that carries no dashboard.
	e.plan.alertID = hyperdx.ID(alert)
	e.plan.dashboardID, _ = alert["dashboardId"].(string)
	e.plan.tileID, _ = alert["tileId"].(string)
	if e.plan.dashboardID == "" {
		e.plan.dashboardID = a.Status.HyperdxDashboardID
	}
	a.Status.OkSince = OkSince(a.Status, st, now())
	a.Status.HyperdxAlertID = e.plan.alertID
	if d, _ := alert["dashboardId"].(string); d != "" {
		a.Status.HyperdxDashboardID = d
	}
	a.Status.State = st
	a.SetConditions(prv1.Available())

	// The tile first: it decides what is evaluated, so pushing a threshold against a stale query
	// would briefly evaluate the new bound over the old one. A log alert compares its `where`; a
	// metric alert its whole select and source, since a new aggregation or metric is a new query.
	if e.plan.dashboardID != "" {
		if a.Aggregated() {
			live, liveSrc, found, err := e.hdx.TileSelect(ctx, e.plan.dashboardID)
			if err != nil {
				e.firings.start(a, e.rows, st)
				// Ready=Available above mirrors HyperDX's live state, read in this call; only the
				// tile read failed, so it stands (provider-runtime v1.8.0).
				return reconciler.ExternalObservation{}, reconciler.ReadyAssessed(e.specDrift(a, err))
			}
			e.plan.where = found && hyperdx.SelectDrift(live, liveSrc, e.tileSource, e.sel)
		} else {
			liveWhere, found, err := e.hdx.TileWhere(ctx, e.plan.dashboardID)
			if err != nil {
				e.firings.start(a, e.rows, st)
				return reconciler.ExternalObservation{}, reconciler.ReadyAssessed(e.specDrift(a, err))
			}
			e.plan.where = found && liveWhere != a.Spec.Where
			// A tile that aggregated (an Alert switched back to counting), or that reads the other
			// source (an Alert switched between logs and traces), can hold the same `where`: it is
			// rewritten whole.
			if found && !e.plan.where {
				live, liveSrc, _, err := e.hdx.TileSelect(ctx, e.plan.dashboardID)
				// A tile with no aggFn is a count: only an explicit other function is a switch,
				// so a standard alert's tile is never rewritten on account of this check.
				if fn, ok := live.First()["aggFn"]; err == nil && ok && fn != nil && pyfmt.Str(fn) != "count" {
					e.plan.where = true
				}
				if err == nil && liveSrc != hyperdx.ID(e.tileSource) {
					e.plan.where = true
				}
			}
		}
	}
	e.plan.drift = hyperdx.AlertDrift(alert, hyperdxName(e.home, a), fields(e.home, a))

	e.firings.start(a, e.rows, st)
	if e.plan.deleteShared != "" || e.plan.where || len(e.plan.drift) > 0 {
		return reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: false, Diff: e.plan.String()}, nil
	}
	// An empty error clears a SpecDrift's message once the spec matches.
	a.Status.Phase, a.Status.Error = v1alpha1.PhaseSynced, ""
	// A pass with a push reads the groups after it (Update), so a failed read never holds a push
	// back. The live state was read, so Ready stands on a failed one.
	if err := e.groups(ctx, a); err != nil {
		return reconciler.ExternalObservation{}, reconciler.ReadyAssessed(err)
	}
	return reconciler.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, nil
}

// release leaves a HyperDX alert several Alerts claim to the Alert it is named after, if any. Any
// other claimant drops the id and creates its own alert in this pass; a shared alert named after
// none of them is deleted (by Update), so it stops evaluating one Alert's `where` for another.
// The claimants are the Alerts of every namespace, by their HyperDX names: one HyperDX alert serves
// one Alert, whichever namespace it is in.
func (e *external) release(ctx context.Context, a *v1alpha1.Alert, byID map[string]map[string]any) error {
	id := a.Status.HyperdxAlertID
	if id == "" {
		return nil
	}
	var all v1alpha1.AlertList
	if err := e.kube.List(ctx, &all); err != nil {
		return err
	}
	self := hyperdxName(e.home, a)
	claimants := []string{self}
	for i := range all.Items {
		o := &all.Items[i]
		if o.UID != a.UID && !meta.WasDeleted(o) && o.Status.HyperdxAlertID == id {
			claimants = append(claimants, hyperdxName(e.home, o))
		}
	}
	if len(claimants) < 2 {
		return nil
	}
	alertName, _ := byID[id]["name"].(string)
	owner := ""
	for _, n := range claimants {
		if n == alertName {
			owner = n
			break
		}
	}
	if owner == self {
		return nil
	}
	if owner == "" && byID[id] != nil {
		e.plan.deleteShared = id
		delete(byID, id)
	}
	a.Status.HyperdxAlertID, a.Status.Phase = "", v1alpha1.PhasePending
	e.log.Info(fmt.Sprintf("[reconciler] Alert %s: released shared hyperdx %s", self, id))
	return nil
}

// Create is never called: Observe never reports the HyperDX alert missing (see Observe).
func (e *external) Create(ctx context.Context, mg resource.Managed) error {
	return nil
}

// Update deletes a shared alert, creates the dashboard and alert or pushes the spec onto them.
func (e *external) Update(ctx context.Context, mg resource.Managed) error {
	a, ok := mg.(*v1alpha1.Alert)
	if !ok {
		return errNotAlert
	}
	if why := e.plan.invalid; why != "" {
		a.Status.Phase, a.Status.Error = v1alpha1.PhaseInvalid, why
		e.log.Info(fmt.Sprintf("[reconciler] Alert %s refused: %s", hyperdxName(e.home, a), why))
		return nil
	}
	if id := e.plan.deleteShared; id != "" {
		if err := e.hdx.DeleteAlert(ctx, id); err != nil {
			return err
		}
	}
	if e.plan.create {
		if err := e.create(ctx, a); err != nil {
			return err
		}
	} else if err := e.push(ctx, a); err != nil {
		return err
	}
	return e.groups(ctx, a)
}

// create makes the dashboard tile, then an alert on it named after the Alert.
func (e *external) create(ctx context.Context, a *v1alpha1.Alert) error {
	dash, tile, err := e.hdx.EnsureTile(ctx, dashboardName(e.home, a), e.tileSource, e.sel, e.plan.taken)
	if err != nil {
		return failed(a, err)
	}
	res, err := e.hdx.EnsureAlert(ctx, hyperdxName(e.home, a), dash, tile, e.webhookID, fields(e.home, a))
	if err != nil {
		return failed(a, err)
	}
	st := pyfmt.Str(res["state"])
	a.Status.HyperdxAlertID, a.Status.HyperdxDashboardID = pyfmt.Str(res["id"]), dash
	a.Status.OkSince = OkSince(a.Status, st, now())
	a.Status.State, a.Status.Phase = st, v1alpha1.PhaseSynced
	a.SetConditions(prv1.Available())
	e.log.Info(fmt.Sprintf("[reconciler] synced Alert %s -> hyperdx %s (%s)", hyperdxName(e.home, a), a.Status.HyperdxAlertID, st))
	e.firings.start(a, e.rows, st)
	return nil
}

// push puts the spec onto the live alert where they disagree. The spec spans two objects:
// threshold, thresholdType, interval, message and consecutiveWindows live on the alert; `where`,
// the select and groupBy on the dashboard tile.
// The alert's name is pushed too: it ties the HyperDX alert to its Alert.
func (e *external) push(ctx context.Context, a *v1alpha1.Alert) error {
	tile := e.plan.tileID
	if tile == "" {
		tile = "count"
	}
	var changed []string
	if e.plan.where {
		if err := e.hdx.UpdateTile(ctx, e.plan.dashboardID, dashboardName(e.home, a), e.tileSource, e.sel, tile); err != nil {
			return e.specDrift(a, err)
		}
		changed = append(changed, "where")
	}
	if len(e.plan.drift) > 0 {
		if _, err := e.hdx.UpdateAlert(ctx, e.plan.alertID, hyperdxName(e.home, a), e.plan.dashboardID, tile, e.webhookID, fields(e.home, a)); err != nil {
			return e.specDrift(a, err)
		}
		changed = append(changed, e.plan.drift...)
	}
	a.Status.Phase, a.Status.Error = v1alpha1.PhaseSynced, ""
	if len(changed) > 0 {
		e.log.Info(fmt.Sprintf("[reconciler] Alert %s: pushed %s to hyperdx %s", hyperdxName(e.home, a), strings.Join(changed, ", "), e.plan.alertID))
	}
	return nil
}

// Delete removes the HyperDX alert and dashboard, best-effort: a failure still lets the Alert go,
// so a delete is never wedged on HyperDX.
func (e *external) Delete(ctx context.Context, mg resource.Managed) error {
	a, ok := mg.(*v1alpha1.Alert)
	if !ok {
		return errNotAlert
	}
	if id := a.Status.HyperdxAlertID; id != "" {
		_ = e.hdx.DeleteAlert(ctx, id)
	}
	if id := a.Status.HyperdxDashboardID; id != "" {
		_ = e.hdx.DeleteDashboard(ctx, id)
	}
	a.Status.HyperdxAlertID, a.Status.HyperdxDashboardID = "", ""
	e.firings.forget(a.UID)
	e.log.Info(fmt.Sprintf("[reconciler] finalized Alert %s (removed HyperDX alert+dashboard)", hyperdxName(e.home, a)))
	return nil
}

// firings hands an Alert's firings to the incident writer: one per poll interval while the
// HyperDX alert is ALERT. Reconciles the poll did not schedule (a spec edit, an error's retry)
// fire nothing, so an alert fires once a pass as it always has. Only the home namespace's Alerts
// reach the writer.
type firings struct {
	home     string // the home namespace: the only one whose firings open Incidents
	interval time.Duration
	fire     func(context.Context, incident.Alert, incident.Records)
	ctx      context.Context
	hdx      func() *hyperdx.Client
	log      logging.Logger
	// run runs a firing; off the reconcile, in its own goroutine.
	run func(func())

	mu   sync.Mutex
	last map[types.UID]time.Time
}

// fire starts the incident writer on a firing, off the reconcile: a comparison takes seconds, a
// new incident's RCA minutes. Its records are the alert's `where` rows in source (its logs or its
// traces), one line per distinct record.
func (f *firings) start(a *v1alpha1.Alert, source map[string]any, state string) {
	if state != "ALERT" {
		return
	}
	f.mu.Lock()
	if t, ok := f.last[a.UID]; ok && time.Since(t) < f.interval {
		f.mu.Unlock()
		return
	}
	f.last[a.UID] = time.Now()
	f.mu.Unlock()
	if a.Paused() {
		f.log.Info(fmt.Sprintf("[reconciler] Alert %s is paused; firing skipped", hyperdxName(f.home, a)))
		return
	}
	// The RCA agent reads the whole platform, so its analysis of a tenant's alert could quote other
	// tenants' data: an Alert outside the home namespace is the ALERT state mirrored on it and
	// HyperDX's notification, never a comparison, an Incident or an RCA.
	if a.Namespace != f.home {
		f.log.Info(fmt.Sprintf("[reconciler] Alert %s is outside %s; no incident (only %s Alerts are root-caused)", hyperdxName(f.home, a), f.home, f.home))
		return
	}
	// The incident writer compares and root-causes the RECORDS (log records or spans) an alert
	// counts. A metric has
	// none, and an aggregated value is not a count (the prompt would describe the wrong
	// threshold), so an aggregated firing is the ALERT state mirrored on the Alert, and HyperDX's
	// notification through the webhook.
	if a.Aggregated() {
		f.log.Info(fmt.Sprintf("[reconciler] Alert %s fires on an aggregated value; no incident (incidents read counted log records)", hyperdxName(f.home, a)))
		return
	}
	alert := incident.Alert{Namespace: a.Namespace, HumanAnalysis: a.Labels[incident.LabelAnalysis] == incident.AnalysisHuman, Alert: compare.Alert{
		Name: a.Name, DisplayName: a.Spec.DisplayName, Where: a.Spec.Where, Interval: a.Spec.Interval,
		Threshold: pyfmt.NumberStr(threshold(a.Spec)), ThresholdType: a.Spec.ThresholdType, Message: a.Spec.Message,
		Source: a.Spec.Source,
	}}
	hdx, sourceID, group := f.hdx(), hyperdx.ID(source), compare.GroupOf(a.Spec.Source)
	records := func(ctx context.Context, where string, seconds int) ([]compare.Row, error) {
		return hdx.RecordCounts(ctx, sourceID, where, seconds, group)
	}
	f.run(func() { f.fire(f.ctx, alert, records) })
}

func (f *firings) forget(uid types.UID) {
	f.mu.Lock()
	delete(f.last, uid)
	f.mu.Unlock()
}
