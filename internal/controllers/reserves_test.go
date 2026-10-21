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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

// metallb is the address group metallb of count addresses, named by index.
func metallb(count int32) v1alpha1.AddressGroup {
	return v1alpha1.AddressGroup{Name: "metallb", Count: count, DNSName: "{{ .Cluster }}-metallb-{{ .Index }}.example.test"}
}

// reserveNetwork is layoutNetwork with a VIP and metallb 2.
func reserveNetwork(ns, name string) *v1alpha1.NetBoxNetwork {
	nw := layoutNetwork(ns, name)
	nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{}
	nw.Spec.Addresses = []v1alpha1.AddressGroup{metallb(2)}
	return nw
}

func setGroups(g *WithT, nw *v1alpha1.NetBoxNetwork, groups ...v1alpha1.AddressGroup) {
	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) { s.Addresses = groups })
}

// reservesOf are the NetBox addresses of group in nw by index, retired ones left out.
func reservesOf(g Gomega, nw *v1alpha1.NetBoxNetwork, group string) map[int]netbox.Object {
	prefix := keys.Key("NetBoxNetwork", nw, "addresses", group, "")
	found, err := admin.FindByRefPrefix(ctx, keys, netbox.IPAddresses, prefix)
	g.Expect(err).NotTo(HaveOccurred())
	out := map[int]netbox.Object{}
	for _, o := range found {
		if strings.HasSuffix(keys.RefOf(o), "/retired") {
			continue
		}
		i, err := strconv.Atoi(strings.TrimPrefix(keys.RefOf(o), prefix))
		g.Expect(err).NotTo(HaveOccurred())
		out[i] = o
	}
	return out
}

// reserveAddrs are the addresses of the reserves of group in nw from index 1, with the mask.
func reserveAddrs(g Gomega, nw *v1alpha1.NetBoxNetwork, group string) []string {
	res := reservesOf(g, nw, group)
	out := make([]string, len(res))
	for i := range out {
		out[i] = res[i+1].Address
	}
	return out
}

// at are the addresses at offsets of p with the mask of the L2 network.
func at(p netip.Prefix, offsets ...int) []string {
	out := make([]string, 0, len(offsets))
	for _, o := range offsets {
		out = append(out, addr(p, o)+"/22")
	}
	return out
}

func TestReservesPlacedUnderVIP(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := reserveNetwork(ns, "default")
	p := readyLayout(g, nw)
	g.Expect(nw.Status.ControlPlaneEndpoint.Host).To(Equal(addr(p, 15)))
	g.Expect(spans(rangesIn(g, nw, "cp"))).To(Equal([]string{span(p, 0, 2)}))
	g.Expect(spans(rangesIn(g, nw, "workers"))).To(Equal([]string{span(p, 3, 12)}))
	g.Expect(nw.Status.Layout.Addresses).To(Equal([]v1alpha1.AddressGroupLayout{{Name: "metallb", Count: 2, Addresses: []string{addr(p, 14), addr(p, 13)}}}))

	res := reservesOf(g, nw, "metallb")
	g.Expect(res).To(HaveLen(2))
	for i, offset := range map[int]int{1: 14, 2: 13} {
		ip := res[i]
		g.Expect(keys.RefOf(ip)).To(Equal(refOf("NetBoxNetwork", ns, "default", "addresses", "metallb", strconv.Itoa(i))))
		g.Expect(ip.Address).To(Equal(addr(p, offset) + "/22"))
		g.Expect(ip.Status.Value).To(Equal("active"))
		g.Expect(ip.Role.Value).To(BeEmpty())
		g.Expect(ip.DNSName).To(Equal(fmt.Sprintf("%s-metallb-%d.example.test", ns, i)))
		g.Expect(texts{Description: ip.Description, Comments: ip.Comments}).To(Equal(texts{fmt.Sprintf("%s/default // %s metallb %d", ns, ns, i), managedComments}))
	}
	waitEvent(g, ns, "default", "ReserveCreated")

	// Steady state writes nothing.
	touch(g, nw)
	g.Consistently(func(g Gomega) {
		for i, ip := range reservesOf(g, nw, "metallb") {
			g.Expect(ip.LastUpdated).To(Equal(res[i].LastUpdated))
		}
	}, "5s").Should(Succeed())
}

