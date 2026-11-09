// Package scriptcheck checks the howToFix scripts of an analysis before the writer stores them. A
// script that cannot run is worse than none: the incident controller runs precondition and verify
// every poll interval, an unknown result changes nothing, and the incident sits with a check that
// never answers. Four rules, each a failure seen on a live cluster:
//
//   - BashSyntax: bash parses the script. The parser is mvdan.cc/sh's bash dialect, in process:
//     the image is distroless and has no bash, and nothing the model wrote is ever executed here.
//   - JQSyntax: every jq program a script passes literally to jq parses (github.com/itchyny/gojq's
//     parser). Only the parse is checked, not the compile: gojq's builtins differ from jq's, so a
//     missing function is not proof of a broken filter, while a syntax error ("Invalid escape") is.
//   - ReadsAlert: a precondition or verify never reads the Alert CR (alerts.observability.krateo.io).
//     They test the root-cause object; the Alert is the signal, and once it is deleted or edited a
//     check that reads it answers unknown forever.
//   - CoreSuffix: no kind is named with a .core group suffix (nodes.core): kubectl rejects it with
//     "the server doesn't have a resource type".
//
// The extraction is conservative: a word that is not literal in the source (a variable, a command
// substitution) is not guessed at, so the rules refuse only what is certainly wrong.
package scriptcheck

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/itchyny/gojq"
	"mvdan.cc/sh/v3/syntax"
)

// Rule names one check.
type Rule string

const (
	BashSyntax Rule = "bash-syntax"
	JQSyntax   Rule = "jq-syntax"
	ReadsAlert Rule = "reads-alert"
	CoreSuffix Rule = "core-suffix"
)

// Problem is one rule a script breaks.
type Problem struct {
	// Script is the howToFix key: precondition, apply, verify or rollback.
	Script string
	Rule   Rule
	// Line is the 1-based line of the script the problem is on; 0 when it has none.
	Line   int
	Detail string
}

// String is the problem as the agent reads it in a correction request, and as status.error and
// missingContext quote it.
func (p Problem) String() string {
	where := p.Script
	if p.Line > 0 {
		where = fmt.Sprintf("%s, line %d", p.Script, p.Line)
	}
	return where + ": " + p.Detail
}

// Scripts are the howToFix scripts Check reads, in order. applyAction is not a script.
var Scripts = []string{"precondition", "apply", "verify", "rollback"}

// checks are the scripts the incident controller runs: the ones ReadsAlert applies to. apply and
// rollback are a human's, and a fix may well write the Alert.
var checks = map[string]bool{"precondition": true, "verify": true}

// Check is every problem of fix's scripts, in Scripts order. A script that is absent or not a string
// is the parser's business (report.Parse drops such a howToFix), not a problem here.
func Check(fix map[string]any) []Problem {
	var out []Problem
	for _, name := range Scripts {
		if src, ok := fix[name].(string); ok && src != "" {
			out = append(out, Script(name, src)...)
		}
	}
	return out
}

