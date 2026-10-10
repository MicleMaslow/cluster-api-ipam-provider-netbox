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
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
	ipampredicates "sigs.k8s.io/cluster-api-ipam-provider-in-cluster/pkg/predicates"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/cluster-api/util/labels"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netmath"
)

const poolFinalizer = "netbox.ipam.cluster.x-k8s.io/pool"

// NetBoxIPPoolReconciler keeps a NetBox IP range for every range of a NetBoxIPPool.
type NetBoxIPPoolReconciler struct {
	client.Client
	NetBox           *netbox.Client
	Owner            netbox.Owner
	WatchFilterValue string
}

// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=netboxippools,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=netboxippools/status;netboxippools/finalizers,verbs=get;update;patch
// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=ipaddresses,verbs=get;list;watch

// SetupWithManager sets up the controller with the Manager.
func (r *NetBoxIPPoolReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &ipamv1.IPAddress{}, poolRefField, func(o client.Object) []string {
		a, ok := asType[*ipamv1.IPAddress](ctx, o)
		if !ok {
			return nil
		}
		return poolRefName(a.Spec.PoolRef)
	}); err != nil {
		return err
	}
	label := predicates.ResourceHasFilterLabel(mgr.GetScheme(), ctrl.LoggerFrom(ctx), r.WatchFilterValue)
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(options).
		For(&v1alpha1.NetBoxIPPool{}, builder.WithPredicates(label)).
		Watches(&v1alpha1.NetBoxIPPool{}, handler.EnqueueRequestsFromMapFunc(r.poolToPools), builder.WithPredicates(label, rangesMoved)).
		Watches(&v1alpha1.NetBoxNetwork{}, handler.EnqueueRequestsFromMapFunc(r.networkToPools), builder.WithPredicates(label)).
		// ipamutil creates IPAddresses without the watch label; Reconcile skips pools of another filter.
		Watches(&ipamv1.IPAddress{}, handler.EnqueueRequestsFromMapFunc(addressToPool),
			builder.WithPredicates(ipampredicates.AddressReferencesPoolKind(poolGK))).
		Watches(&clusterv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.clusterToPools), builder.WithPredicates(label, clusterEvents)).
		Complete(r)
}

// rangesMoved passes the pool changes that can free addresses for another pool: its NetBox ranges changed or it is gone.
var rangesMoved = predicate.Funcs{
	CreateFunc: func(event.CreateEvent) bool { return false },
	UpdateFunc: func(e event.UpdateEvent) bool {
		o, ok := e.ObjectOld.(*v1alpha1.NetBoxIPPool)
		n, ok2 := e.ObjectNew.(*v1alpha1.NetBoxIPPool)
		return ok && ok2 && !slices.Equal(o.Status.Ranges, n.Status.Ranges)
	},
	GenericFunc: func(event.GenericEvent) bool { return false },
}

func addressToPool(ctx context.Context, o client.Object) []reconcile.Request {
	a, ok := asType[*ipamv1.IPAddress](ctx, o)
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Spec.PoolRef.Name}}}
}

func (r *NetBoxIPPoolReconciler) networkToPools(ctx context.Context, o client.Object) []reconcile.Request {
	return r.poolsIn(ctx, o.GetNamespace(), o.GetName())
}

// poolToPools wakes the other pools in the networks of a pool, which may wait for its addresses (RangeOverlap).
func (r *NetBoxIPPoolReconciler) poolToPools(ctx context.Context, o client.Object) []reconcile.Request {
	pool, ok := asType[*v1alpha1.NetBoxIPPool](ctx, o)
	if !ok {
		return nil
	}
	return slices.DeleteFunc(r.poolsIn(ctx, pool.Namespace, poolNetworks(pool)...), func(req reconcile.Request) bool {
		return req.Name == pool.Name
	})
}

