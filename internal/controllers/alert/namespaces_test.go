package alert

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
	"github.com/krateo-platformops/alert-provider/internal/hyperdx/hyperdxtest"
	"github.com/krateo-platformops/alert-provider/internal/incident"
)

const (
	teamA  = "team-a"
	teamB  = "team-b"
	shared = "llm-budget"
)

// tenantCR is alertCR in namespace.
func tenantCR(namespace, name, where string) *v1alpha1.Alert {
	a := alertCR(name, where, v1alpha1.AlertStatus{})
	a.Namespace, a.UID = namespace, types.UID("uid-"+namespace+"-"+name)
	return a
}

// --- the HyperDX name ---

func TestTheHyperDXNameIsBareAtHomeAndQualifiedElsewhere(t *testing.T) {
	eq(t, "home", HyperDXName(ns, ns, shared), shared)
	eq(t, "tenant", HyperDXName(ns, teamA, shared), "team-a/llm-budget")
	eq(t, "home dashboard", DashboardName(ns, ns, shared), "krateo-alert-llm-budget")
	eq(t, "tenant dashboard", DashboardName(ns, teamA, shared), "krateo-alert-team-a/llm-budget")
	// another home namespace: the bare name follows it
	eq(t, "other home", HyperDXName("platform", "platform", shared), shared)
	eq(t, "krateo-system off home", HyperDXName("platform", ns, shared), "krateo-system/llm-budget")
}

func TestTheHyperDXNamesOfOneAlertNameInThreeNamespacesNeverCollide(t *testing.T) {
	seen := map[string]string{}
	for _, n := range []string{ns, teamA, teamB} {
		for _, name := range []string{HyperDXName(ns, n, shared), DashboardName(ns, n, shared)} {
			if prev, ok := seen[name]; ok {
				t.Errorf("%s and %s share %q", prev, n, name)
			}
			seen[name] = n
		}
	}
	// "." and "-" are legal in names and namespaces, "/" in neither: no home name can spell a
	// tenant's.
	if HyperDXName(ns, "team", "a.llm-budget") == HyperDXName(ns, "team-a", "llm-budget") ||
		HyperDXName(ns, "team-a.x", shared) == HyperDXName(ns, "team-a", "x."+shared) {
		t.Error("two namespaces' names collide")
	}
}

func TestTheLongestQualifiedNameFitsHyperDX(t *testing.T) {
	// a namespace is a DNS label (63), an object name a DNS subdomain (253); HyperDX takes 512.
	n := HyperDXName(ns, strings.Repeat("n", 63), strings.Repeat("a", 253))
	if len(n) != 317 || len(n) > 512 {
		t.Errorf("len %d", len(n))
	}
}

// --- the reconcile, across namespaces ---

func TestOneAlertNameInThreeNamespacesGetsThreeHyperDXAlerts(t *testing.T) {
	h := newHarness(t, alertCR(shared, "where-home", v1alpha1.AlertStatus{}),
		tenantCR(teamA, shared, "where-a"), tenantCR(teamB, shared, "where-b"))
	h.pass(2)
	by := h.byName()
	eq(t, "names", len(by), 3)
	tiles := map[any]bool{}
	for n, want := range map[string]struct{ hdx, where string }{
		ns:    {shared, "where-home"},
		teamA: {"team-a/llm-budget", "where-a"},
		teamB: {"team-b/llm-budget", "where-b"},
	} {
		live := by[want.hdx]
		if live == nil {
			t.Fatalf("%s: no HyperDX alert %q in %v", n, want.hdx, by)
		}
		st := h.getIn(n, shared).Status
		eq(t, n+" id", st.HyperdxAlertID, live["id"])
		eq(t, n+" where", h.api.WhereOf(live), want.where)
		eq(t, n+" dashboard", h.api.Get("dashboards", st.HyperdxDashboardID)["name"], DashboardPrefix+want.hdx)
		eq(t, n+" phase", st.Phase, "Synced")
		tiles[live["tileId"]] = true
	}
	eq(t, "tiles", len(tiles), 3)
	// converged: a name compared bare would rename the tenants' alerts on every pass
	h.api.ClearWrites()
	h.pass(2)
	eq(t, "writes", nonWebhookWrites(h.api), []hyperdxtest.Write{})
}

func TestATenantSpecPushKeepsTheQualifiedNames(t *testing.T) {
	h := newHarness(t, alertCR(shared, "where-home", v1alpha1.AlertStatus{}), tenantCR(teamA, shared, "where-a"))
	h.pass(1)
	a := h.getIn(teamA, shared)
	a.Spec.Where, a.Spec.Threshold = "where-a2", "7"
	if err := h.kube.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	h.pass(1)
	st := h.getIn(teamA, shared).Status
	live := h.api.Get("alerts", st.HyperdxAlertID)
	eq(t, "pushed", []any{h.api.WhereOf(live), live["threshold"], live["name"]}, []any{"where-a2", 7, "team-a/llm-budget"})
	eq(t, "dashboard", h.api.Get("dashboards", st.HyperdxDashboardID)["name"], "krateo-alert-team-a/llm-budget")
	h.api.ClearWrites()
	h.pass(2)
	eq(t, "writes", nonWebhookWrites(h.api), []hyperdxtest.Write{})
}

