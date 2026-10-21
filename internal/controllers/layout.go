/*
Copyright 2026 The cluster-api-ipam-provider-netbox Authors.

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
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/layout"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netmath"
)

// maxPoolRanges is the number of NetBox ranges a pool may take, as status.layout allows.
const maxPoolRanges = 32

// poolOf is the pool name of size.
func poolOf(name string, size intstr.IntOrString) layout.Pool {
	if size.Type == intstr.String {
		return layout.Pool{Name: name, Rest: true}
	}
	return layout.Pool{Name: name, Size: size.IntValue()}
}

// sizeOf is the size of p as spec.pools has it.
func sizeOf(p layout.Pool) intstr.IntOrString {
	if p.Rest {
		return intstr.FromString("rest")
	}
	return intstr.FromInt32(int32(p.Size))
}

// layoutOf is the layout of status.layout.
func layoutOf(st v1alpha1.NetworkLayout) layout.Layout {
	var l layout.Layout
	for _, p := range st.Pools {
		pp := layout.PlacedPool{Pool: poolOf(p.Name, p.Size)}
		for _, r := range p.Ranges {
			from, ferr := netip.ParseAddr(r.Start)
			to, terr := netip.ParseAddr(r.End)
			if ferr == nil && terr == nil {
				pp.Ranges = append(pp.Ranges, layout.Span{From: from, To: to})
			}
		}
		l.Pools = append(l.Pools, pp)
	}
	for _, g := range st.Addresses {
		pg := layout.PlacedGroup{Group: layout.Group{Name: g.Name, Count: int(g.Count)}}
		for _, s := range g.Addresses {
			if a, err := netip.ParseAddr(s); err == nil {
				pg.Addrs = append(pg.Addrs, a)
			}
		}
		l.Groups = append(l.Groups, pg)
	}
	return l
}

// reconcileLayout places spec.pools and spec.addresses of nw in its prefix, again only when their names, sizes or counts
// change or while the VIP waits to be reserved, and brings the layout about in order: reserves the layout drops, then
// the InClusterIPPools (allocator InCluster), then the ranges of the pools, then the reserves it holds, then the
// Secrets the pools and groups publish in. A step that fails stops the next ones; a change that would move an
// allocated address out of its pool is refused as a whole. Once the ranges are there, the IPAddresses of the pools
// that sync go to NetBox (allocator InCluster). Its error is LayoutReady's. endpoint is the host of the Cluster's
// endpoint: never adopted by a group, as it is the VIP to be.
func (r *NetBoxNetworkReconciler) reconcileLayout(ctx context.Context, nw *v1alpha1.NetBoxNetwork, endpoint string) error {
	inCluster := nw.Spec.Allocator == v1alpha1.AllocatorInCluster
	if inCluster && !r.InCluster {
		return waitFor(v1alpha1.ReasonAllocatorUnavailable,
			errors.New("InClusterIPPool is not served: install cluster-api-ipam-provider-in-cluster and restart the provider"))
	}
	prefix, err := netip.ParsePrefix(nw.Status.Prefix)
	if err != nil {
		return err
	}
	pools := make([]layout.Pool, 0, len(nw.Spec.Pools))
	for _, p := range nw.Spec.Pools {
		pools = append(pools, poolOf(p.Name, p.Size))
	}
	groups := make([]layout.Group, 0, len(nw.Spec.Addresses))
	for _, g := range nw.Spec.Addresses {
		groups = append(groups, groupOf(g))
	}
	prev := layoutOf(nw.Status.Layout)
	if len(prev.Pools) == 0 && len(prev.Groups) == 0 {
		if prev, err = r.restore(ctx, nw, prefix, pools); err != nil {
			return err
		}
	}
	// The VIP takes the highest free address outside the ranges: until it is reserved, the layout keeps that one.
	wantVIP := nw.Spec.ControlPlaneEndpoint != nil && nw.Status.ControlPlaneEndpoint.Host == ""
	replan := wantVIP || !slices.EqualFunc(prev.Pools, pools, func(p layout.PlacedPool, s layout.Pool) bool { return p.Pool == s }) ||
		!slices.EqualFunc(prev.Groups, groups, func(p layout.PlacedGroup, g layout.Group) bool { return p.Group == g })
	// InClusterIPPools exclude what is in use now, so the space is read on every reconcile.
	var (
		s         layout.Space
		allocated []netip.Addr
	)
	if replan || inCluster {
		if s, err = r.space(ctx, nw, prefix); err != nil {
			return err
		}
		s.WantVIP = wantVIP
		if replan {
			s.Adopt = tail(s, pools, groups, prev, endpoint)
		}
		if allocated, err = r.allocatedIn(ctx, nw, prefix); err != nil {
			return err
		}
	}
	next := prev
	if replan {
		if next, err = layout.Plan(s, pools, groups, prev); err != nil {
			return withReason(v1alpha1.ReasonInvalidSpec, err)
		}
		for _, p := range next.Pools {
			if len(p.Ranges) > maxPoolRanges {
				return withReason(v1alpha1.ReasonInvalidSpec, fmt.Errorf("pool %s would take %d NetBox ranges around addresses in use, at most %d", p.Name, len(p.Ranges), maxPoolRanges))
			}
		}
		if moved := layout.Moved(prev, next, allocated); len(moved) > 0 {
			return retryLater(v1alpha1.ReasonRangeInUse, fmt.Errorf("spec.pools and spec.addresses would move allocated addresses out of their pool or reserve them: %s", listAddresses(moved)))
		}
		if !reflect.DeepEqual(next, prev) {
			ctrl.LoggerFrom(ctx).Info("Placed the pools and address groups of the network", "network", klog.KObj(nw), "pools", len(next.Pools), "groups", len(next.Groups))
		}
	}
	if err := r.dropExcessReserves(ctx, nw, next); err != nil {
		return err
	}
	var (
		conflict error // AddressConflict stops nothing
		live     map[string][]netip.Addr
	)
	if inCluster {
		live, _ = liveIn(next, s, allocated)
		if conflict = r.applyInClusterPools(ctx, nw, next, s, allocated); conflict != nil && errReason(conflict) != v1alpha1.ReasonAddressConflict {
			return conflict
		}
	}
	if err := r.applyRanges(ctx, nw, next); err != nil {
		return err
	}
	// Only what is applied is published: whoever reads the Secrets sees no address the layout has not reached.
	var publishErr, syncErr error
	reserveErr := r.applyReserves(ctx, nw, next)
	if reserveErr == nil {
		publishErr = r.publish(ctx, nw, publishData(nw, next, live))
	}
	if inCluster {
		syncErr = r.syncAddresses(ctx, nw)
	}
	return foldErrors([]error{reserveErr, conflict, publishErr, syncErr})
}

// restore takes the layout of nw back from its active NetBox ranges and its reserves, as after clusterctl move, and
// checks the pools against pools, whose sizes they then carry. Ranges retained by a deleted network of its name go;
// its reserves inside prefix are taken back, those outside retired. With allocator InCluster and no ranges, the
// InClusterIPPools of a running cluster give the positions of their pools (inClusterPositions), which lead pools.
func (r *NetBoxNetworkReconciler) restore(ctx context.Context, nw *v1alpha1.NetBoxNetwork, prefix netip.Prefix, pools []layout.Pool) (layout.Layout, error) {
	rangesRef := poolRangeRef(r.Keys, nw, "")
	found, err := r.NetBox.FindByRefPrefix(ctx, r.Keys, netbox.IPRanges, rangesRef)
	if err != nil {
		return layout.Layout{}, err
	}
	var (
		l        layout.Layout
		retained []netbox.Object
	)
	for _, o := range found {
		if o.Status.Value == "deprecated" {
			retained = append(retained, o)
			continue
		}
		from, to, err := netBoxBounds(o)
		if err != nil {
			return layout.Layout{}, err
		}
		name := strings.TrimPrefix(r.Keys.RefOf(o), rangesRef)
		i := slices.IndexFunc(l.Pools, func(p layout.PlacedPool) bool { return p.Name == name })
		if i < 0 {
			i = len(l.Pools)
			l.Pools = append(l.Pools, layout.PlacedPool{Pool: layout.Pool{Name: name}})
		}
		l.Pools[i].Ranges = append(l.Pools[i].Ranges, layout.Span{From: from, To: to})
	}
	if err := r.releaseRanges(ctx, nw, retained, v1alpha1.DeletionPolicyDelete); err != nil {
		return layout.Layout{}, err
	}
	reserves, err := r.NetBox.FindByRefPrefix(ctx, r.Keys, netbox.IPAddresses, reservesRef(r.Keys, nw))
	if err != nil {
		return layout.Layout{}, err
	}
	if err := r.retireReserves(ctx, nw, prefix, reserves); err != nil {
		return layout.Layout{}, err
	}
	l.Groups = restoreGroups(r.Keys, nw, prefix, reserves)
	from, want := "NetBox ranges", pools
	if len(l.Pools) == 0 && nw.Spec.Allocator == v1alpha1.AllocatorInCluster {
		if l.Pools, err = r.inClusterPositions(ctx, nw, prefix); err != nil {
			return layout.Layout{}, err
		}
		from, want = "InClusterIPPools", pools[:len(l.Pools)]
	}
	if len(l.Pools) == 0 {
		return l, nil
	}
	for _, p := range l.Pools {
		slices.SortFunc(p.Ranges, func(a, b layout.Span) int { return a.From.Compare(b.From) })
	}
	slices.SortFunc(l.Pools, func(a, b layout.PlacedPool) int { return a.Ranges[0].From.Compare(b.Ranges[0].From) })
	if err := layout.Check(l, want); err != nil {
		return layout.Layout{}, withReason(v1alpha1.ReasonInvalidSpec, fmt.Errorf("spec.pools do not match the %s of the network: %w", from, err))
	}
	for i := range l.Pools {
		l.Pools[i].Pool = pools[i]
	}
	ctrl.LoggerFrom(ctx).Info("Restored the layout of the network from NetBox", "network", klog.KObj(nw), "pools", len(l.Pools), "groups", len(l.Groups))
	return l, nil
}

// space is what the layout of nw is placed in: its prefix, where NetBox addresses and ranges that are not the network's
// are taken, and the VIP of its status, else the one NetBox has under its key (a status lost, the VIP not reserved again yet).
// A taken address without a key, alone at its address and in no range, can be adopted too (layout.Space.Adopt).
func (r *NetBoxNetworkReconciler) space(ctx context.Context, nw *v1alpha1.NetBoxNetwork, prefix netip.Prefix) (layout.Space, error) {
	l2, gw, err := l2Of(nw)
	if err != nil {
		return layout.Space{}, err
	}
	ips, ranges, err := r.netBoxIn(ctx, prefix, l2)
	if err != nil {
		return layout.Space{}, err
	}
	s := layout.Space{Prefix: prefix, L2: l2, Gateway: gw}
	if vip, err := netip.ParseAddr(nw.Status.ControlPlaneEndpoint.Host); err == nil {
		s.VIP = vip
	}
	own, claims, pools := r.Keys.Key("NetBoxNetwork", nw)+"/", r.Keys.KindPrefix("IPAddressClaim"), poolRangeRef(r.Keys, nw, "")
	var (
		keyless []netip.Addr
		held    = map[netip.Addr]int{}
		spans   []layout.Span
	)
	for _, ip := range ips {
		ref := r.Keys.RefOf(ip)
		p, err := netip.ParsePrefix(ip.Address)
		if err != nil {
			return layout.Space{}, err
		}
		held[p.Addr()]++
		if ref == "" {
			keyless = append(keyless, p.Addr())
		}
		switch {
		case ref == vipRef(r.Keys, nw):
			if !s.VIP.IsValid() {
				s.VIP = p.Addr()
			}
		case strings.HasPrefix(ref, own), strings.HasPrefix(ref, claims):
		default:
			s.Taken = append(s.Taken, layout.Span{From: p.Addr(), To: p.Addr()})
		}
	}
	for _, o := range ranges {
		from, to, err := netBoxBounds(o)
		if err != nil {
			return layout.Space{}, err
		}
		spans = append(spans, layout.Span{From: from, To: to})
		if !strings.HasPrefix(r.Keys.RefOf(o), pools) {
			s.Taken = append(s.Taken, layout.Span{From: from, To: to})
		}
	}
	for _, a := range keyless {
		if held[a] == 1 && !slices.ContainsFunc(spans, func(sp layout.Span) bool { return netmath.Contains(sp.From, sp.To, a) }) {
			s.Adopt = append(s.Adopt, a)
		}
	}
	return s, nil
}

// tail narrows s.Adopt to the addresses above the pools: those of prev, else those Plan places without adopting
// (all of them when it cannot place them); never endpoint, the host of the Cluster's endpoint.
func tail(s layout.Space, pools []layout.Pool, groups []layout.Group, prev layout.Layout, endpoint string) []netip.Addr {
	top := topOf(prev)
	if !top.IsValid() {
		bare := s
		bare.Adopt = nil
		if l, err := layout.Plan(bare, pools, groups, prev); err == nil {
			top = topOf(l)
		}
	}
	return slices.DeleteFunc(slices.Clone(s.Adopt), func(a netip.Addr) bool { return top.IsValid() && !top.Less(a) || a.String() == endpoint })
}

// topOf is the last address of the ranges of l, zero if it has none.
func topOf(l layout.Layout) netip.Addr {
	var top netip.Addr
	for _, p := range l.Pools {
		for _, r := range p.Ranges {
			top = maxAddr(top, r.To)
		}
	}
	return top
}

// allocatedIn returns the allocated addresses of prefix, ascending: those of the IPAddresses of the namespace of nw,
// of any pool, and those NetBox holds for claims.
func (r *NetBoxNetworkReconciler) allocatedIn(ctx context.Context, nw *v1alpha1.NetBoxNetwork, prefix netip.Prefix) ([]netip.Addr, error) {
	addrs, err := r.ipAddresses(ctx, nw.Namespace)
	if err != nil {
		return nil, err
	}
	return r.allocated(ctx, addrs, prefix.Masked().Addr(), netmath.Last(prefix), nil)
}
