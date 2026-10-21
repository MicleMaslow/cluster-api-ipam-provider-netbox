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
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/layout"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

// rangesIn are the NetBox ranges of pool in nw, ascending, as the layout status has them.
func rangesIn(g Gomega, nw *v1alpha1.NetBoxNetwork, pool string) []v1alpha1.LayoutRange {
	found, err := findRef(netbox.IPRanges, poolRangeRef(keys, nw, pool))
	g.Expect(err).NotTo(HaveOccurred())
	out := make([]v1alpha1.LayoutRange, 0, len(found))
	for _, o := range found {
		from, to, err := netBoxBounds(o)
		g.Expect(err).NotTo(HaveOccurred())
		out = append(out, v1alpha1.LayoutRange{Start: from.String(), End: to.String(), ID: o.ID})
	}
	slices.SortFunc(out, func(a, b v1alpha1.LayoutRange) int {
		return netip.MustParseAddr(a.Start).Compare(netip.MustParseAddr(b.Start))
	})
	return out
}

// span is "start-end" of the offsets from and to of p.
func span(p netip.Prefix, from, to int) string { return addr(p, from) + "-" + addr(p, to) }

// spans are the "start-end" of rs.
func spans(rs []v1alpha1.LayoutRange) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Start+"-"+r.End)
	}
	return out
}

func setPools(g *WithT, nw *v1alpha1.NetBoxNetwork, pools ...v1alpha1.NetworkPool) {
	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) { s.Pools = pools })
}

// waitLayoutReady waits for LayoutReady=True at the generation of nw, with every pool and group of the spec placed
// and the pools in NetBox.
func waitLayoutReady(g *WithT, nw *v1alpha1.NetBoxNetwork) {
	eventually(g, nw, func(g Gomega) {
		c := meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.LayoutReadyCondition)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Status).To(Equal(metav1.ConditionTrue), c.Message)
		g.Expect(c.ObservedGeneration).To(Equal(nw.Generation))
		g.Expect(nw.Status.Layout.Pools).To(HaveLen(len(nw.Spec.Pools)))
		for i, p := range nw.Status.Layout.Pools {
			g.Expect(p.Name).To(Equal(nw.Spec.Pools[i].Name))
			for _, r := range p.Ranges {
				g.Expect(r.ID).NotTo(BeZero(), p.Name)
			}
		}
		g.Expect(nw.Status.Layout.Addresses).To(HaveLen(len(nw.Spec.Addresses)))
		for i, a := range nw.Status.Layout.Addresses {
			g.Expect(a.Name).To(Equal(nw.Spec.Addresses[i].Name))
			g.Expect(a.Addresses).To(HaveLen(int(nw.Spec.Addresses[i].Count)), a.Name)
		}
	})
}

// readyLayout creates nw and waits for its layout; it returns the prefix.
func readyLayout(g *WithT, nw *v1alpha1.NetBoxNetwork) netip.Prefix {
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitLayoutReady(g, nw)
	return netip.MustParsePrefix(nw.Status.Prefix)
}