// NetBox addresses without a key in the tail, as a running cluster has them for MetalLB, are adopted by a group placed
// for the first time, from the top down: the same objects get the key, dns_name and description of the group.
func TestReservesAdoptKeylessTail(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	p := netip.MustParsePrefix("10.10.3.0/28")
	byHandAt := map[int]netbox.Object{}
	for _, offset := range []int{14, 13} {
		byHandAt[offset] = byHand(t, g, netbox.IPAddresses, map[string]any{
			"address": addr(p, offset) + "/22", "status": "active", "dns_name": "by-hand.example.test", "description": "by hand",
		})
	}
	nw := reserveNetwork(ns, "default")
	nw.Spec.PrefixLength, nw.Spec.Prefix = 0, p.String()
	readyLayout(g, nw)
	g.Expect(nw.Status.ControlPlaneEndpoint.Host).To(Equal(addr(p, 15)))
	g.Expect(spans(rangesIn(g, nw, "workers"))).To(Equal([]string{span(p, 3, 12)}))
	g.Expect(nw.Status.Layout.Addresses[0].Addresses).To(Equal([]string{addr(p, 14), addr(p, 13)}))
	g.Eventually(func(g Gomega) {
		res := reservesOf(g, nw, "metallb")
		g.Expect(res).To(HaveLen(2))
		for i, offset := range map[int]int{1: 14, 2: 13} {
			g.Expect(res[i].ID).To(Equal(byHandAt[offset].ID))
			g.Expect(res[i].Address).To(Equal(addr(p, offset) + "/22"))
			g.Expect(res[i].DNSName).To(Equal(fmt.Sprintf("%s-metallb-%d.example.test", ns, i)))
			g.Expect(res[i].Description).To(Equal(fmt.Sprintf("%s/default // %s metallb %d", ns, ns, i)))
		}
	}).Should(Succeed())
	waitEvent(g, ns, "default", "ReserveAdopted")
}