// Script is every problem of the script src, the howToFix key name.
func Script(name, src string) []Problem {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), name)
	if err != nil {
		line := 0
		if pe, ok := err.(syntax.ParseError); ok {
			line = int(pe.Pos.Line())
		}
		return []Problem{{Script: name, Rule: BashSyntax, Line: line,
			Detail: "bash cannot parse it (" + err.Error() + ")."}}
	}
	var out []Problem
	seen := map[string]bool{}
	add := func(p Problem) {
		key := fmt.Sprintf("%s|%d|%s", p.Rule, p.Line, p.Detail)
		if !seen[key] {
			seen[key] = true
			out = append(out, p)
		}
	}
	for _, prog := range jqPrograms(f) {
		if _, err := gojq.Parse(prog.Text); err != nil {
			add(Problem{Script: name, Rule: JQSyntax, Line: prog.Line, Detail: fmt.Sprintf(
				"the jq program %s does not parse (%s). Pass text to jq with --arg instead of writing it inside the filter.",
				quote(prog.Text), err)})
		}
	}
	for _, ref := range kindRefs(f) {
		if coreSuffix.MatchString(ref.Kind) {
			plain := ref.Kind[:strings.IndexByte(ref.Kind, '.')]
			add(Problem{Script: name, Rule: CoreSuffix, Line: ref.Line, Detail: fmt.Sprintf(
				"%s names a core kind with a .core suffix, which kubectl rejects: write %s.", quote(ref.Kind), quote(plain))})
		}
		if checks[name] && ref.Alert {
			add(Problem{Script: name, Rule: ReadsAlert, Line: ref.Line, Detail: fmt.Sprintf(
				"it reads the Alert CR (%s). A %s tests the root-cause object, never the Alert: the Alert is the signal, "+
					"and once it is deleted or edited the check answers unknown forever.", ref.Kind, name)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

func quote(s string) string {
	const limit = 200
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit]) + "…"
	}
	return "`" + s + "`"
}

// Program is a jq program a script passes literally to jq, and the line it is on.
type Program struct {
	Text string
	Line int
}

// JQPrograms are the jq programs of src that its jq calls pass literally, or nil when bash cannot
// parse src.
func JQPrograms(src string) []Program {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err != nil {
		return nil
	}
	return jqPrograms(f)
}

func jqPrograms(f *syntax.File) []Program {
	var out []Program
	syntax.Walk(f, func(n syntax.Node) bool {
		call, ok := n.(*syntax.CallExpr)
		if !ok || command(call) != "jq" {
			return true
		}
		if w := jqFilter(call.Args[1:]); w != nil {
			if text, ok := literal(w); ok {
				out = append(out, Program{Text: text, Line: int(w.Pos().Line())})
			}
		}
		return true
	})
	return out
}

// jqFilter is the word jq reads its program from, or nil when it reads it from a file (-f) or
// there is none. The program is jq's first positional argument; jq 1.7's options that take
// arguments are skipped with them.
func jqFilter(args []*syntax.Word) *syntax.Word {
	for i := 0; i < len(args); i++ {
		s, ok := literal(args[i])
		if !ok {
			return args[i] // not literal: an option or the program, unknowable either way
		}
		switch {
		case s == "--":
			if i+1 < len(args) {
				return args[i+1]
			}
			return nil
		case s == "--arg" || s == "--argjson" || s == "--slurpfile" || s == "--rawfile":
			i += 2
		case s == "--indent":
			i++
		case s == "-f" || s == "--from-file":
			return nil
		case strings.HasPrefix(s, "--"):
		case strings.HasPrefix(s, "-") && len(s) > 1:
			if strings.ContainsRune(s, 'f') {
				return nil
			}
			if strings.HasSuffix(s, "L") {
				i++ // -L takes the next argument; -Ldir carries it
			}
		default:
			return args[i]
		}
	}
	return nil
}

// command is the base name of the command call runs, "" when it is not literal.
func command(call *syntax.CallExpr) string {
	if len(call.Args) == 0 {
		return ""
	}
	s, ok := literal(call.Args[0])
	if !ok {
		return ""
	}
	return s[strings.LastIndexByte(s, '/')+1:]
}

// literal is the value bash gives w, and false when w is not literal in the source: it holds an
// expansion, a substitution or ANSI-C quoting.
func literal(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(unescape(p.Value, func(byte) bool { return true }))
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", false
			}
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			if p.Dollar {
				return "", false
			}
			for _, q := range p.Parts {
				l, ok := q.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(unescape(l.Value, func(c byte) bool { return strings.IndexByte("$`\"\\\n", c) >= 0 }))
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// unescape removes the backslashes bash removes: before any character unquoted, before $ ` " \ and
// a newline inside double quotes (escapes says which). A backslash-newline is removed whole.
func unescape(s string, escapes func(byte) bool) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && escapes(s[i+1]) {
			i++
			if s[i] != '\n' {
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

var (
	// coreSuffix is a core kind with a group suffix kubectl rejects: nodes.core, pods.v1.core.
	coreSuffix = regexp.MustCompile(`^[a-z][a-z0-9]*(\.v1)?\.core$`)
	// alertKind is the Alert CR by its full name, singular or plural, with or without its version.
	alertKind = regexp.MustCompile(`^alerts?(\.v1alpha1)?\.observability\.krateo\.io$`)
)

// kindRef is a kind a script names, and the line it is on. Alert says it is the Alert CR.
type kindRef struct {
	Kind  string
	Line  int
	Alert bool
}

// kindRefs are the Alert and .core kinds src names: the kind arguments of its kubectl calls (get
// pods, pods,nodes, deployments.apps/web, events --for nodes/n1) and its --raw API paths, and any
// other literal word that is the Alert's full name, with or without a /name, as in the assignment
// r=alerts.observability.krateo.io. A bare alert or alerts is the Alert CR only where kubectl reads
// it as a kind.
func kindRefs(f *syntax.File) []kindRef {
	var out []kindRef
	// kind is true for a kind argument of kubectl, false for any other literal word, where only the
	// Alert's full name is unambiguous: a name may end in .core, and alerts may be anything.
	add := func(spec string, line int, kind bool) {
		for _, part := range strings.Split(spec, ",") {
			k, _, _ := strings.Cut(part, "/")
			alert := alertKind.MatchString(k) || kind && (k == "alert" || k == "alerts")
			if alert || kind && coreSuffix.MatchString(k) {
				out = append(out, kindRef{Kind: k, Line: line, Alert: alert})
			}
		}
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.CallExpr:
			if command(n) == "kubectl" {
				for _, r := range kubectlKinds(n.Args[1:]) {
					add(r.spec, r.line, true)
				}
				out = append(out, kubectlRaw(n.Args[1:])...)
			}
		case *syntax.Word:
			if s, ok := literal(n); ok && !strings.ContainsAny(s, " \t\n") {
				add(s, int(n.Pos().Line()), false)
			}
		}
		return true
	})
	return out
}

// kubectlValueFlags are kubectl's flags that take the next argument as their value, when not
// written flag=value. An unknown flag is taken as a boolean.
var kubectlValueFlags = map[string]bool{
	"-n": true, "--namespace": true, "-o": true, "--output": true, "-l": true, "--selector": true,
	"--field-selector": true, "-c": true, "--container": true, "--context": true, "--kubeconfig": true,
	"-s": true, "--server": true, "--request-timeout": true, "--sort-by": true, "--template": true,
	"-L": true, "--label-columns": true, "--chunk-size": true, "--for": true, "--timeout": true,
	"--since": true, "--since-time": true, "--tail": true, "-f": true, "--filename": true, "-k": true,
	"--kustomize": true, "--subresource": true, "-p": true, "--patch": true, "--patch-file": true,
	"--type": true, "--as": true, "--as-group": true, "--as-uid": true, "--cluster": true, "--user": true,
	"--token": true, "--raw": true, "--replicas": true, "--current-replicas": true,
	"--resource-version": true, "--containers": true, "--limits": true, "--requests": true,
	"--overrides": true, "--field-manager": true, "--cascade": true, "--grace-period": true,
	"--image": true, "-v": true, "--v": true, "--max-log-requests": true, "--pod-running-timeout": true,
}

type spec struct {
	spec string
	line int
}

// kubectlArgs are kubectl's positional arguments and its flags' values (flag name -> values), for
// the literal arguments; a non-literal one is skipped.
func kubectlArgs(args []*syntax.Word) ([]spec, map[string][]spec) {
	var pos []spec
	flags := map[string][]spec{}
	for i := 0; i < len(args); i++ {
		s, ok := literal(args[i])
		line := int(args[i].Pos().Line())
		switch {
		case !ok:
			pos = append(pos, spec{})
		case strings.HasPrefix(s, "-") && len(s) > 1:
			name, value, hasValue := strings.Cut(s, "=")
			switch {
			case hasValue:
				flags[name] = append(flags[name], spec{value, line})
			case kubectlValueFlags[name] && i+1 < len(args):
				i++
				if v, ok := literal(args[i]); ok {
					flags[name] = append(flags[name], spec{v, int(args[i].Pos().Line())})
				}
			}
		default:
			pos = append(pos, spec{s, line})
		}
	}
	return pos, flags
}

// kubectlKinds are the resource arguments of a kubectl call: the kind argument of its verb (get
// <kind>, rollout status <kind>, auth can-i get <kind>), every type/name, and events --for.
func kubectlKinds(args []*syntax.Word) []spec {
	pos, flags := kubectlArgs(args)
	if len(pos) == 0 {
		return nil
	}
	at := 1 // get, describe, delete, patch, label, annotate, edit, scale, wait, explain, ...
	switch pos[0].spec {
	case "rollout", "set":
		at = 2
	case "auth":
		at = 3
	case "top", "logs", "exec", "cp", "port-forward", "attach", "cordon", "uncordon", "drain", "config", "api-resources":
		at = -1 // no kind argument: a name, a fixed word or none
	}
	var out []spec
	for i, p := range pos {
		if p.spec != "" && i > 0 && (i == at || strings.Contains(p.spec, "/")) {
			out = append(out, p)
		}
	}
	for _, f := range flags["--for"] {
		if strings.Contains(f.spec, "/") && !strings.HasPrefix(f.spec, "condition=") && !strings.HasPrefix(f.spec, "jsonpath=") {
			out = append(out, f)
		}
	}
	return out
}

// kubectlRaw are the Alert reads among a kubectl call's --raw paths.
func kubectlRaw(args []*syntax.Word) []kindRef {
	_, flags := kubectlArgs(args)
	var out []kindRef
	for _, f := range flags["--raw"] {
		if strings.Contains(f.spec, "/apis/observability.krateo.io/") &&
			(strings.Contains(f.spec, "/alerts/") || strings.HasSuffix(f.spec, "/alerts")) {
			out = append(out, kindRef{Kind: "--raw " + f.spec, Line: f.line, Alert: true})
		}
	}
	return out
}
