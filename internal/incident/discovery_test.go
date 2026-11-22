package incident

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"

	"github.com/krateo-platformops/alert-provider/internal/scriptcheck"
)

// stubDiscovery answers ServerGroupsAndResources with lists and err, counting the calls.
type stubDiscovery struct {
	*fakediscovery.FakeDiscovery
	lists []*metav1.APIResourceList
	err   error
	calls int
}

func (s *stubDiscovery) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	s.calls++
	return nil, s.lists, s.err
}

func newStub(lists []*metav1.APIResourceList, err error) *stubDiscovery {
	return &stubDiscovery{FakeDiscovery: &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}, lists: lists, err: err}
}

var crds = []*metav1.APIResourceList{{GroupVersion: "apiextensions.k8s.io/v1", APIResources: []metav1.APIResource{
	{Name: "customresourcedefinitions", SingularName: "customresourcedefinition", Kind: "CustomResourceDefinition", ShortNames: []string{"crd"}},
	{Name: "customresourcedefinitions/status", Kind: "CustomResourceDefinition"},
}}}

const invented = "kubectl get customresourcedefinitions.apiextensions.krateo.io x -o json"

func TestDiscoveryReadsServedResources(t *testing.T) {
	d := NewDiscovery(newStub(crds, nil), time.Minute, logging.NewNopLogger())
	view := d.Get(context.Background())
	if got := scriptcheck.Unserved(map[string]any{"verify": invented}, view); len(got) != 1 {
		t.Fatalf("the invented group was not refused: %v", got)
	}
	if got := scriptcheck.Unserved(map[string]any{"verify": "kubectl get crd x; kubectl get customresourcedefinitions.apiextensions.k8s.io y"}, view); len(got) != 0 {
		t.Errorf("served names were refused: %v", got)
	}
}

// A group-version that fails discovery makes its group unknown, so nothing in it is refused.
func TestDiscoveryPartialFailureIsUnknown(t *testing.T) {
	partial := &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{
		{Group: "apiextensions.krateo.io", Version: "v1"}: errors.New("503"),
	}}
	view := NewDiscovery(newStub(crds, partial), time.Minute, logging.NewNopLogger()).Get(context.Background())
	if view == nil {
		t.Fatal("a partial discovery gave no view at all")
	}
	if got := scriptcheck.Unserved(map[string]any{"verify": invented}, view); len(got) != 0 {
		t.Errorf("a group whose discovery failed was called unserved: %v", got)
	}
}

// A failed refresh keeps the last good view; with none there is no view, and nothing is judged.
func TestDiscoveryFailureKeepsTheLastView(t *testing.T) {
	stub := newStub(crds, nil)
	d := NewDiscovery(stub, time.Minute, logging.NewNopLogger())
	now := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	first := d.Get(context.Background())
	if d.Get(context.Background()) != first || stub.calls != 1 {
		t.Errorf("within ttl it refreshed (calls %d)", stub.calls)
	}
	now = now.Add(2 * time.Minute)
	stub.err = errors.New("connection refused")
	if got := d.Get(context.Background()); got != first || stub.calls != 2 {
		t.Errorf("a failed refresh did not keep the last view (calls %d)", stub.calls)
	}
	if got := NewDiscovery(newStub(nil, errors.New("down")), time.Minute, logging.NewNopLogger()).Get(context.Background()); got != nil {
		t.Errorf("no good view yet, but got one")
	}
	var nilD *Discovery
	if nilD.Get(context.Background()) != nil {
		t.Error("a nil Discovery returned a view")
	}
}

// The writer refuses such a howToFix like any other script that cannot run: no howToFix stored,
// and a correction that names the served group.
func TestParsedRefusesAnUnservedGroup(t *testing.T) {
	view := NewDiscovery(newStub(crds, nil), time.Minute, logging.NewNopLogger()).Get(context.Background())
	fix := map[string]any{"precondition": invented + " || exit 2\nexit 0\n", "apply": "true\n", "verify": "exit 0\n"}
	r := parsed("", map[string]any{"howToFix": fix}, nil, view)
	if _, stored := r.st["howToFix"]; stored || len(r.correct) != 1 {
		t.Fatalf("howToFix stored=%v, correct=%v", stored, r.correct)
	}
	if want := "customresourcedefinitions.apiextensions.k8s.io"; !strings.Contains(r.correct[0], want) {
		t.Errorf("the correction does not name %s: %s", want, r.correct[0])
	}
	if r := parsed("", map[string]any{"howToFix": fix}, nil, nil); len(r.correct) != 0 {
		t.Errorf("without discovery it was refused: %v", r.correct)
	}
}