// poolsIn returns the pools of namespace with a range in one of networks.
func (r *NetBoxIPPoolReconciler) poolsIn(ctx context.Context, namespace string, networks ...string) []reconcile.Request {
	pools := &v1alpha1.NetBoxIPPoolList{}
	if err := r.List(ctx, pools, client.InNamespace(namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range pools.Items {
		if slices.ContainsFunc(poolNetworks(&pools.Items[i]), func(n string) bool { return slices.Contains(networks, n) }) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&pools.Items[i])})
		}
	}
	return reqs
}

// poolNetworks are the networks a pool has ranges in: those of the spec and the removed ones it still holds.
func poolNetworks(pool *v1alpha1.NetBoxIPPool) []string {
	var names []string
	for _, rg := range pool.Spec.Ranges {
		names = append(names, rg.NetworkName)
	}
	for _, rs := range pool.Status.Ranges {
		if !slices.Contains(names, rs.NetworkName) {
			names = append(names, rs.NetworkName)
		}
	}
	return names
}

func (r *NetBoxIPPoolReconciler) clusterToPools(ctx context.Context, o client.Object) []reconcile.Request {
	pools := &v1alpha1.NetBoxIPPoolList{}
	if err := r.List(ctx, pools, client.InNamespace(o.GetNamespace()), client.MatchingLabels{clusterv1.ClusterNameLabel: o.GetName()}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(pools.Items))
	for i := range pools.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&pools.Items[i])})
	}
	return reqs
}

