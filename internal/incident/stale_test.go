package incident

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/alert-provider/internal/pyfmt"
	"github.com/krateo-platformops/alert-provider/internal/tracing/tracingtest"
)

const day = 24 * time.Hour

// newWindowCase is newCase with a cover window.
func newWindowCase(t *testing.T, window time.Duration) *writerCase {
	c := newCase(t)
	c.w.cfg.CoverWindow = window
	return c
}

// opened is the one incident of the alert other than the seeded ones in seeded.
func (c *writerCase) opened(seeded ...string) map[string]any {
	c.t.Helper()
	items, err := c.kube.List(context.Background(), ns, LabelAlert+"="+alertName)
	if err != nil {
		c.t.Fatal(err)
	}
	var found []map[string]any
	for _, i := range items {
		name := str(metadata(i), "name")
		if !strings.Contains(" "+strings.Join(seeded, " ")+" ", " "+name+" ") {
			found = append(found, i)
		}
	}
	if len(found) != 1 {
		c.t.Fatalf("%d new incidents, want 1", len(found))
	}
	return found[0]
}

func annotation(obj map[string]any, key string) string {
	a, _ := metadata(obj)["annotations"].(map[string]any)
	s, _ := a[key].(string)
	return s
}

// An Open incident opened 25h ago that has covered nothing since is stale: the comparison never
// sees it, even though the model would name it, so the firing opens a new incident and its RCA.
// MUTATION: ignoring CoverWindow in stale fails this; passing stale incidents to the comparison
// fails this.
func TestAStaleOpenIncidentIsNotComparedAndTheFiringOpensANewIncidentWithAnRCA(t *testing.T) {
	c := newWindowCase(t, day)
	old := alertName + "-old"
	c.analyzed(old, ago(25*time.Hour), "")
	c.match = old
	c.fire(fireOpt{})
	eq(t, "compared", len(c.compared), 0)
	eq(t, "covered", c.took(old), 0)
	eq(t, "incidents", c.kube.count(), 2)
	eq(t, "rca", len(c.rca), 1)
	eq(t, "new incident state", st(c.opened(old))["state"], "Open")
	// The stale one is left to the incident controller.
	eq(t, "stale untouched", len(c.kube.objectPatches(old)), 0)
	eq(t, "stale state", st(c.kube.get(ns, old))["state"], "Open")
}

// The new incident names the stale incidents it superseded, newest first, at most five.
// MUTATION: not writing AnnotationSupersedesStale fails this.
func TestTheNewIncidentNamesTheStaleIncidentsItSupersedes(t *testing.T) {
	c := newWindowCase(t, day)
	var seeded []string
	for h := 25; h < 32; h++ { // seven stale ones, the newest 25h old
		name := fmt.Sprintf("%s-h%d", alertName, h)
		c.analyzed(name, ago(time.Duration(h)*time.Hour), "")
		seeded = append(seeded, name)
	}
	c.fire(fireOpt{})
	eq(t, "supersedes", annotation(c.opened(seeded...), AnnotationSupersedesStale), strings.Join(seeded[:5], ","))
}

// Only the stale ones are left out: a fresh incident is still compared, and when it does not cover
// the firing the new incident names just the stale one.
func TestAFreshIncidentIsComparedBesideAStaleOne(t *testing.T) {
	c := newWindowCase(t, day)
	fresh, old := alertName+"-fresh", alertName+"-old"
	c.analyzed(old, ago(30*time.Hour), "")
	c.analyzed(fresh, ago(time.Hour), "")
	c.fire(fireOpt{}) // the model names none
	eq(t, "compared", c.compared, [][]string{{fresh}})
	eq(t, "supersedes", annotation(c.opened(old, fresh), AnnotationSupersedesStale), old)
}

// An incident opened 25h ago that covered a firing 1h ago is fresh: it is compared and covers.
func TestAnIncidentRecoveredInsideTheWindowIsStillComparedAndCovers(t *testing.T) {
	c := newWindowCase(t, day)
	old := alertName + "-old"
	c.analyzed(old, ago(25*time.Hour), "")
	c.kube.annotate(ns, old, AnnotationLastCoveredAt, ago(time.Hour))
	c.match = old
	c.fire(fireOpt{})
	eq(t, "compared", c.compared, [][]string{{old}})
	eq(t, "covered", c.took(old), 1)
	eq(t, "incidents", c.kube.count(), 1)
	eq(t, "rca", len(c.rca), 0)
}

