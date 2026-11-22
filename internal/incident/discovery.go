package incident

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"

	"github.com/krateo-platformops/alert-provider/internal/scriptcheck"
)

// Discovery is the cluster's API discovery as scriptcheck.Unserved reads it, refreshed at most
// every ttl. A failed refresh keeps the last good view (logged); with none, it is nil and
// scriptcheck judges nothing: an unknown cluster never makes a script look wrong.
type Discovery struct {
	client discovery.DiscoveryInterface
	ttl    time.Duration
	log    logging.Logger
	now    func() time.Time

	mu   sync.Mutex
	at   time.Time
	last *scriptcheck.Discovery
}

// NewDiscovery returns a Discovery of client, refreshed at most every ttl.
func NewDiscovery(client discovery.DiscoveryInterface, ttl time.Duration, log logging.Logger) *Discovery {
	return &Discovery{client: client, ttl: ttl, log: log, now: time.Now}
}

// Get is the current view, refreshing it when it is older than ttl.
func (d *Discovery) Get(ctx context.Context) *scriptcheck.Discovery {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last != nil && d.now().Sub(d.at) < d.ttl {
		return d.last
	}
	view, err := read(d.client)
	if err != nil {
		logging.WithContext(d.log, ctx).Info(fmt.Sprintf("[scriptcheck] API discovery failed, keeping the previous view: %v", err))
		return d.last
	}
	d.last, d.at = view, d.now()
	return d.last
}

// read is one discovery of client. A group-version that fails discovery marks its group partial
// (scriptcheck never calls a partial group unserved); any other error is no view at all.
func read(client discovery.DiscoveryInterface) (*scriptcheck.Discovery, error) {
	_, lists, err := client.ServerGroupsAndResources()
	var failed []string
	if err != nil {
		var partial *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &partial) {
			return nil, err
		}
		for gv := range partial.Groups {
			failed = append(failed, gv.Group)
		}
	}
	var resources []scriptcheck.APIResource
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") { // a subresource (pods/log)
				continue
			}
			resources = append(resources, scriptcheck.APIResource{Group: gv.Group, Version: gv.Version,
				Plural: r.Name, Singular: r.SingularName, Kind: r.Kind, ShortNames: r.ShortNames})
		}
	}
	return scriptcheck.NewDiscovery(resources, failed), nil
}
