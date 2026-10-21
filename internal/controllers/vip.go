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

	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netmath"
)

// defaultAPIPort is the port of the API server when spec.controlPlaneEndpoint.port is not set.
const defaultAPIPort = 6443

// vipRef is the key of the VIP of nw in NetBox.
func vipRef(k netbox.Keys, nw *v1alpha1.NetBoxNetwork) string {
	return k.Key("NetBoxNetwork", nw, "vip")
}

// vipUsable reports whether a can be the VIP: inside prefix, not the network, broadcast or gateway of l2,
// held by no NetBox address and inside no NetBox range.
func vipUsable(a netip.Addr, prefix, l2 netip.Prefix, gw netip.Addr, taken []netip.Addr, ranges [][2]netip.Addr) bool {
	if !prefix.Contains(a) || a == l2.Addr() || a == netmath.Last(l2) || a == gw || slices.Contains(taken, a) {
		return false
	}
	return !slices.ContainsFunc(ranges, func(r [2]netip.Addr) bool { return netmath.Contains(r[0], r[1], a) })
}

// pickVIP returns the highest usable address of prefix.
func pickVIP(prefix, l2 netip.Prefix, gw netip.Addr, taken []netip.Addr, ranges [][2]netip.Addr) (netip.Addr, bool) {
	for a := netmath.Last(prefix); prefix.Contains(a); a = a.Prev() {
		if vipUsable(a, prefix, l2, gw, taken, ranges) {
			return a, true
		}
	}
	return netip.Addr{}, false
}

// reserveVIP returns the VIP of nw in prefix, creating it in NetBox if it is not there yet.
// A valid preferred is the only candidate; otherwise the highest usable address is taken once the
// Cluster is found, as its endpoint may be the address to take.
func (r *NetBoxNetworkReconciler) reserveVIP(ctx context.Context, nw *v1alpha1.NetBoxNetwork, prefix netip.Prefix, preferred netip.Addr, clusterFound bool) (netip.Addr, error) {
	ref := vipRef(r.Keys, nw)
	found, err := r.NetBox.FindByRef(ctx, r.Keys, netbox.IPAddresses, ref)
	if err != nil {
		return netip.Addr{}, err
	}
	// A VIP retained by a deleted network of this name outside the new prefix gives way to a new one.
	if len(found) == 1 && found[0].Status.Value == "deprecated" {
		if a, err := netip.ParsePrefix(found[0].Address); err == nil && !prefix.Contains(a.Addr()) {
			if err := r.retire(ctx, nw, netbox.IPAddresses, found[0], ref, "VIPRetired"); err != nil {
				return netip.Addr{}, err
			}
			found = nil
		}
	}
	var ip netbox.Object
	switch len(found) {
	case 0:
		if !preferred.IsValid() && !clusterFound {
			return netip.Addr{}, waitFor(v1alpha1.ReasonWaitingForCluster, fmt.Errorf("cluster %s not found", nw.Spec.ClusterName))
		}
		ip, err = r.createVIP(ctx, nw, prefix, preferred)
	case 1:
		ip = found[0]
		// The check after a create can have failed: until the status has the VIP, it is checked here.
		if nw.Status.ControlPlaneEndpoint.Host == "" {
			err = r.dropDuplicate(ctx, nw, ip)
		}
	default:
		err = retryLater(v1alpha1.ReasonDuplicate, fmt.Errorf("%d NetBox addresses carry %s", len(found), ref))
	}
	if err != nil {
		return netip.Addr{}, err
	}
	p, err := netip.ParsePrefix(ip.Address)
	if err != nil {
		return netip.Addr{}, err
	}
	if !prefix.Contains(p.Addr()) {
		return netip.Addr{}, retryLater(v1alpha1.ReasonPrefixMismatch, fmt.Errorf("NetBox address %s of %s is outside %s", p, ref, prefix))
	}
	if want, err := netip.ParseAddr(nw.Status.ControlPlaneEndpoint.Host); err == nil && want != p.Addr() {
		return netip.Addr{}, retryLater(v1alpha1.ReasonEndpointConflict, fmt.Errorf("NetBox address %s of %s is not the VIP %s; move it back in NetBox", p.Addr(), ref, want))
	}
	nwRef := r.Keys.Key("NetBoxNetwork", nw)
	dns, err := dnsNameOf(nw)
	if err != nil {
		return netip.Addr{}, err
	}
	fields := map[string]any{}
	if ip.Status.Value == "deprecated" {
		fields["status"], fields["changelog_message"] = "active", "capi: reclaimed by "+nwRef
	}
	if ip.DNSName != dns {
		fields["dns_name"] = dns
		if _, ok := fields["changelog_message"]; !ok {
			fields["changelog_message"] = "capi: dns_name of " + nwRef
		}
	}
	_, attrs, err := networkTexts(nw)
	if err != nil {
		return netip.Addr{}, err
	}
	// One PATCH, one changelog entry; attributes NetBox refuses do not hold back the status and dns_name.
	err = syncAttrs(ctx, r.NetBox, r.Recorder, nw, netbox.IPAddresses, ip, attrs, fields, "capi: attributes of "+nwRef)
	if err != nil && len(fields) > 0 {
		if _, ferr := r.NetBox.Patch(ctx, netbox.IPAddresses, ip.ID, "", fields); ferr != nil {
			return netip.Addr{}, ferr
		}
	}
	return p.Addr(), attrsFailed("the VIP "+ip.Address, err)
}

