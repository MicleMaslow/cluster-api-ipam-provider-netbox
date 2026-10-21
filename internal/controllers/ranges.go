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
	"net/url"
	"slices"
	"strings"

	"k8s.io/klog/v2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/layout"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netmath"
)

// poolRangeRef is the key of the NetBox ranges of pool in nw; with pool "", the start of the keys of all its pools.
func poolRangeRef(k netbox.Keys, nw *v1alpha1.NetBoxNetwork, pool string) string {
	return k.Key("NetBoxNetwork", nw, "pool", pool)
}

// netBoxBounds are the addresses of a NetBox IP range, which NetBox gives with the mask.
func netBoxBounds(o netbox.Object) (netip.Addr, netip.Addr, error) {
	start, err := netip.ParsePrefix(o.StartAddress)
	if err != nil {
		return netip.Addr{}, netip.Addr{}, err
	}
	end, err := netip.ParsePrefix(o.EndAddress)
	return start.Addr(), end.Addr(), err
}

// applyRanges brings the NetBox ranges of nw to the pools of l and records l, with the ids of the ranges, in status.layout.
// The address groups of l go to the status with the pools: applyReserves creates them after.
func (r *NetBoxNetworkReconciler) applyRanges(ctx context.Context, nw *v1alpha1.NetBoxNetwork, l layout.Layout) error {
	addrs, err := r.ipAddresses(ctx, nw.Namespace)
	if err != nil {
		return err
	}
	return underLock(ctx, r.NetBox, func(ctx context.Context, nb *netbox.Client) error {
		found, err := nb.FindByRefPrefix(ctx, r.Keys, netbox.IPRanges, poolRangeRef(r.Keys, nw, ""))
		if err != nil {
			return err
		}
		rr := *r
		rr.NetBox = nb
		st, err := rr.syncRanges(ctx, nw, l, addrs, found)
		// An empty layout is recorded only once its ranges are gone: until then LayoutReady stays and retries.
		if !errors.Is(err, netbox.ErrWriteNeeded) && (err == nil || len(st.Pools) > 0) {
			nw.Status.Layout = st
		}
		return err
	})
}

