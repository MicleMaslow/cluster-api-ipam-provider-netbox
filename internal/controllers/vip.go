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
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netmath"
)

// defaultAPIPort is the port of the API server when spec.controlPlaneEndpoint.port is not set.
const defaultAPIPort = 6443

// vipRef is the key of the VIP of nw in NetBox.
func vipRef(nw *v1alpha1.NetBoxNetwork) string { return refOf("NetBoxNetwork", nw) + "/vip" }

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

// reservedOf are the addresses of nw's prefix that a pool range must not cover: the gateway and the VIP.
func reservedOf(nw *v1alpha1.NetBoxNetwork, gw netip.Addr) []netip.Addr {
	reserved := []netip.Addr{gw}
	if vip, err := netip.ParseAddr(nw.Status.ControlPlaneEndpoint.Host); err == nil {
		reserved = append(reserved, vip)
	}
	return reserved
}

// reserveVIP returns the VIP of nw in prefix, creating it in NetBox if it is not there yet.
// A valid preferred is the only candidate; otherwise the highest usable address is taken once the
// Cluster is found, as its endpoint may be the address to take.
func (r *NetBoxNetworkReconciler) reserveVIP(ctx context.Context, nw *v1alpha1.NetBoxNetwork, prefix netip.Prefix, preferred netip.Addr, clusterFound bool) (netip.Addr, error) {
	ref := vipRef(nw)
	found, err := r.NetBox.FindByRef(ctx, netbox.IPAddresses, r.Owner, ref)
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
	nwRef := refOf("NetBoxNetwork", nw)
	fields := map[string]any{}
	if ip.Status.Value == "deprecated" {
		fields["status"], fields["changelog_message"] = "active", "capi: reclaimed by "+nwRef
	}
	if dns := dnsNameOf(nw); ip.DNSName != dns {
		fields["dns_name"] = dns
		if _, ok := fields["changelog_message"]; !ok {
			fields["changelog_message"] = "capi: dns_name of " + nwRef
		}
	}
	if len(fields) > 0 {
		if _, err := r.NetBox.Patch(ctx, netbox.IPAddresses, ip.ID, "", fields); err != nil {
			return netip.Addr{}, err
		}
	}
	return p.Addr(), nil
}

// dnsNameOf is the dns_name of the VIP; NetBox stores it in lower case.
func dnsNameOf(nw *v1alpha1.NetBoxNetwork) string {
	if nw.Spec.ControlPlaneEndpoint == nil {
		return ""
	}
	return strings.ToLower(nw.Spec.ControlPlaneEndpoint.DNSName)
}

// beforeVIPCreate runs between the pick of the VIP address and its create; a test seam.
var beforeVIPCreate = func() {}

// createVIP creates the VIP in NetBox: preferred if it is valid, else the highest usable address of prefix.
func (r *NetBoxNetworkReconciler) createVIP(ctx context.Context, nw *v1alpha1.NetBoxNetwork, prefix netip.Prefix, preferred netip.Addr) (netbox.Object, error) {
	ref := vipRef(nw)
	if err := ownedByOtherOwner(ctx, r.NetBox, r.Owner, netbox.IPAddresses, ref); err != nil {
		return netbox.Object{}, err
	}
	l2, gw, err := l2Of(nw)
	if err != nil {
		return netbox.Object{}, err
	}
	taken, ranges, err := r.occupied(ctx, prefix, l2)
	if err != nil {
		return netbox.Object{}, err
	}
	vip := preferred
	if vip.IsValid() {
		if !vipUsable(vip, prefix, l2, gw, taken, ranges) {
			return netbox.Object{}, retryLater(v1alpha1.ReasonEndpointConflict,
				fmt.Errorf("%s cannot be the VIP: it is outside %s, a service address of %s, or used in NetBox", vip, prefix, l2))
		}
	} else {
		var ok bool
		if vip, ok = pickVIP(prefix, l2, gw, taken, ranges); !ok {
			return netbox.Object{}, retryLater(v1alpha1.ReasonExhausted, fmt.Errorf("no free address for the VIP in %s", prefix))
		}
	}
	beforeVIPCreate()
	nwRef := refOf("NetBoxNetwork", nw)
	ip, err := r.NetBox.Create(ctx, netbox.IPAddresses, r.Owner.Managed(ref, nw.Spec.ClusterName, "capi: VIP for "+nwRef, map[string]any{
		"address": netip.PrefixFrom(vip, l2.Bits()).String(), "role": "vip", "status": "active",
		"dns_name": dnsNameOf(nw), "description": nw.Spec.ClusterName + " control plane endpoint",
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
		msg := fmt.Sprintf("capi: VIP of %s duplicates NetBox address #%d", refOf("NetBoxNetwork", nw), o.ID)
		if err := r.NetBox.Delete(ctx, netbox.IPAddresses, ip.ID, "", msg); err != nil {
			return err
		}
		log.Info("Deleted the control plane VIP, an older NetBox address holds it", "network", klog.KObj(nw), "address", ip.Address, "other", o.ID)
		normalEvent(r.Recorder, nw, "VIPDeleted", "Deleted the control plane VIP %s: NetBox address #%d holds it too", ip.Address, o.ID)
		return retryLater(v1alpha1.ReasonEndpointConflict, fmt.Errorf("%s is also held by NetBox address #%d", p.Addr(), o.ID))
	}
	return nil
}

// occupied returns the global-VRF addresses NetBox holds inside prefix and the IP ranges inside l2.
// NetBox's parent filter finds a range only when it lies wholly inside the parent, so a range that
// reaches past prefix is found through l2.
func (r *NetBoxNetworkReconciler) occupied(ctx context.Context, prefix, l2 netip.Prefix) ([]netip.Addr, [][2]netip.Addr, error) {
	ips, err := r.NetBox.List(ctx, netbox.IPAddresses, url.Values{"parent": {prefix.Masked().String()}, "vrf_id": {"null"}})
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
	// Ranges that cross the boundary of l2 are not seen.
	objs, err := r.NetBox.List(ctx, netbox.IPRanges, url.Values{"parent": {l2.String()}})
	if err != nil {
		return nil, nil, err
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

// releaseVIP lets go of the VIP of nw: deleted under deletionPolicy Delete, otherwise deprecated.
func (r *NetBoxNetworkReconciler) releaseVIP(ctx context.Context, nw *v1alpha1.NetBoxNetwork) error {
	found, err := r.NetBox.FindByRef(ctx, netbox.IPAddresses, r.Owner, vipRef(nw))
	if err != nil {
		return err
	}
	released := "capi: released by " + refOf("NetBoxNetwork", nw)
	for _, ip := range found {
		if nw.Spec.DeletionPolicy == v1alpha1.DeletionPolicyDelete {
			err = r.NetBox.Delete(ctx, netbox.IPAddresses, ip.ID, "", released)
		} else {
			_, err = r.NetBox.Patch(ctx, netbox.IPAddresses, ip.ID, "", map[string]any{"status": "deprecated", "changelog_message": released})
		}
		if errors.Is(err, netbox.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		ctrl.LoggerFrom(ctx).Info("Released the control plane VIP", "network", klog.KObj(nw), "address", ip.Address, "policy", nw.Spec.DeletionPolicy)
		normalEvent(r.Recorder, nw, "VIPReleased", "Released the control plane VIP %s (deletionPolicy %s)", ip.Address, nw.Spec.DeletionPolicy)
	}
	return nil
}
