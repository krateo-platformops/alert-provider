package scriptcheck

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// UnservedGroup: a precondition or verify names a resource by <resource>.<group> (or
// <resource>.<version>.<group>) that this cluster does not serve: the group is not served at all,
// or does not serve that resource. Seen on a live cluster: a precondition and verify ran
// `kubectl get customresourcedefinitions.apiextensions.krateo.io …`, an invented group (CRDs are
// apiextensions.k8s.io), and exited 2 on every run, so the incident never had a verdict.
//
// It applies to the checks only: they run before and after the fix against the cluster as it is,
// so everything they read must be served now, while apply may legitimately create an API. Only a
// dotted reference is judged (a bare plural, kind or short name may be one a fix installs), and a
// group whose discovery failed is unknown, never unserved: the rule refuses only what discovery
// proves wrong.
const UnservedGroup Rule = "unserved-group"

// APIResource is one resource the cluster's API discovery lists.
type APIResource struct {
	Group      string // "" for the core group
	Version    string
	Plural     string
	Singular   string
	Kind       string
	ShortNames []string
}

// Discovery is what the cluster serves, as its API discovery reported it.
type Discovery struct {
	// names: group -> every name a resource of it answers to (plural, singular, lowercased kind,
	// short names), over all its served versions.
	names map[string]map[string]bool
	// versions: group -> its served versions.
	versions map[string]map[string]bool
	// partial: groups at least one of whose versions failed discovery.
	partial map[string]bool
}

// NewDiscovery is the Discovery of resources, with failedGroups the groups (any version) whose
// discovery failed. Names are compared lowercased, as kubectl resolves them.
func NewDiscovery(resources []APIResource, failedGroups []string) *Discovery {
	d := &Discovery{names: map[string]map[string]bool{}, versions: map[string]map[string]bool{}, partial: map[string]bool{}}
	for _, g := range failedGroups {
		d.partial[strings.ToLower(g)] = true
	}
	for _, r := range resources {
		g := strings.ToLower(r.Group)
		if d.names[g] == nil {
			d.names[g] = map[string]bool{}
			d.versions[g] = map[string]bool{}
		}
		d.versions[g][strings.ToLower(r.Version)] = true
		for _, n := range append([]string{r.Plural, r.Singular, r.Kind}, r.ShortNames...) {
			if n != "" {
				d.names[g][strings.ToLower(n)] = true
			}
		}
	}
	return d
}

// apiVersion is a Kubernetes API version, as in <resource>.<version>.<group>.
var apiVersion = regexp.MustCompile(`^v[0-9]+((alpha|beta)[0-9]+)?$`)

// unserved says whether d proves kind unserved, and the group it was read as. A kind without a dot,
// a .core one (CoreSuffix's), or one naming a group whose discovery failed is never unserved.
func (d *Discovery) unserved(kind string) (string, bool) {
	k := strings.ToLower(kind)
	name, rest, dotted := strings.Cut(k, ".")
	if !dotted || name == "" || rest == "" || coreSuffix.MatchString(k) {
		return "", false
	}
	// kubectl reads <resource>.<group> and <resource>.<version>.<group>: either reading served is
	// enough, and either reading's group being unknown is enough to stay silent.
	type reading struct{ group, version string }
	readings := []reading{{group: rest}}
	if v, g, ok := strings.Cut(rest, "."); ok && apiVersion.MatchString(v) {
		readings = append(readings, reading{group: g, version: v})
	}
	for _, r := range readings {
		if d.partial[r.group] {
			return "", false
		}
		if r.version != "" && !d.versions[r.group][r.version] {
			continue
		}
		if d.names[r.group][name] {
			return "", false
		}
	}
	return readings[len(readings)-1].group, true
}

// servedAs are the full names under which d serves name (pods, or deployments.apps), sorted.
func (d *Discovery) servedAs(name string) []string {
	var out []string
	for g, names := range d.names {
		if names[name] {
			if g == "" {
				out = append(out, name)
			} else {
				out = append(out, name+"."+g)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Unserved is every UnservedGroup problem of fix's precondition and verify against d, in Scripts
// order. A nil d checks nothing, and so does a script bash cannot parse (BashSyntax reports it).
func Unserved(fix map[string]any, d *Discovery) []Problem {
	if d == nil {
		return nil
	}
	var out []Problem
	for _, name := range Scripts {
		src, ok := fix[name].(string)
		if !ok || src == "" || !checks[name] {
			continue
		}
		f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), name)
		if err != nil {
			continue
		}
		seen := map[string]bool{}
		for _, ref := range resourceRefs(f) {
			group, bad := d.unserved(ref.spec)
			if !bad || seen[fmt.Sprintf("%d|%s", ref.line, ref.spec)] {
				continue
			}
			seen[fmt.Sprintf("%d|%s", ref.line, ref.spec)] = true
			plain := strings.ToLower(ref.spec[:strings.IndexByte(ref.spec, '.')])
			detail := fmt.Sprintf("%s names API group %s, which this cluster does not serve for %s, so kubectl answers "+
				"\"the server doesn't have a resource type\" and the script exits 2 on every run.", quote(ref.spec), quote(group), quote(plain))
			if alt := d.servedAs(plain); len(alt) > 0 {
				detail += " It is served as " + strings.Join(alt, ", ") + "."
			} else {
				detail += " Read only resources the cluster serves (kubectl api-resources)."
			}
			out = append(out, Problem{Script: name, Rule: UnservedGroup, Line: ref.line, Detail: detail})
		}
	}
	return out
}

// resourceRefs are the resource arguments of f's kubectl calls (see kubectlKinds), one per kind of
// a comma list, without a /name. kubectl explain is left out: its argument is a field path
// (pods.spec.containers), not a group.
func resourceRefs(f *syntax.File) []spec {
	var out []spec
	syntax.Walk(f, func(n syntax.Node) bool {
		call, ok := n.(*syntax.CallExpr)
		if !ok || command(call) != "kubectl" {
			return true
		}
		if pos, _ := kubectlArgs(call.Args[1:]); len(pos) > 0 && pos[0].spec == "explain" {
			return true
		}
		for _, r := range kubectlKinds(call.Args[1:]) {
			for _, part := range strings.Split(r.spec, ",") {
				k, _, _ := strings.Cut(part, "/")
				if k != "" {
					out = append(out, spec{spec: k, line: r.line})
				}
			}
		}
		return true
	})
	return out
}