// The keyless object at the Cluster's endpoint is never adopted by a group, even on a network without a VIP: groups
// adopt the keyless objects under it, and it stays as it is.
func TestReservesAdoptKeepOffClusterEndpoint(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	p := netip.MustParsePrefix("10.10.3.0/28")
	setClusterEndpoint(t, ns, addr(p, 15), 6443)
	byHandAt := map[int]netbox.Object{}
	for _, offset := range []int{15, 14, 13} {
		byHandAt[offset] = byHand(t, g, netbox.IPAddresses, map[string]any{"address": addr(p, offset) + "/22", "status": "active", "description": "by hand"})
	}
	nw := layoutNetwork(ns, "default")
	nw.Spec.PrefixLength, nw.Spec.Prefix = 0, p.String()
	nw.Spec.Addresses = []v1alpha1.AddressGroup{metallb(2)}
	readyLayout(g, nw)
	g.Expect(nw.Status.Layout.Addresses[0].Addresses).To(Equal([]string{addr(p, 14), addr(p, 13)}))
	g.Expect(spans(rangesIn(g, nw, "workers"))).To(Equal([]string{span(p, 3, 12)}))
	g.Eventually(func(g Gomega) {
		res := reservesOf(g, nw, "metallb")
		g.Expect(res).To(HaveLen(2))
		g.Expect(res[1].ID).To(Equal(byHandAt[14].ID))
		g.Expect(res[2].ID).To(Equal(byHandAt[13].ID))
	}).Should(Succeed())
	ep, _, err := admin.Get(ctx, netbox.IPAddresses, byHandAt[15].ID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(keys.RefOf(ep)).To(BeEmpty())
	g.Expect(ep.Description).To(Equal("by hand"))
	g.Expect(ep.LastUpdated).To(Equal(byHandAt[15].LastUpdated))
}

func TestReserveCountUpDown(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := reserveNetwork(ns, "default")
	p := readyLayout(g, nw)
	workers := rangesIn(g, nw, "workers")[0].ID

	setGroups(g, nw, metallb(3))
	waitLayoutReady(g, nw)
	g.Expect(rangesIn(g, nw, "workers")).To(Equal([]v1alpha1.LayoutRange{{Start: addr(p, 3), End: addr(p, 11), ID: workers}}))
	g.Expect(reserveAddrs(g, nw, "metallb")).To(Equal(at(p, 14, 13, 12)))
	third := reservesOf(g, nw, "metallb")[3].ID

	g.Expect(nw.Spec.DeletionPolicy).To(Equal(v1alpha1.DeletionPolicyRetain))
	setGroups(g, nw, metallb(2))
	waitLayoutReady(g, nw)
	g.Expect(rangesIn(g, nw, "workers")).To(Equal([]v1alpha1.LayoutRange{{Start: addr(p, 3), End: addr(p, 12), ID: workers}}))
	g.Expect(reserveAddrs(g, nw, "metallb")).To(Equal(at(p, 14, 13)))
	_, _, err := admin.Get(ctx, netbox.IPAddresses, third)
	g.Expect(err).To(MatchError(netbox.ErrNotFound))
}

// An object put by hand inside a placed pool does not move the pools before the group that changes.
func TestReserveCountKeepsPlacedPools(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := reserveNetwork(ns, "default")
	p := readyLayout(g, nw)
	cp := rangesIn(g, nw, "cp")
	byHand(t, g, netbox.IPAddresses, map[string]any{"address": addr(p, 1) + "/22", "status": "active"})

	setGroups(g, nw, metallb(3))
	waitLayoutReady(g, nw)
	g.Expect(rangesIn(g, nw, "cp")).To(Equal(cp))
	g.Expect(nw.Status.Layout.Pools[0].Ranges).To(Equal(cp))
	g.Expect(spans(rangesIn(g, nw, "workers"))).To(Equal([]string{span(p, 3, 11)}))
	g.Expect(reserveAddrs(g, nw, "metallb")).To(Equal(at(p, 14, 13, 12)))
}

// A VIP added to a live network whose rest reaches the top takes the top: rest gives up its last address.
func TestVIPAddedToLiveNetwork(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	p := readyLayout(g, nw)
	workers := rangesIn(g, nw, "workers")
	g.Expect(spans(workers)).To(Equal([]string{span(p, 3, 15)}))

	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) { s.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{} })
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	waitLayoutReady(g, nw)
	// The patch helper writes conditions before the rest of the status.
	eventually(g, nw, func(g Gomega) { g.Expect(nw.Status.ControlPlaneEndpoint.Host).To(Equal(addr(p, 15))) })
	g.Expect(rangesIn(g, nw, "workers")).To(Equal([]v1alpha1.LayoutRange{{Start: addr(p, 3), End: addr(p, 14), ID: workers[0].ID}}))
	g.Expect(spans(rangesIn(g, nw, "cp"))).To(Equal([]string{span(p, 0, 2)}))
}