func TestLayoutNetBoxAllocator(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	p := readyLayout(g, nw)
	cp, workers := rangesIn(g, nw, "cp"), rangesIn(g, nw, "workers")
	g.Expect(spans(cp)).To(Equal([]string{span(p, 0, 2)}))
	g.Expect(spans(workers)).To(Equal([]string{span(p, 3, 15)}))
	g.Expect(nw.Status.Layout.Pools).To(Equal([]v1alpha1.PoolLayout{
		{Name: "cp", Size: intstr.FromInt32(3), Ranges: cp},
		{Name: "workers", Size: intstr.FromString("rest"), Ranges: workers},
	}))
	rng, _, err := admin.Get(ctx, netbox.IPRanges, cp[0].ID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rng.Status.Value).To(Equal("active"))
	g.Expect(rng.MarkPopulated).To(BeFalse())
	g.Expect(texts{Description: rng.Description, Comments: rng.Comments}).To(Equal(texts{ns + "/cp // pool of cluster " + ns, managedComments}))
	g.Expect(keys.RefOf(rng)).To(Equal(refOf("NetBoxNetwork", ns, "default", "pool", "cp")))

	pool := newPool(ns, "control-plane", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "cp"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	g.Expect(pool.Status.Ranges).To(Equal([]v1alpha1.IPPoolRangeStatus{
		{NetworkName: "default", ID: cp[0].ID, StartAddress: addr(p, 0), EndAddress: addr(p, 2), Ready: true},
	}))
	claim := newClaim(ns, "m1", "control-plane")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	g.Expect(address(g, claim).Spec.Address).To(Equal(addr(p, 0)))
}

func TestLayoutRestGrowsAndShrinks(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	p := readyLayout(g, nw)
	workers := rangesIn(g, nw, "workers")[0].ID

	setPools(g, nw, netPool("cp", 3), netPool("workers", 0), netPool("extra", 2))
	waitLayoutReady(g, nw)
	g.Expect(rangesIn(g, nw, "workers")).To(Equal([]v1alpha1.LayoutRange{{Start: addr(p, 3), End: addr(p, 13), ID: workers}}))
	extra := rangesIn(g, nw, "extra")
	g.Expect(spans(extra)).To(Equal([]string{span(p, 14, 15)}))

	setPools(g, nw, netPool("cp", 3), netPool("workers", 0))
	waitLayoutReady(g, nw)
	g.Expect(rangesIn(g, nw, "workers")).To(Equal([]v1alpha1.LayoutRange{{Start: addr(p, 3), End: addr(p, 15), ID: workers}}))
	g.Expect(rangesIn(g, nw, "extra")).To(BeEmpty())
	_, _, err := admin.Get(ctx, netbox.IPRanges, extra[0].ID)
	g.Expect(err).To(MatchError(netbox.ErrNotFound))
}

func TestLayoutChangeRejectedWithAllocated(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	p := readyLayout(g, nw)
	before := nw.Status.Layout
	pool := newPool(ns, "workers", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "workers"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	m1 := newClaim(ns, "m1", "workers")
	g.Expect(k8s.Create(ctx, m1)).To(Succeed())
	g.Expect(address(g, m1).Spec.Address).To(Equal(addr(p, 3)))

	setPools(g, nw, netPool("cp", 4), netPool("workers", 0))
	waitTypedCondition(g, nw, v1alpha1.LayoutReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonRangeInUse)
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.LayoutReadyCondition).Message).To(ContainSubstring(addr(p, 3)))
	g.Expect(nw.Status.Layout).To(Equal(before))
	g.Expect(spans(rangesIn(g, nw, "cp"))).To(Equal([]string{span(p, 0, 2)}))
	g.Expect(spans(rangesIn(g, nw, "workers"))).To(Equal([]string{span(p, 3, 15)}))

	setPools(g, nw, netPool("cp", 3), netPool("workers", 0))
	waitLayoutReady(g, nw)
	g.Expect(nw.Status.Layout).To(Equal(before))
}

// An address NetBox gave to a claim counts as allocated before its IPAddress exists.
func TestLayoutChangeRejectedForNetBoxAllocation(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	p := readyLayout(g, nw)
	byHand(t, g, netbox.IPAddresses, keys.Managed(netbox.Attrs{}, refOf("IPAddressClaim", ns, "ghost"), "test: allocated",
		map[string]any{"address": addr(p, 15) + "/22", "status": "active"}))

	setPools(g, nw, netPool("cp", 3), netPool("workers", 0), netPool("extra", 1))
	waitTypedCondition(g, nw, v1alpha1.LayoutReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonRangeInUse)
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.LayoutReadyCondition).Message).To(ContainSubstring(addr(p, 15)))
	g.Expect(rangesIn(g, nw, "extra")).To(BeEmpty())
}

func TestLayoutInvalidSpec(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	nw.Spec.Pools[0] = netPool("cp", 20)
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitTypedCondition(g, nw, v1alpha1.LayoutReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec)
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.LayoutReadyCondition).Message).To(ContainSubstring("cp 20"))
	waitReady(g, nw)
	g.Consistently(func(g Gomega) { g.Expect(ofNamespace(g, netbox.IPRanges, ns)).To(BeEmpty()) }).Should(Succeed())
}