// beforeVIPCreate runs between the pick of the VIP address and its create; a test seam.
var beforeVIPCreate = func() {}

// createVIP creates the VIP in NetBox: preferred if it is valid, else the highest usable address of prefix. A preferred
// address, the Cluster's endpoint, held only by a NetBox address without a key is the VIP of a running cluster: that
// one is adopted (adoptVIP).
func (r *NetBoxNetworkReconciler) createVIP(ctx context.Context, nw *v1alpha1.NetBoxNetwork, prefix netip.Prefix, preferred netip.Addr) (netbox.Object, error) {
	l2, gw, err := l2Of(nw)
	if err != nil {
		return netbox.Object{}, err
	}
	_, attrs, err := networkTexts(nw)
	if err != nil {
		return netbox.Object{}, err
	}
	dns, err := dnsNameOf(nw)
	if err != nil {
		return netbox.Object{}, err
	}
	taken, ranges, err := r.occupied(ctx, prefix, l2)
	if err != nil {
		return netbox.Object{}, err
	}
	// Reserves and ranges placed in the layout are taken before they are created.
	var planned []netip.Addr
	for _, g := range nw.Status.Layout.Addresses {
		for _, s := range g.Addresses {
			if a, err := netip.ParseAddr(s); err == nil {
				planned = append(planned, a)
			}
		}
	}
	for _, p := range nw.Status.Layout.Pools {
		for _, lr := range p.Ranges {
			from, ferr := netip.ParseAddr(lr.Start)
			to, terr := netip.ParseAddr(lr.End)
			if ferr == nil && terr == nil {
				ranges = append(ranges, [2]netip.Addr{from, to})
			}
		}
	}
	taken = append(taken, planned...)
	vip := preferred
	switch {
	case !vip.IsValid():
		// Nor is an allocated address outside them, of a pool that is not the network's.
		addrs, err := r.ipAddresses(ctx, nw.Namespace)
		if err != nil {
			return netbox.Object{}, err
		}
		var ok bool
		if vip, ok = pickVIP(prefix, l2, gw, append(taken, addrs...), ranges); !ok {
			return netbox.Object{}, retryLater(v1alpha1.ReasonExhausted, fmt.Errorf("no free address for the VIP in %s", prefix))
		}
	case !vipUsable(vip, prefix, l2, gw, taken, ranges):
		// A VIP the status has is never adopted: the address of a lost one was taken.
		if nw.Status.ControlPlaneEndpoint.Host == "" && vipUsable(vip, prefix, l2, gw, planned, ranges) {
			if ip, ok, err := r.adoptVIP(ctx, nw, vip, attrs, dns); ok || err != nil {
				return ip, err
			}
		}
		return netbox.Object{}, retryLater(v1alpha1.ReasonEndpointConflict,
			fmt.Errorf("%s cannot be the VIP: it is outside %s, a service address of %s, or used in NetBox", vip, prefix, l2))
	}
	beforeVIPCreate()
	nwRef := r.Keys.Key("NetBoxNetwork", nw)
	ip, err := r.NetBox.Create(ctx, netbox.IPAddresses, r.Keys.Managed(attrs, vipRef(r.Keys, nw), "capi: VIP for "+nwRef, map[string]any{
		"address": netip.PrefixFrom(vip, l2.Bits()).String(), "role": "vip", "status": "active", "dns_name": dns,
	}))
	if err != nil {
		return ip, err
	}
	ctrl.LoggerFrom(ctx).Info("Reserved the control plane VIP", "network", klog.KObj(nw), "address", ip.Address)
	normalEvent(r.Recorder, nw, "VIPReserved", "Reserved the control plane VIP %s", ip.Address)
	if err := r.dropDuplicate(ctx, nw, ip); err != nil {
		return netbox.Object{}, err
	}
	return ip, nil
}