// Reconcile reconciles a NetBoxIPPool.
func (r *NetBoxIPPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	pool := &v1alpha1.NetBoxIPPool{}
	if err := r.Get(ctx, req.NamespacedName, pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if r.WatchFilterValue != "" && !labels.HasWatchLabel(pool, r.WatchFilterValue) {
		return ctrl.Result{}, nil
	}
	if paused, err := isPaused(ctx, r.Client, pool, pool.Labels[clusterv1.ClusterNameLabel]); err != nil || paused {
		return ctrl.Result{}, err
	}
	helper, err := patch.NewHelper(pool, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		if err := helper.Patch(ctx, pool); err != nil {
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()
	if !pool.DeletionTimestamp.IsZero() {
		err = r.reconcileDelete(ctx, pool)
		setDeleting(&pool.Status.Conditions, pool.Generation, err)
		return toResult(err)
	}
	if controllerutil.AddFinalizer(pool, poolFinalizer) {
		return ctrl.Result{}, nil
	}
	err = r.reconcileNormal(ctx, pool)
	ready := err
	if slices.ContainsFunc(pool.Status.Ranges, func(rs v1alpha1.IPPoolRangeStatus) bool { return rs.Ready }) {
		ready = nil
	}
	if ferr := validateAddressFields(pool.Spec.IPAddress); ferr != nil {
		ready = withReason(v1alpha1.ReasonInvalidSpec, fmt.Errorf("ipAddress: %w", ferr))
	}
	setReady(&pool.Status.Conditions, pool.Generation, ready)
	setCondition(&pool.Status.Conditions, v1alpha1.RangesReadyCondition, v1alpha1.ReasonReady, pool.Generation, err)
	return toResult(err)
}

// reconcileNormal brings every range to NetBox on its own: a range that cannot be served does not stop the others.
func (r *NetBoxIPPoolReconciler) reconcileNormal(ctx context.Context, pool *v1alpha1.NetBoxIPPool) error {
	addrs, err := r.poolAddresses(ctx, pool)
	if err != nil {
		return err
	}
	// The network of the first range sets the L2: every range shares its subnet and gateway; without it none serves.
	first := pool.Spec.Ranges[0].NetworkName
	head := &v1alpha1.NetBoxNetwork{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: pool.Namespace, Name: first}, head); err != nil {
		if !apierrors.IsNotFound(err) {
			return apiError(err)
		}
		head = nil
	}
	var (
		l2 netip.Prefix
		gw netip.Addr
	)
	if head != nil {
		if l2, gw, err = l2Of(head); err != nil {
			return err
		}
	}
	prev := map[string]*v1alpha1.IPPoolRangeStatus{}
	for i := range pool.Status.Ranges {
		prev[pool.Status.Ranges[i].NetworkName] = &pool.Status.Ranges[i]
	}
	var (
		ranges []v1alpha1.IPPoolRangeStatus
		errs   []error
	)
	add := func(network string, rs *v1alpha1.IPPoolRangeStatus, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("range %s: %w", network, err))
		}
		if rs == nil || slices.ContainsFunc(ranges, func(o v1alpha1.IPPoolRangeStatus) bool { return o.NetworkName == network }) {
			return
		}
		rs.Reason, rs.Message = "", ""
		if err != nil {
			rs.Reason, rs.Message = errReason(err), err.Error()
		}
		ranges = append(ranges, *rs)
	}
	inSpec := map[string]bool{}
	for _, rg := range pool.Spec.Ranges {
		inSpec[rg.NetworkName] = true
		last := prev[rg.NetworkName]
		nw, err := r.usableNetwork(ctx, pool.Namespace, rg.NetworkName)
		switch {
		case err != nil:
		case head == nil:
			err = waitFor(v1alpha1.ReasonNetworkNotReady, fmt.Errorf("network %s of the first range is missing", first))
		default:
			if nl2, ngw, lerr := l2Of(nw); lerr != nil {
				err = lerr
			} else if nl2 != l2 || ngw != gw {
				err = retryLater(v1alpha1.ReasonMixedNetworks, fmt.Errorf("networks %s and %s are in different L2 networks; a pool stays in one", head.Name, nw.Name))
			}
		}
		if err != nil {
			add(rg.NetworkName, notReady(last), err)
			continue
		}
		rs, err := r.reconcileRange(ctx, pool, nw, rg, last, addrs)
		add(rg.NetworkName, rs, err)
	}
	// Removed ranges are found in NetBox: the status may be gone, as after clusterctl move.
	prefix := rangeRef(pool, "")
	owned, err := r.NetBox.FindByRefPrefix(ctx, netbox.IPRanges, r.Owner, prefix)
	if err != nil {
		errs = append(errs, fmt.Errorf("removed ranges: %w", err))
		for _, rs := range pool.Status.Ranges {
			if !inSpec[rs.NetworkName] {
				ranges = append(ranges, *notReady(&rs))
			}
		}
	}
	for _, o := range owned {
		if network := strings.TrimPrefix(o.Ref(), prefix); !inSpec[network] {
			kept, err := r.removeRange(ctx, pool, network, o, addrs)
			add(network, kept, err)
		}
	}

	total, used := 0, 0
	for _, rs := range ranges {
		start, end, err := statusBounds(rs)
		if err != nil || !rs.Ready {
			continue
		}
		total += netmath.Count(start, end)
		for _, a := range addrs {
			if netmath.Contains(start, end, a) {
				used++
			}
		}
	}
	pool.Status.Ranges = ranges
	pool.Status.IPAddresses = &v1alpha1.NetBoxIPPoolStatusIPAddresses{Total: int32(total), Used: int32(used), Free: int32(total - used)}
	if head != nil {
		pool.Status.Gateway, pool.Status.AddressPrefixLength = gw.String(), int32(l2.Bits())
	}
	return foldErrors(errs)
}

// usableNetwork gets a network the pool can have a range in.
func (r *NetBoxIPPoolReconciler) usableNetwork(ctx context.Context, namespace, name string) (*v1alpha1.NetBoxNetwork, error) {
	nw := &v1alpha1.NetBoxNetwork{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, nw); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, waitFor(v1alpha1.ReasonNetworkNotReady, fmt.Errorf("network %s: %w", name, err))
		}
		return nil, apiError(err)
	}
	if !networkUsable(nw) {
		return nil, waitFor(v1alpha1.ReasonNetworkNotReady, fmt.Errorf("network %s is not ready", name))
	}
	return nw, nil
}

