// Package v1alpha1 contains the Alert API of the observability.krateo.io group.
// +kubebuilder:object:generate=true
// +groupName=observability.krateo.io
// +versionName=v1alpha1
package v1alpha1

import (
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Package type metadata.
const (
	Group   = "observability.krateo.io"
	Version = "v1alpha1"
)

var (
	// SchemeGroupVersion is group version used to register these objects.
	SchemeGroupVersion = schema.GroupVersion{Group: Group, Version: Version}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme. It is apimachinery's
	// builder: controller-runtime's scheme.Builder is deprecated since v0.24.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
)

var (
	AlertKind             = reflect.TypeOf(Alert{}).Name()
	AlertGroupKind        = schema.GroupKind{Group: Group, Kind: AlertKind}.String()
	AlertKindAPIVersion   = AlertKind + "." + SchemeGroupVersion.String()
	AlertGroupVersionKind = SchemeGroupVersion.WithKind(AlertKind)
)

var (
	MetricKind             = reflect.TypeOf(Metric{}).Name()
	MetricGroupVersionKind = SchemeGroupVersion.WithKind(MetricKind)
)

// addKnownTypes registers the types as controller-runtime's scheme.Builder.Register did.
func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion, &Alert{}, &AlertList{}, &Metric{}, &MetricList{})
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
