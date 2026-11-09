package alert

import (
	"context"
	"sync"
	"time"

	"github.com/krateo-platformops/alert-provider/internal/hyperdx"
)

// alertsList is the HyperDX alerts list every Alert's pass shares: one GET /api/v2/alerts serves
// every Observe for up to ttl, where each Alert finds its own alert by id or name as it would in
// a list of its own. HyperDX rate-limits its API, and a list per Alert per poll is what it
// refused.
//
// Any write the controller makes to the alerts or the dashboards drops it (invalidate), so a pass
// never reads a list from before a write of this process: an Alert that just created its HyperDX
// alert finds it on its next pass instead of creating a second one. What it can miss is another
// writer's change, and the live state, for at most ttl: the poll interval, the granularity the
// state is mirrored at anyway.
//
// Concurrent readers of an expired list share one fetch (single flight). The lists it returns are
// shared: read them, never change them.
type alertsList struct {
	ttl time.Duration
	now func() time.Time

	mu     sync.Mutex
	gen    uint64 // bumped by invalidate; a fetch started under an older one is not kept
	list   []map[string]any
	at     time.Time // when the kept list's fetch started
	kept   bool
	flight *fetch
	joined int // readers that waited on another's fetch, for the tests
}

// fetch is one GET in flight; done closes when list and err are set.
type fetch struct {
	done chan struct{}
	gen  uint64
	list []map[string]any
	err  error
}

func newAlertsList(ttl time.Duration) *alertsList {
	return &alertsList{ttl: ttl, now: time.Now}
}

// get is the alerts list, and whether this caller made no call for it: a list fetched less than
// ttl ago, or one another caller was fetching. A failed fetch is not kept, and every caller that
// waited on it gets its error.
func (l *alertsList) get(ctx context.Context, hdx *hyperdx.Client) ([]map[string]any, bool, error) {
	l.mu.Lock()
	if l.kept && l.now().Sub(l.at) < l.ttl {
		list := l.list
		l.mu.Unlock()
		return list, true, nil
	}
	if f := l.flight; f != nil {
		l.joined++
		l.mu.Unlock()
		select {
		case <-f.done:
			return f.list, true, f.err
		case <-ctx.Done():
			return nil, true, ctx.Err()
		}
	}
	f := &fetch{done: make(chan struct{}), gen: l.gen}
	l.flight = f
	started := l.now()
	l.mu.Unlock()

	f.list, f.err = hdx.ListAlerts(ctx)

	l.mu.Lock()
	if l.flight == f {
		l.flight = nil
	}
	if f.err == nil && f.gen == l.gen {
		l.list, l.at, l.kept = f.list, started, true
	}
	l.mu.Unlock()
	close(f.done)
	return f.list, false, f.err
}

// invalidate drops the list, and detaches a fetch in flight: it may have been answered before the
// write, so the next reader fetches anew and the detached answer is not kept.
func (l *alertsList) invalidate() {
	l.mu.Lock()
	l.gen++
	l.list, l.kept, l.flight = nil, false, nil
	l.mu.Unlock()
}
