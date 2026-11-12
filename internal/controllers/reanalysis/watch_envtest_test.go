//go:build integration

package reanalysis

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"

	"github.com/krateo-platformops/alert-provider/internal/incident"
)

type recorder struct {
	mu    sync.Mutex
	calls map[string]int
}

func (r *recorder) Reanalyze(_, _ context.Context, ns, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[ns+"/"+name]++
	return nil
}

func (r *recorder) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls["krateo-system/"+name]
}

func (r *recorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func heap() uint64 {
	runtime.GC()
	debug.FreeOSMemory()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func newIncident(name string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(incident.IncidentGVK)
	u.SetNamespace("krateo-system")
	u.SetName(name)
	return u
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The watch caches Incidents as metadata only, so their prompts never reach the controller's
// heap (demo: 1957 Incidents of about 20 KB each OOMKilled the whole-object cache under 256Mi);
// every Incident the cache first lists is handed to the writer once (a request filed while the
// controller was down), a spec write is handed over, and a status write is not.
func TestEnvtestTheWatchHoldsMetadataAndPassesSpecWrites(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test needs envtest's apiserver (make test-integration sets it)")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("testdata")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	direct, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := direct.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "krateo-system"}}); err != nil {
		t.Fatal(err)
	}
	const n = 500
	prompt := strings.Repeat("the analysis prompt, as long as a real one. ", 460) // ~20 KB
	for i := 0; i < n; i++ {
		spec := map[string]any{"prompt": fmt.Sprintf("%d %s", i, prompt), "closed": true}
		if i == 7 {
			spec["reanalysis"] = map[string]any{"feedback": "filed while the controller was down"}
		}
		if err := direct.Create(ctx, newIncident(fmt.Sprintf("inc-%03d", i), spec)); err != nil {
			t.Fatal(err)
		}
	}

	before := heap()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  clientgoscheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{"krateo-system": {}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{calls: map[string]int{}}
	if err := Setup(mgr, Options{Writer: rec, AnalysisContext: ctx, Log: logging.NewNopLogger()}); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()
	eventually(t, "every listed Incident handed over once", func() bool { return rec.total() == n })
	if c := rec.count("inc-007"); c != 1 {
		t.Fatalf("the request filed while down: handed over %d times, want 1", c)
	}
	// 500 prompts of ~20 KB are ~10 MB of JSON, several times that as unstructured maps; the
	// metadata cache stays a small fraction of it (measured: ~1.3 MiB, against ~12 MiB whole).
	grown := int64(heap()) - int64(before)
	t.Logf("heap grown by %d KiB for %d Incidents", grown>>10, n)
	if grown > 4<<20 { // metadata: ~1.3 MiB; whole objects: ~12 MiB
		t.Fatalf("the cache grew the heap by %d MiB for %d Incidents: their bodies are being cached", grown>>20, n)
	}

	// A status write (no generation change) is not handed over.
	st := newIncident("inc-010", nil)
	st.Object["status"] = map[string]any{"state": "Closed"}
	if err := direct.Status().Patch(ctx, st, client.Merge); err != nil {
		t.Fatal(err)
	}
	// A spec write (a reanalysis request) is.
	rq := newIncident("inc-020", map[string]any{"reanalysis": map[string]any{"feedback": "again"}})
	if err := direct.Patch(ctx, rq, client.Merge); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the spec write handed over", func() bool { return rec.count("inc-020") == 2 })
	time.Sleep(time.Second) // the status write's event, if it passed, has arrived by now
	if c := rec.count("inc-010"); c != 1 {
		t.Fatalf("a status write was handed over (%d calls, want the startup one only)", c)
	}
}