// Reserves placed before the VIP keep the top: the VIP takes the highest free address under them.
func TestVIPAddedUnderKeptReserves(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	nw.Spec.Addresses = []v1alpha1.AddressGroup{metallb(2)}
	p := readyLayout(g, nw)
	workers := rangesIn(g, nw, "workers")
	g.Expect(spans(workers)).To(Equal([]string{span(p, 3, 13)}))
	before := reservesOf(g, nw, "metallb")
	g.Expect(reserveAddrs(g, nw, "metallb")).To(Equal(at(p, 15, 14)))

	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) { s.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{} })
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	waitLayoutReady(g, nw)
	// The patch helper writes conditions before the rest of the status.
	eventually(g, nw, func(g Gomega) { g.Expect(nw.Status.ControlPlaneEndpoint.Host).To(Equal(addr(p, 13))) })
	g.Expect(rangesIn(g, nw, "workers")).To(Equal([]v1alpha1.LayoutRange{{Start: addr(p, 3), End: addr(p, 12), ID: workers[0].ID}}))
	after := reservesOf(g, nw, "metallb")
	for i := range before {
		g.Expect(after[i].ID).To(Equal(before[i].ID))
		g.Expect(after[i].Address).To(Equal(before[i].Address))
	}
}

func TestReserveRejectedWithAllocated(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := reserveNetwork(ns, "default")
	p := readyLayout(g, nw)
	before := nw.Status.Layout
	g.Expect(k8s.Create(ctx, &ipamv1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "m1"},
		Spec: ipamv1.IPAddressSpec{
			Address: addr(p, 12), Prefix: ptr.To(int32(22)), Gateway: "10.10.0.1", ClaimRef: ipamv1.IPAddressClaimReference{Name: "m1"},
			PoolRef: ipamv1.IPPoolReference{APIGroup: v1alpha1.GroupVersion.Group, Kind: v1alpha1.NetBoxIPPoolKind, Name: "workers"},
		},
	})).To(Succeed())

	setGroups(g, nw, metallb(3))
	waitTypedCondition(g, nw, v1alpha1.LayoutReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonRangeInUse)
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.LayoutReadyCondition).Message).To(ContainSubstring(addr(p, 12)))
	g.Expect(nw.Status.Layout).To(Equal(before))
	g.Expect(reservesOf(g, nw, "metallb")).To(HaveLen(2))
	g.Expect(spans(rangesIn(g, nw, "workers"))).To(Equal([]string{span(p, 3, 12)}))
}

// dns_name and the texts follow the templates of the group, and come back after an edit by hand.
func TestReserveTextsSynced(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := reserveNetwork(ns, "default")
	readyLayout(g, nw)
	first := reservesOf(g, nw, "metallb")[1]

	setGroups(g, nw, v1alpha1.AddressGroup{Name: "metallb", Count: 2, DNSName: "{{ .Cluster }}-lb-{{ .Index }}.example.test", Description: "lb {{ .Index }}"})
	synced := func(g Gomega) {
		res := reservesOf(g, nw, "metallb")
		for i := 1; i <= 2; i++ {
			g.Expect(res[i].DNSName).To(Equal(fmt.Sprintf("%s-lb-%d.example.test", ns, i)))
			g.Expect(res[i].Description).To(Equal(fmt.Sprintf("lb %d", i)))
		}
		g.Expect(res[1].ID).To(Equal(first.ID))
	}
	g.Eventually(synced).Should(Succeed())

	_, err := admin.Patch(ctx, netbox.IPAddresses, first.ID, "", map[string]any{"dns_name": "by-hand.example.test", "description": "by hand"})
	g.Expect(err).NotTo(HaveOccurred())
	touch(g, nw)
	g.Eventually(synced).Should(Succeed())
}

func TestReservesFollowDeletionPolicy(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	for _, policy := range []v1alpha1.DeletionPolicy{v1alpha1.DeletionPolicyRetain, v1alpha1.DeletionPolicyDelete} {
		name := strings.ToLower(string(policy))
		nw := layoutNetwork(ns, name)
		nw.Spec.DeletionPolicy = policy
		nw.Spec.Addresses = []v1alpha1.AddressGroup{metallb(2)}
		readyLayout(g, nw)
		res := reservesOf(g, nw, "metallb")
		g.Expect(res).To(HaveLen(2))

		g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
		g.Eventually(gone(nw)).Should(BeTrue())
		for _, o := range res {
			ip, _, err := admin.Get(ctx, netbox.IPAddresses, o.ID)
			if policy == v1alpha1.DeletionPolicyDelete {
				g.Expect(err).To(MatchError(netbox.ErrNotFound), name)
				continue
			}
			g.Expect(err).NotTo(HaveOccurred(), name)
			g.Expect(ip.Status.Value).To(Equal("deprecated"), name)
		}
		waitEvent(g, ns, name, "ReserveReleased")
	}
}

