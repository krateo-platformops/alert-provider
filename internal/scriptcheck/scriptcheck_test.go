package scriptcheck

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// The scripts of the RCA instructions' first example (internal/report/instructions.txt): valid,
// so every rule must pass them untouched.
var valid = map[string]any{
	"precondition": `#!/usr/bin/env bash
# Holds while payments-api still has the 128Mi memory limit it was OOMKilled at.
d=$(kubectl get deployments.apps payments-api -n payments -o json --ignore-not-found) || exit 2
[[ -n $d ]] || exit 0
jq -e --arg c payments-api --arg m 128Mi 'any(.spec.template.spec.containers[];
  .name == $c and .resources.limits.memory == $m)' <<<"$d" >/dev/null
case $? in 0) exit 1 ;; 1) exit 0 ;; *) exit 2 ;; esac
`,
	"apply": `#!/usr/bin/env bash
# Raise payments-api's memory limit from 128Mi, where it was OOMKilled, to 512Mi.
set -euo pipefail
kubectl set resources deployments.apps payments-api -n payments -c payments-api --limits=memory=512Mi
`,
	"verify": `#!/usr/bin/env bash
# Fixed once payments-api is off the 128Mi limit and every replica runs the new spec.
d=$(kubectl get deployments.apps payments-api -n payments -o json) || exit 2
jq -e --arg c payments-api --arg m 128Mi 'any(.spec.template.spec.containers[];
    .name == $c and .resources.limits.memory != $m)
  and .status.observedGeneration >= .metadata.generation
  and .status.updatedReplicas == .spec.replicas
  and .status.availableReplicas == .spec.replicas' <<<"$d" >/dev/null
case $? in 0) exit 0 ;; 1) exit 1 ;; *) exit 2 ;; esac
`,
	"rollback": `#!/usr/bin/env bash
# Undo apply: put payments-api's memory limit back to 128Mi, the value read before the fix.
set -euo pipefail
kubectl set resources deployments.apps payments-api -n payments -c payments-api --limits=memory=128Mi
`,
}

func rules(ps []Problem) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, p.Script+"|"+string(p.Rule))
	}
	return out
}

func TestValidScriptsPassUntouched(t *testing.T) {
	if ps := Check(valid); len(ps) != 0 {
		t.Fatalf("problems in valid scripts: %v", ps)
	}
	more := map[string]string{
		"pull secret patch": `kubectl patch deployments.apps web -n shop --type merge \
  -p '{"spec":{"template":{"spec":{"imagePullSecrets":[{"name":"regcred"}]}}}}'`,
		"core kinds by plural":                    `kubectl get nodes n1 -o json || exit 2; kubectl get pods,serviceaccounts -A || exit 2`,
		"events for a node":                       `kubectl events --for nodes/n1 -o json || exit 2`,
		"wait for a condition":                    `kubectl wait --for=condition=Ready pods/p -n x --timeout=5s || exit 2`,
		"an object named alerts":                  `kubectl get configmaps alerts -n x -o json || exit 2`,
		"a namespace named alerts":                `kubectl get deployments.apps -n alerts -o json || exit 2`,
		"jq text via --arg":                       `jq -e --arg t 'DELETED' '.type == $t' <<<"$e"`,
		"jq reading the Alert's kind in an event": `jq -e --arg k Alert 'any(.items[]; .involvedObject.kind == $k)' <<<"$e"`,
		"flux alerts":                             `kubectl get alerts.notification.toolkit.fluxcd.io a -n flux-system -o json || exit 2`,
		"a comment naming the Alert":              "# not alerts.observability.krateo.io: the workload\nkubectl get pods -n x || exit 2",
	}
	for name, src := range more {
		if ps := Script("precondition", src); len(ps) != 0 {
			t.Errorf("%s: %v", name, ps)
		}
	}
}

