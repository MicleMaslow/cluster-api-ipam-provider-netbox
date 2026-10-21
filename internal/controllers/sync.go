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
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	inclusterv1 "sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netmath"
)

// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=ipaddressclaims,verbs=get;list;watch

// syncRef is the key of the NetBox address that syncs the IPAddress address of pool in nw; address "" starts the keys
// of the pool, pool "" those of every pool.
func syncRef(k netbox.Keys, nw *v1alpha1.NetBoxNetwork, pool, address string) string {
	if pool == "" {
		return k.Key("NetBoxNetwork", nw, "sync", "")
	}
	return k.Key("NetBoxNetwork", nw, "sync", pool, address)
}

// synced is a NetBox address the sync of a network wants: that of the IPAddress name.
type synced struct {
	name, ref string
	addr      netip.Prefix
	f         ipFields
	keep      bool // fields unknown (the claim is gone, a template fails): an existing address is left as it is
}

// addressToNetwork wakes the network that controls the InClusterIPPool of an IPAddress.
func (r *NetBoxNetworkReconciler) addressToNetwork(ctx context.Context, o client.Object) []reconcile.Request {
	a, ok := asType[*ipamv1.IPAddress](ctx, o)
	if !ok || a.Spec.PoolRef.APIGroup != inclusterv1.GroupVersion.Group || a.Spec.PoolRef.Kind != "InClusterIPPool" {
		return nil
	}
	pool := &inclusterv1.InClusterIPPool{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: a.Spec.PoolRef.Name}, pool); err != nil {
		return nil
	}
	ref := metav1.GetControllerOfNoCopy(pool)
	if ref == nil || ref.Kind != "NetBoxNetwork" {
		return nil
	}
	if gv, err := schema.ParseGroupVersion(ref.APIVersion); err != nil || gv.Group != v1alpha1.GroupVersion.Group {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: ref.Name}}}
}

// syncAddresses brings the NetBox addresses under the sync keys of nw to the IPAddresses of the InClusterIPPools of
// its pools that sync, under the lock. Those no IPAddress wants go first; a missing one is created, the NetBox address
// of ours at its address deleted just before (an address a new IPAddress reuses), while one of someone else there is
// AddressConflict and stays; the others follow the templates and the network. A deleted network wants none: all go,
// whatever its deletionPolicy.
func (r *NetBoxNetworkReconciler) syncAddresses(ctx context.Context, nw *v1alpha1.NetBoxNetwork) error {
	want, invalid := r.wantSynced(ctx, nw)
	if invalid != nil && errReason(invalid) != v1alpha1.ReasonInvalidSpec {
		return invalid
	}
	err := underLock(ctx, r.NetBox, func(ctx context.Context, nb *netbox.Client) error {
		rr := *r
		rr.NetBox = nb
		return rr.applySynced(ctx, nw, want)
	})
	return foldErrors([]error{invalid, err})
}

