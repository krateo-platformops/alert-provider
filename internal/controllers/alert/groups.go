package alert

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/krateo-platformops/alert-provider/apis/alert/v1alpha1"
	"github.com/krateo-platformops/alert-provider/internal/hyperdx"
)

// clock is the time a pass computes status.groups at.
var clock = time.Now

// intervals are the lengths of the intervals the CRD admits.
var intervals = map[string]time.Duration{"1m": time.Minute, "5m": 5 * time.Minute, "15m": 15 * time.Minute,
	"30m": 30 * time.Minute, "1h": time.Hour, "6h": 6 * time.Hour, "12h": 12 * time.Hour, "1d": 24 * time.Hour}

// Window is the range status.groups reads at now: n completed intervals, aligned to the epoch as
// ClickHouse's toStartOfInterval aligns HyperDX's alert buckets, judged one interval late. The
// interval now falls in has only begun; the one before it has ended but may not be ingested yet,
// and judging it would drop a breaching group for a pass, then list it again. So the window ends
// at the start of that interval, and status.groups trails HyperDX's state by one interval, on
// purpose. With 1m intervals and n 5, at 12:07:30 it is [12:01, 12:06).
func Window(now time.Time, interval time.Duration, n int) (start, end time.Time) {
	ms := interval.Milliseconds()
	end = time.UnixMilli(now.UnixMilli()/ms*ms - ms).UTC()
	return end.Add(-time.Duration(n) * interval), end
}

// series is one group's evaluation over the window.
type series struct {
	group  []string
	values []*float64 // per interval; nil where the series has no data
}

// Groups are status.groups from the buckets of the window that starts at start: the groups whose
// value breaches the threshold in each of its n intervals, HyperDX's per-group condition for
// firing with numConsecutiveWindows n. An interval with no value is no breach, as HyperDX skips a
// gap and so restarts the count. Within one interval, a breaching row wins over another row of the
// same group, as HyperDX keeps a bucket's worst case. A group already in prev keeps its since. The
// second value is how many groups past v1alpha1.MaxGroups are left out.
func Groups(spec v1alpha1.AlertSpec, prev []v1alpha1.AlertGroup, buckets []hyperdx.Bucket, start time.Time, interval time.Duration, n int) ([]v1alpha1.AlertGroup, int32) {
	thr, kind := threshold(spec), spec.ThresholdType
	breaches := func(v float64) bool {
		b, _ := Exceeds(v, kind, thr, spec.ThresholdMax)
		return b
	}
	all := map[string]*series{}
	for _, b := range buckets {
		i := int(b.Start.Sub(start) / interval)
		if !b.Has || b.Start.Before(start) || i >= n {
			continue
		}
		key := strings.Join(b.Group, "\x00")
		s := all[key]
		if s == nil {
			s = &series{group: b.Group, values: make([]*float64, n)}
			all[key] = s
		}
		if old := s.values[i]; old == nil || !breaches(*old) || breaches(b.Value) {
			v := b.Value
			s.values[i] = &v
		}
	}

	var out []v1alpha1.AlertGroup
	for _, s := range all {
		firing := true
		for _, v := range s.values {
			if v == nil || !breaches(*v) {
				firing = false
				break
			}
		}
		if !firing {
			continue
		}
		g := v1alpha1.AlertGroup{Group: map[string]string{}, Value: number(*s.values[n-1]),
			Since: start.UTC().Format(time.RFC3339)}
		for i, k := range spec.GroupBy {
			g.Group[k] = s.group[i]
		}
		for _, p := range prev {
			if sameGroup(p.Group, g.Group) && p.Since != "" {
				g.Since = p.Since
				break
			}
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		for _, k := range spec.GroupBy {
			if a, b := out[i].Group[k], out[j].Group[k]; a != b {
				return a < b
			}
		}
		return false
	})
	if len(out) <= v1alpha1.MaxGroups {
		return out, 0
	}
	var left int32 // counted rather than converted from an int len (gosec G115)
	for range out[v1alpha1.MaxGroups:] {
		left++
	}
	return out[:v1alpha1.MaxGroups], left
}

func sameGroup(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func number(f float64) json.Number { return json.Number(strconv.FormatFloat(f, 'f', -1, 64)) }

// groups writes status.groups of a grouped Alert, from the same select and groupBy its tile
// evaluates: HyperDX's v2 API answers only the alert's rolled-up state, and never resolves a
// series that stops reporting. An ungrouped Alert has none. A failed query leaves them as they
// were and is the pass's error.
func (e *external) groups(ctx context.Context, a *v1alpha1.Alert) error {
	if !a.Grouped() {
		a.Status.Groups, a.Status.GroupsTruncated = nil, 0
		return nil
	}
	name := a.Spec.Interval
	if name == "" {
		name = "5m"
	}
	interval, ok := intervals[name]
	if !ok { // the CRD's enum admits no other value
		return fmt.Errorf("status.groups: interval %q is not one of the CRD's", name)
	}
	n := a.Windows()
	start, end := Window(clock(), interval, n)
	buckets, err := e.hdx.GroupBuckets(ctx, e.tileSource, e.sel, name, start, end)
	if err != nil {
		return failed(a, fmt.Errorf("status.groups: %w", err))
	}
	a.Status.Groups, a.Status.GroupsTruncated = Groups(a.Spec, a.Status.Groups, buckets, start, interval, n)
	return nil
}