// An incident opened 1h ago covers, as without a window.
func TestAnIncidentOpenedInsideTheWindowCovers(t *testing.T) {
	c := newWindowCase(t, day)
	inc := alertName + "-x"
	c.analyzed(inc, ago(time.Hour), "")
	c.match = inc
	c.fire(fireOpt{})
	eq(t, "compared", c.compared, [][]string{{inc}})
	eq(t, "covered", c.took(inc), 1)
	eq(t, "incidents", c.kube.count(), 1)
	eq(t, "rca", len(c.rca), 0)
}

// The window is CoverWindow: with 1h, an incident 2h old is stale and one 30m old covers.
func TestTheCoverWindowIsTheConfiguredOne(t *testing.T) {
	c := newWindowCase(t, time.Hour)
	old := alertName + "-old"
	c.analyzed(old, ago(2*time.Hour), "")
	c.match = old
	c.fire(fireOpt{})
	eq(t, "compared at 2h", len(c.compared), 0)
	eq(t, "incidents at 2h", c.kube.count(), 2)

	c = newWindowCase(t, time.Hour)
	c.analyzed(old, ago(30*time.Minute), "")
	c.match = old
	c.fire(fireOpt{})
	eq(t, "covered at 30m", c.took(old), 1)
	eq(t, "incidents at 30m", c.kube.count(), 1)
}

// A covering firing stamps its time on the incident, which keeps it fresh past the window from
// its creation.
// MUTATION: skipping recordCover fails this.
func TestACoveringFiringRecordsItsCoverTime(t *testing.T) {
	c := newWindowCase(t, day)
	inc := alertName + "-x"
	c.analyzed(inc, ago(2*time.Hour), "")
	c.match = inc
	before := time.Now().UTC().Add(-time.Second)
	c.fire(fireOpt{})
	eq(t, "covered", c.took(inc), 1)
	stamp := annotation(c.kube.get(ns, inc), AnnotationLastCoveredAt)
	at, ok := pyfmt.ParseTime(stamp)
	if !ok || at.Before(before.Truncate(time.Second)) || at.After(time.Now().UTC()) {
		t.Fatalf("%s = %q, want the firing's time", AnnotationLastCoveredAt, stamp)
	}
	eq(t, "status writes", len(c.kube.statusWrites()), 0)
	// The stamp alone keeps it a candidate once its creation is out of the window.
	c.kube.clear()
	c.analyzed(inc, ago(25*time.Hour), "")
	c.kube.annotate(ns, inc, AnnotationLastCoveredAt, stamp)
	c.fire(fireOpt{})
	eq(t, "covered past its creation's window", c.took(inc), 2)
}

// The cover time is rewritten only once it is older than a 24th of the window, so an alert firing
// every pass does not patch its incident every pass.
func TestTheCoverTimeIsNotRewrittenEveryFiring(t *testing.T) {
	c := newWindowCase(t, day)
	inc := alertName + "-x"
	c.analyzed(inc, ago(2*time.Hour), "")
	c.match = inc
	c.fire(fireOpt{})
	c.fire(fireOpt{})
	eq(t, "covered", c.took(inc), 2)
	eq(t, "cover-time patches", len(c.kube.objectPatches(inc)), 1)
}

// A Resolved incident inside its grace covers, but takes no cover time: it is not open.
func TestAResolvedIncidentTakesNoCoverTime(t *testing.T) {
	c := newWindowCase(t, day)
	c.ended(alertName+"-r", "Resolved", 30*time.Second, ago(2*time.Hour))
	c.fire(fireOpt{})
	eq(t, "covered", c.took(alertName+"-r"), 1)
	eq(t, "cover-time patches", len(c.kube.objectPatches(alertName+"-r")), 0)
}

// CoverWindow 0 is the behaviour before it: every open incident is compared, and nothing is written.
func TestAZeroCoverWindowComparesEveryOpenIncident(t *testing.T) {
	c := newWindowCase(t, 0)
	old := alertName + "-old"
	c.analyzed(old, ago(30*day), "")
	c.match = old
	c.fire(fireOpt{})
	eq(t, "compared", c.compared, [][]string{{old}})
	eq(t, "covered", c.took(old), 1)
	eq(t, "cover-time patches", len(c.kube.objectPatches(old)), 0)
}

// incident.fire counts the stale incidents the firing skipped.
func TestTheFiringSpanCountsTheStaleIncidentsItSkipped(t *testing.T) {
	rec := tracingtest.Install(t)
	c := newWindowCase(t, day)
	c.analyzed(alertName+"-a", ago(25*time.Hour), "")
	c.analyzed(alertName+"-b", ago(26*time.Hour), "")
	c.fire(fireOpt{})
	fire := rec.One("incident.fire")
	if attrOf(fire, "incident.stale_skipped") != "2" || attrOf(fire, "incident.opened") != "true" {
		t.Errorf("incident.fire attributes %v", fire.Attributes)
	}
}