// A network recreated after Retain takes back the reserves of its name at their addresses.
func TestReservesReclaimedAfterRetain(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := reserveNetwork(ns, "default")
	p := readyLayout(g, nw)
	before := reservesOf(g, nw, "metallb")
	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	g.Eventually(gone(nw)).Should(BeTrue())
	g.Expect(reservesOf(g, nw, "metallb")[1].Status.Value).To(Equal("deprecated"))

	nw = reserveNetwork(ns, "default")
	g.Expect(readyLayout(g, nw)).To(Equal(p))
	g.Expect(nw.Status.Layout.Addresses[0].Addresses).To(Equal([]string{addr(p, 14), addr(p, 13)}))
	after := reservesOf(g, nw, "metallb")
	g.Expect(after).To(HaveLen(2))
	for i, o := range before {
		g.Expect(after[i].ID).To(Equal(o.ID))
		g.Expect(after[i].Address).To(Equal(o.Address))
		g.Expect(after[i].Status.Value).To(Equal("active"))
	}
}

// While the VIP of a network is not reserved yet, the layout keeps the top for it: rest stops below it.
func TestLayoutKeepsTopForVIP(t *testing.T) {
	g := NewWithT(t)
	var writes []string
	r := networkOn(rangeNetBox(t, "", nil, &writes))
	nw := layoutNetwork("ns", "default")
	nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{}
	nw.Status.Prefix = "10.10.0.16/28"
	g.Expect(r.reconcileLayout(ctx, nw, "")).To(Succeed())
	g.Expect(spans(nw.Status.Layout.Pools[1].Ranges)).To(Equal([]string{"10.10.0.19-10.10.0.30"}))
}

// While the VIP is not reserved (its reservation failed in this pass), the keyless object at the Cluster's endpoint is
// not adopted by a group: it is the VIP to be. The group adopts the keyless objects under it.
func TestAdoptKeepsOffClusterEndpoint(t *testing.T) {
	g := NewWithT(t)
	keyless := func(id int, a string) string {
		return fmt.Sprintf(`{"id":%d,"address":"%s/22","status":{"value":"active"},"custom_fields":{}}`, id, a)
	}
	ips := strings.Join([]string{keyless(1, "10.10.0.29"), keyless(2, "10.10.0.30"), keyless(3, "10.10.0.31")}, ",")
	var writes []string
	r := networkOn(rangeNetBox(t, "", func(int32) string { return ips }, &writes))
	nw := reserveNetwork("ns", "default")
	nw.Status.Prefix = "10.10.0.16/28"
	g.Expect(r.reconcileLayout(ctx, nw, "10.10.0.31")).To(Succeed())
	g.Expect(nw.Status.Layout.Addresses).To(Equal([]v1alpha1.AddressGroupLayout{{Name: "metallb", Count: 2, Addresses: []string{"10.10.0.30", "10.10.0.29"}}}))
	g.Expect(spans(nw.Status.Layout.Pools[1].Ranges)).To(Equal([]string{"10.10.0.19-10.10.0.27"}), "the VIP keeps 10.10.0.28")
}