// syncRanges is the section of applyRanges that underLock runs on found, the ranges under the keys of nw. Ranges left
// over go and kept ones shrink first, then kept ones grow and new ones are created: no write overlaps a range of nw
// not yet shrunk. The first failed write stops the rest. It returns the layout status: the spans of l with the ids
// of the ranges that hold, or are to hold, them.
func (r *NetBoxNetworkReconciler) syncRanges(ctx context.Context, nw *v1alpha1.NetBoxNetwork, l layout.Layout, addrs []netip.Addr, found []netbox.Object) (v1alpha1.NetworkLayout, error) {
	l2, _, err := l2Of(nw)
	if err != nil {
		return nw.Status.Layout, err
	}
	bits := l2.Bits()
	prefix := poolRangeRef(r.Keys, nw, "")
	byPool := map[string][]netbox.Object{}
	for _, o := range found {
		name := strings.TrimPrefix(r.Keys.RefOf(o), prefix)
		byPool[name] = append(byPool[name], o)
	}
	held := make([][]*netbox.Object, len(l.Pools))
	type leftover struct {
		o    netbox.Object
		keep func(netip.Addr) bool
	}
	var left []leftover
	for i, p := range l.Pools {
		var rest []netbox.Object
		held[i], rest = pairRanges(p, byPool[p.Name])
		delete(byPool, p.Name)
		for _, o := range rest {
			left = append(left, leftover{o, inPool(p)})
		}
	}
	for _, objs := range byPool {
		for _, o := range objs {
			left = append(left, leftover{o: o})
		}
	}
	status := func() v1alpha1.NetworkLayout {
		var st v1alpha1.NetworkLayout
		for i, p := range l.Pools {
			pl := v1alpha1.PoolLayout{Name: p.Name, Size: sizeOf(p.Pool)}
			for j, s := range p.Ranges {
				lr := v1alpha1.LayoutRange{Start: s.From.String(), End: s.To.String()}
				if o := held[i][j]; o != nil {
					lr.ID = o.ID
				}
				pl.Ranges = append(pl.Ranges, lr)
			}
			st.Pools = append(st.Pools, pl)
		}
		for _, g := range l.Groups {
			gl := v1alpha1.AddressGroupLayout{Name: g.Name, Count: int32(g.Count)}
			for _, a := range g.Addrs {
				gl.Addresses = append(gl.Addresses, a.String())
			}
			st.Addresses = append(st.Addresses, gl)
		}
		return st
	}

	for _, lo := range left {
		if err := r.releaseRange(ctx, nw, lo.o, v1alpha1.DeletionPolicyDelete, addrs, lo.keep); err != nil {
			return status(), err
		}
	}
	for i, p := range l.Pools {
		ref := poolRangeRef(r.Keys, nw, p.Name)
		for j, s := range p.Ranges {
			o := held[i][j]
			if o == nil {
				continue
			}
			from, to, err := netBoxBounds(*o)
			if err != nil {
				return status(), err
			}
			if !from.Less(s.From) && !s.To.Less(to) {
				continue
			}
			if lo, hi := maxAddr(from, s.From), minAddr(to, s.To); !hi.Less(lo) {
				*o, err = r.resizeRange(ctx, nw, ref, *o, lo, hi, bits, addrs, inPool(p))
			} else { // nothing in common: a new range takes the span
				err = r.releaseRange(ctx, nw, *o, v1alpha1.DeletionPolicyDelete, addrs, inPool(p))
				held[i][j] = nil
			}
			if err != nil {
				return status(), err
			}
		}
	}

	var attrsErrs []error
	for i, p := range l.Pools {
		ref := poolRangeRef(r.Keys, nw, p.Name)
		t := rangeTexts(nw, p.Name)
		for j, s := range p.Ranges {
			o := held[i][j]
			if o == nil {
				created, err := r.createRange(ctx, nw, ref, p.Name, s, bits, t)
				if err != nil {
					return status(), err
				}
				held[i][j] = &created
				continue
			}
			from, to, err := netBoxBounds(*o)
			if err != nil {
				return status(), err
			}
			if from != s.From || to != s.To || o.MarkPopulated != populated(nw) || o.Status.Value == "deprecated" {
				if *o, err = r.resizeRange(ctx, nw, ref, *o, s.From, s.To, bits, addrs, inPool(p)); err != nil {
					return status(), err
				}
			}
			aerr := syncAttrs(ctx, r.NetBox, r.Recorder, nw, netbox.IPRanges, *o, t.on(networkAttrs(nw)), nil, "capi: attributes of "+ref)
			attrsErrs = append(attrsErrs, attrsFailed("the range "+s.From.String()+"-"+s.To.String(), aerr))
		}
	}
	return status(), foldErrors(attrsErrs)
}

// pairRanges pairs the spans of p with the NetBox ranges have of the pool: a range with the bounds of a span first,
// then the others in order of their start. It returns the range of each span, nil for none, and the ranges left over.
func pairRanges(p layout.PlacedPool, have []netbox.Object) ([]*netbox.Object, []netbox.Object) {
	start := func(o netbox.Object) netip.Addr {
		a, _, _ := netBoxBounds(o)
		return a
	}
	slices.SortFunc(have, func(a, b netbox.Object) int { return start(a).Compare(start(b)) })
	held := make([]*netbox.Object, len(p.Ranges))
	used := make([]bool, len(have))
	for j, s := range p.Ranges {
		for k := range have {
			if from, to, err := netBoxBounds(have[k]); err == nil && !used[k] && from == s.From && to == s.To {
				held[j], used[k] = &have[k], true
				break
			}
		}
	}
	k := 0
	for j := range held {
		for ; held[j] == nil && k < len(have); k++ {
			if !used[k] {
				held[j], used[k] = &have[k], true
			}
		}
	}
	var rest []netbox.Object
	for k, o := range have {
		if !used[k] {
			rest = append(rest, o)
		}
	}
	return held, rest
}

// inPool accepts the addresses of the spans of p.
func inPool(p layout.PlacedPool) func(netip.Addr) bool {
	return func(a netip.Addr) bool {
		return slices.ContainsFunc(p.Ranges, func(s layout.Span) bool { return netmath.Contains(s.From, s.To, a) })
	}
}

func maxAddr(a, b netip.Addr) netip.Addr {
	if a.Less(b) {
		return b
	}
	return a
}

