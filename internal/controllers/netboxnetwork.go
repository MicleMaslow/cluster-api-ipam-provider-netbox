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
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/annotations"
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
)

const networkFinalizer = "netbox.ipam.cluster.x-k8s.io/network"

// poolLeft passes the pool changes that can unblock a network waiting to be deleted:
// the pool is gone, or the networks it has ranges in changed.
var poolLeft = builder.WithPredicates(predicate.Funcs{
	CreateFunc: func(event.CreateEvent) bool { return false },
	UpdateFunc: func(e event.UpdateEvent) bool {
		o, ok := e.ObjectOld.(*v1alpha1.NetBoxIPPool)
		n, ok2 := e.ObjectNew.(*v1alpha1.NetBoxIPPool)
		return ok && ok2 && !slices.Equal(poolNetworks(o), poolNetworks(n))
	},
	GenericFunc: func(event.GenericEvent) bool { return false },
})

// NetBoxNetworkReconciler holds a NetBox prefix for every NetBoxNetwork.
type NetBoxNetworkReconciler struct {
	client.Client
	NetBox           *netbox.Client
	Owner            netbox.Owner
	WatchFilterValue string
}

// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=netboxnetworks,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=netboxnetworks/status;netboxnetworks/finalizers,verbs=get;update;patch
// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=netboxippools,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch;patch

// SetupWithManager sets up the controller with the Manager.
func (r *NetBoxNetworkReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &v1alpha1.NetBoxNetwork{}, clusterNameField, func(o client.Object) []string {
		nw, ok := asType[*v1alpha1.NetBoxNetwork](ctx, o)
		if !ok {
			return nil
		}
		return []string{nw.Spec.ClusterName}
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(options).
		For(&v1alpha1.NetBoxNetwork{}).
		Watches(&clusterv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.clusterToNetworks), builder.WithPredicates(clusterEvents)).
		Watches(&v1alpha1.NetBoxIPPool{}, handler.EnqueueRequestsFromMapFunc(poolToNetworks), poolLeft).
		WithEventFilter(predicates.ResourceHasFilterLabel(mgr.GetScheme(), ctrl.LoggerFrom(ctx), r.WatchFilterValue)).
		Complete(r)
}

func (r *NetBoxNetworkReconciler) clusterToNetworks(ctx context.Context, o client.Object) []reconcile.Request {
	list := &v1alpha1.NetBoxNetworkList{}
	if err := r.List(ctx, list, client.InNamespace(o.GetNamespace()), client.MatchingFields{clusterNameField: o.GetName()}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}

func poolToNetworks(ctx context.Context, o client.Object) []reconcile.Request {
	pool, ok := asType[*v1alpha1.NetBoxIPPool](ctx, o)
	if !ok {
		return nil
	}
	networks := poolNetworks(pool)
	reqs := make([]reconcile.Request, 0, len(networks))
	for _, n := range networks {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: pool.Namespace, Name: n}})
	}
	return reqs
}

