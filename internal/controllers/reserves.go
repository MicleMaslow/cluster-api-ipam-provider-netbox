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
	"strconv"
	"strings"

	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/layout"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

// reserveRef is the key of address index of group in nw.
func reserveRef(k netbox.Keys, nw *v1alpha1.NetBoxNetwork, group string, index int) string {
	return k.Key("NetBoxNetwork", nw, "addresses", group, strconv.Itoa(index))
}

// reservesRef starts the keys of all reserves of nw.
func reservesRef(k netbox.Keys, nw *v1alpha1.NetBoxNetwork) string {
	return k.Key("NetBoxNetwork", nw, "addresses", "")
}

// groupOf is the layout group of g.
func groupOf(g v1alpha1.AddressGroup) layout.Group {
	return layout.Group{Name: g.Name, Count: int(g.Count)}
}

// retired reports whether ref is a key moved aside by retire.
func retired(ref string) bool { return strings.HasSuffix(ref, "/retired") }

// retireReserves moves the reserves a deleted network of the name of nw retained outside prefix to retiredRef: they
// stay deprecated where they are, and new reserves take their keys.
func (r *NetBoxNetworkReconciler) retireReserves(ctx context.Context, nw *v1alpha1.NetBoxNetwork, prefix netip.Prefix, found []netbox.Object) error {
	for _, o := range found {
		ref := r.Keys.RefOf(o)
		if p, err := netip.ParsePrefix(o.Address); err != nil || retired(ref) || o.Status.Value != "deprecated" || prefix.Contains(p.Addr()) {
			continue
		}
		if err := r.retire(ctx, nw, netbox.IPAddresses, o, ref, "ReserveRetired"); err != nil {
			return err
		}
	}
	return nil
}

// restoreGroups takes the groups of spec.addresses back from found, the reserves of nw in NetBox: a group keeps its
// addresses inside prefix from index 1 up to the first one missing.
func restoreGroups(k netbox.Keys, nw *v1alpha1.NetBoxNetwork, prefix netip.Prefix, found []netbox.Object) []layout.PlacedGroup {
	byRef := map[string]netip.Addr{}
	for _, o := range found {
		if p, err := netip.ParsePrefix(o.Address); err == nil && prefix.Contains(p.Addr()) {
			byRef[k.RefOf(o)] = p.Addr()
		}
	}
	var out []layout.PlacedGroup
	for _, g := range nw.Spec.Addresses {
		var addrs []netip.Addr
		for i := 1; ; i++ {
			a, ok := byRef[reserveRef(k, nw, g.Name, i)]
			if !ok {
				break
			}
			addrs = append(addrs, a)
		}
		if len(addrs) > 0 {
			out = append(out, layout.PlacedGroup{Group: layout.Group{Name: g.Name, Count: len(addrs)}, Addrs: addrs})
		}
	}
	return out
}

// dropExcessReserves deletes the reserves of nw that l does not hold, whatever the deletionPolicy: a pool can take
// their addresses next.
func (r *NetBoxNetworkReconciler) dropExcessReserves(ctx context.Context, nw *v1alpha1.NetBoxNetwork, l layout.Layout) error {
	keep := map[string]bool{}
	for _, g := range l.Groups {
		for i := range g.Addrs {
			keep[reserveRef(r.Keys, nw, g.Name, i+1)] = true
		}
	}
	return underLock(ctx, r.NetBox, func(ctx context.Context, nb *netbox.Client) error {
		found, err := nb.FindByRefPrefix(ctx, r.Keys, netbox.IPAddresses, reservesRef(r.Keys, nw))
		if err != nil {
			return err
		}
		for _, o := range found {
			ref := r.Keys.RefOf(o)
			if keep[ref] || retired(ref) {
				continue
			}
			if err := nb.Delete(ctx, netbox.IPAddresses, o.ID, "", "capi: dropped by "+r.Keys.Key("NetBoxNetwork", nw)); errors.Is(err, netbox.ErrNotFound) {
				continue
			} else if err != nil {
				return err
			}
			ctrl.LoggerFrom(ctx).Info("Deleted a reserved address", "network", klog.KObj(nw), "ref", ref, "address", o.Address)
			normalEvent(r.Recorder, nw, "ReserveDeleted", "Deleted the reserved address %s (%s)", o.Address, ref)
		}
		return nil
	})
}

// applyReserves brings the reserves of nw in NetBox to the groups of l: each address is an active NetBox address
// under reserveRef, with the mask of the L2 network, the dns_name and texts of its group's templates, and the owner and
// tenant of nw. A reserve found elsewhere or deprecated is moved back and made active.
func (r *NetBoxNetworkReconciler) applyReserves(ctx context.Context, nw *v1alpha1.NetBoxNetwork, l layout.Layout) error {
	l2, _, err := l2Of(nw)
	if err != nil {
		return err
	}
	type reserve struct {
		ref  string
		addr netip.Prefix
		f    ipFields
	}
	var want []reserve
	for _, g := range l.Groups {
		var spec v1alpha1.AddressGroup
		for _, s := range nw.Spec.Addresses {
			if s.Name == g.Name {
				spec = s
			}
		}
		for i, a := range g.Addrs {
			f, err := groupFields(nw, spec, i+1)
			if err != nil {
				return withReason(v1alpha1.ReasonInvalidSpec, fmt.Errorf("addresses %s: %w", g.Name, err))
			}
			want = append(want, reserve{reserveRef(r.Keys, nw, g.Name, i+1), netip.PrefixFrom(a, l2.Bits()), f})
		}
	}
	if len(want) == 0 {
		return nil
	}
	return underLock(ctx, r.NetBox, func(ctx context.Context, nb *netbox.Client) error {
		found, err := nb.FindByRefPrefix(ctx, r.Keys, netbox.IPAddresses, reservesRef(r.Keys, nw))
		if err != nil {
			return err
		}
		byRef := map[string][]netbox.Object{}
		for _, o := range found {
			byRef[r.Keys.RefOf(o)] = append(byRef[r.Keys.RefOf(o)], o)
		}
		rr := *r
		rr.NetBox = nb
		var attrsErrs []error
		for _, w := range want {
			err := rr.syncReserve(ctx, nw, w.ref, w.addr, w.f, byRef[w.ref])
			attrsErr, err := splitAttrs(err)
			if err != nil {
				return err
			}
			attrsErrs = append(attrsErrs, attrsErr)
		}
		return foldErrors(attrsErrs)
	})
}

