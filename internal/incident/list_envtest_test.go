//go:build integration

package incident

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// pagingReader records the Limit and Continue of every List it passes on.
type pagingReader struct {
	client.Reader
	mu    sync.Mutex
	pages []client.ListOptions
}

func (r *pagingReader) List(ctx context.Context, l client.ObjectList, opts ...client.ListOption) error {
	o := client.ListOptions{}
	o.ApplyOptions(opts)
	r.mu.Lock()
	r.pages = append(r.pages, o)
	r.mu.Unlock()
	return r.Reader.List(ctx, l, opts...)
}

// The real List reads Incidents page by page from a real apiserver and returns every one of
// them, none with spec.prompt: an alert's hundreds of Incidents are never held whole (demo:
// listing 1957 in one piece OOMKilled the provider at startup under 256Mi).
func TestEnvtestListPagesAndDropsPrompts(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test needs envtest's apiserver (make test-integration sets it)")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "controllers", "reanalysis", "testdata")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := context.Background()
	direct, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := direct.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	const n = 250
	prompt := strings.Repeat("an analysis prompt as long as a real one. ", 480) // ~20 KB
	for i := 0; i < n; i++ {
		alert := alertName
		if i%5 == 0 {
			alert = "another-alert"
		}
		u := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"prompt": prompt, "closed": true}}}
		u.SetGroupVersionKind(IncidentGVK)
		u.SetNamespace(ns)
		u.SetName(fmt.Sprintf("inc-%03d", i))
		u.SetLabels(map[string]string{LabelAlert: alert})
		if err := direct.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	reader := &pagingReader{Reader: direct}
	k := NewKube(reader, direct)
	items, err := k.List(ctx, ns, LabelAlert)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != n {
		t.Fatalf("listed %d Incidents, want %d", len(items), n)
	}
	seen := map[string]bool{}
	for _, it := range items {
		name := metadata(it)["name"].(string)
		if seen[name] {
			t.Fatalf("%s listed twice", name)
		}
		seen[name] = true
		if _, ok := spec(it)["prompt"]; ok {
			t.Fatalf("%s listed with its prompt", name)
		}
		if spec(it)["closed"] != true {
			t.Fatalf("%s lost the rest of its spec: %v", name, spec(it))
		}
	}
	if len(reader.pages) < 3 {
		t.Fatalf("%d List calls for %d Incidents: not paged", len(reader.pages), n)
	}
	for i, p := range reader.pages {
		if p.Limit != listPage || (i > 0 && p.Continue == "") {
			t.Fatalf("call %d: limit %d continue %q", i, p.Limit, p.Continue)
		}
	}

	// A selector with a value lists only that alert's Incidents, paged the same way.
	byAlert, err := k.List(ctx, ns, LabelAlert+"=another-alert")
	if err != nil {
		t.Fatal(err)
	}
	if len(byAlert) != n/5 {
		t.Fatalf("listed %d of another-alert's Incidents, want %d", len(byAlert), n/5)
	}

	// Get still returns the whole Incident, prompt included.
	full, err := k.Get(ctx, ns, "inc-001")
	if err != nil {
		t.Fatal(err)
	}
	if spec(full)["prompt"] != prompt {
		t.Fatal("Get lost the prompt")
	}
}