// A NetBox address inside the span of a pool splits its range around it.
func TestLayoutTakenAddressSplitsRange(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.LayoutReadyCondition)).To(BeNil())
	p := netip.MustParsePrefix(nw.Status.Prefix)
	byHand(t, g, netbox.IPAddresses, map[string]any{"address": addr(p, 1) + "/22", "status": "active"})

	setPools(g, nw, netPool("cp", 3), netPool("workers", 0))
	waitLayoutReady(g, nw)
	cp := rangesIn(g, nw, "cp")
	g.Expect(spans(cp)).To(Equal([]string{span(p, 0, 0), span(p, 2, 3)}))
	g.Expect(spans(rangesIn(g, nw, "workers"))).To(Equal([]string{span(p, 4, 15)}))
	g.Expect(nw.Status.Layout.Pools[0].Ranges).To(Equal(cp))
}

// A network that lost its status, as after clusterctl move, takes its layout back from its NetBox ranges.
func TestLayoutRestoredAfterStatusLoss(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	readyLayout(g, nw)
	cp, workers := rangesIn(g, nw, "cp"), rangesIn(g, nw, "workers")

	forget(g, nw)
	nw = layoutNetwork(ns, "default")
	readyLayout(g, nw)
	g.Expect(nw.Status.Layout.Pools[0].Ranges).To(Equal(cp))
	g.Expect(nw.Status.Layout.Pools[1].Ranges).To(Equal(workers))
	g.Expect(ofNamespace(g, netbox.IPRanges, ns)).To(HaveLen(2))

	forget(g, nw)
	nw = layoutNetwork(ns, "default")
	nw.Spec.Pools[0] = netPool("cp", 4)
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitTypedCondition(g, nw, v1alpha1.LayoutReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec)
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.LayoutReadyCondition).Message).To(ContainSubstring("cp 3"))
	g.Expect(rangesIn(g, nw, "cp")).To(Equal(cp))
	g.Expect(rangesIn(g, nw, "workers")).To(Equal(workers))
}

// The ranges a deleted network retained give way to the layout of a new network of its name.
func TestLayoutRetainedRangesReplaced(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	readyLayout(g, nw)
	old := append(rangesIn(g, nw, "cp"), rangesIn(g, nw, "workers")...)
	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	g.Eventually(gone(nw)).Should(BeTrue())
	for _, r := range old {
		rng, _, err := admin.Get(ctx, netbox.IPRanges, r.ID)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(rng.Status.Value).To(Equal("deprecated"))
		g.Expect(rng.MarkPopulated).To(BeTrue())
	}

	nw = layoutNetwork(ns, "default")
	nw.Spec.Pools[0] = netPool("cp", 4)
	p := readyLayout(g, nw)
	g.Expect(spans(rangesIn(g, nw, "cp"))).To(Equal([]string{span(p, 0, 3)}))
	g.Expect(spans(rangesIn(g, nw, "workers"))).To(Equal([]string{span(p, 4, 15)}))
	for _, r := range old {
		_, _, err := admin.Get(ctx, netbox.IPRanges, r.ID)
		g.Expect(err).To(MatchError(netbox.ErrNotFound))
	}
}

func TestNetworkDeletionReleasesRanges(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	nw.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete
	readyLayout(g, nw)
	g.Expect(ofNamespace(g, netbox.IPRanges, ns)).To(HaveLen(2))

	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	g.Eventually(gone(nw)).Should(BeTrue())
	g.Expect(ofNamespace(g, netbox.IPRanges, ns)).To(BeEmpty())
	waitEvent(g, ns, "default", "RangeReleased")
}

func TestPoolWaitsForLayoutPool(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	readyLayout(g, nw)
	pool := newPool(ns, "extra", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "missing"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonNetworkNotReady)
	g.Expect(meta.FindStatusCondition(pool.Status.Conditions, v1alpha1.RangesReadyCondition).Message).To(ContainSubstring("pool missing is not placed in network default"))

	setPools(g, nw, netPool("cp", 3), netPool("workers", 0), netPool("missing", 2))
	waitReady(g, pool)
}

func TestPoolDuplicateLayoutPool(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	readyLayout(g, layoutNetwork(ns, "default"))
	first := newPool(ns, "first", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "cp"})
	g.Expect(k8s.Create(ctx, first)).To(Succeed())
	waitReady(g, first)
	second := newPool(ns, "second", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "cp"})
	g.Expect(k8s.Create(ctx, second)).To(Succeed())
	waitCondition(g, second, metav1.ConditionFalse, v1alpha1.ReasonDuplicate)
	g.Expect(meta.FindStatusCondition(second.Status.Conditions, v1alpha1.ReadyCondition).Message).To(ContainSubstring("first"))
	g.Consistently(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(first), first)).To(Succeed())
		g.Expect(meta.IsStatusConditionTrue(first.Status.Conditions, v1alpha1.ReadyCondition)).To(BeTrue())
	}).Should(Succeed())
}