// wantSynced returns by key the NetBox addresses the sync of nw wants: one per IPAddress of the InClusterIPPool of a
// pool that syncs, its fields from the templates of the pool for the claim of the IPAddress. A template that does not
// render is InvalidSpec, returned with the rest.
func (r *NetBoxNetworkReconciler) wantSynced(ctx context.Context, nw *v1alpha1.NetBoxNetwork) (map[string]synced, error) {
	want := map[string]synced{}
	templates := map[string]v1alpha1.IPAddressFields{}
	for _, p := range nw.Spec.Pools {
		if ptr.Deref(p.Sync.Enabled, false) {
			templates[p.Name] = p.Sync.IPAddress
		}
	}
	if len(templates) == 0 || !nw.DeletionTimestamp.IsZero() {
		return want, nil
	}
	// Only pools nw controls: the addresses of one another network holds (Duplicate) are not its own.
	for name := range templates {
		pool := &inclusterv1.InClusterIPPool{}
		err := r.Get(ctx, client.ObjectKey{Namespace: nw.Namespace, Name: name}, pool)
		if client.IgnoreNotFound(err) != nil {
			return nil, apiError(err)
		}
		if err != nil || !managedBy(pool, nw) {
			delete(templates, name)
		}
	}
	l2, _, err := l2Of(nw)
	if err != nil {
		return nil, err
	}
	list := &ipamv1.IPAddressList{}
	if err := r.List(ctx, list, client.InNamespace(nw.Namespace)); err != nil {
		return nil, apiError(err)
	}
	var invalid []error
	for i := range list.Items {
		a := &list.Items[i]
		pool := a.Spec.PoolRef
		t, ok := templates[pool.Name]
		ip, err := netip.ParseAddr(a.Spec.Address)
		if !ok || pool.APIGroup != inclusterv1.GroupVersion.Group || pool.Kind != "InClusterIPPool" || err != nil {
			continue
		}
		w := synced{name: a.Name, ref: syncRef(r.Keys, nw, pool.Name, a.Name), addr: netip.PrefixFrom(ip, l2.Bits())}
		claim, err := r.claimOf(ctx, a)
		switch {
		case err != nil:
			return nil, err
		case claim == nil: // the IPAddress goes too
			w.keep = true
		default:
			if w.f, err = claimFields(t, pool.Name, claim); err != nil {
				w.keep = true
				invalid = append(invalid, withReason(v1alpha1.ReasonInvalidSpec, fmt.Errorf("pool %s: sync.ipAddress: %w", pool.Name, err)))
			}
		}
		want[w.ref] = w
	}
	return want, foldErrors(invalid)
}

// claimOf returns the claim of a, read past the cache if the cache lacks it; nil if it is gone.
func (r *NetBoxNetworkReconciler) claimOf(ctx context.Context, a *ipamv1.IPAddress) (*ipamv1.IPAddressClaim, error) {
	claim, key := &ipamv1.IPAddressClaim{}, client.ObjectKey{Namespace: a.Namespace, Name: a.Spec.ClaimRef.Name}
	err := r.Get(ctx, key, claim)
	if apierrors.IsNotFound(err) {
		err = r.APIReader.Get(ctx, key, claim)
	}
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, apiError(err)
	}
	return claim, nil
}