func minAddr(a, b netip.Addr) netip.Addr {
	if a.Less(b) {
		return a
	}
	return b
}

// populated reports whether the ranges of nw are closed to allocation in NetBox (mark_populated): with allocator
// InCluster, the InClusterIPPools hand out their addresses, never NetBox.
func populated(nw *v1alpha1.NetBoxNetwork) bool {
	return nw.Spec.Allocator == v1alpha1.AllocatorInCluster
}

// createRange creates the NetBox range of span s of pool under ref.
func (r *NetBoxNetworkReconciler) createRange(ctx context.Context, nw *v1alpha1.NetBoxNetwork, ref, pool string, s layout.Span, bits int, t texts) (netbox.Object, error) {
	fields := map[string]any{"start_address": netip.PrefixFrom(s.From, bits).String(), "end_address": netip.PrefixFrom(s.To, bits).String(), "status": "active"}
	if populated(nw) {
		fields["mark_populated"] = true
	}
	var obj netbox.Object
	err := r.clearingDeprecated(ctx, nw, ref, s.From, s.To, 0, func() (err error) {
		obj, err = r.NetBox.Create(ctx, netbox.IPRanges, r.Keys.Managed(t.on(networkAttrs(nw)), ref, "capi: range "+ref, fields))
		return err
	})
	if err != nil {
		return obj, err
	}
	ctrl.LoggerFrom(ctx).Info("Created the NetBox range", "network", klog.KObj(nw), "pool", pool, "id", obj.ID, "range", s.From.String()+"-"+s.To.String())
	normalEvent(r.Recorder, nw, "RangeCreated", "Created the NetBox range %s-%s of pool %s", s.From, s.To, pool)
	return obj, nil
}

// resizeRange brings the NetBox range cur to start-end, active, and open to allocation unless populated. Addresses it
// gives up are fenced first: none but those keep accepts may be allocated; a populated range needs no fence, the
// InClusterIPPool gave them up before. It returns the range as it is after the change.
func (r *NetBoxNetworkReconciler) resizeRange(ctx context.Context, nw *v1alpha1.NetBoxNetwork, ref string, cur netbox.Object, start, end netip.Addr, bits int, addrs []netip.Addr, keep func(netip.Addr) bool) (netbox.Object, error) {
	cur, etag, err := r.NetBox.Get(ctx, netbox.IPRanges, cur.ID)
	if err != nil {
		return cur, err
	}
	from, to, err := netBoxBounds(cur)
	if err != nil {
		return cur, err
	}
	var fenced bool
	if !populated(nw) && (from.Less(start) || end.Less(to)) {
		etag, fenced, err = r.fence(ctx, cur, etag, addrs, keep, fmt.Sprintf("NetBox range stays %s-%s while addresses outside the pool are allocated", from, to))
		if err != nil {
			return cur, err
		}
	}
	message := "capi: resized by " + ref
	if cur.Status.Value == "deprecated" {
		message = "capi: reclaimed by " + ref
	}
	err = r.clearingDeprecated(ctx, nw, ref, start, end, cur.ID, func() error {
		_, err := r.NetBox.Patch(ctx, netbox.IPRanges, cur.ID, etag, map[string]any{
			"start_address": netip.PrefixFrom(start, bits).String(), "end_address": netip.PrefixFrom(end, bits).String(),
			"status": "active", "mark_populated": populated(nw), "changelog_message": message,
		})
		return err
	})
	if err != nil {
		if fenced { // no range stays closed to allocation behind a failed change
			_, _ = r.NetBox.Patch(ctx, netbox.IPRanges, cur.ID, "", map[string]any{"mark_populated": false, "changelog_message": "capi: opened again by " + ref})
		}
		return cur, err
	}
	ctrl.LoggerFrom(ctx).Info("Updated the NetBox range", "network", klog.KObj(nw), "ref", ref, "id", cur.ID, "from", from.String()+"-"+to.String(), "to", start.String()+"-"+end.String())
	normalEvent(r.Recorder, nw, "RangeUpdated", "Updated the NetBox range %s-%s to %s-%s", from, to, start, end)
	cur.StartAddress, cur.EndAddress = netip.PrefixFrom(start, bits).String(), netip.PrefixFrom(end, bits).String()
	cur.Status.Value, cur.MarkPopulated = "active", populated(nw)
	return cur, nil
}