// rangeObject is the NetBox range 7 of pool workers of network default.
func rangeObject(start, end string, closed bool) string {
	return `{"id":7,"start_address":"` + start + `/22","end_address":"` + end + `/22","mark_populated":` + strconv.FormatBool(closed) +
		`,"status":{"value":"active"},` + textsOf("ns/workers // pool of cluster ns") + `,"custom_fields":{"capi_ref":"NetBoxNetwork/ns/default/pool/workers"}}`
}

// rangeNetBox serves o, if not empty, as the only NetBox range, the claim addresses ips returns on the n-th list of
// addresses, and records the writes.
func rangeNetBox(t *testing.T, o string, ips func(n int32) string, writes *[]string) *httptest.Server {
	t.Helper()
	var lists atomic.Int32
	srv := httptest.NewServer(writesNeedLock(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method != http.MethodGet:
			b, _ := io.ReadAll(r.Body)
			*writes = append(*writes, r.Method+" "+r.URL.Path+" "+string(b))
			_, _ = io.WriteString(w, `{"id":8}`)
		case r.URL.Path == "/api/ipam/ip-ranges/7/":
			w.Header().Set("ETag", `"e1"`)
			_, _ = io.WriteString(w, o)
		case r.URL.Path == "/api/ipam/ip-ranges/" && o != "":
			_, _ = io.WriteString(w, `{"results":[`+o+`]}`)
		case r.URL.Path == "/api/ipam/ip-addresses/" && ips != nil:
			_, _ = io.WriteString(w, `{"results":[`+ips(lists.Add(1))+`]}`)
		default:
			_, _ = io.WriteString(w, `{"results":[]}`)
		}
	})))
	t.Cleanup(srv.Close)
	return srv
}

// workersAt is the layout of network default with pool workers at from-to of 10.10.0.0/22.
func workersAt(from, to string) layout.Layout {
	return layout.Layout{Pools: []layout.PlacedPool{{
		Pool: layout.Pool{Name: "workers", Rest: true}, Ranges: []layout.Span{{From: netip.MustParseAddr(from), To: netip.MustParseAddr(to)}},
	}}}
}

// Only ranges that are to change, or are not there yet, wait for the lock; ranges as they should be do not.
// A new range carries the network's owner and tenant and the default texts; the ids reach the status.
func TestApplyRangesTakesLockToWrite(t *testing.T) {
	g := NewWithT(t)
	for name, tc := range map[string]struct {
		existing      string
		end           string
		owner, tenant int64
		writes        []string
		id            int64
	}{
		"unchanged": {existing: rangeObject("10.10.0.18", "10.10.0.19", false), end: "10.10.0.19", id: 7},
		"resized": {existing: rangeObject("10.10.0.18", "10.10.0.19", false), end: "10.10.0.20", id: 7, writes: []string{
			`PATCH /api/ipam/ip-ranges/7/ {"changelog_message":"capi: resized by NetBoxNetwork/ns/default/pool/workers","end_address":"10.10.0.20/22","mark_populated":false,"start_address":"10.10.0.18/22","status":"active"}`,
		}},
		"attributes": {existing: rangeObject("10.10.0.18", "10.10.0.19", false), end: "10.10.0.19", owner: 1, id: 7, writes: []string{
			`PATCH /api/ipam/ip-ranges/7/ {"changelog_message":"capi: attributes of NetBoxNetwork/ns/default/pool/workers","owner":1}`,
		}},
		"created": {end: "10.10.0.19", owner: 1, tenant: 7, id: 8, writes: []string{
			`POST /api/ipam/ip-ranges/ {"changelog_message":"capi: range NetBoxNetwork/ns/default/pool/workers","comments":"` + managedComments +
				`","custom_fields":{"capi_ref":"NetBoxNetwork/ns/default/pool/workers"},"description":"ns/workers // pool of cluster ns",` +
				`"end_address":"10.10.0.19/22","owner":1,"start_address":"10.10.0.18/22","status":"active","tenant":7}`,
		}},
	} {
		var writes []string
		r := networkOn(rangeNetBox(t, tc.existing, nil, &writes))
		nw := newNetwork("ns", "default")
		nw.Status.Prefix, nw.Status.OwnerID, nw.Status.TenantID = "10.10.0.16/28", tc.owner, tc.tenant
		var err error
		g.Expect(returnsWhileLocked(func() { err = r.applyRanges(ctx, nw, workersAt("10.10.0.18", tc.end)) })).To(Equal(len(tc.writes) == 0), name)
		g.Expect(err).NotTo(HaveOccurred(), name)
		g.Expect(writes).To(Equal(tc.writes), name)
		g.Expect(nw.Status.Layout.Pools).To(Equal([]v1alpha1.PoolLayout{{Name: "workers", Size: intstr.FromString("rest"),
			Ranges: []v1alpha1.LayoutRange{{Start: "10.10.0.18", End: tc.end, ID: tc.id}}}}), name)
	}
}

