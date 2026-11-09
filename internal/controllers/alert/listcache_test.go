package alert

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/ratelimit"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"
	"github.com/krateo-platformops/provider-runtime/pkg/resource/fake"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
	"github.com/krateo-platformops/alert-provider/internal/tracing/tracingtest"
)

// fakeClock is a settable time for the shared list's TTL.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// shareList gives the harness the shared alerts list Setup gives the controller, with a TTL of
// ttl on a clock the test moves.
func (h *harness) shareList(ttl time.Duration) *fakeClock {
	c := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	h.conn.alerts = newAlertsList(ttl)
	h.conn.alerts.now = c.now
	return c
}

// observe is Connect then Observe on the home-namespace Alert name, as one pass begins.
func (h *harness) observe(name string) reconciler.ExternalObservation {
	h.t.Helper()
	a := h.get(name)
	ext, err := h.conn.Connect(ctx, a)
	if err != nil {
		h.t.Fatal(err)
	}
	obs, err := ext.Observe(ctx, a)
	if err != nil {
		h.t.Fatal(err)
	}
	return obs
}

func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("a%02d", i)
	}
	return out
}

// synced is a harness whose Alerts all have their HyperDX alerts, converged, sharing one list
// with a one-minute TTL.
func synced(t *testing.T, n int) (*harness, *fakeClock) {
	t.Helper()
	var crs []*v1alpha1.Alert
	for _, name := range names(n) {
		crs = append(crs, alertCR(name, "SeverityText = '"+name+"'", v1alpha1.AlertStatus{}))
	}
	h := newHarness(t, crs...)
	h.pass(2)
	for _, name := range names(n) {
		if st := h.status(name); st.Phase != v1alpha1.PhaseSynced || st.HyperdxAlertID == "" {
			t.Fatalf("%s did not converge: %+v", name, st)
		}
	}
	c := h.shareList(time.Minute)
	h.api.ClearLists()
	h.api.ClearWrites()
	return h, c
}

// The passes of one poll interval share one GET /api/v2/alerts; the next interval reads it again.
// Each Alert still mirrors its own HyperDX alert from the shared list.
// MUTATION: a list that ignores its TTL (kept forever once fetched) fails the second count.
func TestOnePollIntervalListsTheAlertsOnce(t *testing.T) {
	const n = 6
	h, c := synced(t, n)

	h.pass(1)
	eq(t, "alert lists in one interval", h.api.Lists("alerts"), 1)
	eq(t, "writes", len(nonWebhookWrites(h.api)), 0)
	for _, name := range names(n) {
		live := h.byName()[name]
		st := h.status(name)
		eq(t, name+" mirrors its own alert", st.HyperdxAlertID, live["id"])
		eq(t, name+" phase", st.Phase, v1alpha1.PhaseSynced)
	}

	c.advance(59 * time.Second)
	h.pass(1)
	eq(t, "alert lists inside the TTL", h.api.Lists("alerts"), 1)

	c.advance(time.Second) // one TTL after the fetch
	h.pass(1)
	eq(t, "alert lists after the TTL", h.api.Lists("alerts"), 2)
}

// The live state the shared list carries is at most one TTL old: a change HyperDX makes is
// mirrored once the list expires.
func TestTheSharedListMirrorsStateOneIntervalLate(t *testing.T) {
	h, c := synced(t, 2)
	h.pass(1)
	h.api.Set("alerts", h.status("a00").HyperdxAlertID, map[string]any{"state": "ALERT"})
	h.pass(1)
	eq(t, "state inside the TTL", h.status("a00").State, "OK")
	c.advance(time.Minute)
	h.pass(1)
	eq(t, "state after the TTL", h.status("a00").State, "ALERT")
}

