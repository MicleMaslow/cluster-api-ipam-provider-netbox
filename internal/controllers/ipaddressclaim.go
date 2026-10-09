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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/pkg/ipamutil"
	ipampredicates "sigs.k8s.io/cluster-api-ipam-provider-in-cluster/pkg/predicates"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

// reasonAllocating is the Ready reason of a claim whose address is allocated but not yet recorded; CAPI defines none.
const reasonAllocating = "Allocating"

// reasonAddressConflict is the Ready reason of a claim whose address NetBox gave to another object; CAPI defines none.
const reasonAddressConflict = "AddressConflict"

var errAddressConflict = errors.New("address conflict")

// ProviderAdapter plugs NetBoxIPPool into ipamutil's IPAddressClaim lifecycle.
type ProviderAdapter struct {
	Client      client.Client
	NetBox      *netbox.Client
	Owner       netbox.Owner
	Concurrency int
}

var _ ipamutil.ProviderAdapter = &ProviderAdapter{}

// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=ipaddressclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=ipaddressclaims/status;ipaddressclaims/finalizers,verbs=get;update;patch
// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=ipaddresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=ipaddresses/finalizers,verbs=update

// SetupWithManager adds the claim watches that the NetBoxIPPool needs.
func (a *ProviderAdapter) SetupWithManager(ctx context.Context, b *ctrl.Builder) error {
	b.WithOptions(controller.Options{MaxConcurrentReconciles: a.Concurrency}).
		For(&ipamv1.IPAddressClaim{}, builder.WithPredicates(ipampredicates.ClaimReferencesPoolKind(poolGK))).
		Watches(&v1alpha1.NetBoxIPPool{}, handler.EnqueueRequestsFromMapFunc(a.poolToClaims), builder.WithPredicates(predicate.Or(poolOpened(ctx), poolReadied(ctx), addressFieldsChanged)))
	return nil
}

// ClaimHandlerFor returns the handler of claim.
func (a *ProviderAdapter) ClaimHandlerFor(_ client.Client, claim *ipamv1.IPAddressClaim) ipamutil.ClaimHandler {
	return &claimHandler{ProviderAdapter: a, claim: claim}
}

func (a *ProviderAdapter) poolToClaims(ctx context.Context, o client.Object) []reconcile.Request {
	claims := &ipamv1.IPAddressClaimList{}
	if err := a.Client.List(ctx, claims, client.InNamespace(o.GetNamespace()), client.MatchingFields{poolRefField: o.GetName()}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(claims.Items))
	for i := range claims.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&claims.Items[i])})
	}
	return reqs
}

// poolOpened passes pool updates after which waiting claims can get an address.
func poolOpened(ctx context.Context) predicate.Funcs {
	canAllocate := func(o client.Object) bool {
		p, ok := asType[*v1alpha1.NetBoxIPPool](ctx, o)
		return ok && meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ReadyCondition) && p.Status.IPAddresses != nil && p.Status.IPAddresses.Free > 0
	}
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return !canAllocate(e.ObjectOld) && canAllocate(e.ObjectNew)
	}}
}

// poolReadied passes pool updates that make it ready, even when it is full: allocated claims sync their address fields.
func poolReadied(ctx context.Context) predicate.Funcs {
	ready := func(o client.Object) bool {
		p, ok := asType[*v1alpha1.NetBoxIPPool](ctx, o)
		return ok && meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ReadyCondition)
	}
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return !ready(e.ObjectOld) && ready(e.ObjectNew)
	}}
}

// addressFieldsChanged passes pool updates that change the fields of its addresses.
var addressFieldsChanged = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	o, ok := e.ObjectOld.(*v1alpha1.NetBoxIPPool)
	n, ok2 := e.ObjectNew.(*v1alpha1.NetBoxIPPool)
	return ok && ok2 && o.Spec.IPAddress != n.Spec.IPAddress
}}

type claimHandler struct {
	*ProviderAdapter
	claim *ipamv1.IPAddressClaim
	pool  *v1alpha1.NetBoxIPPool
}

var _ ipamutil.ClaimHandler = &claimHandler{}

