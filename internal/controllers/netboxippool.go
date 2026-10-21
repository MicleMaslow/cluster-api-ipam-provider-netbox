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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/utils/ptr"
	ipampredicates "sigs.k8s.io/cluster-api-ipam-provider-in-cluster/pkg/predicates"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/cluster-api/util/labels"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/paused"
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
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netmath"
)

const poolFinalizer = "netbox.ipam.cluster.x-k8s.io/pool"

// NetBoxIPPoolReconciler serves the NetBox ranges of the network pools of a NetBoxIPPool, which their networks own.
type NetBoxIPPoolReconciler struct {
	client.Client
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
		Watches(&v1alpha1.NetBoxIPPool{}, handler.EnqueueRequestsFromMapFunc(r.poolToPools), builder.WithPredicates(label, poolRangesChanged)).
		Watches(&v1alpha1.NetBoxNetwork{}, handler.EnqueueRequestsFromMapFunc(r.networkToPools), builder.WithPredicates(label)).
		// ipamutil creates IPAddresses without the watch label; Reconcile skips pools of another filter.
		Watches(&ipamv1.IPAddress{}, handler.EnqueueRequestsFromMapFunc(addressToPool),
			builder.WithPredicates(ipampredicates.AddressReferencesPoolKind(poolGK))).
		Watches(&clusterv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.clusterToPools), builder.WithPredicates(label, clusterEvents)).
		Complete(r)
}

// poolRangesChanged passes the pool changes that can start or end a Duplicate of another pool: it is created or gone,
// or its ranges changed.
var poolRangesChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		o, ok := e.ObjectOld.(*v1alpha1.NetBoxIPPool)
		n, ok2 := e.ObjectNew.(*v1alpha1.NetBoxIPPool)
		return ok && ok2 && !slices.Equal(o.Spec.Ranges, n.Spec.Ranges)
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

// poolToPools wakes the other pools in the networks of a pool, which may be or become its Duplicate.
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

// poolNetworks are the networks of the ranges of a pool.
func poolNetworks(pool *v1alpha1.NetBoxIPPool) []string {
	var names []string
	for _, rg := range pool.Spec.Ranges {
		if !slices.Contains(names, rg.NetworkName) {
			names = append(names, rg.NetworkName)
		}
	}
	return names
}

// clusterToPools wakes the pools of a Cluster: those with its label and those with a range in one of its networks.
func (r *NetBoxIPPoolReconciler) clusterToPools(ctx context.Context, o client.Object) []reconcile.Request {
	pools := &v1alpha1.NetBoxIPPoolList{}
	if err := r.List(ctx, pools, client.InNamespace(o.GetNamespace()), client.MatchingLabels{clusterv1.ClusterNameLabel: o.GetName()}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(pools.Items))
	for i := range pools.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&pools.Items[i])})
	}
	networks := &v1alpha1.NetBoxNetworkList{}
	if err := r.List(ctx, networks, client.InNamespace(o.GetNamespace()), client.MatchingFields{clusterNameField: o.GetName()}); err != nil {
		return reqs
	}
	names := make([]string, 0, len(networks.Items))
	for _, nw := range networks.Items {
		names = append(names, nw.Name)
	}
	return append(reqs, r.poolsIn(ctx, o.GetNamespace(), names...)...)
}