// Every write the controller makes to HyperDX's alerts drops the shared list, so the next Observe
// reads the list anew and sees its own write: a created alert is found, not created again; a
// pushed spec is not pushed again; a deleted alert is not adopted by the Alert recreated after it.
// MUTATION: a client that never calls OnWrite (no invalidation) fails each subtest.
func TestAWriteDropsTheSharedList(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		h, _ := synced(t, 1)
		h.pass(1) // the list is fresh
		if err := h.kube.Create(ctx, alertCR("fresh", "SeverityText = 'fresh'", v1alpha1.AlertStatus{})); err != nil {
			t.Fatal(err)
		}
		if err := h.reconcile("fresh"); err != nil { // creates its HyperDX alert
			t.Fatal(err)
		}
		lists := h.api.Lists("alerts")
		obs := h.observe("fresh")
		eq(t, "the next Observe reads the list", h.api.Lists("alerts"), lists+1)
		eq(t, "it finds its alert: nothing to do", obs.ResourceUpToDate, true)
		eq(t, "diff", obs.Diff, "")
	})
	t.Run("update", func(t *testing.T) {
		h, _ := synced(t, 1)
		h.pass(1)
		h.edit("a00", func(a *v1alpha1.Alert) { a.Spec.Threshold = "9" })
		if err := h.reconcile("a00"); err != nil { // pushes the threshold
			t.Fatal(err)
		}
		eq(t, "pushed", h.byName()["a00"]["threshold"], float64(9))
		lists := h.api.Lists("alerts")
		obs := h.observe("a00")
		eq(t, "the next Observe reads the list", h.api.Lists("alerts"), lists+1)
		eq(t, "the push is seen: nothing to do", obs.ResourceUpToDate, true)
		eq(t, "diff", obs.Diff, "")
	})
	t.Run("delete", func(t *testing.T) {
		h, _ := synced(t, 2)
		h.pass(1)
		if err := h.kube.Delete(ctx, h.get("a00")); err != nil {
			t.Fatal(err)
		}
		if err := h.reconcile("a00"); err != nil { // deletes its HyperDX alert and dashboard
			t.Fatal(err)
		}
		if h.byName()["a00"] != nil {
			t.Fatal("the HyperDX alert was kept")
		}
		_ = h.reconcile("a00") // drops the finalizer
		if err := h.kube.Create(ctx, alertCR("a00", "SeverityText = 'again'", v1alpha1.AlertStatus{})); err != nil {
			t.Fatal(err)
		}
		lists := h.api.Lists("alerts")
		obs := h.observe("a00")
		eq(t, "the next Observe reads the list", h.api.Lists("alerts"), lists+1)
		if !strings.Contains(obs.Diff, "create") {
			t.Errorf("the recreated Alert adopted the deleted HyperDX alert: diff %q", obs.Diff)
		}
	})
}