// The four failures from krateo-057, each refused with its rule. MUTATION: skipping any one rule
// in Script fails its case here.
func TestEachRuleRefusesItsFixture(t *testing.T) {
	for _, c := range []struct {
		name, script, src string
		rule              Rule
		line              int
	}{
		{"broken bash", "precondition", "#!/usr/bin/env bash\nif [[ -n $d ]]; then\n  exit 1\n", BashSyntax, 2},
		{"bad jq escape", "verify", "#!/usr/bin/env bash\nd=$(kubectl get pods -n x -o json) || exit 2\njq -e '.items[0].metadata.name | test(\"web\\-1\")' <<<\"$d\"\n", JQSyntax, 3},
		{"the Alert CR", "precondition", "#!/usr/bin/env bash\nw=$(kubectl get alerts.observability.krateo.io a -n krateo-system -o jsonpath='{.spec.where}') || exit 2\n", ReadsAlert, 2},
		{"nodes.core", "verify", "#!/usr/bin/env bash\nkubectl get nodes.core n1 -o json || exit 2\n", CoreSuffix, 2},
	} {
		ps := Script(c.script, c.src)
		if len(ps) != 1 || ps[0].Rule != c.rule || ps[0].Script != c.script || ps[0].Line != c.line {
			t.Errorf("%s: %v, want one %s on line %d", c.name, ps, c.rule, c.line)
		}
	}
}

func TestTheAlertCRIsRefusedInEveryForm(t *testing.T) {
	for _, src := range []string{
		"kubectl get alerts.observability.krateo.io a -n x -o json",
		"kubectl get alert.observability.krateo.io a -n x",
		"kubectl get alerts.v1alpha1.observability.krateo.io a -n x",
		"kubectl get alerts.observability.krateo.io/a -n x",
		"kubectl get -n x alerts a",
		"kubectl get alert a -n x",
		"kubectl get pods,alerts.observability.krateo.io -n x",
		"kubectl -n x get alerts/a -o json",
		"kubectl get --raw /apis/observability.krateo.io/v1alpha1/namespaces/x/alerts/a",
		"kubectl get --raw=/apis/observability.krateo.io/v1alpha1/alerts",
		"r=alerts.observability.krateo.io\nkubectl get \"$r\" a -n x",
		`kubectl get "alerts.observability.krateo.io" a -n x`,
		"d=$(kubectl get alerts.observability.krateo.io a -n x -o json) || exit 2",
		"kubectl events -n x --for alerts.observability.krateo.io/a",
	} {
		for _, script := range []string{"precondition", "verify"} {
			if ps := Script(script, src); len(ps) == 0 || ps[0].Rule != ReadsAlert {
				t.Errorf("%s %q: %v", script, src, ps)
			}
		}
	}
}

// apply and rollback are a human's: a fix may write the Alert (narrow its where).
func TestApplyAndRollbackMayTouchTheAlert(t *testing.T) {
	src := "w=$(kubectl get alerts.observability.krateo.io a -n x -o jsonpath='{.spec.where}')\n" +
		"kubectl patch alerts.observability.krateo.io a -n x --type merge -p \"$(jq -n --arg w \"$w\" '{spec: {where: $w}}')\""
	for _, script := range []string{"apply", "rollback"} {
		if ps := Script(script, src); len(ps) != 0 {
			t.Errorf("%s: %v", script, ps)
		}
	}
}

func TestACoreSuffixIsRefusedInEveryScriptAndForm(t *testing.T) {
	for _, src := range []string{
		"kubectl get nodes.core n1",
		"kubectl get pods.core -n x",
		"kubectl get pods.v1.core p -n x",
		"kubectl get nodes.core/n1 -o json",
		"kubectl describe deployments.apps/web,pods.core/p -n x",
		"kubectl rollout status nodes.core/n1",
		"kubectl events --for=nodes.core/n1",
		"kubectl auth can-i get pods.core -n x",
	} {
		for _, script := range Scripts {
			if ps := Script(script, src); len(ps) != 1 || ps[0].Rule != CoreSuffix {
				t.Errorf("%s %q: %v", script, src, ps)
			}
		}
	}
	// A name or any other word may end in .core; only a kind argument is a kind.
	for _, src := range []string{"kubectl get configmaps settings.core -n x", "echo redis.core", "h=db.core"} {
		if ps := Script("verify", src); len(ps) != 0 {
			t.Errorf("%q: %v", src, ps)
		}
	}
}

func TestEveryScriptIsChecked(t *testing.T) {
	broken := map[string]any{}
	for _, s := range Scripts {
		broken[s] = "if true; then\n"
	}
	if got, want := rules(Check(broken)), []string{"precondition|bash-syntax", "apply|bash-syntax", "verify|bash-syntax", "rollback|bash-syntax"}; !cmp.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if ps := Check(map[string]any{"precondition": 1, "applyAction": map[string]any{"verb": "patch"}}); len(ps) != 0 {
		t.Errorf("non-scripts checked: %v", ps)
	}
}