// pauseCluster returns the Cluster for the pool's Paused condition: the first paused one of its label and its
// networks, else the Cluster of its label, nil without one.
func (r *NetBoxIPPoolReconciler) pauseCluster(ctx context.Context, pool *v1alpha1.NetBoxIPPool) (*clusterv1.Cluster, error) {
	names := []string{pool.Labels[clusterv1.ClusterNameLabel]}
	for _, n := range poolNetworks(pool) {
		nw := &v1alpha1.NetBoxNetwork{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: pool.Namespace, Name: n}, nw); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, err
			}
			continue
		}
		names = append(names, nw.Spec.ClusterName)
	}
	var labeled *clusterv1.Cluster
	for i, name := range names {
		if name == "" {
			continue
		}
		cluster := &clusterv1.Cluster{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: pool.Namespace, Name: name}, cluster); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, err
			}
			continue
		}
		if ptr.Deref(cluster.Spec.Paused, false) {
			return cluster, nil
		}
		if i == 0 {
			labeled = cluster
		}
	}
	return labeled, nil
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
	cluster, err := r.pauseCluster(ctx, pool)
	if err != nil {
		return ctrl.Result{}, err
	}
	// The helper comes first: a Paused condition that only lags the generation is set in memory, for it to write.
	helper, err := patch.NewHelper(pool, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	if isPaused, requeue, err := paused.EnsurePausedCondition(ctx, r.Client, cluster, pool); err != nil || isPaused || requeue {
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
	if ferr := validatePoolTemplates(pool); ferr != nil {
		ready = withReason(v1alpha1.ReasonInvalidSpec, ferr)
	}
	setReady(&pool.Status.Conditions, pool.Generation, ready)
	setCondition(&pool.Status.Conditions, v1alpha1.RangesReadyCondition, v1alpha1.ReasonReady, pool.Generation, err)
	return toResult(err)
}

// reconcileNormal reflects the NetBox ranges of the network pools of the spec in the status, each range on its own:
// one that cannot serve does not stop the others.
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
	others := &v1alpha1.NetBoxIPPoolList{}
	if err := r.List(ctx, others, client.InNamespace(pool.Namespace)); err != nil {
		return apiError(err)
	}
	var (
		ranges []v1alpha1.IPPoolRangeStatus
		errs   []error
	)
	for _, rg := range pool.Spec.Ranges {
		placed, err := r.placedPool(ctx, pool, rg, head, l2, gw, others.Items)
		if err != nil {
			err = fmt.Errorf("range %s/%s: %w", rg.NetworkName, rg.Pool, err)
			errs = append(errs, err)
		}
		pending := false
		for _, lr := range placed.Ranges {
			rs := v1alpha1.IPPoolRangeStatus{NetworkName: rg.NetworkName, ID: lr.ID, StartAddress: lr.Start, EndAddress: lr.End, Ready: err == nil && lr.ID != 0}
			if err != nil {
				rs.Reason, rs.Message = errReason(err), err.Error()
			}
			pending = pending || err == nil && lr.ID == 0
			ranges = append(ranges, rs)
		}
		if pending {
			errs = append(errs, waitFor(v1alpha1.ReasonNetworkNotReady, fmt.Errorf("range %s/%s: the network has not created all its NetBox ranges yet", rg.NetworkName, rg.Pool)))
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
	if ptr.Deref(pool.Spec.OmitGateway, false) {
		pool.Status.Gateway = ""
	}
	return foldErrors(errs)
}

// placedPool returns the layout of the network pool of rg and why it cannot serve: its network is not usable, is in
// another L2 network than head (l2, gw), has not placed the pool of its spec, or the pool serves another pool of
// others, created before this one (of two created at once, the one with the lower name).
func (r *NetBoxIPPoolReconciler) placedPool(ctx context.Context, pool *v1alpha1.NetBoxIPPool, rg v1alpha1.IPPoolRange, head *v1alpha1.NetBoxNetwork, l2 netip.Prefix, gw netip.Addr, others []v1alpha1.NetBoxIPPool) (v1alpha1.PoolLayout, error) {
	nw, err := r.usableNetwork(ctx, pool.Namespace, rg.NetworkName)
	if nw == nil {
		return v1alpha1.PoolLayout{}, err
	}
	if nw.Spec.Allocator == v1alpha1.AllocatorInCluster {
		return v1alpha1.PoolLayout{}, waitFor(v1alpha1.ReasonInvalidRange, fmt.Errorf("network %s hands out addresses through InClusterIPPools", nw.Name))
	}
	var placed v1alpha1.PoolLayout
	if i := slices.IndexFunc(nw.Status.Layout.Pools, func(p v1alpha1.PoolLayout) bool { return p.Name == rg.Pool }); i >= 0 {
		placed = nw.Status.Layout.Pools[i]
	}
	if err != nil {
		return placed, err
	}
	if head == nil {
		return placed, waitFor(v1alpha1.ReasonNetworkNotReady, fmt.Errorf("network %s of the first range is missing", pool.Spec.Ranges[0].NetworkName))
	}
	if nl2, ngw, err := l2Of(nw); err != nil {
		return placed, err
	} else if nl2 != l2 || ngw != gw {
		return placed, retryLater(v1alpha1.ReasonMixedNetworks, fmt.Errorf("networks %s and %s are in different L2 networks; a pool stays in one", head.Name, nw.Name))
	}
	if placed.Name == "" || !slices.ContainsFunc(nw.Spec.Pools, func(p v1alpha1.NetworkPool) bool { return p.Name == rg.Pool }) {
		return placed, waitFor(v1alpha1.ReasonNetworkNotReady, fmt.Errorf("pool %s is not placed in network %s", rg.Pool, nw.Name))
	}
	for i := range others {
		o := &others[i]
		before := o.CreationTimestamp.Before(&pool.CreationTimestamp) || o.CreationTimestamp.Equal(&pool.CreationTimestamp) && o.Name < pool.Name
		if o.Name != pool.Name && before && slices.Contains(o.Spec.Ranges, rg) {
			return placed, waitFor(v1alpha1.ReasonDuplicate, fmt.Errorf("pool %s of network %s serves NetBoxIPPool %s", rg.Pool, nw.Name, o.Name))
		}
	}
	return placed, nil
}

// usableNetwork gets a network the pool can have a range in; one that is not usable comes with the error.
func (r *NetBoxIPPoolReconciler) usableNetwork(ctx context.Context, namespace, name string) (*v1alpha1.NetBoxNetwork, error) {
	nw := &v1alpha1.NetBoxNetwork{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, nw); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, waitFor(v1alpha1.ReasonNetworkNotReady, fmt.Errorf("network %s: %w", name, err))
		}
		return nil, apiError(err)
	}
	if !networkUsable(nw) {
		return nw, waitFor(v1alpha1.ReasonNetworkNotReady, fmt.Errorf("network %s is not ready", name))
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

func statusBounds(rs v1alpha1.IPPoolRangeStatus) (netip.Addr, netip.Addr, error) {
	start, err := netip.ParseAddr(rs.StartAddress)
	if err != nil {
		return start, start, err
	}
	end, err := netip.ParseAddr(rs.EndAddress)
	return start, end, err
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
	if !deletedForMove(pool) {
		addrs, err := r.poolAddresses(ctx, pool)
		if err != nil {
			return err
		}
		if len(addrs) > 0 {
			return waitFor(v1alpha1.ReasonWaitingForAddresses, fmt.Errorf("%d addresses are still allocated from the pool", len(addrs)))
		}
	}
	controllerutil.RemoveFinalizer(pool, poolFinalizer)
	return nil
}
