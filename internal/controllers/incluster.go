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
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	inclusterv1 "sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/layout"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netmath"
)

// inClusterGracePeriod is the addressReuseGracePeriodSeconds of the InClusterIPPools of networks.
const inClusterGracePeriod int32 = 60

// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=inclusterippools,verbs=get;list;watch;create;update;patch;delete

// inClusterServed reports whether the API serves InClusterIPPool, the CRD of cluster-api-ipam-provider-in-cluster.
func inClusterServed(m meta.RESTMapper) (bool, error) {
	_, err := m.RESTMapping(schema.GroupKind{Group: inclusterv1.GroupVersion.Group, Kind: "InClusterIPPool"}, inclusterv1.GroupVersion.Version)
	if meta.IsNoMatchError(err) {
		return false, nil
	}
	return err == nil, err
}

// inClusterSpec is the spec of the InClusterIPPool of p: the span of its ranges with the mask and gateway of the L2
// network, without the addresses between the ranges and live, those in use in the span.
func inClusterSpec(nw *v1alpha1.NetBoxNetwork, p layout.PlacedPool, live []netip.Addr) inclusterv1.InClusterIPPoolSpec {
	l2, gw, _ := l2Of(nw) // the CRD validates both
	out := p.Gaps()
	for _, a := range live {
		out = append(out, layout.Span{From: a, To: a})
	}
	slices.SortFunc(out, func(a, b layout.Span) int { return a.From.Compare(b.From) })
	var excluded []string
	for i := 0; i < len(out); {
		s := out[i]
		for i++; i < len(out) && !s.To.Next().Less(out[i].From); i++ {
			s.To = maxAddr(s.To, out[i].To)
		}
		excluded = append(excluded, spanString(s))
	}
	return inclusterv1.InClusterIPPoolSpec{
		Addresses: []string{p.Ranges[0].From.String() + "-" + p.Ranges[len(p.Ranges)-1].To.String()},
		Prefix:    l2.Bits(), Gateway: gw.String(), ExcludedAddresses: excluded,
		AddressReuseGracePeriodSeconds: ptr.To(inClusterGracePeriod),
	}
}

// spanString is "a" or "a-b".
func spanString(s layout.Span) string {
	if s.From == s.To {
		return s.From.String()
	}
	return s.From.String() + "-" + s.To.String()
}