// A reserve placed in the status but not created yet is not free for a new VIP.
func TestVIPKeepsOffPlannedReserves(t *testing.T) {
	g := NewWithT(t)
	var created []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			_, _ = io.WriteString(w, `{"results":[]}`)
			return
		}
		var body struct{ Address string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		created = append(created, body.Address)
		_, _ = fmt.Fprintf(w, `{"id":8,"address":%q}`, body.Address)
	}))
	t.Cleanup(srv.Close)
	r := networkOn(srv)
	nw := vipNetwork("ns", "default")
	nw.Status.Prefix = "10.10.0.16/28"
	nw.Status.Layout.Addresses = []v1alpha1.AddressGroupLayout{{Name: "metallb", Count: 2, Addresses: []string{"10.10.0.31", "10.10.0.30"}}}
	_, err := r.createVIP(ctx, nw, netip.MustParsePrefix("10.10.0.16/28"), netip.Addr{})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created).To(Equal([]string{"10.10.0.29/22"}))

	_, err = r.createVIP(ctx, nw, netip.MustParsePrefix("10.10.0.16/28"), netip.MustParseAddr("10.10.0.30"))
	g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonEndpointConflict))
	g.Expect(created).To(HaveLen(1))

	// Nor is a range placed for a pool.
	nw.Status.Layout.Pools = []v1alpha1.PoolLayout{{Name: "workers", Size: intstr.FromString("rest"), Ranges: []v1alpha1.LayoutRange{{Start: "10.10.0.16", End: "10.10.0.29"}}}}
	_, err = r.createVIP(ctx, nw, netip.MustParsePrefix("10.10.0.16/28"), netip.Addr{})
	g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonExhausted))
	g.Expect(created).To(HaveLen(1))
}

// A group is not placed on an allocated address outside the pools (an IPAddress of another pool): RangeInUse with the
// address, and nothing is written.
func TestReserveRefusesAllocatedAddress(t *testing.T) {
	g := NewWithT(t)
	var writes []string
	r := networkOn(rangeNetBox(t, "", nil, &writes))
	r.Client = claimClient(inClusterAddress("ns", "m1", "other", "10.10.0.31"))
	nw := layoutNetwork("ns", "default")
	nw.Spec.Addresses = []v1alpha1.AddressGroup{metallb(2)}
	nw.Status.Prefix = "10.10.0.16/28"
	err := r.reconcileLayout(ctx, nw, "")
	g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonRangeInUse))
	g.Expect(err).To(MatchError(ContainSubstring("10.10.0.31")))
	g.Expect(writes).To(BeEmpty())
	g.Expect(nw.Status.Layout.Addresses).To(BeEmpty())
}

// Nor is a new VIP.
func TestVIPKeepsOffAllocated(t *testing.T) {
	g := NewWithT(t)
	var created []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			_, _ = io.WriteString(w, `{"results":[]}`)
			return
		}
		var body struct{ Address string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		created = append(created, body.Address)
		_, _ = fmt.Fprintf(w, `{"id":8,"address":%q}`, body.Address)
	}))
	t.Cleanup(srv.Close)
	r := networkOn(srv)
	r.Client = claimClient(inClusterAddress("ns", "m1", "other", "10.10.0.31"))
	nw := vipNetwork("ns", "default")
	nw.Status.Prefix = "10.10.0.16/28"
	_, err := r.createVIP(ctx, nw, netip.MustParsePrefix("10.10.0.16/28"), netip.Addr{})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created).To(Equal([]string{"10.10.0.30/22"}))
}