// A shrink first closes the range to allocation (mark_populated) and checks that no address it gives up is allocated:
// one held by an IPAddress stops it before any write; one NetBox hands out meanwhile (the third list: the read-only
// pass, then before and after the close) opens the range again.
func TestApplyRangesFences(t *testing.T) {
	g := NewWithT(t)
	claimIP := `{"id":30,"address":"10.10.0.19/22","custom_fields":{"capi_ref":"IPAddressClaim/ns/m1"}}`
	held := &ipamv1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "m1"},
		Spec:       ipamv1.IPAddressSpec{Address: "10.10.0.19", PoolRef: ipamv1.IPPoolReference{APIGroup: v1alpha1.GroupVersion.Group, Kind: v1alpha1.NetBoxIPPoolKind, Name: "workers"}},
	}
	const (
		closeIt = `PATCH /api/ipam/ip-ranges/7/ {"changelog_message":"capi: closed to allocation by NetBoxNetwork/ns/default/pool/workers","mark_populated":true}`
		openIt  = `PATCH /api/ipam/ip-ranges/7/ {"changelog_message":"capi: opened again by NetBoxNetwork/ns/default/pool/workers","mark_populated":false}`
		shrink  = `PATCH /api/ipam/ip-ranges/7/ {"changelog_message":"capi: resized by NetBoxNetwork/ns/default/pool/workers","end_address":"10.10.0.18/22","mark_populated":false,"start_address":"10.10.0.18/22","status":"active"}`
	)
	for name, tc := range map[string]struct {
		objs   []client.Object
		ips    func(n int32) string
		reason string
		writes []string
	}{
		"free":                {writes: []string{closeIt, shrink}},
		"held by IPAddress":   {objs: []client.Object{held}, reason: v1alpha1.ReasonRangeInUse},
		"allocated meanwhile": {ips: func(n int32) string { return map[bool]string{true: claimIP}[n > 2] }, reason: v1alpha1.ReasonRangeInUse, writes: []string{closeIt, openIt}},
	} {
		var writes []string
		r := networkOn(rangeNetBox(t, rangeObject("10.10.0.18", "10.10.0.19", false), tc.ips, &writes))
		r.Client = claimClient(tc.objs...)
		nw := newNetwork("ns", "default")
		nw.Status.Prefix = "10.10.0.16/28"
		err := r.applyRanges(ctx, nw, workersAt("10.10.0.18", "10.10.0.18"))
		if tc.reason == "" {
			g.Expect(err).NotTo(HaveOccurred(), name)
		} else {
			g.Expect(errReason(err)).To(Equal(tc.reason), name)
			g.Expect(err).To(MatchError(ContainSubstring("10.10.0.19")), name)
		}
		g.Expect(writes).To(Equal(tc.writes), name)
		g.Expect(strings.Join(writes, "\n")).NotTo(ContainSubstring(`"status":"deprecated"`), name)
	}
}

