package incident

import (
	"context"
	"encoding/json"
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/krateo-platformops/alert-provider/internal/pyfmt"
)

// The Incident contract (incident-controller apis/incident/v1alpha1).
var (
	IncidentGVK     = schema.GroupVersionKind{Group: "observability.krateo.io", Version: "v1alpha1", Kind: "Incident"}
	incidentListGVK = IncidentGVK.GroupVersion().WithKind("IncidentList")
)

// Kube reads and writes Incidents. Objects are JSON values decoded with pyfmt.Decode. Errors carry
// the apiserver's status code (see code).
type Kube interface {
	Get(ctx context.Context, namespace, name string) (map[string]any, error)
	// List returns the Incidents in namespace that match the label selector, WITHOUT spec.prompt:
	// the prompt is nearly all of an Incident (about 20 KB), an alert can hold hundreds of
	// Incidents, and no list reader needs it. Get one Incident for its prompt.
	List(ctx context.Context, namespace, selector string) ([]map[string]any, error)
	Create(ctx context.Context, obj map[string]any) (map[string]any, error)
	// PatchStatus merges patch into the Incident's status subresource. A patch whose
	// metadata.resourceVersion is set is conditioned on it.
	PatchStatus(ctx context.Context, namespace, name string, patch map[string]any) error
	// Patch merges patch into the Incident.
	Patch(ctx context.Context, namespace, name string, patch map[string]any) error
}

// code is the apiserver status code of err, or 0.
func code(err error) int {
	var s apierrors.APIStatus
	if errors.As(err, &s) {
		return int(s.Status().Code)
	}
	return 0
}

// NewKube returns a Kube that reads past any cache, with reader, and writes with c.
func NewKube(reader client.Reader, c client.Client) Kube {
	return &kube{reader: reader, client: c}
}

type kube struct {
	reader client.Reader
	client client.Client
}

func decode(obj map[string]any) (map[string]any, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	v, err := pyfmt.Decode(b)
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	return m, nil
}

func (k *kube) Get(ctx context.Context, namespace, name string) (map[string]any, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(IncidentGVK)
	if err := k.reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, u); err != nil {
		return nil, err
	}
	return decode(u.Object)
}

func (k *kube) List(ctx context.Context, namespace, selector string) ([]map[string]any, error) {
	sel, err := labels.Parse(selector)
	if err != nil {
		return nil, err
	}
	// Page by page, each page's prompts dropped before it is kept: what is held is one page and
	// about 1 KB per Incident, never every Incident whole (demo: 1957 Incidents, 37.8 MB of JSON,
	// OOMKilled the provider at startup under 256Mi when listed in one piece).
	var out []map[string]any
	cont := ""
	for {
		l := &unstructured.UnstructuredList{}
		l.SetGroupVersionKind(incidentListGVK)
		if err := k.reader.List(ctx, l, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel},
			client.Limit(listPage), client.Continue(cont)); err != nil {
			return nil, err
		}
		for i := range l.Items {
			unstructured.RemoveNestedField(l.Items[i].Object, "spec", "prompt")
			m, err := decode(l.Items[i].Object)
			if err != nil {
				return nil, err
			}
			out = append(out, m)
		}
		if cont = l.GetContinue(); cont == "" {
			return out, nil
		}
	}
}

// listPage is how many Incidents one List call reads.
const listPage = 100

func (k *kube) Create(ctx context.Context, obj map[string]any) (map[string]any, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(b); err != nil {
		return nil, err
	}
	if err := k.client.Create(ctx, u); err != nil {
		return nil, err
	}
	return decode(u.Object)
}

func (k *kube) PatchStatus(ctx context.Context, namespace, name string, patch map[string]any) error {
	u, p, err := mergePatch(namespace, name, patch)
	if err != nil {
		return err
	}
	return k.client.Status().Patch(ctx, u, p)
}

func (k *kube) Patch(ctx context.Context, namespace, name string, patch map[string]any) error {
	u, p, err := mergePatch(namespace, name, patch)
	if err != nil {
		return err
	}
	return k.client.Patch(ctx, u, p)
}

func mergePatch(namespace, name string, patch map[string]any) (*unstructured.Unstructured, client.Patch, error) {
	b, err := json.Marshal(patch)
	if err != nil {
		return nil, nil, err
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(IncidentGVK)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u, client.RawPatch(types.MergePatchType, b), nil
}