// fence closes the NetBox range cur to allocation (mark_populated) once no address in it but
// those keep accepts is allocated, and checks again: from there none can appear until the caller acts with the returned ETag.
// It reports whether it closed the range. Allocated addresses are RangeInUse, explained by why; found on the
// second check, they open the range again. It writes only under netBoxWrites, so no allocation of ours lands in between.
func (r *NetBoxNetworkReconciler) fence(ctx context.Context, cur netbox.Object, etag string, addrs []netip.Addr, keep func(netip.Addr) bool, why string) (string, bool, error) {
	from, to, err := netBoxBounds(cur)
	if err != nil {
		return etag, false, err
	}
	inUse := func() error {
		held, err := r.allocated(ctx, addrs, from, to, keep)
		if err == nil && len(held) > 0 {
			err = retryLater(v1alpha1.ReasonRangeInUse, fmt.Errorf("%s: %s", why, listAddresses(held)))
		}
		return err
	}
	if err := inUse(); err != nil || cur.MarkPopulated {
		return etag, false, err
	}
	populated := func(etag string, on bool, message string) (string, error) {
		return r.NetBox.Patch(ctx, netbox.IPRanges, cur.ID, etag, map[string]any{"mark_populated": on, "changelog_message": message + r.Keys.RefOf(cur)})
	}
	next, err := populated(etag, true, "capi: closed to allocation by ")
	if err != nil {
		return etag, false, err
	}
	if err := inUse(); err != nil {
		if reopened, oerr := populated(next, false, "capi: opened again by "); oerr == nil {
			return reopened, false, err
		}
		return next, true, err
	}
	return next, true, nil
}

// clearingDeprecated runs write. When NetBox finds start-end overlapping another range, our deprecated ranges
// (those of networks) in the way that hold no addresses are deleted and write runs once more;
// a range still in the way is RangeOverlap.
func (r *NetBoxNetworkReconciler) clearingDeprecated(ctx context.Context, nw *v1alpha1.NetBoxNetwork, ref string, start, end netip.Addr, self int64, write func() error) error {
	err := write()
	if !errors.Is(err, netbox.ErrOverlap) {
		return err
	}
	deprecated, err := r.NetBox.List(ctx, netbox.IPRanges, url.Values{"parent": {nw.Status.Prefix}, "status": {"deprecated"}})
	if err != nil {
		return err
	}
	for _, o := range deprecated {
		from, to, err := netBoxBounds(o)
		if err != nil || o.ID == self || to.Less(start) || end.Less(from) || !strings.HasPrefix(r.Keys.RefOf(o), r.Keys.KindPrefix("NetBoxNetwork")) {
			continue
		}
		held, err := r.allocated(ctx, nil, from, to, nil)
		if err != nil {
			return err
		}
		if len(held) > 0 {
			continue
		}
		if err := r.NetBox.Delete(ctx, netbox.IPRanges, o.ID, "", "capi: replaced by "+ref); err != nil {
			return err
		}
		ctrl.LoggerFrom(ctx).Info("Deleted a deprecated NetBox range in the way", "network", klog.KObj(nw), "ref", ref, "id", o.ID, "range", from.String()+"-"+to.String())
		normalEvent(r.Recorder, nw, "RangeReleased", "Deleted the deprecated NetBox range %s-%s that was in the way", from, to)
	}
	if err := write(); errors.Is(err, netbox.ErrOverlap) {
		return retryLater(v1alpha1.ReasonRangeOverlap, err)
	} else if err != nil {
		return err
	}
	return nil
}