// setReady sets the claim's Ready condition; ipamutil patches the claim even when a handler fails.
func (h *claimHandler) setReady(status metav1.ConditionStatus, reason, message string) {
	conditions.Set(h.claim, metav1.Condition{Type: ipamv1.IPAddressClaimReadyCondition, Status: status, Reason: reason, Message: message})
}

// poolNotReady reports a pool that is missing or not ready, unless the claim already holds its address:
// then a pool blip must not turn the claim not ready, and no event would turn it back.
func (h *claimHandler) poolNotReady(message string) {
	if h.claim.Status.AddressRef.Name == "" {
		h.setReady(metav1.ConditionFalse, ipamv1.IPAddressClaimReadyPoolNotReadyReason, message)
	}
}

// FetchPool loads the claim's NetBoxIPPool.
// Until the pool is ready a live claim waits: the result makes ipamutil end the reconcile without an error,
// and poolOpened wakes the claim. A deleted claim needs no ready pool.
func (h *claimHandler) FetchPool(ctx context.Context) (client.Object, *ctrl.Result, error) {
	pool := &v1alpha1.NetBoxIPPool{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.claim.Namespace, Name: h.claim.Spec.PoolRef.Name}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			h.poolNotReady(err.Error())
		}
		return nil, nil, err
	}
	if h.claim.DeletionTimestamp.IsZero() && !poolReady(pool) {
		h.poolNotReady(fmt.Sprintf("pool %s is not ready", pool.Name))
		ctrl.LoggerFrom(ctx).Info("Waiting for the pool to get ready", "pool", klog.KObj(pool))
		return nil, &ctrl.Result{}, nil
	}
	pool.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind(v1alpha1.NetBoxIPPoolKind))
	h.pool = pool
	return pool, nil, nil
}

func poolReady(p *v1alpha1.NetBoxIPPool) bool {
	return meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ReadyCondition) && p.Status.AddressPrefixLength != 0 &&
		slices.ContainsFunc(p.Status.Ranges, func(rs v1alpha1.IPPoolRangeStatus) bool { return rs.Ready })
}

// EnsureAddress fills address from the claim's NetBox IP, allocating it first if needed.
// An address already allocated is checked in NetBox and restored if it was deleted there.
// A failure is reported in the claim's Ready condition and returned, so the claim backs off.
// The exception is a NetBox error while an allocated address is checked: an outage does not undo the allocation.
func (h *claimHandler) EnsureAddress(ctx context.Context, address *ipamv1.IPAddress) (*ctrl.Result, error) {
	netBoxWrites.Lock()
	defer netBoxWrites.Unlock()
	var err error
	recheck := address.Spec.Address != "" // addresses never move once allocated
	if recheck {
		err = h.restore(ctx, address)
	} else {
		err = h.allocate(ctx, address)
	}
	if err != nil {
		switch {
		case errors.Is(err, errAddressConflict):
			h.setReady(metav1.ConditionFalse, reasonAddressConflict, err.Error())
		case recheck:
		case errors.Is(err, errPoolExhausted):
			h.setReady(metav1.ConditionFalse, ipamv1.IPAddressClaimReadyPoolExhaustedReason, err.Error())
		default:
			h.setReady(metav1.ConditionFalse, ipamv1.IPAddressClaimReadyAllocationFailedReason, err.Error())
		}
		return nil, err
	}
	// ipamutil records AddressRef only once the IPAddress exists, and that update wakes the claim.
	// Unknown, not a deleted condition: an empty status is invalid.
	if h.claim.Status.AddressRef.Name == "" {
		h.setReady(metav1.ConditionUnknown, reasonAllocating, "address allocated, waiting for the IPAddress object")
		return nil, nil
	}
	h.setReady(metav1.ConditionTrue, clusterv1.ReadyReason, "") // CAPI defines no reason for a ready claim
	return nil, nil
}