// Observes that find the list expired while it is being read wait for that one read: n
// concurrent Observes make one GET /api/v2/alerts. Run with -race.
// MUTATION: a list without single flight (every expired reader fetches) fails the count.
func TestConcurrentObservesShareOneListRead(t *testing.T) {
	const n = 8
	h, _ := synced(t, n)

	var arrived atomic.Int32
	release := make(chan struct{})
	h.api.SetGate(func(method, kind, id string) {
		if method == http.MethodGet && kind == "alerts" && id == "" {
			arrived.Add(1)
			<-release
		}
	})
	var wg sync.WaitGroup
	obs := make([]reconciler.ExternalObservation, n)
	errs := make([]error, n)
	for i, name := range names(n) {
		a := h.get(name)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ext, err := h.conn.Connect(ctx, a)
			if err != nil {
				errs[i] = err
				return
			}
			obs[i], errs[i] = ext.Observe(ctx, a)
		}()
	}
	// Every Observe is either reading the list or waiting on the one read: none can be early or late.
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.conn.alerts.mu.Lock()
		joined := h.conn.alerts.joined
		h.conn.alerts.mu.Unlock()
		if int(arrived.Load())+joined >= n {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("after 10s %d reads and %d waiters, want %d in all", arrived.Load(), joined, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	wg.Wait()
	h.api.SetGate(nil)

	eq(t, "alert lists", h.api.Lists("alerts"), 1)
	for i, name := range names(n) {
		if errs[i] != nil {
			t.Fatalf("%s: %v", name, errs[i])
		}
		eq(t, name+" up to date", obs[i].ResourceUpToDate, true)
	}
}

// A write while the list is being read detaches that read: the next reader fetches anew, and the
// detached answer, which may predate the write, is not kept.
func TestAWriteDuringAReadDetachesIt(t *testing.T) {
	h, _ := synced(t, 1)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.api.SetGate(func(method, kind, id string) {
		if method == http.MethodGet && kind == "alerts" && id == "" {
			once.Do(func() { close(started); <-release })
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = h.conn.alerts.get(ctx, h.conn.hdx())
	}()
	<-started
	h.conn.alerts.invalidate()
	close(release)
	<-done
	h.api.SetGate(nil)

	_, cached, err := h.conn.alerts.get(ctx, h.conn.hdx())
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "the read after the write is its own", cached, false)
	eq(t, "alert lists", h.api.Lists("alerts"), 2)
}

// alert.observe says whether it read the shared list or made the call.
func TestTheObserveSpanSaysWhetherTheListWasShared(t *testing.T) {
	h, _ := synced(t, 2)
	rec := tracingtest.Install(t)
	h.tracedReconcile(ctx, "a00")
	h.tracedReconcile(ctx, "a01")
	spans := rec.Named("alert.observe")
	if len(spans) != 2 {
		t.Fatalf("%d alert.observe spans", len(spans))
	}
	eq(t, "first", attr(spans[0], string(attrListCached)), "false")
	eq(t, "second", attr(spans[1], string(attrListCached)), "true")
}

// reconcileGoverned is one reconcile of the home-namespace Alert name by provider-runtime's own
// Reconciler with the rate-limit governor, wired as Setup wires them.
func (h *harness) reconcileGoverned(g *ratelimit.Governor, name string) (reconcile.Result, error) {
	h.t.Helper()
	r := reconciler.NewReconciler(&fake.Manager{Client: h.kube, Scheme: h.kube.Scheme()},
		resource.ManagedKind(v1alpha1.AlertGroupVersionKind),
		reconciler.WithExternalConnecter(h.conn),
		reconciler.WithFinalizer(resource.NewAPIFinalizer(h.kube, Finalizer)),
		reconciler.WithPollInterval(h.conn.interval),
		reconciler.WithRateLimitGovernor(g),
	)
	return r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
}

func governor(t *testing.T) *ratelimit.Governor {
	t.Helper()
	hold := MaxThrottle
	g, err := ratelimit.New(ratelimit.Options{MaxConcurrentReconciles: 1, MaxHold: &hold})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// throttledPass is a synced Alert's pass on which HyperDX answers the alerts list 429 with
// header, through provider-runtime with the governor.
func throttledPass(t *testing.T, header http.Header) (*harness, *ratelimit.Governor, reconcile.Result) {
	t.Helper()
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	h.pass(2)
	before := h.status(k)
	h.api.FailHeader = header
	h.api.Fail = func(method, kind, id string) int {
		if method == http.MethodGet && kind == "alerts" {
			return http.StatusTooManyRequests
		}
		return 0
	}
	g := governor(t)
	res, err := h.reconcileGoverned(g, k)
	if err != nil {
		t.Fatalf("a 429 failed the reconcile: %v", err)
	}
	a := h.get(k)
	eq(t, "phase", a.Status.Phase, before.Phase)
	eq(t, "state", a.Status.State, before.State)
	eq(t, "error", a.Status.Error, "")
	synced := a.GetCondition(prv1.TypeSynced)
	eq(t, "synced reason", synced.Reason, prv1.ReasonReconcileDeferred)
	eq(t, "synced", string(synced.Status), "False")
	return h, g, res
}

func near(t *testing.T, what string, got, want time.Duration) {
	t.Helper()
	if got > want || got < want-2*time.Second {
		t.Errorf("%s: %v, want about %v", what, got, want)
	}
}

// A 429 with Retry-After defers the pass until then: no error, no error condition, the status as it
// was; and the passes until then make no call to HyperDX.
// MUTATION: treating a 429 as any HTTP error (throttle reporting false) fails it.
func TestA429DefersThePassUntilRetryAfter(t *testing.T) {
	h, g, res := throttledPass(t, http.Header{"Retry-After": {"7"}})
	near(t, "requeue", res.RequeueAfter, 7*time.Second)

	lists, writes := h.api.Lists("alerts"), len(h.api.Writes())
	h.api.Fail = nil
	res, err := h.reconcileGoverned(g, k)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "the next pass still waits", res.RequeueAfter, 7*time.Second)
	eq(t, "no list while held", h.api.Lists("alerts"), lists)
	eq(t, "no write while held", len(h.api.Writes()), writes)
}

// A Retry-After given as an HTTP-date is read as one.
func TestA429RetryAfterDateIsHonoured(t *testing.T) {
	_, _, res := throttledPass(t, http.Header{"Retry-After": {time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)}})
	near(t, "requeue", res.RequeueAfter, 30*time.Second)
}

// A 429 that names no time waits a poll interval.
func TestA429WithoutRetryAfterWaitsAPollInterval(t *testing.T) {
	_, _, res := throttledPass(t, nil)
	near(t, "requeue", res.RequeueAfter, time.Minute)
}

// A Retry-After past MaxThrottle is cut to it.
func TestA429RetryAfterIsCapped(t *testing.T) {
	_, _, res := throttledPass(t, http.Header{"Retry-After": {"86400"}})
	near(t, "requeue", res.RequeueAfter, MaxThrottle)
}

// A 429 on Connect's calls (the sources, the webhook) defers the pass the same way.
func TestA429OnConnectDefersThePass(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	h.pass(2)
	h.api.FailHeader = http.Header{"Retry-After": {"7"}}
	h.api.Fail = func(method, kind, id string) int {
		if kind == "sources" {
			return http.StatusTooManyRequests
		}
		return 0
	}
	res, err := h.reconcileGoverned(governor(t), k)
	if err != nil {
		t.Fatalf("a 429 failed the reconcile: %v", err)
	}
	near(t, "requeue", res.RequeueAfter, 7*time.Second)
	eq(t, "phase", h.status(k).Phase, v1alpha1.PhaseSynced)
	eq(t, "synced reason", h.get(k).GetCondition(prv1.TypeSynced).Reason, prv1.ReasonReconcileDeferred)
}

// Any other HTTP error is still the pass's error.
func TestA500IsStillAnError(t *testing.T) {
	h := newHarness(t, alertCR(k, "w", v1alpha1.AlertStatus{}))
	h.pass(2)
	h.api.Fail = func(method, kind, id string) int {
		if method == http.MethodGet && kind == "alerts" {
			return http.StatusInternalServerError
		}
		return 0
	}
	if _, err := h.reconcileGoverned(governor(t), k); err == nil {
		t.Fatal("a 500 did not fail the reconcile")
	}
	eq(t, "synced reason", h.get(k).GetCondition(prv1.TypeSynced).Reason, prv1.ReasonReconcileError)
}