// allocated returns the addresses in from-to that IPAddresses (addrs) or our NetBox addresses (those of claims) hold,
// ascending, leaving out those keep accepts. NetBox also shows an allocation whose IPAddress is not created yet.
func (r *NetBoxNetworkReconciler) allocated(ctx context.Context, addrs []netip.Addr, from, to netip.Addr, keep func(netip.Addr) bool) ([]netip.Addr, error) {
	ips, err := r.NetBox.List(ctx, netbox.IPAddresses, url.Values{"parent": {netmath.Cover(from, to).String()}})
	if err != nil {
		return nil, err
	}
	all := slices.Clone(addrs)
	for _, ip := range ips {
		if !strings.HasPrefix(r.Keys.RefOf(ip), r.Keys.KindPrefix("IPAddressClaim")) {
			continue
		}
		if p, err := netip.ParsePrefix(ip.Address); err == nil {
			all = append(all, p.Addr())
		}
	}
	slices.SortFunc(all, netip.Addr.Compare)
	var out []netip.Addr
	for _, a := range slices.Compact(all) {
		if netmath.Contains(from, to, a) && (keep == nil || !keep(a)) {
			out = append(out, a)
		}
	}
	return out, nil
}

// listAddresses names the first addresses of held; a condition message has a size limit.
func listAddresses(held []netip.Addr) string {
	const shown = 10
	names := make([]string, 0, min(len(held), shown))
	for _, a := range held[:min(len(held), shown)] {
		names = append(names, a.String())
	}
	if len(held) > shown {
		return strings.Join(names, ", ") + fmt.Sprintf(" and %d more", len(held)-shown)
	}
	return strings.Join(names, ", ")
}

// releaseRange deletes the NetBox range o of nw, or deprecates it under Retain, closed to allocation (mark_populated),
// once no address in it but those keep accepts is allocated (RangeInUse). A deprecated range is kept under Retain as it is.
// It runs under netBoxWrites.
func (r *NetBoxNetworkReconciler) releaseRange(ctx context.Context, nw *v1alpha1.NetBoxNetwork, o netbox.Object, policy v1alpha1.DeletionPolicy, addrs []netip.Addr, keep func(netip.Addr) bool) error {
	if policy == v1alpha1.DeletionPolicyRetain && o.Status.Value == "deprecated" {
		return nil
	}
	cur, etag, err := r.NetBox.Get(ctx, netbox.IPRanges, o.ID)
	if errors.Is(err, netbox.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if etag, _, err = r.fence(ctx, cur, etag, addrs, keep, "kept while addresses in it are allocated"); err != nil {
		return err
	}
	released := "capi: released by " + r.Keys.Key("NetBoxNetwork", nw)
	if policy == v1alpha1.DeletionPolicyDelete {
		err = r.NetBox.Delete(ctx, netbox.IPRanges, cur.ID, etag, released)
	} else {
		_, err = r.NetBox.Patch(ctx, netbox.IPRanges, cur.ID, etag, map[string]any{
			"status": "deprecated", "mark_populated": true, "changelog_message": released,
		})
	}
	switch {
	case errors.Is(err, netbox.ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	ctrl.LoggerFrom(ctx).Info("Released the NetBox range", "network", klog.KObj(nw), "ref", r.Keys.RefOf(cur), "id", cur.ID, "policy", policy)
	normalEvent(r.Recorder, nw, "RangeReleased", "Released the NetBox range %s-%s (deletionPolicy %s)", cur.StartAddress, cur.EndAddress, policy)
	return nil
}

// releaseRanges releases the NetBox ranges in found, those of nw, as policy says.
func (r *NetBoxNetworkReconciler) releaseRanges(ctx context.Context, nw *v1alpha1.NetBoxNetwork, found []netbox.Object, policy v1alpha1.DeletionPolicy) error {
	if len(found) == 0 {
		return nil
	}
	addrs, err := r.ipAddresses(ctx, nw.Namespace)
	if err != nil {
		return err
	}
	return underLock(ctx, r.NetBox, func(ctx context.Context, nb *netbox.Client) error {
		rr := *r
		rr.NetBox = nb
		for _, o := range found {
			if err := rr.releaseRange(ctx, nw, o, policy, addrs, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

// ipAddresses returns the addresses of the IPAddresses of namespace, of any pool.
func (r *NetBoxNetworkReconciler) ipAddresses(ctx context.Context, namespace string) ([]netip.Addr, error) {
	list := &ipamv1.IPAddressList{}
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, apiError(err)
	}
	addrs := make([]netip.Addr, 0, len(list.Items))
	for _, a := range list.Items {
		if addr, err := netip.ParseAddr(a.Spec.Address); err == nil {
			addrs = append(addrs, addr)
		}
	}
	return addrs, nil
}