// restore makes sure NetBox still holds address for the claim, and puts it back if someone deleted it.
// An address NetBox gave to another object is a conflict: nothing of it is touched.
func (h *claimHandler) restore(ctx context.Context, address *ipamv1.IPAddress) error {
	ref := refOf("IPAddressClaim", h.claim)
	found, err := h.NetBox.FindByRef(ctx, netbox.IPAddresses, h.Owner, ref)
	if err != nil {
		return err
	}
	if len(found) > 1 {
		return fmt.Errorf("%d NetBox addresses carry %s", len(found), ref)
	}
	if len(found) == 1 {
		if p, err := netip.ParsePrefix(found[0].Address); err != nil || p.Addr().String() != address.Spec.Address {
			return fmt.Errorf("%w: NetBox address of %s is %s, allocated %s", errAddressConflict, ref, found[0].Address, address.Spec.Address)
		}
		return h.syncFields(ctx, ref, found[0])
	}
	addr, err := netip.ParseAddr(address.Spec.Address)
	if err != nil {
		return err
	}
	cidr := netip.PrefixFrom(addr, int(ptr.Deref(address.Spec.Prefix, h.pool.Status.AddressPrefixLength))).String()
	// Without the mask NetBox matches the host, as its own uniqueness check does.
	taken, err := h.NetBox.List(ctx, netbox.IPAddresses, url.Values{"address": {address.Spec.Address}, "vrf_id": {"null"}})
	if err != nil {
		return err
	}
	if len(taken) > 0 {
		return fmt.Errorf("%w: %s is used in NetBox by another object", errAddressConflict, address.Spec.Address)
	}
	f := h.fieldsOrEmpty(ctx, h.claim)
	fields := h.Owner.Managed(ref, clusterOf(h.claim), "capi: address restored for "+ref,
		map[string]any{"address": cidr, "status": "active", "dns_name": f.DNSName, "description": f.Description})
	if _, err := h.NetBox.Create(ctx, netbox.IPAddresses, fields); err != nil {
		return err
	}
	ctrl.LoggerFrom(ctx).Info("Restored the NetBox address deleted outside the provider", "claim", klog.KObj(h.claim), "address", cidr)
	return nil
}

// syncFields brings dns_name and description of ip to the pool's templates.
func (h *claimHandler) syncFields(ctx context.Context, ref string, ip netbox.Object) error {
	f, err := addressFields(h.pool, h.claim)
	if err != nil {
		return err
	}
	if ip.DNSName == f.DNSName && ip.Description == f.Description {
		return nil
	}
	_, err = h.NetBox.Patch(ctx, netbox.IPAddresses, ip.ID, "", map[string]any{
		"dns_name": f.DNSName, "description": f.Description, "changelog_message": "capi: address fields for " + ref,
	})
	return err
}

// fieldsOrEmpty renders the fields of the address of claim, empty ones if they do not render:
// an address is restored or handed back even then. The claim's recheck reports the error.
func (h *claimHandler) fieldsOrEmpty(ctx context.Context, claim *ipamv1.IPAddressClaim) ipFields {
	f, err := addressFields(h.pool, claim)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Writing the NetBox address with empty dns_name and description", "claim", klog.KObj(claim))
	}
	return f
}

func (h *claimHandler) allocate(ctx context.Context, address *ipamv1.IPAddress) error {
	ip, err := h.findOrAllocate(ctx, refOf("IPAddressClaim", h.claim))
	if err != nil {
		return err
	}
	p, err := netip.ParsePrefix(ip.Address)
	if err != nil {
		return err
	}
	address.Spec.Address = p.Addr().String()
	address.Spec.Prefix = ptr.To(h.pool.Status.AddressPrefixLength)
	address.Spec.Gateway = h.pool.Status.Gateway
	return nil
}

// maxAllocationAttempts bounds the addresses handed back to their claims before one is kept.
const maxAllocationAttempts = 10

var errPoolExhausted = errors.New("no free addresses")