// Removing every pool while NetBox refuses to delete the ranges keeps the layout in the status: LayoutReady stays on
// and the removal is retried.
func TestApplyRangesKeepsLayoutWhenRemovalFails(t *testing.T) {
	g := NewWithT(t)
	var writes []string
	srv := httptest.NewServer(writesNeedLock(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method != http.MethodGet:
			writes = append(writes, r.Method+" "+r.URL.Path)
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"detail":"x"}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":7}`)
		case r.URL.Path == "/api/ipam/ip-ranges/7/":
			w.Header().Set("ETag", `"e1"`)
			_, _ = io.WriteString(w, rangeObject("10.10.0.18", "10.10.0.19", false))
		case r.URL.Path == "/api/ipam/ip-ranges/":
			_, _ = io.WriteString(w, `{"results":[`+rangeObject("10.10.0.18", "10.10.0.19", false)+`]}`)
		default:
			_, _ = io.WriteString(w, `{"results":[]}`)
		}
	})))
	t.Cleanup(srv.Close)
	r := networkOn(srv)
	nw := newNetwork("ns", "default")
	nw.Status.Prefix = "10.10.0.16/28"
	nw.Status.Layout.Pools = []v1alpha1.PoolLayout{{Name: "workers", Size: intstr.FromString("rest"),
		Ranges: []v1alpha1.LayoutRange{{Start: "10.10.0.18", End: "10.10.0.19", ID: 7}}}}
	before := *nw.Status.DeepCopy()

	err := r.applyRanges(ctx, nw, layout.Layout{})
	g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonForbidden))
	g.Expect(toResult(err)).To(Equal(ctrl.Result{RequeueAfter: netBoxRequeueAfter}))
	g.Expect(writes).To(Equal([]string{"PATCH /api/ipam/ip-ranges/7/", "DELETE /api/ipam/ip-ranges/7/"}))
	g.Expect(nw.Status.Layout).To(Equal(before.Layout))
}

// The same end to end: the ranges may not change (their owner is read-only for the token), every pool is removed.
// LayoutReady stays False with Forbidden, the ranges and status.layout stay; once they may go, they go.
func TestLayoutRemovalForbiddenKeepsLayout(t *testing.T) {
	g := newG(t)
	z := newZone(t, g)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	nw.Spec.Owner = &v1alpha1.OwnerRef{Name: z.ownerName}
	readyLayout(g, nw)
	before := nw.Status.Layout

	moveOwner(g, z.owner, "capi")
	t.Cleanup(func() { moveOwner(g, z.owner, "zones") })
	setPools(g, nw)
	waitTypedCondition(g, nw, v1alpha1.LayoutReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonForbidden)
	g.Consistently(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
		c := meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.LayoutReadyCondition)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Reason).To(Equal(v1alpha1.ReasonForbidden))
		g.Expect(nw.Status.Layout).To(Equal(before))
		g.Expect(ofNamespace(g, netbox.IPRanges, ns)).To(HaveLen(2))
	}).Should(Succeed())

	moveOwner(g, z.owner, "zones")
	touch(g, nw)
	g.Eventually(func(g Gomega) {
		g.Expect(ofNamespace(g, netbox.IPRanges, ns)).To(BeEmpty())
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
		g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.LayoutReadyCondition)).To(BeNil())
		g.Expect(nw.Status.Layout.Pools).To(BeEmpty())
	}).Should(Succeed())
}

// Only addresses above the pools are adopted: above those placed before, else above those Plan would place.
func TestTail(t *testing.T) {
	g := NewWithT(t)
	a := netip.MustParseAddr
	s := layout.Space{Prefix: netip.MustParsePrefix("10.10.0.16/28"), L2: netip.MustParsePrefix("10.10.0.0/22"), Gateway: a("10.10.0.1"), VIP: a("10.10.0.31")}
	for _, k := range []string{"10.10.0.17", "10.10.0.29", "10.10.0.30"} {
		s.Taken = append(s.Taken, layout.Span{From: a(k), To: a(k)})
		s.Adopt = append(s.Adopt, a(k))
	}
	pools := []layout.Pool{{Name: "cp", Size: 3}, {Name: "workers", Rest: true}}
	groups := []layout.Group{{Name: "metallb", Count: 2}}
	tailOf := []netip.Addr{a("10.10.0.29"), a("10.10.0.30")}
	g.Expect(tail(s, pools, groups, layout.Layout{}, "")).To(Equal(tailOf), "above the pools Plan places")
	g.Expect(tail(s, pools, groups, workersAt("10.10.0.16", "10.10.0.18"), "")).To(Equal(tailOf), "above the pools placed")
	g.Expect(tail(s, nil, groups, layout.Layout{}, "")).To(Equal(s.Adopt), "no pools")
	g.Expect(tail(s, pools, groups, layout.Layout{}, "10.10.0.30")).To(Equal(tailOf[:1]), "not the Cluster's endpoint")
}