// Reconcile reconciles a NetBoxNetwork.
func (r *NetBoxNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	nw := &v1alpha1.NetBoxNetwork{}
	if err := r.Get(ctx, req.NamespacedName, nw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if paused, err := isPaused(ctx, r.Client, nw, nw.Spec.ClusterName); err != nil || paused {
		return ctrl.Result{}, err
	}
	helper, err := patch.NewHelper(nw, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		if err := helper.Patch(ctx, nw); err != nil {
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()
	if !nw.DeletionTimestamp.IsZero() {
		err = r.reconcileDelete(ctx, nw)
		setDeleting(&nw.Status.Conditions, nw.Generation, err)
		return toResult(err)
	}
	if controllerutil.AddFinalizer(nw, networkFinalizer) {
		return ctrl.Result{}, nil
	}
	var cluster *clusterv1.Cluster
	if nw.Spec.ControlPlaneEndpoint == nil {
		meta.RemoveStatusCondition(&nw.Status.Conditions, v1alpha1.ControlPlaneEndpointReadyCondition)
	} else if cluster, err = r.getCluster(ctx, nw); err != nil {
		return ctrl.Result{}, err
	}
	vipErr, err := r.reconcileNormal(ctx, nw, cluster)
	setReady(&nw.Status.Conditions, nw.Generation, err)
	if err != nil {
		setWaitingForPrefix(nw, v1alpha1.MetadataSyncedCondition)
		if nw.Spec.ControlPlaneEndpoint != nil {
			setWaitingForPrefix(nw, v1alpha1.ControlPlaneEndpointReadyCondition)
		}
		return toResult(err)
	}
	if nw.Spec.ControlPlaneEndpoint != nil {
		vipErr = r.syncClusterEndpoint(ctx, nw, cluster, vipErr)
		setCondition(&nw.Status.Conditions, v1alpha1.ControlPlaneEndpointReadyCondition, v1alpha1.ReasonReady, nw.Generation, vipErr)
	}
	err = r.syncMetadata(ctx, nw, nw.Status.PrefixID)
	setCondition(&nw.Status.Conditions, v1alpha1.MetadataSyncedCondition, v1alpha1.ReasonSynced, nw.Generation, err)
	return toResult(foldErrors([]error{vipErr, err}))
}

// getCluster returns the Cluster of nw from the cache, or nil if there is none yet.
func (r *NetBoxNetworkReconciler) getCluster(ctx context.Context, nw *v1alpha1.NetBoxNetwork) (*clusterv1.Cluster, error) {
	cluster := &clusterv1.Cluster{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: nw.Namespace, Name: nw.Spec.ClusterName}, cluster); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return cluster, nil
}

func hostOf(cluster *clusterv1.Cluster) string {
	if cluster == nil {
		return ""
	}
	return cluster.Spec.ControlPlaneEndpoint.Host
}

// syncClusterEndpoint sets the endpoint of the Cluster to the VIP once, while it has no host or no port
// (CAPI copies a port alone from the infrastructure cluster).
// Its result is ControlPlaneEndpointReady: nil only when the VIP is reserved and the Cluster has it.
// Any other endpoint is a conflict and the Cluster is left alone.
func (r *NetBoxNetworkReconciler) syncClusterEndpoint(ctx context.Context, nw *v1alpha1.NetBoxNetwork, cluster *clusterv1.Cluster, vipErr error) error {
	if vipErr != nil {
		return vipErr
	}
	if cluster == nil {
		return waitFor(v1alpha1.ReasonWaitingForCluster, fmt.Errorf("cluster %s not found", nw.Spec.ClusterName))
	}
	ep, cur := nw.Status.ControlPlaneEndpoint, cluster.Spec.ControlPlaneEndpoint
	switch {
	case cur.Host == ep.Host && cur.Port == ep.Port:
		return nil
	case cur.IsValid():
		return retryLater(v1alpha1.ReasonEndpointConflict, fmt.Errorf("cluster %s has endpoint %s, the VIP is %s:%d", cluster.Name, cur.String(), ep.Host, ep.Port))
	case annotations.IsPaused(cluster, nw):
		return waitFor(v1alpha1.ReasonWaitingForCluster, fmt.Errorf("cluster %s is paused", cluster.Name))
	}
	before := cluster.DeepCopy()
	cluster.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: ep.Host, Port: ep.Port}
	// The lock keeps an endpoint that was set meanwhile, e.g. copied from the infrastructure cluster.
	if err := r.Patch(ctx, cluster, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return apiError(err)
	}
	return nil
}

// setWaitingForPrefix leaves condType Unknown until the prefix is Ready.
func setWaitingForPrefix(nw *v1alpha1.NetBoxNetwork, condType string) {
	meta.SetStatusCondition(&nw.Status.Conditions, metav1.Condition{
		Type: condType, Status: metav1.ConditionUnknown, Reason: v1alpha1.ReasonWaitingForPrefix,
		Message: "the prefix is not Ready", ObservedGeneration: nw.Generation,
	})
}

// reconcileNormal brings the prefix to NetBox, then the VIP. The prefix decides Ready (err); a VIP that
// cannot be reserved is vipErr and leaves Ready alone, so adding a VIP to a live network stops no pool.
func (r *NetBoxNetworkReconciler) reconcileNormal(ctx context.Context, nw *v1alpha1.NetBoxNetwork, cluster *clusterv1.Cluster) (vipErr, err error) {
	netBoxWrites.Lock()
	defer netBoxWrites.Unlock()
	prefix, err := r.reconcilePrefix(ctx, nw)
	if err != nil {
		return nil, err
	}
	// Without the field the VIP stays in NetBox, and in the status so that pools keep off it.
	if nw.Spec.ControlPlaneEndpoint == nil {
		return nil, nil
	}
	vip, err := r.reserveVIP(ctx, nw, prefix, preferredVIP(nw, hostOf(cluster), prefix), cluster != nil)
	if err != nil {
		return err, nil
	}
	nw.Status.ControlPlaneEndpoint = v1alpha1.APIEndpoint{Host: vip.String(), Port: cmp.Or(nw.Spec.ControlPlaneEndpoint.Port, defaultAPIPort)}
	return nil, nil
}

// preferredVIP is the address a new VIP must take. A VIP lost from NetBox comes back at the address in the
// status, or reports EndpointConflict: it never moves. Without one, a Cluster endpoint inside prefix is kept.
func preferredVIP(nw *v1alpha1.NetBoxNetwork, clusterHost string, prefix netip.Prefix) netip.Addr {
	if a, err := netip.ParseAddr(nw.Status.ControlPlaneEndpoint.Host); err == nil {
		return a
	}
	if a, err := netip.ParseAddr(clusterHost); err == nil && prefix.Contains(a) {
		return a
	}
	return netip.Addr{}
}

// reconcilePrefix finds, carves or adopts the prefix of nw and records it in the status. The caller holds netBoxWrites.
func (r *NetBoxNetworkReconciler) reconcilePrefix(ctx context.Context, nw *v1alpha1.NetBoxNetwork) (netip.Prefix, error) {
	parent, err := netip.ParsePrefix(nw.Spec.ParentPrefix)
	if err != nil {
		return netip.Prefix{}, err
	}
	parent = parent.Masked()
	l2, _, err := l2Of(nw)
	if err != nil {
		return netip.Prefix{}, err
	}
	ref := refOf("NetBoxNetwork", nw)
	found, err := r.NetBox.FindByRef(ctx, netbox.Prefixes, r.Owner, ref)
	if err != nil {
		return netip.Prefix{}, err
	}
	var p netbox.Object
	switch len(found) {
	case 0:
		if err := ownedByOtherOwner(ctx, r.NetBox, r.Owner, netbox.Prefixes, ref); err != nil {
			return netip.Prefix{}, err
		}
		if nw.Status.PrefixID != 0 || nw.Status.Prefix != "" {
			return netip.Prefix{}, retryLater(v1alpha1.ReasonPrefixNotFound, fmt.Errorf("NetBox prefix %s of %s is gone, restore it", nw.Status.Prefix, ref))
		}
		if p, err = r.acquire(ctx, nw, ref, parent); err != nil {
			return netip.Prefix{}, err
		}
	case 1:
		p = found[0]
	default:
		return netip.Prefix{}, retryLater(v1alpha1.ReasonDuplicate, fmt.Errorf("%d NetBox prefixes carry %s", len(found), ref))
	}
	cidr, err := checkPrefix(nw, parent, p.Prefix)
	if err != nil {
		return netip.Prefix{}, retryLater(v1alpha1.ReasonPrefixMismatch, err)
	}
	if nw.Spec.Prefix == "" && p.Status.Value == "deprecated" {
		if _, err := r.NetBox.Patch(ctx, netbox.Prefixes, p.ID, "", map[string]any{"status": "active", "changelog_message": "capi: reclaimed by " + ref}); err != nil {
			return netip.Prefix{}, err
		}
	}
	if want := ptr.Deref(nw.Spec.IsPool, false); nw.Spec.Prefix == "" && p.IsPool != want {
		if _, err := r.NetBox.Patch(ctx, netbox.Prefixes, p.ID, "", map[string]any{"is_pool": want, "changelog_message": "capi: is_pool of " + ref}); err != nil {
			return netip.Prefix{}, err
		}
	}
	nw.Status.Prefix, nw.Status.PrefixID, nw.Status.AddressPrefixLength = cidr.String(), p.ID, int32(l2.Bits())
	return cidr, nil
}

// syncMetadata applies spec.customFields and spec.tags to the prefix in one update; If-Match keeps concurrent edits.
func (r *NetBoxNetworkReconciler) syncMetadata(ctx context.Context, nw *v1alpha1.NetBoxNetwork, id int64) error {
	for _, f := range []string{netbox.FieldRef, netbox.FieldCluster} {
		if _, ok := nw.Spec.CustomFields[f]; ok {
			return withReason(v1alpha1.ReasonInvalidSpec, fmt.Errorf("custom field %s is managed by the provider", f))
		}
	}
	if len(nw.Spec.CustomFields) == 0 && len(nw.Status.CustomFieldKeys) == 0 && len(nw.Spec.Tags) == 0 && len(nw.Status.Tags) == 0 {
		return nil
	}
	obj, etag, err := r.NetBox.Get(ctx, netbox.Prefixes, id)
	if err != nil {
		return err
	}
	customFields, owned, err := mergeCustomFields(obj.CustomFields, nw.Spec.CustomFields, nw.Status.CustomFieldKeys)
	if err != nil {
		return err
	}
	fields := map[string]any{}
	if len(customFields) > 0 {
		fields["custom_fields"] = customFields
	}
	if tags, changed := mergeTags(tagSlugs(obj.Tags), nw.Spec.Tags, nw.Status.Tags); changed {
		fields["tags"] = tagRefs(tags)
	}
	if len(fields) > 0 {
		fields["changelog_message"] = "capi: metadata of " + refOf("NetBoxNetwork", nw)
		if _, err := r.NetBox.Patch(ctx, netbox.Prefixes, id, etag, fields); err != nil {
			return err
		}
	}
	nw.Status.CustomFieldKeys = owned
	nw.Status.Tags = slices.Sorted(slices.Values(nw.Spec.Tags))
	return nil
}

// acquire carves a new prefix from the parent, or adopts spec.prefix.
func (r *NetBoxNetworkReconciler) acquire(ctx context.Context, nw *v1alpha1.NetBoxNetwork, ref string, parent netip.Prefix) (netbox.Object, error) {
	fields := r.Owner.Managed(ref, nw.Spec.ClusterName, "capi: network "+ref, map[string]any{
		"status": "active", "description": "CAPI cluster " + nw.Spec.ClusterName,
	})
	if nw.Spec.Prefix != "" {
		return r.adopt(ctx, ref, nw.Spec.Prefix, parent, fields)
	}
	parents, err := r.NetBox.List(ctx, netbox.Prefixes, url.Values{"prefix": {parent.String()}, "vrf_id": {"null"}})
	if err != nil {
		return netbox.Object{}, err
	}
	if len(parents) != 1 {
		return netbox.Object{}, retryLater(v1alpha1.ReasonParentNotFound, fmt.Errorf("want one parent prefix %s in the global VRF, found %d", parent, len(parents)))
	}
	fields["prefix_length"] = nw.Spec.PrefixLength
	fields["is_pool"] = ptr.Deref(nw.Spec.IsPool, false)
	p, err := r.NetBox.Create(ctx, netbox.AvailablePrefixes(parents[0].ID), fields)
	if errors.Is(err, netbox.ErrConflict) {
		return p, retryLater(v1alpha1.ReasonExhausted, fmt.Errorf("no free /%d left in %s", nw.Spec.PrefixLength, parent))
	}
	return p, err
}

// adopt takes over a prefix a NetBox admin gave our owner, or creates it when it does not exist.
// Of an existing prefix only capi_ref and capi_cluster are written, and the status of one we retained;
// the rest stays as the admin set it.
func (r *NetBoxNetworkReconciler) adopt(ctx context.Context, ref, cidr string, parent netip.Prefix, fields map[string]any) (netbox.Object, error) {
	want, err := netip.ParsePrefix(cidr)
	if err != nil {
		return netbox.Object{}, err
	}
	want = want.Masked()
	if !parent.Contains(want.Addr()) || want.Bits() < parent.Bits() {
		return netbox.Object{}, retryLater(v1alpha1.ReasonPrefixMismatch, fmt.Errorf("%s is not inside %s", want, parent))
	}
	found, err := r.NetBox.List(ctx, netbox.Prefixes, url.Values{"prefix": {want.String()}, "vrf_id": {"null"}})
	switch {
	case err != nil:
		return netbox.Object{}, err
	case len(found) == 0:
		fields["prefix"] = want.String()
		return r.NetBox.Create(ctx, netbox.Prefixes, fields)
	case len(found) > 1:
		return netbox.Object{}, retryLater(v1alpha1.ReasonDuplicate, fmt.Errorf("%d NetBox prefixes %s", len(found), want))
	case !r.Owner.Owns(found[0]):
		return netbox.Object{}, retryLater(v1alpha1.ReasonNotOwned, fmt.Errorf("prefix %s exists without our owner; set its owner in NetBox to adopt it", want))
	}
	if other := heldBy(found[0], ref); other != "" {
		return netbox.Object{}, retryLater(v1alpha1.ReasonDuplicate, fmt.Errorf("prefix %s is held by %s", want, other))
	}
	patch := map[string]any{"custom_fields": fields["custom_fields"], "changelog_message": "capi: adopted by " + ref}
	if found[0].Status.Value == "deprecated" && found[0].Ref() != "" {
		patch["status"] = "active" // retained by a network of our namespace: heldBy lets no other ref through
	}
	_, err = r.NetBox.Patch(ctx, netbox.Prefixes, found[0].ID, "", patch)
	return found[0], err
}

// heldBy returns the ref of another object the prefix is bound to.
// A deprecated prefix is free to adopt, but only from the namespace of ref.
func heldBy(o netbox.Object, ref string) string {
	got := o.Ref()
	if got == ref || o.Status.Value == "deprecated" && path.Dir(got) == path.Dir(ref) {
		return ""
	}
	return got
}

// checkPrefix makes sure the NetBox prefix still is what the spec asks for.
func checkPrefix(nw *v1alpha1.NetBoxNetwork, parent netip.Prefix, got string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(got)
	if err != nil {
		return p, err
	}
	want, _ := netip.ParsePrefix(nw.Spec.Prefix)
	switch {
	case !parent.Contains(p.Addr()) || p.Bits() < parent.Bits():
		return p, fmt.Errorf("NetBox prefix %s is outside parent %s", p, parent)
	case nw.Spec.PrefixLength != 0 && p.Bits() != int(nw.Spec.PrefixLength):
		return p, fmt.Errorf("NetBox prefix %s is not a /%d", p, nw.Spec.PrefixLength)
	case nw.Spec.Prefix != "" && p != want.Masked():
		return p, fmt.Errorf("NetBox prefix %s is not %s", p, nw.Spec.Prefix)
	}
	return p, nil
}

func (r *NetBoxNetworkReconciler) reconcileDelete(ctx context.Context, nw *v1alpha1.NetBoxNetwork) error {
	if deletedForMove(nw) {
		controllerutil.RemoveFinalizer(nw, networkFinalizer)
		return nil
	}
	pools := &v1alpha1.NetBoxIPPoolList{}
	if err := r.List(ctx, pools, client.InNamespace(nw.Namespace)); err != nil {
		return apiError(err)
	}
	for i := range pools.Items {
		if slices.Contains(poolNetworks(&pools.Items[i]), nw.Name) {
			return waitFor(v1alpha1.ReasonWaitingForPools, fmt.Errorf("network is still used by NetBoxIPPool %s", pools.Items[i].Name))
		}
	}
	ref := refOf("NetBoxNetwork", nw)
	found, err := r.NetBox.FindByRef(ctx, netbox.Prefixes, r.Owner, ref)
	if err != nil {
		return err
	}
	released := "capi: released by " + ref
	for _, p := range found {
		switch {
		case nw.Spec.Prefix != "":
			// An adopted prefix is only let go: its status, tags and other custom fields stay.
			_, err = r.NetBox.Patch(ctx, netbox.Prefixes, p.ID, "", map[string]any{
				"custom_fields": map[string]any{netbox.FieldRef: nil, netbox.FieldCluster: nil}, "changelog_message": released,
			})
		case nw.Spec.DeletionPolicy == v1alpha1.DeletionPolicyDelete:
			err = r.NetBox.Delete(ctx, netbox.Prefixes, p.ID, released)
		default:
			_, err = r.NetBox.Patch(ctx, netbox.Prefixes, p.ID, "", map[string]any{"status": "deprecated", "changelog_message": released})
		}
		if err != nil && !errors.Is(err, netbox.ErrNotFound) {
			return err
		}
	}
	if err := r.releaseVIP(ctx, nw); err != nil {
		return err
	}
	controllerutil.RemoveFinalizer(nw, networkFinalizer)
	return nil
}