// findOrAllocate returns the claim's NetBox address, allocating one if no earlier attempt did.
// An address that another claim of the pool still holds was deleted in NetBox by hand: it goes back to that claim
// and the next one is taken.
func (h *claimHandler) findOrAllocate(ctx context.Context, ref string) (netbox.Object, error) {
	for range maxAllocationAttempts {
		ip, err := h.findOrCreate(ctx, ref)
		if err != nil {
			return ip, err
		}
		holder, err := h.heldBy(ctx, ip)
		if err != nil || holder == nil {
			return ip, err
		}
		holderRef := refOf("IPAddressClaim", holder)
		f := h.fieldsOrEmpty(ctx, holder)
		fields := h.Owner.Managed(holderRef, clusterOf(holder), "capi: restored for "+holderRef,
			map[string]any{"status": "active", "dns_name": f.DNSName, "description": f.Description})
		if _, err := h.NetBox.Patch(ctx, netbox.IPAddresses, ip.ID, "", fields); err != nil {
			return netbox.Object{}, err
		}
		ctrl.LoggerFrom(ctx).Info("Gave the NetBox address deleted outside the provider back to its claim",
			"claim", klog.KObj(h.claim), "holder", klog.KObj(holder), "address", ip.Address)
	}
	return netbox.Object{}, fmt.Errorf("pool %s: the next %d free addresses are held by other claims, retrying", h.pool.Name, maxAllocationAttempts)
}

// heldBy returns the live claim, other than h.claim, whose IPAddress in the pool holds the address of ip.
func (h *claimHandler) heldBy(ctx context.Context, ip netbox.Object) (*ipamv1.IPAddressClaim, error) {
	p, err := netip.ParsePrefix(ip.Address)
	if err != nil {
		return nil, err
	}
	addresses := &ipamv1.IPAddressList{}
	if err := h.Client.List(ctx, addresses, client.InNamespace(h.pool.Namespace), client.MatchingFields{poolRefField: h.pool.Name}); err != nil {
		return nil, err
	}
	for _, a := range addresses.Items {
		if a.Spec.Address != p.Addr().String() || a.Spec.ClaimRef.Name == h.claim.Name {
			continue
		}
		holder := &ipamv1.IPAddressClaim{}
		err := h.Client.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: a.Spec.ClaimRef.Name}, holder)
		if client.IgnoreNotFound(err) != nil {
			return nil, err
		}
		if err == nil && holder.DeletionTimestamp.IsZero() { // a going claim has released its address
			return holder, nil
		}
	}
	return nil, nil
}

func (h *claimHandler) findOrCreate(ctx context.Context, ref string) (netbox.Object, error) {
	found, err := h.NetBox.FindByRef(ctx, netbox.IPAddresses, h.Owner, ref)
	switch {
	case err != nil:
		return netbox.Object{}, err
	case len(found) == 1:
		return found[0], nil
	case len(found) > 1:
		return netbox.Object{}, fmt.Errorf("%d NetBox addresses carry %s", len(found), ref)
	}
	if err := ownedByOtherOwner(ctx, h.NetBox, h.Owner, netbox.IPAddresses, ref); err != nil {
		return netbox.Object{}, err
	}
	f, err := addressFields(h.pool, h.claim)
	if err != nil {
		return netbox.Object{}, err
	}
	fields := h.Owner.Managed(ref, clusterOf(h.claim), "capi: address for "+ref,
		map[string]any{"status": "active", "dns_name": f.DNSName, "description": f.Description})
	for _, rs := range h.pool.Status.Ranges {
		if !rs.Ready {
			continue
		}
		ip, err := h.NetBox.Create(ctx, netbox.AvailableIPs(rs.ID), fields)
		if errors.Is(err, netbox.ErrConflict) {
			continue // range is full, try the next one
		}
		return ip, err
	}
	return netbox.Object{}, fmt.Errorf("pool %s: %w", h.pool.Name, errPoolExhausted)
}

// ReleaseAddress deletes the claim's NetBox IP.
func (h *claimHandler) ReleaseAddress(ctx context.Context) (*ctrl.Result, error) {
	if deletedForMove(h.claim) {
		return nil, nil
	}
	// Serialised with a hand-back to this claim.
	netBoxWrites.Lock()
	defer netBoxWrites.Unlock()
	ref := refOf("IPAddressClaim", h.claim)
	found, err := h.NetBox.FindByRef(ctx, netbox.IPAddresses, h.Owner, ref)
	if err != nil {
		return nil, err
	}
	for _, ip := range found {
		if err := h.NetBox.Delete(ctx, netbox.IPAddresses, ip.ID, "capi: released by "+ref); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func clusterOf(claim *ipamv1.IPAddressClaim) string {
	if claim.Spec.ClusterName != "" {
		return claim.Spec.ClusterName
	}
	return claim.Labels[clusterv1.ClusterNameLabel]
}