// foldErrors makes one error of errs, nils left out: the reason of the first, the most urgent requeue, every message.
func foldErrors(errs []error) error {
	agg := kerrors.NewAggregate(errs)
	if agg == nil {
		return nil
	}
	errs = agg.Errors()
	out := reasonError{reason: errReason(errs[0]), kind: kindWait, err: agg}
	for _, err := range errs {
		re, ok := asReason(err)
		switch {
		case ok && re.reason == v1alpha1.ReasonInvalidSpec: // never retried, as in toResult
		case !ok || re.kind == kindError:
			out.kind = kindError
		case re.kind == kindRetry && out.kind == kindWait:
			out.kind = kindRetry
		}
	}
	return out
}

// networkUsable reports whether a pool can use the network's prefix: it is Ready, or it is being deleted
// and held back by the pools, so it keeps its prefix and the pools go on allocating until they are gone.
// A deleting network counts only with WaitingForPools: Ready=True there is stale and Deleting is set
// once the prefix is released.
func networkUsable(nw *v1alpha1.NetBoxNetwork) bool {
	ready := meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.ReadyCondition)
	switch {
	case nw.Status.Prefix == "" || ready == nil:
		return false
	case !nw.DeletionTimestamp.IsZero():
		return ready.Reason == v1alpha1.ReasonWaitingForPools
	}
	return ready.Status == metav1.ConditionTrue
}

// l2Of returns the nodes' subnet of nw (gateway with addressPrefixLength, by default the parent mask) and its gateway.
func l2Of(nw *v1alpha1.NetBoxNetwork) (netip.Prefix, netip.Addr, error) {
	parent, err := netip.ParsePrefix(nw.Spec.ParentPrefix)
	if err != nil {
		return parent, netip.Addr{}, err
	}
	gw, err := netip.ParseAddr(nw.Spec.Gateway)
	if err != nil {
		return parent, gw, err
	}
	bits := parent.Bits()
	if nw.Spec.AddressPrefixLength != 0 {
		bits = int(nw.Spec.AddressPrefixLength)
	}
	l2, err := gw.Prefix(bits)
	return l2, gw, err
}

// rangeAddresses turns a range's offsets into addresses, refusing ones a VM must never get.
func rangeAddresses(prefix netip.Prefix, rg v1alpha1.IPPoolRange, l2 netip.Prefix, reserved ...netip.Addr) (netip.Addr, netip.Addr, error) {
	start, err := netmath.AddressAt(prefix, int(rg.Start))
	if err != nil {
		return start, start, fmt.Errorf("range in %s: start: %w", rg.NetworkName, err)
	}
	end, err := netmath.AddressAt(prefix, int(rg.End))
	if err != nil {
		return start, end, fmt.Errorf("range in %s: end: %w", rg.NetworkName, err)
	}
	if err := checkSpan(prefix, start, end, l2, reserved...); err != nil {
		return start, end, fmt.Errorf("range in %s: %w", rg.NetworkName, err)
	}
	return start, end, nil
}

// checkSpan refuses start-end if it is reversed, leaves prefix or covers the network or broadcast of l2 or a reserved address.
func checkSpan(prefix netip.Prefix, start, end netip.Addr, l2 netip.Prefix, reserved ...netip.Addr) error {
	switch {
	case end.Less(start):
		return fmt.Errorf("start %s is after end %s", start, end)
	case !prefix.Contains(start) || !prefix.Contains(end):
		return fmt.Errorf("%s-%s is not inside %s", start, end, prefix)
	}
	for _, bad := range append([]netip.Addr{l2.Addr(), netmath.Last(l2)}, reserved...) {
		if netmath.Contains(start, end, bad) {
			return fmt.Errorf("%s-%s covers %s (network or broadcast of %s, or a reserved address)", start, end, bad, l2)
		}
	}
	return nil
}