// applySynced is the section of syncAddresses that underLock runs.
func (r *NetBoxNetworkReconciler) applySynced(ctx context.Context, nw *v1alpha1.NetBoxNetwork, want map[string]synced) error {
	prefix := syncRef(r.Keys, nw, "", "")
	found, err := r.NetBox.FindByRefPrefix(ctx, r.Keys, netbox.IPAddresses, prefix)
	if err != nil {
		return err
	}
	have := map[string]netbox.Object{}
	for _, o := range found {
		ref := r.Keys.RefOf(o)
		_, wanted := want[ref]
		if _, dup := have[ref]; !wanted || dup {
			if err := r.dropSynced(ctx, nw, o); err != nil {
				return err
			}
			continue
		}
		have[ref] = o
	}
	attrs := networkAttrs(nw)
	var (
		errs      []error
		conflicts []string
		create    = map[int64][]synced{} // by the NetBox range at the address
	)
	for _, ref := range slices.Sorted(maps.Keys(want)) {
		w := want[ref]
		o, ok := have[ref]
		switch {
		case w.keep:
		case ok:
			moved, err := r.syncIP(ctx, nw, ref, "the address "+w.addr.String()+" of IPAddress "+w.name, o, w.addr, w.f.DNSName, w.f.on(attrs))
			attrsErr, err := splitAttrs(err)
			if err != nil {
				return err
			}
			errs = append(errs, attrsErr)
			if moved {
				ctrl.LoggerFrom(ctx).Info("Put a synced address back", "network", klog.KObj(nw), "ref", ref, "from", o.Address, "address", w.addr.String())
				normalEvent(r.Recorder, nw, "SyncedAddressReclaimed", "Put the NetBox address of IPAddress %s back at %s", w.name, w.addr)
			}
		default:
			id := rangeAt(nw, w.addr.Addr())
			create[id] = append(create[id], w)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(create)) {
		var (
			ready []synced
			ours  [][]netbox.Object // at the address of each of ready
		)
		for _, w := range create[id] {
			at, err := r.NetBox.List(ctx, netbox.IPAddresses, url.Values{"address": {w.addr.Addr().String()}, "vrf_id": {"null"}})
			if err != nil {
				return err
			}
			if slices.ContainsFunc(at, func(o netbox.Object) bool { return !strings.HasPrefix(r.Keys.RefOf(o), prefix) }) {
				conflicts = append(conflicts, fmt.Sprintf("IPAddress %s (%s)", w.name, w.addr.Addr()))
				continue
			}
			ready, ours = append(ready, w), append(ours, at)
		}
		if len(ready) == 0 {
			continue
		}
		err := r.unfenced(ctx, nw, id, func() error {
			for i, w := range ready {
				for _, o := range ours[i] {
					if err := r.dropSynced(ctx, nw, o); err != nil {
						return err
					}
				}
				if err := r.createSynced(ctx, nw, w, attrs); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(conflicts) > 0 {
		errs = append(errs, retryLater(v1alpha1.ReasonAddressConflict,
			fmt.Errorf("NetBox addresses of others hold the addresses of %s, which are not synced", strings.Join(conflicts, ", "))))
	}
	return foldErrors(errs)
}

// rangeAt is the id of the NetBox range of nw that holds a, 0 for none.
func rangeAt(nw *v1alpha1.NetBoxNetwork, a netip.Addr) int64 {
	for _, p := range nw.Status.Layout.Pools {
		for _, lr := range p.Ranges {
			from, ferr := netip.ParseAddr(lr.Start)
			to, terr := netip.ParseAddr(lr.End)
			if ferr == nil && terr == nil && netmath.Contains(from, to, a) {
				return lr.ID
			}
		}
	}
	return 0
}

// unfenced runs create with the NetBox range id of nw open (mark_populated false), 0 for none: NetBox refuses to
// create an address inside a populated range, not to change, move or delete one. The range is closed again whatever
// create does, an expired ctx and a panic included (the HTTP client's timeout bounds the close); one a failed close
// leaves open is closed by the next applyRanges. It runs under netBoxWrites.
func (r *NetBoxNetworkReconciler) unfenced(ctx context.Context, nw *v1alpha1.NetBoxNetwork, id int64, create func() error) (err error) {
	if id == 0 {
		return create()
	}
	set := func(ctx context.Context, on bool, message string) error {
		_, err := r.NetBox.Patch(ctx, netbox.IPRanges, id, "", map[string]any{"mark_populated": on, "changelog_message": message + r.Keys.Key("NetBoxNetwork", nw)})
		return err
	}
	if err := set(ctx, false, "capi: opened to sync addresses by "); err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, set(context.WithoutCancel(ctx), true, "capi: closed to allocation by "))
	}()
	return create()
}

// createSynced creates the NetBox address w of nw with attrs.
func (r *NetBoxNetworkReconciler) createSynced(ctx context.Context, nw *v1alpha1.NetBoxNetwork, w synced, attrs netbox.Attrs) error {
	ip, err := r.NetBox.Create(ctx, netbox.IPAddresses, r.Keys.Managed(w.f.on(attrs), w.ref, "capi: address for "+w.ref,
		map[string]any{"address": w.addr.String(), "status": "active", "dns_name": w.f.DNSName}))
	if err != nil {
		return err
	}
	ctrl.LoggerFrom(ctx).Info("Synced an address to NetBox", "network", klog.KObj(nw), "ref", w.ref, "address", ip.Address, "id", ip.ID)
	normalEvent(r.Recorder, nw, "AddressSynced", "Synced the address %s of IPAddress %s to NetBox", w.addr, w.name)
	return nil
}

// dropSynced deletes o, a NetBox address under a sync key of nw.
func (r *NetBoxNetworkReconciler) dropSynced(ctx context.Context, nw *v1alpha1.NetBoxNetwork, o netbox.Object) error {
	if err := r.NetBox.Delete(ctx, netbox.IPAddresses, o.ID, "", "capi: released by "+r.Keys.Key("NetBoxNetwork", nw)); err != nil {
		return err
	}
	ctrl.LoggerFrom(ctx).Info("Deleted a synced address", "network", klog.KObj(nw), "ref", r.Keys.RefOf(o), "address", o.Address)
	normalEvent(r.Recorder, nw, "SyncedAddressDeleted", "Deleted the synced NetBox address %s (%s)", o.Address, r.Keys.RefOf(o))
	return nil
}
