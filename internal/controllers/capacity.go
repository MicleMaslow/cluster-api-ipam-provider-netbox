/*
Copyright The cluster-api-ipam-provider-netbox Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/cluster-api/util/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

var (
	parentSize = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "netbox_ipam_parent_size_addresses",
		Help: "Addresses of a parent prefix of the networks.",
	}, []string{"parent"})
	parentFree = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "netbox_ipam_parent_free_addresses",
		Help: "Addresses of a parent prefix that no child prefix in NetBox covers.",
	}, []string{"parent"})
	parentFreePrefixes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "netbox_ipam_parent_free_prefixes",
		Help: "Prefixes of a length the networks of a parent prefix use that NetBox can still carve out of it.",
	}, []string{"parent", "length"})
)

func init() {
	crmetrics.Registry.MustRegister(parentSize, parentFree, parentFreePrefixes)
}

// parentCapacity sets the parent metrics every Period from the free child prefixes NetBox reports for the parent
// prefixes of the networks. It runs on the leader only.
type parentCapacity struct {
	Reader           client.Reader
	NetBox           *netbox.Client
	WatchFilterValue string
	Period           time.Duration
}

func (p *parentCapacity) Start(ctx context.Context) error {
	t := time.NewTicker(p.Period)
	defer t.Stop()
	for {
		if err := p.collect(ctx); err != nil && ctx.Err() == nil {
			ctrl.LoggerFrom(ctx).Error(err, "parent prefix capacity")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// collect replaces the parent metrics. A parent NetBox fails on loses its series until the next run.
func (p *parentCapacity) collect(ctx context.Context) error {
	list := &v1alpha1.NetBoxNetworkList{}
	if err := p.Reader.List(ctx, list); err != nil {
		return err
	}
	lengths := map[netip.Prefix]map[int]bool{}
	for i := range list.Items {
		nw := &list.Items[i]
		if p.WatchFilterValue != "" && !labels.HasWatchLabel(nw, p.WatchFilterValue) {
			continue
		}
		parent, err := netip.ParsePrefix(nw.Spec.ParentPrefix)
		if err != nil {
			continue
		}
		parent = parent.Masked()
		if lengths[parent] == nil {
			lengths[parent] = map[int]bool{}
		}
		// the length the network carves, or that of the prefix it adopted
		if nw.Spec.PrefixLength > 0 {
			lengths[parent][int(nw.Spec.PrefixLength)] = true
		} else if got, err := netip.ParsePrefix(nw.Status.Prefix); err == nil {
			lengths[parent][got.Bits()] = true
		}
	}
	parentSize.Reset()
	parentFree.Reset()
	parentFreePrefixes.Reset()
	var errs []error
	for parent, ls := range lengths {
		free, err := p.free(ctx, parent)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		label := parent.String()
		parentSize.WithLabelValues(label).Set(size(parent))
		parentFree.WithLabelValues(label).Set(sum(free))
		for l := range ls {
			parentFreePrefixes.WithLabelValues(label, strconv.Itoa(l)).Set(fits(free, l))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d of %d parents: %v", len(errs), len(lengths), errs)
	}
	return nil
}

// free returns the free child prefixes NetBox reports for parent, in the global VRF as the networks carve.
func (p *parentCapacity) free(ctx context.Context, parent netip.Prefix) ([]netip.Prefix, error) {
	found, err := p.NetBox.List(ctx, netbox.Prefixes, url.Values{"prefix": {parent.String()}, "vrf_id": {"null"}})
	if err != nil {
		return nil, err
	}
	if len(found) != 1 {
		return nil, fmt.Errorf("want one parent prefix %s in the global VRF, found %d", parent, len(found))
	}
	objs, err := p.NetBox.Available(ctx, netbox.AvailablePrefixes(found[0].ID))
	if err != nil {
		return nil, err
	}
	free := make([]netip.Prefix, 0, len(objs))
	for _, o := range objs {
		f, err := netip.ParsePrefix(o.Prefix)
		if err != nil {
			return nil, fmt.Errorf("parent %s: available prefix %q: %w", parent, o.Prefix, err)
		}
		free = append(free, f)
	}
	return free, nil
}

func size(p netip.Prefix) float64 { return math.Ldexp(1, p.Addr().BitLen()-p.Bits()) }

func sum(free []netip.Prefix) float64 {
	var n float64
	for _, f := range free {
		n += size(f)
	}
	return n
}

// fits counts the prefixes of length bits that the free prefixes hold: NetBox reports them aligned.
func fits(free []netip.Prefix, bits int) float64 {
	var n float64
	for _, f := range free {
		if f.Bits() <= bits {
			n += math.Ldexp(1, bits-f.Bits())
		}
	}
	return n
}