func TestAProblemSaysWhereAndWhatToDo(t *testing.T) {
	ps := Script("verify", "kubectl get nodes.core n1 || exit 2")
	if len(ps) != 1 {
		t.Fatal(ps)
	}
	if got := ps[0].String(); got != "verify, line 1: `nodes.core` names a core kind with a .core suffix, which kubectl rejects: write `nodes`." {
		t.Errorf("%q", got)
	}
	ps = Script("precondition", "jq -e '\"\\-\"' <<<x")
	if len(ps) != 1 || !strings.Contains(ps[0].String(), "precondition, line 1: the jq program `\"\\-\"` does not parse (invalid escape sequence") {
		t.Errorf("%v", ps)
	}
}

// The programs are jq's first positional argument, literal in the source; options and their
// arguments are skipped, and a program from a file or a variable is not guessed at.
func TestJQProgramsAreExtractedConservatively(t *testing.T) {
	for _, c := range []struct {
		src  string
		want []Program
	}{
		{`jq -e '.a' <<<"$d"`, []Program{{".a", 1}}},
		{`jq -er --arg c x --argjson n 3 '.a == $c' f.json`, []Program{{".a == $c", 1}}},
		{`jq --slurpfile s f --rawfile r g -n '$s'`, []Program{{"$s", 1}}},
		{`jq --indent 4 '.'`, []Program{{".", 1}}},
		{`jq -L lib -n 'include "x"; .'`, []Program{{`include "x"; .`, 1}}},
		{`jq -Llib '.b'`, []Program{{".b", 1}}},
		{`jq -- '.c'`, []Program{{".c", 1}}},
		{`jq ".d == \"x\" and \$v"`, []Program{{`.d == "x" and $v`, 1}}},
		{`jq .e`, []Program{{".e", 1}}},
		{"x=$(kubectl get pods -o json | jq -r '.items[].metadata.name')", []Program{{".items[].metadata.name", 1}}},
		{"a=1\nif jq -e '.f' <<<\"$a\"; then exit 1; fi", []Program{{".f", 2}}},
		{"/usr/bin/jq '.g'", []Program{{".g", 1}}},
		{`jq -f prog.jq data.json`, nil},
		{`jq -nf prog.jq`, nil},
		{`jq "$filter" <<<x`, nil},
		{`jq ".a == $v" <<<x`, nil},
		{`jq $'.a\n' <<<x`, nil},
		{`jq -e`, nil},
		{`echo jq '.h'`, nil},
		{`if true; then`, nil},
	} {
		if got := JQPrograms(c.src); !cmp.Equal(got, c.want) {
			t.Errorf("%q: got %v, want %v", c.src, got, c.want)
		}
	}
}

// guardedRead is the guarded form of an Alert read: --ignore-not-found, then exit 0 on empty.
const guardedRead = `#!/usr/bin/env bash
# Holds while the Alert's where does not exclude svc-a.
w=$(kubectl get alerts.observability.krateo.io a -n krateo-system -o jsonpath='{.spec.where}' --ignore-not-found) || exit 2
[[ -n $w ]] || exit 0
[[ $w == *"'svc-a'"* ]] || exit 1
exit 0
`

// A precondition or verify may read the Alert guarded: a deleted Alert then exits 0, never 2.
func TestAGuardedAlertReadPasses(t *testing.T) {
	for name, src := range map[string]string{
		"[[ -n ]] ||":       guardedRead,
		"[[ -z ]] &&":       "w=$(kubectl get alerts.observability.krateo.io a -n x -o json --ignore-not-found) || exit 2\n[[ -z \"$w\" ]] && exit 0\nexit 1",
		"if [[ -z ]]":       "w=$(kubectl get alert.observability.krateo.io a -n x -o json --ignore-not-found=true) || exit 2\nif [[ -z ${w} ]]; then exit 0; fi\nexit 1",
		"[ -n ] ||":         "o=$(kubectl -n x get alerts.observability.krateo.io/a -o json --ignore-not-found)\n[ -n \"$o\" ] || exit 0\nexit 1",
		"test -z &&":        "o=$(kubectl get alerts a -n x --ignore-not-found -o json) || exit 2\ntest -z \"$o\" && exit 0\nexit 1",
		"two guarded reads": guardedRead + "o=$(kubectl get alerts.observability.krateo.io a -n krateo-system -o json --ignore-not-found) || exit 2\n[[ -n $o ]] || exit 0\n",
	} {
		for _, script := range []string{"precondition", "verify"} {
			if ps := Script(script, src); len(ps) != 0 {
				t.Errorf("%s %s: %v", name, script, ps)
			}
		}
	}
}

