package alert

import "github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"

// DashboardPrefix starts the name of every Alert's single-tile HyperDX dashboard.
const DashboardPrefix = "krateo-alert-"

// HyperDXName is the name of the HyperDX alert of the Alert name in namespace, given the
// provider's home namespace. It is how the reconcile finds, adopts, pushes and releases the
// Alert's HyperDX alert, so every name comparison goes through it.
//
// An Alert in the home namespace is named by its bare metadata.name, the name every release before
// tenant-namespace Alerts gave it: the HyperDX objects of a live install are adopted as they are.
// An Alert in any other namespace is <namespace>/<name>. Neither a namespace nor an object name can
// contain "/", so a qualified name never equals a home one, nor another namespace's: two
// namespaces' llm-budget are team-a/llm-budget and team-b/llm-budget. It is at most
// 63 + 1 + 253 = 317 characters, under HyperDX's 512 for an alert name.
func HyperDXName(home, namespace, name string) string {
	if namespace == home {
		return name
	}
	return namespace + "/" + name
}

// DashboardName is the name of the Alert's HyperDX dashboard, which also names its one tile.
func DashboardName(home, namespace, name string) string {
	return DashboardPrefix + HyperDXName(home, namespace, name)
}

func hyperdxName(home string, a *v1alpha1.Alert) string {
	return HyperDXName(home, a.Namespace, a.Name)
}

func dashboardName(home string, a *v1alpha1.Alert) string {
	return DashboardName(home, a.Namespace, a.Name)
}