// adoptVIP makes the NetBox address at a, if it is the only one there and carries no key, the VIP of nw: it gets the
// key, dns_name and attributes of one. It reports false for any other.
func (r *NetBoxNetworkReconciler) adoptVIP(ctx context.Context, nw *v1alpha1.NetBoxNetwork, a netip.Addr, attrs netbox.Attrs, dns string) (netbox.Object, bool, error) {
	o, ok, err := r.keylessAt(ctx, a)
	if err != nil || !ok {
		return netbox.Object{}, false, err
	}
	fields := r.Keys.Managed(attrs, vipRef(r.Keys, nw), "capi: adopted by "+r.Keys.Key("NetBoxNetwork", nw), map[string]any{"dns_name": dns})
	if _, err := r.NetBox.Patch(ctx, netbox.IPAddresses, o.ID, "", fields); err != nil {
		return netbox.Object{}, false, err
	}
	ctrl.LoggerFrom(ctx).Info("Adopted the control plane VIP", "network", klog.KObj(nw), "address", o.Address, "id", o.ID)
	normalEvent(r.Recorder, nw, "VIPAdopted", "Adopted the NetBox address %s as the control plane VIP", o.Address)
	ip, _, err := r.NetBox.Get(ctx, netbox.IPAddresses, o.ID)
	return ip, true, err
}

// keylessAt returns the global-VRF NetBox address at a if it is the only one there and carries no key.
func (r *NetBoxNetworkReconciler) keylessAt(ctx context.Context, a netip.Addr) (netbox.Object, bool, error) {
	same, err := r.NetBox.List(ctx, netbox.IPAddresses, url.Values{"address": {a.String()}, "vrf_id": {"null"}})
	if err != nil || len(same) != 1 || r.Keys.RefOf(same[0]) != "" {
		return netbox.Object{}, false, err
	}
	return same[0], true, nil
}

// dropDuplicate deletes ip, the VIP of nw, when an older global-VRF NetBox address holds its address too:
// NetBox lets VIP-role addresses share an address, so another creator can have taken it at the same time.
// Of racing creators the older one stays; a VIP that only newer objects share is long-lived and is kept.
func (r *NetBoxNetworkReconciler) dropDuplicate(ctx context.Context, nw *v1alpha1.NetBoxNetwork, ip netbox.Object) error {
	p, err := netip.ParsePrefix(ip.Address)
	if err != nil {
		return err
	}
	same, err := r.NetBox.List(ctx, netbox.IPAddresses, url.Values{"address": {p.Addr().String()}, "vrf_id": {"null"}})
	if err != nil {
		return err
	}
	log := ctrl.LoggerFrom(ctx)
	for _, o := range same {
		if o.ID == ip.ID {
			continue
		}
		if o.ID > ip.ID {
			log.Info("Another NetBox address holds the control plane VIP too, it is newer: keeping the VIP", "network", klog.KObj(nw), "address", ip.Address, "other", o.ID)
			continue
		}
		msg := fmt.Sprintf("capi: VIP of %s duplicates NetBox address #%d", r.Keys.Key("NetBoxNetwork", nw), o.ID)
		if err := r.NetBox.Delete(ctx, netbox.IPAddresses, ip.ID, "", msg); err != nil {
			return err
		}
		log.Info("Deleted the control plane VIP, an older NetBox address holds it", "network", klog.KObj(nw), "address", ip.Address, "other", o.ID)
		normalEvent(r.Recorder, nw, "VIPDeleted", "Deleted the control plane VIP %s: NetBox address #%d holds it too", ip.Address, o.ID)
		return retryLater(v1alpha1.ReasonEndpointConflict, fmt.Errorf("%s is also held by NetBox address #%d", p.Addr(), o.ID))
	}
	return nil
}