// An Alert read without the full guard is refused: the checker recognises one shape only, and when
// unsure it refuses.
// MUTATION: accepting any kubectl read of the Alert (ignoring the guard) fails this.
func TestAnUnguardedAlertReadIsRefused(t *testing.T) {
	for name, src := range map[string]string{
		"no guard at all":          "w=$(kubectl get alerts.observability.krateo.io a -n x -o json) || exit 2\nexit 1",
		"no --ignore-not-found":    "w=$(kubectl get alerts.observability.krateo.io a -n x -o json) || exit 2\n[[ -n $w ]] || exit 0",
		"--ignore-not-found=false": "w=$(kubectl get alerts.observability.krateo.io a -n x --ignore-not-found=false) || exit 2\n[[ -n $w ]] || exit 0",
		"not into a variable":      "kubectl get alerts.observability.krateo.io a -n x --ignore-not-found || exit 2\nexit 1",
		"piped":                    "w=$(kubectl get alerts.observability.krateo.io a -n x -o json --ignore-not-found | jq .spec) || exit 2\n[[ -n $w ]] || exit 0",
		"a comma list":             "w=$(kubectl get pods,alerts.observability.krateo.io -n x --ignore-not-found) || exit 2\n[[ -n $w ]] || exit 0",
		"inside a condition":       "if true; then\nw=$(kubectl get alerts.observability.krateo.io a -n x --ignore-not-found) || exit 2\n[[ -n $w ]] || exit 0\nfi",
		"describe":                 "w=$(kubectl describe alerts.observability.krateo.io a -n x --ignore-not-found) || exit 2\n[[ -n $w ]] || exit 0",
		"--raw":                    "w=$(kubectl get --raw /apis/observability.krateo.io/v1alpha1/namespaces/x/alerts/a) || exit 2\n[[ -n $w ]] || exit 0",
		"through a variable":       "r=alerts.observability.krateo.io\nw=$(kubectl get \"$r\" a -n x --ignore-not-found) || exit 2\n[[ -n $w ]] || exit 0",
	} {
		for _, script := range []string{"precondition", "verify"} {
			if ps := Script(script, src); len(ps) == 0 || ps[0].Rule != ReadsAlert {
				t.Errorf("%s %s: %v", name, script, ps)
			}
		}
	}
}

// --ignore-not-found without an exit 0 on the empty result right after is refused: the deleted
// Alert would still fall through to a verdict or an unknown.
// MUTATION: accepting a read with --ignore-not-found whatever follows it fails this.
func TestAHalfGuardedAlertReadIsRefused(t *testing.T) {
	read := "w=$(kubectl get alerts.observability.krateo.io a -n x -o json --ignore-not-found) || exit 2\n"
	for name, next := range map[string]string{
		"nothing":              "exit 1",
		"exit 2 on empty":      "[[ -n $w ]] || exit 2",
		"exit 1 on empty":      "[[ -z $w ]] && exit 1",
		"-z with ||":           "[[ -z $w ]] || exit 0",
		"-n with &&":           "[[ -n $w ]] && exit 0",
		"another variable":     "[[ -n $o ]] || exit 0",
		"something in between": "echo read\n[[ -n $w ]] || exit 0",
		"an else":              "if [[ -z $w ]]; then exit 0; else exit 1; fi",
		"a default expansion":  "[[ -n ${w:-x} ]] || exit 0",
		"negated":              "! [[ -n $w ]] || exit 0",
	} {
		if ps := Script("precondition", read+next); len(ps) == 0 || ps[0].Rule != ReadsAlert {
			t.Errorf("%s: %v", name, ps)
		}
	}
}
