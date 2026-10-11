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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
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
	APIReader        client.Reader // reads past the cache
	NetBox           *netbox.Client
	Keys             netbox.Keys
	WatchFilterValue string
	Recorder         events.EventRecorder
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
	if r.WatchFilterValue != "" && !labels.HasWatchLabel(nw, r.WatchFilterValue) {
		return ctrl.Result{}, nil
	}
	// An empty status leads to a new prefix or VIP. The cache can lag the status write that follows a conditions
	// write, which woke this reconcile: an empty one is read live.
	if nw.Status.PrefixID == 0 || nw.Spec.ControlPlaneEndpoint != nil && nw.Status.ControlPlaneEndpoint.Host == "" {
		nw = &v1alpha1.NetBoxNetwork{}
		if err := r.APIReader.Get(ctx, req.NamespacedName, nw); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	cluster, err := r.getCluster(ctx, nw)
	if err != nil {
		return ctrl.Result{}, err
	}
	// The helper comes first: a Paused condition that only lags the generation is set in memory, for it to write.
	helper, err := patch.NewHelper(nw, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	if isPaused, requeue, err := paused.EnsurePausedCondition(ctx, r.Client, cluster, nw); err != nil || isPaused || requeue {
		return ctrl.Result{}, err
	}
	defer func() {
		if err := helper.Patch(ctx, nw); err != nil {
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()
	if !nw.DeletionTimestamp.IsZero() {
		err = r.reconcileDelete(ctx, nw, cluster)
		setDeleting(&nw.Status.Conditions, nw.Generation, err)
		return toResult(err)
	}
	if controllerutil.AddFinalizer(nw, networkFinalizer) {
		return ctrl.Result{}, nil
	}
	if nw.Spec.ControlPlaneEndpoint == nil {
		meta.RemoveStatusCondition(&nw.Status.Conditions, v1alpha1.ControlPlaneEndpointReadyCondition)
	}
	var vipErr, attrsErr error
	if _, _, err = networkTexts(nw); err == nil {
		if cluster == nil && nw.Status.PrefixID == 0 {
			// Nothing reaches NetBox before the Cluster: after a move or restore the status is gone, and until the Cluster
			// exists (and is unpaused) nw must not act; the Cluster may also come with an endpoint of its own. The Cluster watch wakes nw.
			err = waitFor(v1alpha1.ReasonWaitingForCluster, fmt.Errorf("cluster %s does not exist", nw.Spec.ClusterName))
		} else {
			vipErr, attrsErr, err = r.reconcileNormal(ctx, nw, cluster)
		}
	}
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
		if !errors.Is(vipErr, errClusterChanged) {
			setCondition(&nw.Status.Conditions, v1alpha1.ControlPlaneEndpointReadyCondition, v1alpha1.ReasonReady, nw.Generation, vipErr)
		}
	}
	err = foldErrors([]error{attrsErr, r.syncMetadata(ctx, nw, nw.Status.PrefixID)})
	setCondition(&nw.Status.Conditions, v1alpha1.MetadataSyncedCondition, v1alpha1.ReasonSynced, nw.Generation, err)
	if errors.Is(vipErr, errClusterChanged) {
		if _, err := toResult(err); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil //nolint:staticcheck // rate-limited: a Cluster that keeps changing backs off
	}
	return toResult(foldErrors([]error{vipErr, err}))
}

// getCluster returns the Cluster of nw from the cache, or nil if there is none.
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

// endpointFor is the endpoint the Cluster must have for vip; the status reports it too. The parts the Cluster has
// are kept, the empty ones filled: host vip, port spec.port, else 6443 (CAPI copies a port alone from the
// infrastructure cluster). A host other than vip, or a port other than spec.port, is a conflict: never overwritten.
func endpointFor(nw *v1alpha1.NetBoxNetwork, cluster *clusterv1.Cluster, vip netip.Addr) (want clusterv1.APIEndpoint, conflict error) {
	var cur clusterv1.APIEndpoint
	if cluster != nil {
		cur = cluster.Spec.ControlPlaneEndpoint
	}
	port := nw.Spec.ControlPlaneEndpoint.Port
	want = clusterv1.APIEndpoint{Host: vip.String(), Port: cmp.Or(port, cur.Port, defaultAPIPort)}
	switch {
	case cur.Host != "" && cur.Host != want.Host:
		conflict = fmt.Errorf("cluster %s has endpoint host %s, the VIP is %s", nw.Spec.ClusterName, cur.Host, want.Host)
	case cur.Port != 0 && cur.Port != want.Port:
		conflict = fmt.Errorf("cluster %s has endpoint port %d, spec.controlPlaneEndpoint.port is %d", nw.Spec.ClusterName, cur.Port, port)
	default:
		return want, nil
	}
	return want, retryLater(v1alpha1.ReasonEndpointConflict, conflict)
}

// errClusterChanged is a Cluster changed since it was read: it is read again at once and the condition stays.
var errClusterChanged = errors.New("the cluster changed since it was read")

// syncClusterEndpoint fills the empty parts of the Cluster endpoint (endpointFor).
// Its result is ControlPlaneEndpointReady: nil only when the VIP is reserved and the Cluster has it.
func (r *NetBoxNetworkReconciler) syncClusterEndpoint(ctx context.Context, nw *v1alpha1.NetBoxNetwork, cluster *clusterv1.Cluster, vipErr error) error {
	if vipErr != nil {
		return vipErr
	}
	if cluster == nil {
		return waitFor(v1alpha1.ReasonWaitingForCluster, fmt.Errorf("cluster %s not found", nw.Spec.ClusterName))
	}
	vip, err := netip.ParseAddr(nw.Status.ControlPlaneEndpoint.Host)
	if err != nil {
		return err
	}
	want, err := endpointFor(nw, cluster, vip)
	if err != nil || want == cluster.Spec.ControlPlaneEndpoint {
		return err
	}
	before := cluster.DeepCopy()
	cluster.Spec.ControlPlaneEndpoint = want
	// The lock keeps an endpoint that was set meanwhile, e.g. copied from the infrastructure cluster.
	err = r.Patch(ctx, cluster, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	switch {
	case apierrors.IsConflict(err):
		return errClusterChanged
	case err != nil:
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
// Owner, tenant, description or comments that cannot be synced onto them are attrsErr, for MetadataSynced.
func (r *NetBoxNetworkReconciler) reconcileNormal(ctx context.Context, nw *v1alpha1.NetBoxNetwork, cluster *clusterv1.Cluster) (vipErr, attrsErr, err error) {
	if err := r.resolveAttrs(ctx, nw); err != nil {
		return nil, nil, err
	}
	before := nw.Status.DeepCopy()
	err = underLock(ctx, r.NetBox, func(ctx context.Context, nb *netbox.Client) error {
		rr := *r
		rr.NetBox = nb
		var perr error
		vipErr, attrsErr, perr = rr.prefixAndVIP(ctx, nw, cluster)
		if werr := errors.Join(perr, vipErr, attrsErr); errors.Is(werr, netbox.ErrWriteNeeded) {
			nw.Status = *before // the locked pass starts from what the first one found
			return werr
		}
		return perr
	})
	return vipErr, attrsErr, err
}

// prefixAndVIP is the section of reconcileNormal that underLock runs.
func (r *NetBoxNetworkReconciler) prefixAndVIP(ctx context.Context, nw *v1alpha1.NetBoxNetwork, cluster *clusterv1.Cluster) (vipErr, attrsErr, err error) {
	prefix, err := r.reconcilePrefix(ctx, nw)
	if attrsErr, err = splitAttrs(err); err != nil {
		return nil, nil, err
	}
	// Without the field the VIP stays in NetBox, and in the status so that pools keep off it.
	if nw.Spec.ControlPlaneEndpoint == nil {
		return nil, attrsErr, nil
	}
	vip, err := r.reserveVIP(ctx, nw, prefix, preferredVIP(nw, hostOf(cluster), prefix), cluster != nil)
	vipAttrsErr, err := splitAttrs(err)
	attrsErr = errors.Join(attrsErr, vipAttrsErr)
	if err != nil {
		return err, attrsErr, nil
	}
	ep, _ := endpointFor(nw, cluster, vip) // a conflict is syncClusterEndpoint's to report
	nw.Status.ControlPlaneEndpoint = v1alpha1.APIEndpoint{Host: ep.Host, Port: ep.Port}
	return nil, attrsErr, nil
}

// resolveAttrs caches spec.owner and spec.tenant as NetBox IDs in the status, for the objects of nw and its pools.
// NetBox is asked (a name or slug looked up, an id checked) only when the generation changed or nothing is cached.
// Unset is 0, without a request.
func (r *NetBoxNetworkReconciler) resolveAttrs(ctx context.Context, nw *v1alpha1.NetBoxNetwork) error {
	ready := meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.ReadyCondition)
	// These reasons set Ready without resolving, or after a resolve that failed for one of the two and kept the other's cache.
	unresolved := []string{
		v1alpha1.ReasonWaitingForCluster, v1alpha1.ReasonInvalidSpec, v1alpha1.ReasonNetBoxError, v1alpha1.ReasonForbidden,
		v1alpha1.ReasonOwnerNotFound, v1alpha1.ReasonTenantNotFound,
	}
	fresh := ready != nil && ready.ObservedGeneration == nw.Generation && !slices.Contains(unresolved, ready.Reason)
	o, t := ptr.Deref(nw.Spec.Owner, v1alpha1.OwnerRef{}), ptr.Deref(nw.Spec.Tenant, v1alpha1.TenantRef{})
	nb := r.NetBox
	return errors.Join(
		resolveID(ctx, &nw.Status.OwnerID, o.ID, o.Name, fresh, nb.CheckOwner, nb.LookupOwner, v1alpha1.ReasonOwnerNotFound),
		resolveID(ctx, &nw.Status.TenantID, t.ID, t.Slug, fresh, nb.CheckTenant, nb.LookupTenant, v1alpha1.ReasonTenantNotFound),
	)
}

// resolveID sets *cached to id once check finds it (or may not tell), or to the ID find finds name under. A fresh
// cache of the same is kept. Not found drops it; any other failure keeps it, so that a NetBox outage does not wake
// every claim of the network twice, and the next reconcile asks again (resolveAttrs).
func resolveID(ctx context.Context, cached *int64, id int64, name string, fresh bool,
	check func(context.Context, int64) error, find func(context.Context, string) (int64, error), notFound string) error {
	switch {
	case id == 0 && name == "":
		*cached = 0
		return nil
	case fresh && *cached != 0 && (id == 0 || id == *cached):
		return nil
	}
	got, err := id, error(nil)
	if id != 0 {
		// A token that may not view owners or tenants can still use an id: NetBox refuses a missing one on create.
		if err = check(ctx, id); errors.Is(err, netbox.ErrForbidden) {
			ctrl.LoggerFrom(ctx).Info("The token may not check the id in NetBox, it is taken as given", "id", id, "reason", err.Error())
			err = nil
		}
	} else {
		got, err = find(ctx, name)
	}
	if errors.Is(err, netbox.ErrNotFound) {
		*cached = 0
		return retryLater(notFound, err)
	}
	if err != nil {
		return err
	}
	*cached = got
	return nil
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

// reconcilePrefix finds, carves or adopts the prefix of nw and records it in the status.
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
	ref := r.Keys.Key("NetBoxNetwork", nw)
	found, err := r.NetBox.FindByRef(ctx, r.Keys, netbox.Prefixes, ref)
	if err != nil {
		return netip.Prefix{}, err
	}
	// A prefix retained by a deleted network of this name that does not fit the spec gives way to a new one.
	if len(found) == 1 && nw.Status.PrefixID == 0 && found[0].Status.Value == "deprecated" {
		if _, err := checkPrefix(nw, parent, found[0].Prefix); err != nil {
			if err := r.retire(ctx, nw, netbox.Prefixes, found[0], ref, "PrefixRetired"); err != nil {
				return netip.Prefix{}, err
			}
			found = nil
		}
	}
	var p netbox.Object
	switch len(found) {
	case 0:
		if nw.Status.PrefixID != 0 || nw.Status.Prefix != "" {
			return netip.Prefix{}, retryLater(v1alpha1.ReasonPrefixNotFound, fmt.Errorf("NetBox prefix %s of %s is gone, restore it", nw.Status.Prefix, ref))
		}
		// A live VIP means a live network whose status was lost; a retained one is deprecated.
		vips, err := r.NetBox.FindByRef(ctx, r.Keys, netbox.IPAddresses, vipRef(r.Keys, nw))
		if err != nil {
			return netip.Prefix{}, err
		}
		if slices.ContainsFunc(vips, func(o netbox.Object) bool { return o.Status.Value != "deprecated" }) {
			return netip.Prefix{}, retryLater(v1alpha1.ReasonPrefixNotFound, fmt.Errorf("the VIP of %s is in NetBox, the prefix is gone; restore it", ref))
		}
		if p, err = r.acquire(ctx, nw, ref, parent); err != nil {
			return netip.Prefix{}, err
		}
		ctrl.LoggerFrom(ctx).Info("Acquired the NetBox prefix", "network", klog.KObj(nw), "prefix", p.Prefix, "id", p.ID)
		normalEvent(r.Recorder, nw, "PrefixAcquired", "Acquired the NetBox prefix %s", p.Prefix)
	case 1:
		p = found[0]
	default:
		return netip.Prefix{}, retryLater(v1alpha1.ReasonDuplicate, fmt.Errorf("%d NetBox prefixes carry %s", len(found), ref))
	}
	cidr, err := checkPrefix(nw, parent, p.Prefix)
	if err != nil {
		return netip.Prefix{}, retryLater(v1alpha1.ReasonPrefixMismatch, err)
	}
	// A carved prefix is kept active; one under spec.prefix only when taken back from a deleted network of this name.
	if p.Status.Value == "deprecated" && (nw.Spec.Prefix == "" || nw.Status.PrefixID == 0 && len(found) == 1) {
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
	// An adopted prefix keeps the attributes its admin set.
	if nw.Spec.Prefix != "" {
		return cidr, nil
	}
	attrs, _, err := networkTexts(nw)
	if err != nil {
		return netip.Prefix{}, err
	}
	err = syncAttrs(ctx, r.NetBox, r.Recorder, nw, netbox.Prefixes, p, attrs, nil, "capi: attributes of "+ref)
	return cidr, attrsFailed("the prefix "+p.Prefix, err)
}

// retiredRef is the key a retained object moves to when it does not fit the spec of a new network of its name.
func retiredRef(ref string) string { return ref + "/retired" }

// retire moves o, retained by a deleted network of nw's name, to retiredRef, so that a new object takes ref.
func (r *NetBoxNetworkReconciler) retire(ctx context.Context, nw *v1alpha1.NetBoxNetwork, path string, o netbox.Object, ref, reason string) error {
	if _, err := r.NetBox.Patch(ctx, path, o.ID, "", map[string]any{
		"custom_fields": map[string]any{r.Keys.Ref: retiredRef(ref)}, "changelog_message": "capi: retired by " + r.Keys.Key("NetBoxNetwork", nw),
	}); err != nil {
		return err
	}
	addr := cmp.Or(o.Prefix, o.Address)
	ctrl.LoggerFrom(ctx).Info("Retired a retained NetBox object that does not fit the spec", "network", klog.KObj(nw), "address", addr, "id", o.ID, "ref", retiredRef(ref))
	normalEvent(r.Recorder, nw, reason, "Retired %s: retained by a deleted network, it does not fit the spec", addr)
	return nil
}

// syncMetadata applies spec.customFields and spec.tags to the prefix in one update; If-Match keeps concurrent edits.
func (r *NetBoxNetworkReconciler) syncMetadata(ctx context.Context, nw *v1alpha1.NetBoxNetwork, id int64) error {
	if _, ok := nw.Spec.CustomFields[r.Keys.Ref]; ok {
		return withReason(v1alpha1.ReasonInvalidSpec, fmt.Errorf("custom field %s is managed by the provider", r.Keys.Ref))
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
		fields["changelog_message"] = "capi: metadata of " + r.Keys.Key("NetBoxNetwork", nw)
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
	attrs, _, err := networkTexts(nw)
	if err != nil {
		return netbox.Object{}, err
	}
	fields := r.Keys.Managed(attrs, ref, "capi: network "+ref, map[string]any{"status": "active"})
	if nw.Spec.Prefix != "" {
		return r.adopt(ctx, nw, ref, parent, fields)
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

// adopt takes over spec.prefix, or creates it when it does not exist. An existing prefix is taken when it carries
// no key or ours (heldBy); the token's permissions in NetBox limit which ones it may take.
// Of an existing prefix only our key is written, and the status of one we retained; the rest stays as the admin set it.
func (r *NetBoxNetworkReconciler) adopt(ctx context.Context, nw *v1alpha1.NetBoxNetwork, ref string, parent netip.Prefix, fields map[string]any) (netbox.Object, error) {
	want, err := netip.ParsePrefix(nw.Spec.Prefix)
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
	}
	cur, etag, err := r.NetBox.Get(ctx, netbox.Prefixes, found[0].ID)
	if err != nil {
		return netbox.Object{}, err
	}
	other, err := r.heldBy(ctx, nw.Namespace, cur, ref)
	switch {
	case err != nil:
		return netbox.Object{}, err
	case other != "":
		return netbox.Object{}, retryLater(v1alpha1.ReasonDuplicate, fmt.Errorf("prefix %s is held by %s", want, other))
	}
	patch := map[string]any{"custom_fields": map[string]any{r.Keys.Ref: ref}, "changelog_message": "capi: adopted by " + ref}
	if cur.Status.Value == "deprecated" && r.Keys.RefOf(cur) != "" {
		patch["status"] = "active" // retained by a network of our namespace: heldBy lets no other ref through
	}
	_, err = r.NetBox.Patch(ctx, netbox.Prefixes, cur.ID, etag, patch)
	return cur, err
}

// heldBy returns the ref of another object the prefix is bound to. A deprecated prefix keyed in the namespace
// of ref is free when retired, or when no network of ns has the name in its key.
func (r *NetBoxNetworkReconciler) heldBy(ctx context.Context, ns string, o netbox.Object, ref string) (string, error) {
	got := r.Keys.RefOf(o)
	switch {
	case got == ref:
		return "", nil
	case o.Status.Value != "deprecated" || !strings.HasPrefix(got, path.Dir(ref)+"/"):
		return got, nil
	case strings.HasSuffix(got, "/retired") && path.Dir(got) != path.Dir(ref): // not a network named retired
		return "", nil
	}
	err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: path.Base(got)}, &v1alpha1.NetBoxNetwork{})
	switch {
	case apierrors.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", apiError(err)
	}
	return got, nil
}

// letGo removes our key from the adopted prefix id; its other custom fields stay.
func (r *NetBoxNetworkReconciler) letGo(ctx context.Context, id int64, message string) error {
	_, err := r.NetBox.Patch(ctx, netbox.Prefixes, id, "", map[string]any{"custom_fields": map[string]any{r.Keys.Ref: nil}, "changelog_message": message})
	return err
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

func (r *NetBoxNetworkReconciler) reconcileDelete(ctx context.Context, nw *v1alpha1.NetBoxNetwork, cluster *clusterv1.Cluster) error {
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
	// A deleted VIP could go to another cluster while this one still serves on it; a retained one is never handed out.
	if vip := nw.Status.ControlPlaneEndpoint.Host; nw.Spec.DeletionPolicy == v1alpha1.DeletionPolicyDelete && vip != "" && hostOf(cluster) == vip {
		return waitFor(v1alpha1.ReasonWaitingForCluster, fmt.Errorf("cluster %s still uses the VIP %s", nw.Spec.ClusterName, vip))
	}
	ref := r.Keys.Key("NetBoxNetwork", nw)
	found, err := r.NetBox.FindByRef(ctx, r.Keys, netbox.Prefixes, ref)
	if err != nil {
		return err
	}
	if len(found) == 0 && nw.Status.PrefixID != 0 {
		ctrl.LoggerFrom(ctx).Info("The NetBox prefix of the network is not found under its key: nothing to release",
			"network", klog.KObj(nw), "prefixID", nw.Status.PrefixID, "ref", ref)
	}
	released := "capi: released by " + ref
	for _, p := range found {
		policy := nw.Spec.DeletionPolicy
		switch {
		case nw.Spec.Prefix != "":
			// An adopted prefix is only let go: its status, tags and other custom fields stay.
			policy = v1alpha1.DeletionPolicyRetain
			err = r.letGo(ctx, p.ID, released)
		case nw.Spec.DeletionPolicy == v1alpha1.DeletionPolicyDelete:
			err = r.NetBox.Delete(ctx, netbox.Prefixes, p.ID, "", released)
		default:
			_, err = r.NetBox.Patch(ctx, netbox.Prefixes, p.ID, "", map[string]any{"status": "deprecated", "changelog_message": released})
		}
		if errors.Is(err, netbox.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		ctrl.LoggerFrom(ctx).Info("Released the NetBox prefix", "network", klog.KObj(nw), "prefix", p.Prefix, "id", p.ID, "policy", policy)
		normalEvent(r.Recorder, nw, "PrefixReleased", "Released the NetBox prefix %s (deletionPolicy %s)", p.Prefix, policy)
	}
	if err := r.releaseVIP(ctx, nw); err != nil {
		return err
	}
	controllerutil.RemoveFinalizer(nw, networkFinalizer)
	return nil
}