// netBoxIn returns the global-VRF addresses NetBox holds inside prefix and the IP ranges inside l2.
// NetBox's parent filter finds a range only when it lies wholly inside the parent, so a range that
// reaches past prefix is found through l2; ranges that cross the boundary of l2 are not seen.
func (r *NetBoxNetworkReconciler) netBoxIn(ctx context.Context, prefix, l2 netip.Prefix) (ips, ranges []netbox.Object, err error) {
	if ips, err = r.NetBox.List(ctx, netbox.IPAddresses, url.Values{"parent": {prefix.Masked().String()}, "vrf_id": {"null"}}); err != nil {
		return nil, nil, err
	}
	ranges, err = r.NetBox.List(ctx, netbox.IPRanges, url.Values{"parent": {l2.String()}})
	return ips, ranges, err
}

// occupied returns the addresses and the bounds of the ranges of netBoxIn.
func (r *NetBoxNetworkReconciler) occupied(ctx context.Context, prefix, l2 netip.Prefix) ([]netip.Addr, [][2]netip.Addr, error) {
	ips, objs, err := r.netBoxIn(ctx, prefix, l2)
	if err != nil {
		return nil, nil, err
	}
	taken := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		p, err := netip.ParsePrefix(ip.Address)
		if err != nil {
			return nil, nil, err
		}
		taken = append(taken, p.Addr())
	}
	ranges := make([][2]netip.Addr, 0, len(objs))
	for _, o := range objs {
		from, to, err := netBoxBounds(o)
		if err != nil {
			return nil, nil, err
		}
		ranges = append(ranges, [2]netip.Addr{from, to})
	}
	return taken, ranges, nil
}

// releaseVIP lets go of the VIP of nw (releaseIP).
func (r *NetBoxNetworkReconciler) releaseVIP(ctx context.Context, nw *v1alpha1.NetBoxNetwork) error {
	found, err := r.NetBox.FindByRef(ctx, r.Keys, netbox.IPAddresses, vipRef(r.Keys, nw))
	if err != nil {
		return err
	}
	for _, ip := range found {
		if released, err := r.releaseIP(ctx, nw, ip); err != nil {
			return err
		} else if released {
			ctrl.LoggerFrom(ctx).Info("Released the control plane VIP", "network", klog.KObj(nw), "address", ip.Address, "policy", nw.Spec.DeletionPolicy)
			normalEvent(r.Recorder, nw, "VIPReleased", "Released the control plane VIP %s (deletionPolicy %s)", ip.Address, nw.Spec.DeletionPolicy)
		}
	}
	return nil
}

// releaseIP lets go of ip, an address of nw: deleted under deletionPolicy Delete, otherwise deprecated.
// It reports false for one already gone.
func (r *NetBoxNetworkReconciler) releaseIP(ctx context.Context, nw *v1alpha1.NetBoxNetwork, ip netbox.Object) (bool, error) {
	released := "capi: released by " + r.Keys.Key("NetBoxNetwork", nw)
	var err error
	if nw.Spec.DeletionPolicy == v1alpha1.DeletionPolicyDelete {
		err = r.NetBox.Delete(ctx, netbox.IPAddresses, ip.ID, "", released)
	} else {
		_, err = r.NetBox.Patch(ctx, netbox.IPAddresses, ip.ID, "", map[string]any{"status": "deprecated", "changelog_message": released})
	}
	if errors.Is(err, netbox.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}