func rangeRef(pool *v1alpha1.NetBoxIPPool, network string) string {
	return refOf("NetBoxIPPool", pool) + "/" + network
}

// notReady is rs taken out of service, nil without rs.
func notReady(rs *v1alpha1.IPPoolRangeStatus) *v1alpha1.IPPoolRangeStatus {
	if rs == nil {
		return nil
	}
	c := *rs
	c.Ready = false
	return &c
}

func statusBounds(rs v1alpha1.IPPoolRangeStatus) (netip.Addr, netip.Addr, error) {
	start, err := netip.ParseAddr(rs.StartAddress)
	if err != nil {
		return start, start, err
	}
	end, err := netip.ParseAddr(rs.EndAddress)
	return start, end, err
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

// reconcileRange makes the NetBox range of rg in nw match the spec. It returns the status of the range,
// nil while NetBox has none; last is the one known before.
func (r *NetBoxIPPoolReconciler) reconcileRange(ctx context.Context, pool *v1alpha1.NetBoxIPPool, nw *v1alpha1.NetBoxNetwork, rg v1alpha1.IPPoolRange, last *v1alpha1.IPPoolRangeStatus, addrs []netip.Addr) (*v1alpha1.IPPoolRangeStatus, error) {
	l2, gw, err := l2Of(nw)
	if err != nil {
		return notReady(last), err
	}
	prefix, err := netip.ParsePrefix(nw.Status.Prefix)
	if err != nil {
		return notReady(last), err
	}
	reserved := reservedOf(nw, gw)
	start, end, err := rangeAddresses(prefix, rg, l2, reserved...)
	if err != nil {
		err = retryLater(v1alpha1.ReasonInvalidRange, err)
		// Until the spec is fixed the range keeps its old bounds, if they are still valid.
		if last != nil && last.Ready {
			if from, to, berr := statusBounds(*last); berr == nil && checkSpan(prefix, from, to, l2, reserved...) == nil {
				c := *last
				return &c, err
			}
		}
		return notReady(last), err
	}
	ref := rangeRef(pool, nw.Name)
	netBoxWrites.Lock()
	defer netBoxWrites.Unlock()
	found, err := r.NetBox.FindByRef(ctx, netbox.IPRanges, r.Owner, ref)
	switch {
	case err != nil:
		return notReady(last), err
	case len(found) > 1:
		return notReady(last), retryLater(v1alpha1.ReasonDuplicate, fmt.Errorf("%d NetBox IP ranges carry %s", len(found), ref))
	case len(found) == 1:
		return r.resizeRange(ctx, nw, ref, found[0], start, end, l2.Bits(), addrs)
	}
	// A range over the VIP must not appear: new ranges wait for it, existing ones go on serving.
	if nw.Spec.ControlPlaneEndpoint != nil && nw.Status.ControlPlaneEndpoint.Host == "" {
		return nil, waitFor(v1alpha1.ReasonNetworkNotReady, fmt.Errorf("waiting for the VIP of network %s", nw.Name))
	}
	if err := ownedByOtherOwner(ctx, r.NetBox, r.Owner, netbox.IPRanges, ref); err != nil {
		return nil, err
	}
	var obj netbox.Object
	err = r.clearingDeprecated(ctx, nw, ref, start, end, 0, func() (err error) {
		obj, err = r.NetBox.Create(ctx, netbox.IPRanges, r.Owner.Managed(ref, nw.Spec.ClusterName, "capi: range "+ref, map[string]any{
			"start_address": netip.PrefixFrom(start, l2.Bits()).String(), "end_address": netip.PrefixFrom(end, l2.Bits()).String(),
			"status": "active", "description": "CAPI pool " + pool.Name,
		}))
		return err
	})
	if err != nil {
		return nil, err
	}
	return &v1alpha1.IPPoolRangeStatus{NetworkName: nw.Name, ID: obj.ID, StartAddress: start.String(), EndAddress: end.String(), Ready: true}, nil
}

// rangeStatus is the status of the NetBox range o in network, serving unless deprecated, and its bounds.
func rangeStatus(network string, o netbox.Object) (*v1alpha1.IPPoolRangeStatus, netip.Addr, netip.Addr, error) {
	rs := &v1alpha1.IPPoolRangeStatus{NetworkName: network, ID: o.ID, StartAddress: o.StartAddress, EndAddress: o.EndAddress}
	from, to, err := netBoxBounds(o)
	if err == nil {
		rs.StartAddress, rs.EndAddress, rs.Ready = from.String(), to.String(), o.Status.Value != "deprecated"
	}
	return rs, from, to, err
}

// resizeRange brings the NetBox range cur to start-end, active and open to allocation. While addresses are allocated
// outside start-end, or NetBox refuses the change, the range keeps its bounds and serves unless deprecated;
// addresses still allocated take a deprecated range back.
func (r *NetBoxIPPoolReconciler) resizeRange(ctx context.Context, nw *v1alpha1.NetBoxNetwork, ref string, cur netbox.Object, start, end netip.Addr, bits int, addrs []netip.Addr) (*v1alpha1.IPPoolRangeStatus, error) {
	have, from, to, err := rangeStatus(nw.Name, cur)
	if err != nil || from == start && to == end && have.Ready && !cur.MarkPopulated {
		return have, err
	}
	cur, etag, err := r.NetBox.Get(ctx, netbox.IPRanges, cur.ID)
	if err != nil {
		return have, err
	}
	if have, from, to, err = rangeStatus(nw.Name, cur); err != nil {
		return have, err
	}
	closed := cur.MarkPopulated
	if keep := func(a netip.Addr) bool { return netmath.Contains(start, end, a) }; !keep(from) || !keep(to) {
		var fenced bool
		etag, fenced, err = r.fence(ctx, cur, etag, addrs, keep, fmt.Sprintf("NetBox range stays %s-%s while addresses outside %s-%s are allocated", from, to, start, end))
		closed = closed || fenced
	}
	if err == nil {
		message := "capi: resized by " + ref
		if !have.Ready {
			message = "capi: reclaimed by " + ref
		}
		err = r.clearingDeprecated(ctx, nw, ref, start, end, cur.ID, func() error {
			_, err := r.NetBox.Patch(ctx, netbox.IPRanges, cur.ID, etag, map[string]any{
				"start_address": netip.PrefixFrom(start, bits).String(), "end_address": netip.PrefixFrom(end, bits).String(),
				"status": "active", "mark_populated": false, "changelog_message": message,
			})
			return err
		})
	}
	if err == nil {
		ctrl.LoggerFrom(ctx).Info("Updated the NetBox range", "ref", ref, "id", cur.ID, "from", from.String()+"-"+to.String(), "to", start.String()+"-"+end.String())
		return &v1alpha1.IPPoolRangeStatus{NetworkName: nw.Name, ID: cur.ID, StartAddress: start.String(), EndAddress: end.String(), Ready: true}, nil
	}
	if errReason(err) == v1alpha1.ReasonRangeInUse {
		have.Ready = true
	}
	if have.Ready && (closed || cur.Status.Value == "deprecated") {
		if _, oerr := r.NetBox.Patch(ctx, netbox.IPRanges, cur.ID, etag, map[string]any{
			"status": "active", "mark_populated": false, "changelog_message": "capi: kept by " + ref,
		}); oerr != nil {
			have.Ready = cur.Status.Value != "deprecated"
			return have, oerr
		}
	}
	return have, err
}

// fence closes the NetBox range cur to allocation (mark_populated) once no address in it but those keep accepts
// is allocated, and checks again: from there none can appear until the caller acts with the returned ETag.
// It reports whether it closed the range. Allocated addresses are RangeInUse, explained by why; found on the
// second check, they open the range again. The caller holds netBoxWrites, so no allocation of ours lands in between.
func (r *NetBoxIPPoolReconciler) fence(ctx context.Context, cur netbox.Object, etag string, addrs []netip.Addr, keep func(netip.Addr) bool, why string) (string, bool, error) {
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
		return r.NetBox.Patch(ctx, netbox.IPRanges, cur.ID, etag, map[string]any{"mark_populated": on, "changelog_message": message + cur.Ref()})
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
// in the way that hold no addresses are deleted and write runs once more; a range still in the way is RangeOverlap.
func (r *NetBoxIPPoolReconciler) clearingDeprecated(ctx context.Context, nw *v1alpha1.NetBoxNetwork, ref string, start, end netip.Addr, self int64, write func() error) error {
	err := write()
	if !errors.Is(err, netbox.ErrOverlap) {
		return err
	}
	deprecated, err := r.NetBox.List(ctx, netbox.IPRanges, url.Values{
		"parent": {nw.Status.Prefix}, "owner_id": {strconv.FormatInt(r.Owner.ID, 10)}, "status": {"deprecated"},
	})
	if err != nil {
		return err
	}
	for _, o := range deprecated {
		from, to, err := netBoxBounds(o)
		if err != nil || o.ID == self || to.Less(start) || end.Less(from) {
			continue
		}
		held, err := r.allocated(ctx, nil, from, to, nil)
		if err != nil {
			return err
		}
		if len(held) > 0 {
			continue
		}
		if err := r.NetBox.Delete(ctx, netbox.IPRanges, o.ID, "capi: replaced by "+ref); err != nil {
			return err
		}
		ctrl.LoggerFrom(ctx).Info("Deleted a deprecated NetBox range in the way", "ref", ref, "id", o.ID, "range", from.String()+"-"+to.String())
	}
	if err := write(); errors.Is(err, netbox.ErrOverlap) {
		return retryLater(v1alpha1.ReasonRangeOverlap, err)
	} else if err != nil {
		return err
	}
	return nil
}

// allocated returns the addresses in from-to that the pool's IPAddresses (addrs) or our NetBox addresses hold,
// leaving out those keep accepts. NetBox also shows an allocation whose IPAddress is not created yet.
func (r *NetBoxIPPoolReconciler) allocated(ctx context.Context, addrs []netip.Addr, from, to netip.Addr, keep func(netip.Addr) bool) ([]string, error) {
	ips, err := r.NetBox.List(ctx, netbox.IPAddresses, url.Values{
		"parent": {netmath.Cover(from, to).String()}, "owner_id": {strconv.FormatInt(r.Owner.ID, 10)},
	})
	if err != nil {
		return nil, err
	}
	all := slices.Clone(addrs)
	for _, ip := range ips {
		if p, err := netip.ParsePrefix(ip.Address); err == nil {
			all = append(all, p.Addr())
		}
	}
	slices.SortFunc(all, netip.Addr.Compare)
	var out []string
	for _, a := range slices.Compact(all) {
		if netmath.Contains(from, to, a) && (keep == nil || !keep(a)) {
			out = append(out, a.String())
		}
	}
	return out, nil
}

// listAddresses names the first addresses of held; a condition message has a size limit.
func listAddresses(held []string) string {
	const shown = 10
	if len(held) > shown {
		return strings.Join(held[:shown], ", ") + fmt.Sprintf(" and %d more", len(held)-shown)
	}
	return strings.Join(held, ", ")
}

// removeRange releases the NetBox range o of the pool, whose network left the spec. It returns the status to keep:
// nil once the range is released, not ready while addresses are still allocated in it.
func (r *NetBoxIPPoolReconciler) removeRange(ctx context.Context, pool *v1alpha1.NetBoxIPPool, network string, o netbox.Object, addrs []netip.Addr) (*v1alpha1.IPPoolRangeStatus, error) {
	kept, _, _, err := rangeStatus(network, o)
	kept.Ready = false
	if err == nil {
		err = r.releaseRange(ctx, pool, network, o, addrs)
	}
	if err != nil {
		return kept, err
	}
	return nil, nil
}

// releaseRange deletes or deprecates the NetBox range o of the pool in network, as the network's deletionPolicy says;
// without the network it is deprecated. Addresses still allocated in it keep it (RangeInUse).
// A deprecated range stays closed to allocation (mark_populated).
func (r *NetBoxIPPoolReconciler) releaseRange(ctx context.Context, pool *v1alpha1.NetBoxIPPool, network string, o netbox.Object, addrs []netip.Addr) error {
	policy := v1alpha1.DeletionPolicyRetain
	nw := &v1alpha1.NetBoxNetwork{}
	switch err := r.Get(ctx, client.ObjectKey{Namespace: pool.Namespace, Name: network}, nw); {
	case err == nil:
		policy = nw.Spec.DeletionPolicy
	case !apierrors.IsNotFound(err):
		return apiError(err)
	}
	if policy == v1alpha1.DeletionPolicyRetain && o.Status.Value == "deprecated" {
		return nil
	}
	netBoxWrites.Lock()
	defer netBoxWrites.Unlock()
	cur, etag, err := r.NetBox.Get(ctx, netbox.IPRanges, o.ID)
	if errors.Is(err, netbox.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if etag, _, err = r.fence(ctx, cur, etag, addrs, nil, "kept while addresses in it are allocated"); err != nil {
		return err
	}
	released := "capi: released by " + refOf("NetBoxIPPool", pool)
	if policy == v1alpha1.DeletionPolicyDelete {
		err = r.NetBox.Delete(ctx, netbox.IPRanges, cur.ID, released)
	} else {
		_, err = r.NetBox.Patch(ctx, netbox.IPRanges, cur.ID, etag, map[string]any{
			"status": "deprecated", "mark_populated": true, "changelog_message": released,
		})
	}
	if err != nil && !errors.Is(err, netbox.ErrNotFound) {
		return err
	}
	ctrl.LoggerFrom(ctx).Info("Released the NetBox range of the pool", "pool", klog.KObj(pool), "network", network, "id", cur.ID, "policy", policy)
	return nil
}

// poolAddresses returns the addresses of the pool's IPAddresses.
func (r *NetBoxIPPoolReconciler) poolAddresses(ctx context.Context, pool *v1alpha1.NetBoxIPPool) ([]netip.Addr, error) {
	list := &ipamv1.IPAddressList{}
	if err := r.List(ctx, list, client.InNamespace(pool.Namespace), client.MatchingFields{poolRefField: pool.Name}); err != nil {
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

func (r *NetBoxIPPoolReconciler) reconcileDelete(ctx context.Context, pool *v1alpha1.NetBoxIPPool) error {
	if deletedForMove(pool) {
		controllerutil.RemoveFinalizer(pool, poolFinalizer)
		return nil
	}
	addrs, err := r.poolAddresses(ctx, pool)
	if err != nil {
		return err
	}
	if len(addrs) > 0 {
		return waitFor(v1alpha1.ReasonWaitingForAddresses, fmt.Errorf("%d addresses are still allocated from the pool", len(addrs)))
	}
	prefix := rangeRef(pool, "")
	owned, err := r.NetBox.FindByRefPrefix(ctx, netbox.IPRanges, r.Owner, prefix)
	if err != nil {
		return err
	}
	var errs []error
	for _, o := range owned {
		if err := r.releaseRange(ctx, pool, strings.TrimPrefix(o.Ref(), prefix), o, nil); err != nil {
			errs = append(errs, err)
		}
	}
	if err := foldErrors(errs); err != nil {
		return err
	}
	controllerutil.RemoveFinalizer(pool, poolFinalizer)
	return nil
}