// managedBy reports whether nw is the controller of obj, an InClusterIPPool or a Secret. The name decides, not the UID:
// a network deleted and created again (a restore) is the same network; the apply writes its new UID into the reference,
// which the garbage collector goes by. clusterctl move rewrites the UIDs itself.
func managedBy(obj metav1.Object, nw *v1alpha1.NetBoxNetwork) bool {
	ref := metav1.GetControllerOfNoCopy(obj)
	if ref == nil {
		return false
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	return err == nil && gv.Group == v1alpha1.GroupVersion.Group && ref.Kind == "NetBoxNetwork" && ref.Name == nw.Name
}

// poolWriteError is err of a write to the InClusterIPPool name: a refusal of the in-cluster webhook (BadRequest) is
// reason, checked again every minute.
func poolWriteError(reason, name string, err error) error {
	if apierrors.IsBadRequest(err) {
		return retryLater(reason, fmt.Errorf("InClusterIPPool %s: %w", name, err))
	}
	return apiError(err)
}

// spanOf is the span of "a-b".
func spanOf(s string) (layout.Span, bool) {
	from, to, _ := strings.Cut(s, "-")
	a, aerr := netip.ParseAddr(from)
	b, berr := netip.ParseAddr(to)
	return layout.Span{From: a, To: b}, aerr == nil && berr == nil
}

// addressSpans are the spans of addresses of an InClusterIPPool, as in-cluster reads them: a-b, CIDRs and addresses.
func addressSpans(addrs []string) ([]layout.Span, bool) {
	out := make([]layout.Span, 0, len(addrs))
	for _, a := range addrs {
		sp, ok := spanOf(a)
		if p, err := netip.ParsePrefix(a); err == nil {
			sp, ok = layout.Span{From: p.Masked().Addr(), To: netmath.Last(p.Masked())}, true
		} else if ip, err := netip.ParseAddr(a); err == nil {
			sp, ok = layout.Span{From: ip, To: ip}, true
		}
		if !ok {
			return nil, false
		}
		out = append(out, sp)
	}
	return out, true
}

// inClusterPositions places the pools of spec.pools that have an InClusterIPPool of their name, controlled by nw or by
// nothing, on the free runs of its addresses (layout.Runs): a network that takes over a running cluster keeps the
// positions the pools have. What a pool excludes stays excluded; pools that overlap are InvalidSpec.
func (r *NetBoxNetworkReconciler) inClusterPositions(ctx context.Context, nw *v1alpha1.NetBoxNetwork, prefix netip.Prefix) ([]layout.PlacedPool, error) {
	list := &inclusterv1.InClusterIPPoolList{}
	if err := r.List(ctx, list, client.InNamespace(nw.Namespace)); err != nil {
		return nil, apiError(err)
	}
	var pools []*inclusterv1.InClusterIPPool
	for _, p := range nw.Spec.Pools {
		i := slices.IndexFunc(list.Items, func(o inclusterv1.InClusterIPPool) bool { return o.Name == p.Name })
		if i >= 0 && (metav1.GetControllerOfNoCopy(&list.Items[i]) == nil || managedBy(&list.Items[i], nw)) {
			pools = append(pools, &list.Items[i])
		}
	}
	if len(pools) == 0 {
		return nil, nil
	}
	s, err := r.space(ctx, nw, prefix)
	if err != nil {
		return nil, err
	}
	var found []layout.PlacedPool
	for _, pool := range pools {
		addrs := strings.Join(pool.Spec.Addresses, ", ")
		spans, ok := addressSpans(pool.Spec.Addresses)
		if !ok || len(spans) == 0 || slices.ContainsFunc(spans, func(sp layout.Span) bool { return !prefix.Contains(sp.From) || !prefix.Contains(sp.To) }) {
			return nil, withReason(v1alpha1.ReasonInvalidSpec, fmt.Errorf("InClusterIPPool %s: addresses %s are not inside %s", pool.Name, addrs, prefix))
		}
		excluded, ok := addressSpans(pool.Spec.ExcludedAddresses)
		if !ok {
			return nil, withReason(v1alpha1.ReasonInvalidSpec, fmt.Errorf("InClusterIPPool %s: excludedAddresses %s are not addresses", pool.Name, strings.Join(pool.Spec.ExcludedAddresses, ", ")))
		}
		ps := s
		ps.Taken = append(slices.Clone(s.Taken), excluded...)
		ranges := layout.Runs(ps, spans...)
		if len(ranges) == 0 {
			return nil, withReason(v1alpha1.ReasonInvalidSpec, fmt.Errorf("InClusterIPPool %s: no free address in %s", pool.Name, addrs))
		}
		found = append(found, layout.PlacedPool{Pool: layout.Pool{Name: pool.Name}, Ranges: ranges})
	}
	slices.SortFunc(found, func(a, b layout.PlacedPool) int { return a.Ranges[0].From.Compare(b.Ranges[0].From) })
	for i := 1; i < len(found); i++ {
		if last := found[i-1].Ranges; !last[len(last)-1].To.Less(found[i].Ranges[0].From) {
			return nil, withReason(v1alpha1.ReasonInvalidSpec, fmt.Errorf("InClusterIPPools %s and %s overlap", found[i-1].Name, found[i].Name))
		}
	}
	return found, nil
}

// within reports whether the span of spec lies in that of cur.
func within(spec, cur inclusterv1.InClusterIPPoolSpec) bool {
	if len(cur.Addresses) != 1 {
		return false
	}
	c, cok := spanOf(cur.Addresses[0])
	n, nok := spanOf(spec.Addresses[0])
	return cok && nok && !n.From.Less(c.From) && !c.To.Less(n.To)
}

// liveIn returns, per pool of l, the addresses in use in its span (those s takes, its VIP and the reserves of l) that the
// pool excludes, and the allocated ones among them, ascending, which stay in the pool: the conflicts.
func liveIn(l layout.Layout, s layout.Space, allocated []netip.Addr) (live map[string][]netip.Addr, conflicts []netip.Addr) {
	inUse := slices.Clone(s.Taken)
	if s.VIP.IsValid() {
		inUse = append(inUse, layout.Span{From: s.VIP, To: s.VIP})
	}
	for _, g := range l.Groups {
		for _, a := range g.Addrs {
			inUse = append(inUse, layout.Span{From: a, To: a})
		}
	}
	live = map[string][]netip.Addr{}
	for _, p := range l.Pools {
		first, last := p.Ranges[0].From, p.Ranges[len(p.Ranges)-1].To
		for _, t := range inUse {
			for a, to := maxAddr(t.From, first), minAddr(t.To, last); a.IsValid() && !to.Less(a); a = a.Next() {
				if slices.Contains(allocated, a) {
					conflicts = append(conflicts, a)
				} else {
					live[p.Name] = append(live[p.Name], a)
				}
			}
		}
	}
	slices.SortFunc(conflicts, netip.Addr.Compare)
	return live, slices.Compact(conflicts)
}

// applyInClusterPools brings an InClusterIPPool per pool of l to inClusterSpec and deletes the other pools of nw.
// As with ranges, pools that keep or shrink their span go first, then pools that go, then new and growing ones:
// in-cluster checks the addresses in use per pool, so no address is in two pools at once.
// The addresses in use in the span of a pool (those s takes, its VIP and the reserves of l) are excluded but for
// allocated ones, which stay in the pool and are AddressConflict once every pool is applied. A pool of the name that
// nothing controls is adopted; one another controls is Duplicate; a change the in-cluster webhook refuses is RangeInUse.
// Either stops the rest.
func (r *NetBoxNetworkReconciler) applyInClusterPools(ctx context.Context, nw *v1alpha1.NetBoxNetwork, l layout.Layout, s layout.Space, allocated []netip.Addr) error {
	list := &inclusterv1.InClusterIPPoolList{}
	if err := r.List(ctx, list, client.InNamespace(nw.Namespace)); err != nil {
		return apiError(err)
	}
	have := map[string]*inclusterv1.InClusterIPPool{}
	for i := range list.Items {
		have[list.Items[i].Name] = &list.Items[i]
	}
	live, conflicts := liveIn(l, s, allocated)
	var grow []string
	specs := map[string]inclusterv1.InClusterIPPoolSpec{}
	for _, p := range l.Pools {
		specs[p.Name] = inClusterSpec(nw, p, live[p.Name])
		if cur := have[p.Name]; cur == nil || !within(specs[p.Name], cur.Spec) {
			grow = append(grow, p.Name)
		} else if err := r.applyInClusterPool(ctx, nw, p.Name, specs[p.Name], cur); err != nil {
			return err
		}
	}
	for name, pool := range have {
		if _, ok := specs[name]; !ok && managedBy(pool, nw) && pool.DeletionTimestamp.IsZero() {
			if err := r.deleteInClusterPool(ctx, nw, pool, v1alpha1.ReasonRangeInUse); err != nil {
				return err
			}
		}
	}
	for _, name := range grow {
		if err := r.applyInClusterPool(ctx, nw, name, specs[name], have[name]); err != nil {
			return err
		}
	}
	if len(conflicts) > 0 {
		return retryLater(v1alpha1.ReasonAddressConflict, fmt.Errorf("addresses in use in NetBox are allocated from the InClusterIPPools, which keep them: %s",
			listAddresses(conflicts)))
	}
	return nil
}

// applyInClusterPool creates the InClusterIPPool name of nw with spec, or brings cur, the pool of the name, to it;
// a cur that nothing controls, as a chart renders it, nw takes over.
func (r *NetBoxNetworkReconciler) applyInClusterPool(ctx context.Context, nw *v1alpha1.NetBoxNetwork, name string, spec inclusterv1.InClusterIPPoolSpec, cur *inclusterv1.InClusterIPPool) error {
	log := ctrl.LoggerFrom(ctx)
	switch {
	case cur == nil:
		pool := &inclusterv1.InClusterIPPool{ObjectMeta: metav1.ObjectMeta{Namespace: nw.Namespace, Name: name}, Spec: spec}
		if err := r.own(nw, pool); err != nil {
			return err
		}
		if err := r.Create(ctx, pool); err != nil {
			return apiError(err)
		}
		log.Info("Created the InClusterIPPool", "network", klog.KObj(nw), "pool", klog.KObj(pool), "addresses", spec.Addresses)
		normalEvent(r.Recorder, nw, "InClusterIPPoolCreated", "Created the InClusterIPPool %s", name)
	case metav1.GetControllerOfNoCopy(cur) == nil:
		if err := r.own(nw, cur); err != nil {
			return err
		}
		cur.Spec = spec
		if err := r.Update(ctx, cur); err != nil {
			return poolWriteError(v1alpha1.ReasonRangeInUse, name, err)
		}
		log.Info("Adopted the InClusterIPPool", "network", klog.KObj(nw), "pool", klog.KObj(cur), "addresses", spec.Addresses, "excluded", spec.ExcludedAddresses)
		normalEvent(r.Recorder, nw, "PoolAdopted", "Adopted the InClusterIPPool %s", name)
	case !managedBy(cur, nw):
		return retryLater(v1alpha1.ReasonDuplicate, fmt.Errorf("InClusterIPPool %s exists and is not managed by network %s", name, nw.Name))
	default:
		ref := metav1.GetControllerOfNoCopy(cur)
		if ref.UID == nw.UID && equality.Semantic.DeepEqual(cur.Spec, spec) {
			return nil
		}
		ref.UID, cur.Spec = nw.UID, spec // a stale UID left, the garbage collector would delete the pool
		if err := r.Update(ctx, cur); err != nil {
			return poolWriteError(v1alpha1.ReasonRangeInUse, name, err)
		}
		log.Info("Updated the InClusterIPPool", "network", klog.KObj(nw), "pool", klog.KObj(cur), "addresses", spec.Addresses, "excluded", spec.ExcludedAddresses)
		normalEvent(r.Recorder, nw, "InClusterIPPoolUpdated", "Updated the InClusterIPPool %s", name)
	}
	return nil
}

// own makes nw the controller of pool. The watch filter of this provider, and of in-cluster, passes the pool as it
// passes the network: the pool gets the network's watch label.
func (r *NetBoxNetworkReconciler) own(nw *v1alpha1.NetBoxNetwork, pool *inclusterv1.InClusterIPPool) error {
	if v, ok := nw.Labels[clusterv1.WatchLabel]; ok {
		if pool.Labels == nil {
			pool.Labels = map[string]string{}
		}
		pool.Labels[clusterv1.WatchLabel] = v
	}
	return controllerutil.SetControllerReference(nw, pool, r.Scheme())
}

// deleteInClusterPool deletes pool of nw; a refusal of the in-cluster webhook is reason.
func (r *NetBoxNetworkReconciler) deleteInClusterPool(ctx context.Context, nw *v1alpha1.NetBoxNetwork, pool *inclusterv1.InClusterIPPool, reason string) error {
	if err := client.IgnoreNotFound(r.Delete(ctx, pool)); err != nil {
		return poolWriteError(reason, pool.Name, err)
	}
	ctrl.LoggerFrom(ctx).Info("Deleted the InClusterIPPool", "network", klog.KObj(nw), "pool", klog.KObj(pool))
	normalEvent(r.Recorder, nw, "InClusterIPPoolDeleted", "Deleted the InClusterIPPool %s", pool.Name)
	return nil
}

// deleteInClusterPools deletes the InClusterIPPools of nw and waits until they are gone: while addresses of a pool are
// allocated, the webhook of in-cluster refuses its deletion (checked again every minute), or its finalizer holds it.
// Every pool is tried; the pools go in order of name, so the message stays the same.
func (r *NetBoxNetworkReconciler) deleteInClusterPools(ctx context.Context, nw *v1alpha1.NetBoxNetwork) error {
	list := &inclusterv1.InClusterIPPoolList{}
	if err := r.List(ctx, list, client.InNamespace(nw.Namespace)); err != nil {
		return apiError(err)
	}
	slices.SortFunc(list.Items, func(a, b inclusterv1.InClusterIPPool) int { return strings.Compare(a.Name, b.Name) })
	var (
		left    []string
		refused []error
	)
	for i := range list.Items {
		pool := &list.Items[i]
		if !managedBy(pool, nw) {
			continue
		}
		if pool.DeletionTimestamp.IsZero() {
			if err := r.deleteInClusterPool(ctx, nw, pool, v1alpha1.ReasonWaitingForPools); err != nil {
				refused = append(refused, err)
				continue
			}
		}
		left = append(left, pool.Name)
	}
	switch {
	case len(refused) > 0:
		return foldErrors(refused)
	case len(left) > 0:
		return waitFor(v1alpha1.ReasonWaitingForPools, fmt.Errorf("InClusterIPPool %s still exists", left[0]))
	}
	return nil
}
