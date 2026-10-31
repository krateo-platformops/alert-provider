package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/krateo-platformops/alert-provider/apis"
	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
)

// The Alerts and Metrics are cached in every namespace, everything else in the home one only.
func TestTheCacheTakesAlertsAndMetricsFromEveryNamespace(t *testing.T) {
	o := cacheOptions("krateo-system")
	if len(o.DefaultNamespaces) != 1 {
		t.Fatalf("default namespaces %v", o.DefaultNamespaces)
	}
	if _, ok := o.DefaultNamespaces["krateo-system"]; !ok {
		t.Fatalf("default namespaces %v", o.DefaultNamespaces)
	}
	kinds := map[string]bool{}
	for obj, by := range o.ByObject {
		switch obj.(type) {
		case *v1alpha1.Alert:
			kinds["Alert"] = true
		case *v1alpha1.Metric:
			kinds["Metric"] = true
		default:
			t.Errorf("%T is cached outside the home namespace", obj)
		}
		if _, ok := by.Namespaces[cache.AllNamespaces]; !ok || len(by.Namespaces) != 1 {
			t.Errorf("%T namespaces %v, want all", obj, by.Namespaces)
		}
	}
	if !kinds["Alert"] || !kinds["Metric"] {
		t.Errorf("cached everywhere: %v", kinds)
	}
}

// The cache resolves ByObject's kinds when it is created, so the APIs must be in its scheme by
// then: run registers them before it creates the manager.
func TestTheCacheNeedsTheAPIsInItsScheme(t *testing.T) {
	cfg := &rest.Config{Host: "http://127.0.0.1:1"} // never dialled: the mapper is static
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, kind := range []string{v1alpha1.AlertKind, v1alpha1.MetricKind} {
		mapper.Add(v1alpha1.SchemeGroupVersion.WithKind(kind), meta.RESTScopeNamespace)
	}
	o := cacheOptions("krateo-system")
	o.Scheme, o.Mapper = runtime.NewScheme(), mapper
	if _, err := cache.New(cfg, o); err == nil {
		t.Fatal("a cache over a scheme without the APIs was created")
	}
	o = cacheOptions("krateo-system")
	o.Scheme, o.Mapper = runtime.NewScheme(), mapper
	if err := apis.AddToScheme(o.Scheme); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.New(cfg, o); err != nil {
		t.Fatal(err)
	}
}