// syncReserve brings the reserve ref of nw to addr and f; found are the NetBox addresses under ref.
func (r *NetBoxNetworkReconciler) syncReserve(ctx context.Context, nw *v1alpha1.NetBoxNetwork, ref string, addr netip.Prefix, f ipFields, found []netbox.Object) error {
	attrs := f.on(networkAttrs(nw))
	switch len(found) {
	case 0:
		fields := r.Keys.Managed(attrs, ref, "capi: reserve "+ref, map[string]any{"address": addr.String(), "status": "active", "dns_name": f.DNSName})
		// A NetBox address without a key alone at addr, as a running cluster has them in the tail, is adopted.
		o, ok, err := r.keylessAt(ctx, addr.Addr())
		if err != nil {
			return err
		}
		if ok {
			fields["changelog_message"] = "capi: adopted by " + ref
			if _, err := r.NetBox.Patch(ctx, netbox.IPAddresses, o.ID, "", fields); err != nil {
				return err
			}
			ctrl.LoggerFrom(ctx).Info("Adopted a reserved address", "network", klog.KObj(nw), "ref", ref, "address", addr.String(), "id", o.ID)
			normalEvent(r.Recorder, nw, "ReserveAdopted", "Adopted the NetBox address %s as the reserved address %s", o.Address, ref)
			return nil
		}
		ip, err := r.NetBox.Create(ctx, netbox.IPAddresses, fields)
		if err != nil {
			return err
		}
		ctrl.LoggerFrom(ctx).Info("Reserved an address", "network", klog.KObj(nw), "ref", ref, "address", ip.Address)
		normalEvent(r.Recorder, nw, "ReserveCreated", "Reserved the address %s (%s)", ip.Address, ref)
		return nil
	case 1:
	default:
		return retryLater(v1alpha1.ReasonDuplicate, fmt.Errorf("%d NetBox addresses carry %s", len(found), ref))
	}
	moved, err := r.syncIP(ctx, nw, ref, "the reserved address "+addr.String(), found[0], addr, f.DNSName, attrs)
	if moved {
		ctrl.LoggerFrom(ctx).Info("Reclaimed a reserved address", "network", klog.KObj(nw), "ref", ref, "from", found[0].Address, "address", addr.String())
		normalEvent(r.Recorder, nw, "ReserveReclaimed", "Reclaimed the reserved address %s (%s)", addr, ref)
	}
	return err
}

// syncIP brings ip, the NetBox address what of nw under ref, to addr, active, dns and attrs in one PATCH, as for the
// VIP; attributes NetBox refuses do not hold back the address, status and dns_name (attrsFailed). It reports whether
// the address was put back at addr.
func (r *NetBoxNetworkReconciler) syncIP(ctx context.Context, nw *v1alpha1.NetBoxNetwork, ref, what string, ip netbox.Object, addr netip.Prefix, dns string, attrs netbox.Attrs) (bool, error) {
	fields := map[string]any{}
	if ip.Address != addr.String() || ip.Status.Value != "active" {
		fields["address"], fields["status"], fields["changelog_message"] = addr.String(), "active", "capi: reclaimed by "+ref
	}
	if ip.DNSName != dns {
		fields["dns_name"] = dns
		if _, ok := fields["changelog_message"]; !ok {
			fields["changelog_message"] = "capi: dns_name of " + ref
		}
	}
	err := syncAttrs(ctx, r.NetBox, r.Recorder, nw, netbox.IPAddresses, ip, attrs, fields, "capi: attributes of "+ref)
	if err != nil && len(fields) > 0 {
		if _, ferr := r.NetBox.Patch(ctx, netbox.IPAddresses, ip.ID, "", fields); ferr != nil {
			return false, ferr
		}
	}
	_, moved := fields["address"]
	return moved, attrsFailed(what, err)
}

// releaseReserves lets go of the reserves of nw (releaseIP); retired ones are not its own any more.
func (r *NetBoxNetworkReconciler) releaseReserves(ctx context.Context, nw *v1alpha1.NetBoxNetwork) error {
	found, err := r.NetBox.FindByRefPrefix(ctx, r.Keys, netbox.IPAddresses, reservesRef(r.Keys, nw))
	if err != nil {
		return err
	}
	for _, ip := range found {
		if retired(r.Keys.RefOf(ip)) {
			continue
		}
		if released, err := r.releaseIP(ctx, nw, ip); err != nil {
			return err
		} else if released {
			ctrl.LoggerFrom(ctx).Info("Released a reserved address", "network", klog.KObj(nw), "address", ip.Address, "policy", nw.Spec.DeletionPolicy)
			normalEvent(r.Recorder, nw, "ReserveReleased", "Released the reserved address %s (deletionPolicy %s)", ip.Address, nw.Spec.DeletionPolicy)
		}
	}
	return nil
}
