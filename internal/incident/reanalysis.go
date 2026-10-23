package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/krateo-platformops/alert-provider/internal/pyfmt"
	"github.com/krateo-platformops/alert-provider/internal/report"
)

// analysisKeys are the status fields an analysis writes, and the check results that judge its
// scripts. A reanalysis clears them all: they describe the previous analysis.
var analysisKeys = []string{"report", "rootCause", "sources", "reasoningTrace", "analyzedResources",
	"missingContext", "assumptions", "howToFix", "error", "completedAt", "lastChecks", "verifyingSince"}

// reproduced is the condition the controller sets on an analysis' first precondition run.
const reproduced = "Reproduced"

func spec(obj map[string]any) map[string]any {
	m, _ := obj["spec"].(map[string]any)
	return m
}

// Reanalyze consumes the incident's spec.reanalysis, a person's request for a new analysis. An
// Open or Verifying incident goes back to Analyzing, with the previous analysis and its check
// results cleared and the request recorded in status.reanalysis; then spec.reanalysis is removed
// and the RCA runs again under run, off the caller. Its prompt carries the previous analysis, its
// check results and the request's feedback. In any other state the request is removed unanswered:
// an Analyzing incident is already being analyzed, and a Resolved or Closed one is over.
func (w *Writer) Reanalyze(ctx, run context.Context, ns, name string) error {
	for range writeAttempts {
		incident, err := w.kube.Get(ctx, ns, name)
		if code(err) == 404 {
			return nil
		}
		if err != nil {
			return err
		}
		req, ok := spec(incident)["reanalysis"].(map[string]any)
		if !ok {
			return nil
		}
		if s, _ := state(incident); s != "Open" && s != "Verifying" {
			if err := w.dropRequest(ctx, ns, name); err != nil {
				return err
			}
			w.log.Info(fmt.Sprintf("[incident] %s/%s: reanalysis not run (state %s)", ns, name, s))
			return nil
		}

		feedback := str(req, "feedback")
		prev, _ := status(incident)["reanalysis"].(map[string]any)
		count := int64(1)
		if n, ok := prev["count"].(json.Number); ok {
			if i, err := n.Int64(); err == nil {
				count = i + 1
			}
		}
		st := map[string]any{"state": "Analyzing",
			"reanalysis": map[string]any{"at": pyfmt.Isoformat(w.now()), "feedback": nilIfEmpty(feedback), "count": count}}
		for _, k := range analysisKeys {
			st[k] = nil
		}
		if conds, ok := withoutCondition(status(incident)["conditions"], reproduced); ok {
			st["conditions"] = conds
		}
		prompt := ReanalysisPrompt(str(spec(incident), "prompt"), incident, feedback)
		if err := w.patchStatus(ctx, ns, incident, st); err != nil {
			if code(err) == 409 {
				continue
			}
			return err
		}
		w.log.Info(fmt.Sprintf("[incident] %s/%s: reanalysis %d requested, analyzing", ns, name, count))
		// The incident is Analyzing: a request left behind by a failure here is dropped as
		// unanswered on the next reconcile, and the analysis runs either way.
		if err := w.dropRequest(ctx, ns, name); err != nil {
			w.log.Info(fmt.Sprintf("[err] incident %s/%s: spec.reanalysis not removed (%s)", ns, name, err))
		}
		w.spawn(func() { w.RunAnalysis(run, ns, name, prompt, fmt.Sprintf("%s#reanalysis-%d", name, count)) })
		return nil
	}
	return fmt.Errorf("incident %s/%s: no status write succeeded in %d attempts", ns, name, writeAttempts)
}

func (w *Writer) dropRequest(ctx context.Context, ns, name string) error {
	err := w.kube.Patch(ctx, ns, name, map[string]any{"spec": map[string]any{"reanalysis": nil}})
	if code(err) == 404 {
		return nil
	}
	return err
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// withoutCondition is conds without the condition of type t, and whether it had one.
func withoutCondition(conds any, t string) ([]any, bool) {
	list, _ := conds.([]any)
	out := []any{}
	for _, c := range list {
		if m, ok := c.(map[string]any); ok && m["type"] == t {
			continue
		}
		out = append(out, c)
	}
	return out, len(out) != len(list)
}

// scriptOrder is the order a reanalysis prompt lists the previous scripts in.
var scriptOrder = []string{"precondition", "apply", "verify", "rollback"}

// ReanalysisPrompt is the RCA prompt of a reanalysis: the incident's original prompt with, before
// its output instructions, the previous analysis, the results its scripts gave, and the feedback.
func ReanalysisPrompt(original string, incident map[string]any, feedback string) string {
	base := strings.TrimSuffix(original, report.Instructions)
	st := status(incident)
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\n## This is a reanalysis\n\nA person asked for a new analysis of this incident. The previous one is below; " +
		"it may be wrong. Diagnose again from the live cluster rather than restating it, and write scripts that run. " +
		"A script whose last run gave no verdict (an exit other than 0 or 1) is broken: its output says why.")
	if feedback != "" {
		b.WriteString("\n\nWhat the person found wrong with the previous analysis:\n\n```text\n" + feedback + "\n```")
	}
	if rc, _ := st["rootCause"].(map[string]any); str(rc, "statement") != "" {
		b.WriteString("\n\nPrevious root cause: " + str(rc, "statement"))
	}
	if e := str(st, "error"); e != "" {
		b.WriteString("\n\nThe previous analysis failed: " + e)
	}
	if s, _ := state(incident); s != "" {
		b.WriteString("\n\nThe incident was " + s + ".")
	}
	if flagged(st) {
		b.WriteString(" Its precondition exited 0 on its first run: it did not see the problem the alert reported.")
	}
	fix, _ := st["howToFix"].(map[string]any)
	checks, _ := st["lastChecks"].(map[string]any)
	for _, name := range scriptOrder {
		script := str(fix, name)
		if script == "" {
			continue
		}
		b.WriteString("\n\nPrevious " + name + " script:\n\n```bash\n" + strings.TrimRight(script, "\n") + "\n```")
		if c, ok := checks[name].(map[string]any); ok {
			b.WriteString("\n\n" + lastRun(c))
		}
	}
	return b.String() + report.Instructions
}

// lastRun describes a lastChecks entry.
func lastRun(c map[string]any) string {
	out := "Its last run gave no exit code"
	if v, ok := c["exit"]; ok && v != nil {
		out = "Its last run exited " + pyfmt.Str(v)
	}
	if s := str(c, "since"); s != "" {
		out += " (since " + s + ")"
	}
	if o := str(c, "output"); o != "" {
		return out + " and printed:\n\n```text\n" + o + "\n```"
	}
	return out + "."
}

// flagged says the incident's first precondition run exited 0.
func flagged(st map[string]any) bool {
	list, _ := st["conditions"].([]any)
	for _, c := range list {
		if m, ok := c.(map[string]any); ok && m["type"] == reproduced && m["status"] == "False" {
			return true
		}
	}
	return false
}