// After a status loss, while the VIP is not reserved again, the layout keeps off the VIP NetBox has under its key:
// a VIP below the top stays outside the restored ranges and nothing is written.
func TestLayoutKeepsOffFoundVIP(t *testing.T) {
	g := NewWithT(t)
	rng := func(id int, pool, from, to string) string {
		return fmt.Sprintf(`{"id":%d,"start_address":"%s/22","end_address":"%s/22","status":{"value":"active"},%s,"custom_fields":{"capi_ref":"NetBoxNetwork/ns/default/pool/%s"}}`,
			id, from, to, textsOf("ns/"+pool+" // pool of cluster ns"), pool)
	}
	ranges := strings.Join([]string{rng(1, "cp", "10.10.0.16", "10.10.0.18"), rng(2, "workers", "10.10.0.19", "10.10.0.25"), rng(3, "workers", "10.10.0.27", "10.10.0.31")}, ",")
	vip := `{"id":9,"address":"10.10.0.26/22","status":{"value":"active"},"custom_fields":{"capi_ref":"NetBoxNetwork/ns/default/vip"}}`
	var writes []string
	srv := httptest.NewServer(writesNeedLock(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method != http.MethodGet:
			writes = append(writes, r.Method+" "+r.URL.Path)
			_, _ = io.WriteString(w, `{"id":8}`)
		case r.URL.Path == "/api/ipam/ip-ranges/":
			_, _ = io.WriteString(w, `{"results":[`+ranges+`]}`)
		case r.URL.Path == "/api/ipam/ip-addresses/":
			_, _ = io.WriteString(w, `{"results":[`+vip+`]}`)
		default:
			_, _ = io.WriteString(w, `{"results":[]}`)
		}
	})))
	t.Cleanup(srv.Close)
	r := networkOn(srv)
	nw := layoutNetwork("ns", "default")
	nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{}
	nw.Status.Prefix = "10.10.0.16/28"
	g.Expect(r.reconcileLayout(ctx, nw, "")).To(Succeed())
	g.Expect(writes).To(BeEmpty())
	g.Expect(spans(nw.Status.Layout.Pools[1].Ranges)).To(Equal([]string{"10.10.0.19-10.10.0.25", "10.10.0.27-10.10.0.31"}))
}

// A group template that does not render fails Ready before anything reaches NetBox.
func TestReserveTemplateInvalidSpec(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := reserveNetwork(ns, "default")
	nw.Spec.Addresses[0].DNSName = "{{ .Group }} {{ .Index }}"
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec)
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.ReadyCondition).Message).To(ContainSubstring("addresses metallb"))
	g.Consistently(func(g Gomega) {
		for _, path := range []string{netbox.Prefixes, netbox.IPRanges, netbox.IPAddresses} {
			g.Expect(ofNamespace(g, path, ns)).To(BeEmpty(), path)
		}
	}).Should(Succeed())
}

// A reserve a deleted network retained outside the prefix of a new network of its name is retired, not moved: it
// keeps its address under <key>/retired, a new one is reserved in the new prefix, and nothing deletes the retired one.
func TestReservesRetiredOutsidePrefix(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	nw.Spec.Addresses = []v1alpha1.AddressGroup{metallb(2)}
	p := readyLayout(g, nw)
	before := reservesOf(g, nw, "metallb")
	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	g.Eventually(gone(nw)).Should(BeTrue())

	nw = layoutNetwork(ns, "default")
	nw.Spec.PrefixLength, nw.Spec.DeletionPolicy = 27, v1alpha1.DeletionPolicyDelete
	nw.Spec.Addresses = []v1alpha1.AddressGroup{metallb(2)}
	p2 := readyLayout(g, nw)
	g.Expect(p2.Overlaps(p)).To(BeFalse())
	g.Expect(reserveAddrs(g, nw, "metallb")).To(Equal(at(p2, 31, 30)))
	retired := func(g Gomega) {
		for i, o := range before {
			ip, _, err := admin.Get(ctx, netbox.IPAddresses, o.ID)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(keys.RefOf(ip)).To(Equal(reserveRef(keys, nw, "metallb", i) + "/retired"))
			g.Expect(ip.Address).To(Equal(o.Address))
			g.Expect(ip.Status.Value).To(Equal("deprecated"))
		}
	}
	retired(g)
	waitEvent(g, ns, "default", "ReserveRetired")

	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	g.Eventually(gone(nw)).Should(BeTrue())
	g.Expect(reservesOf(g, nw, "metallb")).To(BeEmpty())
	retired(g)
}