func TestATenantAlertNeverAdoptsTheHomeAlertOfItsName(t *testing.T) {
	h := newHarness(t)
	home := h.seedAlert("krateo-alert-"+shared, shared, "where-home") // a live install's alert
	if err := h.kube.Create(ctx, tenantCR(teamA, shared, "where-a")); err != nil {
		t.Fatal(err)
	}
	h.pass(1)
	st := h.getIn(teamA, shared).Status
	if st.HyperdxAlertID == home {
		t.Fatal("the tenant Alert adopted the home namespace's HyperDX alert")
	}
	eq(t, "home where", h.api.WhereOf(h.api.Get("alerts", home)), "where-home")
	eq(t, "tenant name", h.api.Get("alerts", st.HyperdxAlertID)["name"], "team-a/llm-budget")
}

func TestAHomeAlertAdoptsItsLiveInstallAlertByItsBareName(t *testing.T) {
	h := newHarness(t)
	home := h.seedAlert("krateo-alert-"+shared, shared, "where-home")
	if err := h.kube.Create(ctx, alertCR(shared, "where-home", v1alpha1.AlertStatus{})); err != nil {
		t.Fatal(err)
	}
	h.pass(2)
	eq(t, "adopted", h.status(shared).HyperdxAlertID, home)
	eq(t, "alerts", len(h.api.All("alerts")), 1)
}

func TestALostStatusIDInATenantNamespaceAdoptsItsOwnAlert(t *testing.T) {
	h := newHarness(t, alertCR(shared, "where-home", v1alpha1.AlertStatus{}), tenantCR(teamA, shared, "where-a"))
	h.pass(1)
	first := h.getIn(teamA, shared).Status.HyperdxAlertID
	a := h.getIn(teamA, shared)
	a.Status.HyperdxAlertID = ""
	if err := h.kube.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	h.pass(1)
	eq(t, "id", h.getIn(teamA, shared).Status.HyperdxAlertID, first)
	eq(t, "alerts", len(h.api.All("alerts")), 2)
}

func TestASharedAlertAcrossNamespacesStaysWithTheAlertItIsNamedAfter(t *testing.T) {
	h := newHarness(t)
	owned := h.seedAlert("krateo-alert-team-a/"+shared, "team-a/"+shared, "where-a")
	for _, a := range []*v1alpha1.Alert{alertCR(shared, "where-home", v1alpha1.AlertStatus{}), tenantCR(teamA, shared, "where-a")} {
		if err := h.kube.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		a.Status.HyperdxAlertID = owned
		if err := h.kube.Status().Update(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	h.pass(2)
	eq(t, "tenant keeps it", h.getIn(teamA, shared).Status.HyperdxAlertID, owned)
	home := h.status(shared).HyperdxAlertID
	if home == owned {
		t.Error("the home Alert kept the tenant's alert")
	}
	eq(t, "home name", h.api.Get("alerts", home)["name"], shared)
}

func TestARecreatedWebhookRebuildsTheAlertsOfEveryNamespace(t *testing.T) {
	h := newHarness(t, alertCR(shared, "where-home", v1alpha1.AlertStatus{}), tenantCR(teamA, shared, "where-a"))
	h.pass(1)
	for _, w := range h.api.All("webhooks") {
		h.api.Delete("webhooks", w["id"].(string))
	}
	h.pass(2)
	hook := h.api.All("webhooks")[0]["id"]
	by := h.byName()
	for _, n := range []string{shared, "team-a/" + shared} {
		eq(t, n+" channel", by[n]["channel"].(map[string]any)["webhookId"], hook)
	}
}

func TestATenantAlertWithoutADescriptionPromisesNoTriage(t *testing.T) {
	a := tenantCR(teamA, shared, "w")
	a.Spec.Description = ""
	eq(t, "tenant", fields(ns, a).Message, "team-a/llm-budget threshold crossed.")
	home := alertCR(shared, "w", v1alpha1.AlertStatus{})
	home.Spec.Description = ""
	eq(t, "home", fields(ns, home).Message, "") // hyperdx's default, as before
}

// --- firings ---

type firing struct{ namespace, name string }

func (h *harness) recordFirings() *[]firing {
	var got []firing
	h.conn.firings.fire = func(_ context.Context, a incident.Alert, _ incident.Records) {
		got = append(got, firing{a.Namespace, a.Name})
	}
	return &got
}

func TestOnlyAHomeNamespaceFiringReachesTheIncidentWriter(t *testing.T) {
	h := newHarness(t, alertCR(shared, "where-home", v1alpha1.AlertStatus{}), tenantCR(teamA, shared, "where-a"))
	got := h.recordFirings()
	h.pass(1)
	for _, n := range []string{ns, teamA} {
		h.api.Set("alerts", h.getIn(n, shared).Status.HyperdxAlertID, map[string]any{"state": "ALERT"})
	}
	h.pass(1)
	eq(t, "fired", *got, []firing{{ns, shared}})
	// the tenant's state is still mirrored
	st := h.getIn(teamA, shared).Status
	eq(t, "tenant", []string{st.State, st.Phase}, []string{"ALERT", "Synced"})
}

// countingKube is the incident writer's apiserver: every call is recorded, and each namespace
// holds one analyzed, Open incident of llm-budget, so a firing that reaches the writer is compared.
type countingKube struct {
	mu    sync.Mutex
	calls []string // method namespace
}

func (k *countingKube) record(method, namespace string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, method+" "+namespace)
}

func (k *countingKube) Get(_ context.Context, namespace, name string) (map[string]any, error) {
	k.record("get", namespace)
	return analyzedIncident(namespace, name), nil
}

func (k *countingKube) List(_ context.Context, namespace, _ string) ([]map[string]any, error) {
	k.record("list", namespace)
	return []map[string]any{analyzedIncident(namespace, shared+"-20260101-000000")}, nil
}

func (k *countingKube) Create(_ context.Context, obj map[string]any) (map[string]any, error) {
	m, _ := obj["metadata"].(map[string]any)
	namespace, _ := m["namespace"].(string)
	k.record("create", namespace)
	m["resourceVersion"], m["creationTimestamp"] = "1", time.Now().UTC().Format(time.RFC3339)
	return obj, nil
}

func (k *countingKube) PatchStatus(_ context.Context, namespace, _ string, _ map[string]any) error {
	k.record("patchStatus", namespace)
	return nil
}

func (k *countingKube) Patch(_ context.Context, namespace, _ string, _ map[string]any) error {
	k.record("patch", namespace)
	return nil
}

func analyzedIncident(namespace, name string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": name, "namespace": namespace, "resourceVersion": "1",
			"creationTimestamp": "2026-01-01T00:00:00Z", "labels": map[string]any{incident.LabelAlert: shared}},
		"spec": map[string]any{"alertRef": map[string]any{"name": shared, "namespace": namespace}},
		"status": map[string]any{"state": "Open", "rootCause": map[string]any{"statement": "the cause"},
			"completedAt": "2026-01-01T00:01:00Z"},
	}
}

// The real incident writer behind the controller, its comparison model and RCA agent on one fake
// endpoint: a tenant's firing reaches none of the writer's apiserver, HyperDX's records, the
// comparison or the RCA; the home namespace's firing of the same name reaches all of them.
func TestATenantFiringNeverReachesTheComparisonOrTheRCA(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	agents := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(r.URL.Path, "/llm/") {
			hits["comparison"]++
			// no open incident covers the firing: the writer opens one and runs its RCA
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
				"content": `{"match": null, "reason": "a new problem"}`}}}})
			return
		}
		hits["rca"]++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(agents.Close)
	mc := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kagent.dev/v1alpha2", "kind": "ModelConfig",
		"metadata": map[string]any{"name": "gemini-flash", "namespace": ns},
		"spec": map[string]any{"provider": "OpenAI", "model": "m", "apiKeyPassthrough": true,
			"openAI": map[string]any{"baseUrl": agents.URL + "/llm/v1/"}}}}
	noJWT := func(context.Context) string { return "" }
	comparer := &incident.Comparer{Reader: fake.NewClientBuilder().WithRuntimeObjects(mc).Build(), Namespace: ns,
		ModelConfig: "gemini-flash", Timeout: 5 * time.Second, JWT: noJWT}
	kube := &countingKube{}
	writer := incident.NewWriter(incident.Config{Namespace: ns, MaxConcurrentAnalyses: 1, MaxCandidates: 5},
		kube, &incident.A2A{URL: agents.URL + "/", Timeout: 5 * time.Second, JWT: noJWT}, comparer, logging.NewNopLogger())

	h := newHarness(t, alertCR(shared, "where-home", v1alpha1.AlertStatus{}), tenantCR(teamA, shared, "where-a"))
	h.conn.firings.fire = writer.Fire
	h.api.SetSeries([]any{map[string]any{"group": []any{"Pod x: BackOff"}, "series_0.data": "3"}})
	h.pass(1)
	h.api.Set("alerts", h.getIn(teamA, shared).Status.HyperdxAlertID, map[string]any{"state": "ALERT"})
	h.pass(1)
	eq(t, "tenant: writer calls", kube.calls, []string(nil))
	eq(t, "tenant: records read", len(h.api.SeriesRequests()), 0)
	eq(t, "tenant: agents", hits, map[string]int{})
	eq(t, "tenant: mirrored", h.getIn(teamA, shared).Status.State, "ALERT")

	h.api.Set("alerts", h.status(shared).HyperdxAlertID, map[string]any{"state": "ALERT"})
	h.pass(1)
	eq(t, "home: agents", hits, map[string]int{"comparison": 1, "rca": 1})
	eq(t, "home: records read", len(h.api.SeriesRequests()), 1)
	for _, c := range kube.calls {
		if !strings.HasSuffix(c, " "+ns) {
			t.Errorf("writer call outside %s: %s", ns, c)
		}
	}
	if len(kube.calls) == 0 {
		t.Error("the home firing reached no writer call")
	}
}
