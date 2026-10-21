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
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netmath"
)

func newPool(ns, name string, ranges ...v1alpha1.IPPoolRange) *v1alpha1.NetBoxIPPool {
	return &v1alpha1.NetBoxIPPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{clusterv1.ClusterNameLabel: ns}},
		Spec:       v1alpha1.NetBoxIPPoolSpec{Ranges: ranges},
	}
}

func addr(p netip.Prefix, offset int) string {
	a, _ := netmath.AddressAt(p, offset)
	return a.String()
}

func TestL2Of(t *testing.T) {
	g := NewWithT(t)
	nw := &v1alpha1.NetBoxNetwork{Spec: v1alpha1.NetBoxNetworkSpec{ParentPrefix: "10.10.0.0/22", Gateway: "10.10.0.1"}}
	l2, gw, err := l2Of(nw)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(l2.String()).To(Equal("10.10.0.0/22"))
	g.Expect(gw.String()).To(Equal("10.10.0.1"))

	nw.Spec.ParentPrefix, nw.Spec.AddressPrefixLength = "10.10.1.0/24", 22
	l2, _, err = l2Of(nw)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(l2.String()).To(Equal("10.10.0.0/22"))

	nw.Spec.Gateway = "gw"
	_, _, err = l2Of(nw)
	g.Expect(err).To(HaveOccurred())
}

func TestPoolTreatsNetworkWithoutPrefixAsNotReady(t *testing.T) {
	nw := newNetwork("ns", "default")
	nw.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ReadyCondition, Status: metav1.ConditionTrue}}
	r := &NetBoxIPPoolReconciler{Client: claimClient(nw)}

	err := r.reconcileNormal(ctx, newPool("ns", "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "workers"}))
	var re reasonError
	if !errors.As(err, &re) || re.reason != v1alpha1.ReasonNetworkNotReady {
		t.Errorf("got %v, want reason %s", err, v1alpha1.ReasonNetworkNotReady)
	}
}

// TestPoolOfAnotherWatchFilterIsSkipped: the IPAddress watch has no label filter, so it can enqueue such a pool.
func TestPoolOfAnotherWatchFilterIsSkipped(t *testing.T) {
	g := NewWithT(t)
	pool := newPool("ns", "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "workers"})
	pool.Labels = nil
	c := claimClient(pool)
	r := &NetBoxIPPoolReconciler{Client: c, WatchFilterValue: "w"}

	g.Expect(r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})).To(Equal(ctrl.Result{}))
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
	g.Expect(pool.Finalizers).To(BeEmpty())
}

// A pool serves pools of several networks of one L2 network, in spec order, with the ids their networks created.
func TestPoolServesNetworkPools(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, extra := layoutNetwork(ns, "default"), layoutNetwork(ns, "extra")
	p, e := readyLayout(g, nw), readyLayout(g, extra)

	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "cp"}, v1alpha1.IPPoolRange{NetworkName: "extra", Pool: "cp"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Expect(pool.Status.Ranges).To(Equal([]v1alpha1.IPPoolRangeStatus{
		{NetworkName: "default", ID: nw.Status.Layout.Pools[0].Ranges[0].ID, StartAddress: addr(p, 0), EndAddress: addr(p, 2), Ready: true},
		{NetworkName: "extra", ID: extra.Status.Layout.Pools[0].Ranges[0].ID, StartAddress: addr(e, 0), EndAddress: addr(e, 2), Ready: true},
	}))
	g.Expect(pool.Status.Gateway).To(Equal("10.10.0.1"))
	g.Expect(pool.Status.AddressPrefixLength).To(Equal(int32(22)))
	g.Expect(pool.Status.IPAddresses).To(Equal(&v1alpha1.NetBoxIPPoolStatusIPAddresses{Total: 6, Used: 0, Free: 6}))
}

// Every change the provider makes in NetBox is recorded as an Event on the object behind it.
func TestChangesAreRecordedAsEvents(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	nw.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete
	nw.Spec.Pools = []v1alpha1.NetworkPool{netPool("cp", 3), netPool("workers", 0)}
	readyLayout(g, nw)
	waitEvent(g, ns, "default", "PrefixAcquired")
	waitEvent(g, ns, "default", "VIPReserved")
	waitEvent(g, ns, "default", "RangeCreated")

	setPools(g, nw, netPool("cp", 3), netPool("workers", 0), netPool("extra", 1))
	waitEvent(g, ns, "default", "RangeUpdated")

	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	deleteCluster(t, g, ns) // under Delete the VIP waits for the Cluster that uses it
	waitEvent(g, ns, "default", "RangeReleased")
	waitEvent(g, ns, "default", "VIPReleased")
	waitEvent(g, ns, "default", "PrefixReleased")
}

func TestPoolRejectsMixedNetworks(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	other := layoutNetwork(ns, "other")
	other.Spec.ParentPrefix, other.Spec.Gateway = "10.20.0.0/22", "10.20.0.1"
	readyLayout(g, other)

	pool := newPool(ns, "worker",
		v1alpha1.IPPoolRange{NetworkName: "default", Pool: "cp"},
		v1alpha1.IPPoolRange{NetworkName: "other", Pool: "cp"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	readyRanges := func(g Gomega) []string {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
		var out []string
		for _, rs := range pool.Status.Ranges {
			if rs.Ready {
				out = append(out, rs.NetworkName+" "+rs.StartAddress+"-"+rs.EndAddress)
			}
		}
		return out
	}
	waitCondition(g, pool, metav1.ConditionFalse, v1alpha1.ReasonNetworkNotReady)
	g.Consistently(readyRanges).Should(BeEmpty(), "the network of the first range sets the L2")

	p := readyLayout(g, layoutNetwork(ns, "default"))
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonMixedNetworks)
	waitReady(g, pool)
	g.Eventually(readyRanges).Should(Equal([]string{"default " + span(p, 0, 2)}))
}

// TestPoolSpansLogicalParents: networks carved from two logical parents of one VLAN share an L2, so one pool serves both.
func TestPoolSpansLogicalParents(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	var prefixes []netip.Prefix
	pool := newPool(ns, "worker")
	for i, parent := range []string{"10.10.1.0/24", "10.10.2.0/24"} {
		name := fmt.Sprintf("n%d", i)
		byHand(t, g, netbox.Prefixes, map[string]any{"prefix": parent, "status": "active", "description": "logical parent"})
		nw := newNetwork(ns, name)
		nw.Spec.ParentPrefix, nw.Spec.AddressPrefixLength = parent, 22
		nw.Spec.Pools = []v1alpha1.NetworkPool{netPool("all", 0)}
		prefixes = append(prefixes, readyLayout(g, nw))
		pool.Spec.Ranges = append(pool.Spec.Ranges, v1alpha1.IPPoolRange{NetworkName: name, Pool: "all"})
	}
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Expect(pool.Status.Ranges).To(HaveLen(2))
	for i, rs := range pool.Status.Ranges {
		g.Expect(rs.Ready).To(BeTrue(), rs.Message)
		g.Expect(rs.StartAddress + "-" + rs.EndAddress).To(Equal(span(prefixes[i], 0, 15)))
	}
	g.Expect(pool.Status.AddressPrefixLength).To(Equal(int32(22)))
	g.Expect(pool.Status.IPAddresses.Total).To(Equal(int32(32)))
}

func TestPoolRejectsInvalidAddressTemplate(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	readyLayout(g, layoutNetwork(ns, "default"))
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "workers"})
	pool.Spec.IPAddress.DNSName = "{{ .Owner }} node"
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitCondition(g, pool, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec)
	g.Expect(meta.FindStatusCondition(pool.Status.Conditions, v1alpha1.ReadyCondition).Message).To(ContainSubstring("ipAddress: dnsName"))

	eventually(g, pool, func(g Gomega) {
		pool.Spec.IPAddress.DNSName = "{{ .Owner }}"
		g.Expect(k8s.Update(ctx, pool)).To(Succeed())
	})
	waitReady(g, pool)
}

func TestPoolWaitsForNetwork(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "late", Pool: "cp"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitCondition(g, pool, metav1.ConditionFalse, v1alpha1.ReasonNetworkNotReady)

	g.Expect(k8s.Create(ctx, layoutNetwork(ns, "late"))).To(Succeed())
	waitReady(g, pool)
}

func TestNetworkDeletionWaitsForPool(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, pool := readyPool(g, ns, 3)

	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonWaitingForPools)
	g.Consistently(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
		obj, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(obj.Status.Value).To(Equal("active"))
	}).Should(Succeed())

	g.Expect(k8s.Delete(ctx, pool)).To(Succeed())
	g.Eventually(gone(nw)).Should(BeTrue())
	obj, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(obj.Status.Value).To(Equal("deprecated"))
	ranges := ofNamespace(g, netbox.IPRanges, ns)
	g.Expect(ranges).To(HaveLen(2))
	for _, rng := range ranges {
		g.Expect(rng.Status.Value).To(Equal("deprecated"))
	}
}

func TestPoolDeletionWaitsForAddresses(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	_, pool := readyPool(g, ns, 3)
	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	address(g, claim)

	g.Expect(k8s.Delete(ctx, pool)).To(Succeed())
	waitCondition(g, pool, metav1.ConditionFalse, v1alpha1.ReasonWaitingForAddresses)
	g.Consistently(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
	}).Should(Succeed())

	g.Expect(k8s.Delete(ctx, claim)).To(Succeed())
	g.Eventually(gone(pool)).Should(BeTrue())
}

func TestPoolKeepsAllocatingWhileNetworkDeleting(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, pool := readyPool(g, ns, 3)

	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonWaitingForPools)
	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	address(g, claim)
	waitReady(g, pool)
}

func TestPoolIgnoresCustomFieldErrors(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	nw.Spec.CustomFields = map[string]apiextensionsv1.JSON{"undefined_field_xyz": {Raw: []byte(`"v"`)}}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	waitTypedCondition(g, nw, v1alpha1.MetadataSyncedCondition, metav1.ConditionFalse, v1alpha1.ReasonNetBoxError)

	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "workers"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
}

func setRanges(g *WithT, pool *v1alpha1.NetBoxIPPool, ranges ...v1alpha1.IPPoolRange) {
	g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
		pool.Spec.Ranges = ranges
		g.Expect(k8s.Update(ctx, pool)).To(Succeed())
	}).Should(Succeed())
}

// A pool without the cluster-name label pauses with the Cluster of its network.
func TestPoolPausedByNetworkCluster(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	p := readyLayout(g, layoutNetwork(ns, "default"))
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "cp"})
	pool.Labels = nil
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	start := func(g Gomega) string {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
		g.Expect(pool.Status.Ranges).NotTo(BeEmpty())
		return pool.Status.Ranges[0].StartAddress
	}

	setPaused(t, ns, true)
	waitTypedCondition(g, pool, clusterv1.PausedCondition, metav1.ConditionTrue, clusterv1.PausedReason)
	setRanges(g, pool, v1alpha1.IPPoolRange{NetworkName: "default", Pool: "workers"})
	g.Consistently(start).Should(Equal(addr(p, 0)))

	setPaused(t, ns, false)
	waitTypedCondition(g, pool, clusterv1.PausedCondition, metav1.ConditionFalse, clusterv1.NotPausedReason)
	g.Eventually(start).Should(Equal(addr(p, 3)))
}

func TestOmitGateway(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	readyLayout(g, layoutNetwork(ns, "default"))
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "cp"})
	pool.Spec.OmitGateway = ptr.To(true)
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	g.Expect(pool.Status.Gateway).To(BeEmpty())
	g.Expect(pool.Status.AddressPrefixLength).To(Equal(int32(22)))

	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	a := address(g, claim)
	g.Expect(a.Spec.Address).NotTo(BeEmpty())
	g.Expect(a.Spec.Gateway).To(BeEmpty())
}

func TestFoldErrors(t *testing.T) {
	wait := waitFor(v1alpha1.ReasonNetworkNotReady, errors.New("a"))
	retry := retryLater(v1alpha1.ReasonInvalidRange, errors.New("b"))
	if foldErrors(nil) != nil || foldErrors([]error{nil, nil}) != nil {
		t.Error("no errors: want nil")
	}
	for name, tc := range map[string]struct {
		errs   []error
		reason string
		kind   errKind
	}{
		"wait":              {[]error{wait}, v1alpha1.ReasonNetworkNotReady, kindWait},
		"wait, retry":       {[]error{wait, retry}, v1alpha1.ReasonNetworkNotReady, kindRetry},
		"retry, unexpected": {[]error{retry, errors.New("c")}, v1alpha1.ReasonInvalidRange, kindError},
		"nil skipped":       {[]error{nil, retry, nil}, v1alpha1.ReasonInvalidRange, kindRetry},
		"invalid spec":      {[]error{wait, withReason(v1alpha1.ReasonInvalidSpec, errors.New("d"))}, v1alpha1.ReasonNetworkNotReady, kindWait},
	} {
		var re reasonError
		if err := foldErrors(tc.errs); !errors.As(err, &re) || re.reason != tc.reason || re.kind != tc.kind {
			t.Errorf("%s: got %+v, want reason %s kind %d", name, re, tc.reason, tc.kind)
		}
	}
	if err := foldErrors([]error{wait, retry}); err.Error() != "[a, b]" {
		t.Errorf("message %q", err)
	}
}

// A network that cannot resolve its owner or tenant takes the ranges of its pools out of service like any network that is
// not Ready: a new claim gets no address.
func TestPoolRangeOfHeldNetworkLeavesService(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1alpha1.AddToScheme, clusterv1.AddToScheme, ipamv1.AddToScheme} {
		g.Expect(add(scheme)).To(Succeed())
	}
	for _, reason := range []string{v1alpha1.ReasonOwnerNotFound, v1alpha1.ReasonTenantNotFound} {
		nw := layoutNetwork("ns", "default")
		nw.Status.Prefix, nw.Status.OwnerID = "10.10.0.16/28", 1
		nw.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ReadyCondition, Status: metav1.ConditionFalse, Reason: reason}}
		nw.Status.Layout.Pools = []v1alpha1.PoolLayout{{Name: "workers", Size: intstr.FromString("rest"),
			Ranges: []v1alpha1.LayoutRange{{Start: "10.10.0.18", End: "10.10.0.19", ID: 7}}}}
		pool := newPool("ns", "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "workers"})
		pool.Finalizers = []string{poolFinalizer}
		pool.Status.Conditions = []metav1.Condition{{Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason}}
		pool.Status.Ranges = []v1alpha1.IPPoolRangeStatus{{NetworkName: "default", ID: 7, StartAddress: "10.10.0.18", EndAddress: "10.10.0.19", Ready: true}}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nw, pool).WithStatusSubresource(pool).
			WithIndex(&ipamv1.IPAddress{}, poolRefField, func(o client.Object) []string {
				a, ok := o.(*ipamv1.IPAddress)
				if !ok {
					return nil
				}
				return poolRefName(a.Spec.PoolRef)
			}).Build()
		r := &NetBoxIPPoolReconciler{Client: c}

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
		g.Expect(err).NotTo(HaveOccurred(), reason)
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
		g.Expect(pool.Status.Ranges).To(HaveLen(1), reason)
		g.Expect(pool.Status.Ranges[0].Ready).To(BeFalse(), reason)
		g.Expect(pool.Status.Ranges[0].Reason).To(Equal(v1alpha1.ReasonNetworkNotReady), reason)

		h := testHandler("")
		h.claim.Spec.PoolRef.Name, h.Client = "worker", c
		got, res, err := h.FetchPool(ctx)
		g.Expect(err).NotTo(HaveOccurred(), reason)
		g.Expect(got).To(BeNil(), reason)
		g.Expect(res).NotTo(BeNil(), reason)
		g.Expect(readyOf(h.claim)).To(Equal(poolNotReady), reason)
	}
}

// TestPoolRangesTakeNetworkOwner checks a pool in two networks of different owners and tenants: each range carries
// those of its network, so does an address taken from it, and the claim finds that address again on its recheck.
func TestPoolRangesTakeNetworkOwner(t *testing.T) {
	g := newG(t)
	z := newZone(t, g)
	ns := newEnv(t, nil)
	a, b := layoutNetwork(ns, "a"), layoutNetwork(ns, "b")
	a.Spec.Pools[0] = netPool("cp", 1)
	b.Spec.Owner, b.Spec.Tenant = &v1alpha1.OwnerRef{Name: z.ownerName}, &v1alpha1.TenantRef{Slug: z.tenantSlug}
	for _, nw := range []*v1alpha1.NetBoxNetwork{a, b} {
		readyLayout(g, nw)
		for _, p := range nw.Status.Layout.Pools {
			for _, lr := range p.Ranges {
				rng, _, err := admin.Get(ctx, netbox.IPRanges, lr.ID)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(attrsOf(rng)).To(Equal(networkAttrs(nw)), nw.Name)
			}
		}
	}
	g.Expect(networkAttrs(b)).To(Equal(netbox.Attrs{Owner: z.owner, Tenant: z.tenant}))
	g.Expect(networkAttrs(a)).To(Equal(netbox.Attrs{}), "no spec.owner, no spec.tenant: none written")
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "a", Pool: "cp"}, v1alpha1.IPPoolRange{NetworkName: "b", Pool: "cp"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)

	m1 := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, m1)).To(Succeed())
	g.Expect(address(g, m1).Spec.Address).To(Equal(addr(netip.MustParsePrefix(a.Status.Prefix), 0)))
	m2 := newClaim(ns, "m2", "worker")
	g.Expect(k8s.Create(ctx, m2)).To(Succeed())
	g.Expect(netip.MustParsePrefix(b.Status.Prefix).Contains(netip.MustParseAddr(address(g, m2).Spec.Address))).To(BeTrue())
	ips, err := findRef(netbox.IPAddresses, refOf("IPAddressClaim", ns, "m2"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ips).To(HaveLen(1))
	g.Expect(attrsOf(ips[0])).To(Equal(networkAttrs(b)))

	g.Eventually(func() string { return claimReady(g, m2) }).Should(Equal(readyTrue))
	touch(g, m2)
	g.Consistently(func() string { return claimReady(g, m2) }).Should(Equal(readyTrue))
	again, err := findRef(netbox.IPAddresses, refOf("IPAddressClaim", ns, "m2"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(again).To(HaveLen(1))
	g.Expect(again[0].ID).To(Equal(ips[0].ID), "the recheck found the address, it did not restore it")
}

// vipLayoutNetwork is vipNetwork laid out as cp 3, workers rest.
func vipLayoutNetwork(ns, name string) *v1alpha1.NetBoxNetwork {
	nw := vipNetwork(ns, name)
	nw.Spec.Pools = layoutNetwork(ns, name).Spec.Pools
	return nw
}

// The prefix, the VIP, the ranges and the address are created with the owner and tenant of the network's spec.
func TestAttrsOnEverything(t *testing.T) {
	g := newG(t)
	z := newZone(t, g)
	ns := newEnv(t, nil)
	nw := vipLayoutNetwork(ns, "default")
	nw.Spec.Owner, nw.Spec.Tenant = &v1alpha1.OwnerRef{Name: z.ownerName}, &v1alpha1.TenantRef{ID: z.tenant}
	readyLayout(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "cp"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	address(g, claim)

	want := netbox.Attrs{Owner: z.owner, Tenant: z.tenant}
	g.Expect(networkAttrs(nw)).To(Equal(want))
	objs := slices.Concat(ofNamespace(g, netbox.Prefixes, ns), ofNamespace(g, netbox.IPRanges, ns), ofNamespace(g, netbox.IPAddresses, ns))
	g.Expect(objs).To(HaveLen(5), "prefix, ranges cp and workers, VIP and address")
	for _, o := range objs {
		g.Expect(attrsOf(o)).To(Equal(want), "object %d", o.ID)
	}
}

// Two management zones hold objects of the same namespace and names. The other zone's prefix, VIP, range and address
// (keys with "ci/" in the default mode, bare in the instance mode) are never touched: a network,
// a pool and a claim of these names create objects of their own and delete them; the other zone's keep their last_updated.
func TestOtherZoneSameNames(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	other := keys
	other.Instance = map[string]string{"": "ci", "ci": ""}[keys.Instance]
	parents, err := admin.List(ctx, netbox.Prefixes, url.Values{"prefix": {"10.10.0.0/22"}, "vrf_id": {"null"}})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(parents).To(HaveLen(1))
	nw := vipLayoutNetwork(ns, "default")
	nw.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "cp"})
	claim := newClaim(ns, "m1", "worker")

	type seeded struct {
		path string
		obj  netbox.Object
	}
	var theirs []seeded
	seed := func(create, path, ref string, fields map[string]any) netbox.Object {
		o, err := admin.Create(ctx, create, other.Managed(netbox.Attrs{}, ref, "test: other zone", fields))
		g.Expect(err).NotTo(HaveOccurred())
		t.Cleanup(func() {
			if err := admin.Delete(ctx, path, o.ID, "", "test cleanup"); err != nil {
				t.Error(err)
			}
		})
		o, _, err = admin.Get(ctx, path, o.ID)
		g.Expect(err).NotTo(HaveOccurred())
		theirs = append(theirs, seeded{path, o})
		return o
	}
	prefix := seed(netbox.AvailablePrefixes(parents[0].ID), netbox.Prefixes, other.Key("NetBoxNetwork", nw), map[string]any{"prefix_length": 28, "status": "active"})
	p := netip.MustParsePrefix(prefix.Prefix)
	seed(netbox.IPAddresses, netbox.IPAddresses, vipRef(other, nw), map[string]any{"address": addr(p, 10) + "/22", "role": "vip", "status": "active"})
	seed(netbox.IPRanges, netbox.IPRanges, poolRangeRef(other, nw, "cp"), map[string]any{"start_address": addr(p, 2) + "/22", "end_address": addr(p, 3) + "/22", "status": "active"})
	seed(netbox.IPAddresses, netbox.IPAddresses, other.Key("IPAddressClaim", claim), map[string]any{"address": addr(p, 2) + "/22", "status": "active"})

	readyLayout(g, nw)
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	address(g, claim)
	g.Eventually(func() string { return claimReady(g, claim) }).Should(Equal(readyTrue))
	ours := []struct{ path, ref string }{
		{netbox.Prefixes, keys.Key("NetBoxNetwork", nw)},
		{netbox.IPAddresses, vipRef(keys, nw)},
		{netbox.IPRanges, poolRangeRef(keys, nw, "cp")},
		{netbox.IPAddresses, keys.Key("IPAddressClaim", claim)},
	}
	for i, o := range ours {
		found, err := findRef(o.path, o.ref)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(HaveLen(1), o.ref)
		g.Expect(found[0].ID).NotTo(Equal(theirs[i].obj.ID), o.ref)
	}
	g.Expect(nw.Status.Prefix).NotTo(Equal(prefix.Prefix))

	for _, obj := range []client.Object{claim, pool, nw} {
		if obj == nw {
			deleteCluster(t, g, ns) // the VIP is deleted once no Cluster has it
		}
		g.Expect(k8s.Delete(ctx, obj)).To(Succeed())
		g.Eventually(gone(obj)).Should(BeTrue())
	}
	for _, o := range ours {
		g.Expect(findRef(o.path, o.ref)).To(BeEmpty(), o.ref)
	}
	for _, o := range theirs {
		cur, _, err := admin.Get(ctx, o.path, o.obj.ID)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cur.LastUpdated).To(Equal(o.obj.LastUpdated), "%s %d", o.path, o.obj.ID)
	}
}

// A new spec.tenant reaches the prefix, the VIP, the ranges and the address, each recorded as an Event; the owner stays.
func TestTenantChangeSynced(t *testing.T) {
	g := newG(t)
	z := newZone(t, g)
	next := byHand(t, g, netbox.Tenants, map[string]any{"name": z.tenantSlug + "-next", "slug": z.tenantSlug + "-next"}).ID
	ns := newEnv(t, nil)
	nw := vipLayoutNetwork(ns, "default")
	nw.Spec.Owner, nw.Spec.Tenant = &v1alpha1.OwnerRef{Name: z.ownerName}, &v1alpha1.TenantRef{Slug: z.tenantSlug}
	readyLayout(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "cp"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	address(g, claim)
	g.Eventually(func() string { return claimReady(g, claim) }).Should(Equal(readyTrue))

	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) { s.Tenant = &v1alpha1.TenantRef{ID: next} })
	want := netbox.Attrs{Owner: z.owner, Tenant: next}
	g.Eventually(func(g Gomega) {
		objs := slices.Concat(ofNamespace(g, netbox.Prefixes, ns), ofNamespace(g, netbox.IPRanges, ns), ofNamespace(g, netbox.IPAddresses, ns))
		g.Expect(objs).To(HaveLen(5), "prefix, ranges cp and workers, VIP and address")
		for _, o := range objs {
			g.Expect(attrsOf(o)).To(Equal(want), "object %d", o.ID)
		}
	}).Should(Succeed())
	for _, name := range []string{"default", "m1"} {
		waitEvent(g, ns, name, "AttributesSynced")
	}
}

// The prefix, the VIP, the range and the address get the default description and comments. Changed templates reach them,
// an edit by hand in NetBox is overwritten by the next reconcile. A template that does not render is InvalidSpec on the
// network or the pool; the claim stays Ready.
func TestTextsSynced(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := vipLayoutNetwork(ns, "default")
	readyLayout(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "cp"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	address(g, claim)
	g.Eventually(func() string { return claimReady(g, claim) }).Should(Equal(readyTrue))

	objects := []struct{ path, ref string }{
		{netbox.Prefixes, keys.Key("NetBoxNetwork", nw)},
		{netbox.IPAddresses, vipRef(keys, nw)},
		{netbox.IPRanges, poolRangeRef(keys, nw, "cp")},
		{netbox.IPAddresses, keys.Key("IPAddressClaim", claim)},
	}
	textsAre := func(want ...texts) {
		g.Eventually(func(g Gomega) {
			for i, o := range objects {
				objs, err := findRef(o.path, o.ref)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(objs).To(HaveLen(1), o.ref)
				g.Expect(texts{Description: objs[0].Description, Comments: objs[0].Comments}).To(Equal(want[i]), o.ref)
			}
		}).Should(Succeed())
	}
	rangeTexts := texts{ns + "/cp // pool of cluster " + ns, managedComments}
	textsAre(
		texts{ns + "/default // cluster " + ns, managedComments},
		texts{ns + "/default // " + ns + " control plane endpoint", managedComments},
		rangeTexts,
		texts{ns + "/m1 // " + ns + " worker", managedComments},
	)

	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) {
		s.Description, s.Comments = "{{ .Name }} prefix", "net {{ .Cluster }}"
		s.ControlPlaneEndpoint.Description, s.ControlPlaneEndpoint.Comments = "vip of {{ .Cluster }}", `{{ "" }}`
	})
	eventually(g, pool, func(g Gomega) {
		pool.Spec.IPAddress.Comments = "claim {{ .Claim }}"
		g.Expect(k8s.Update(ctx, pool)).To(Succeed())
	})
	want := []texts{{"default prefix", "net " + ns}, {"vip of " + ns, ""}, rangeTexts, {ns + "/m1 // " + ns + " worker", "claim m1"}}
	textsAre(want...)

	for _, o := range objects {
		objs, err := findRef(o.path, o.ref)
		g.Expect(err).NotTo(HaveOccurred())
		_, err = admin.Patch(ctx, o.path, objs[0].ID, "", map[string]any{"description": "edited by hand", "comments": "by hand"})
		g.Expect(err).NotTo(HaveOccurred())
	}
	for _, obj := range []client.Object{nw, pool, claim} {
		touch(g, obj)
	}
	textsAre(want...)

	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) { s.Description = "{{ .Pool }}" })
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec)
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.ReadyCondition).Message).To(ContainSubstring("description"))
	eventually(g, pool, func(g Gomega) {
		pool.Spec.IPAddress.Comments = "{{ .Nope }}"
		g.Expect(k8s.Update(ctx, pool)).To(Succeed())
	})
	waitCondition(g, pool, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec)
	g.Expect(meta.FindStatusCondition(pool.Status.Conditions, v1alpha1.ReadyCondition).Message).To(ContainSubstring("ipAddress: comments"))
	g.Consistently(func() string { return claimReady(g, claim) }).Should(Equal(readyTrue))
	textsAre(want...)
}

// A description of more than 200 characters is cut to what NetBox stores and then compares equal: once the prefix, the VIP,
// the range and the address are in place, reconciling them again writes nothing.
func TestTextsSteadyState(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	long := strings.Repeat("ж", 250)
	nw := vipLayoutNetwork(ns, "default")
	nw.Spec.Description, nw.Spec.ControlPlaneEndpoint.Description = long, long
	readyLayout(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "cp"})
	pool.Spec.IPAddress.Description = long
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	address(g, claim)
	g.Eventually(func() string { return claimReady(g, claim) }).Should(Equal(readyTrue))

	objects := []struct{ path, ref string }{
		{netbox.Prefixes, keys.Key("NetBoxNetwork", nw)},
		{netbox.IPAddresses, vipRef(keys, nw)},
		{netbox.IPRanges, poolRangeRef(keys, nw, "cp")},
		{netbox.IPAddresses, keys.Key("IPAddressClaim", claim)},
	}
	current := func(g Gomega) []netbox.Object {
		var objs []netbox.Object
		for _, o := range objects {
			found, err := findRef(o.path, o.ref)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(HaveLen(1), o.ref)
			objs = append(objs, found[0])
		}
		return objs
	}
	g.Eventually(func(g Gomega) {
		for i, o := range current(g) {
			if objects[i].path != netbox.IPRanges { // a range has the default texts
				g.Expect(o.Description).To(Equal(string([]rune(long)[:200])), "object %d", o.ID)
			}
		}
	}).Should(Succeed())
	updated := func(g Gomega) []string {
		var stamps []string
		for _, o := range current(g) {
			stamps = append(stamps, o.LastUpdated)
		}
		return stamps
	}
	before := updated(g)
	for _, obj := range []client.Object{nw, pool, claim} {
		touch(g, obj)
	}
	g.Consistently(updated, "5s").Should(Equal(before), "last_updated of the prefix, VIP, range and address")
}

// Objects the token may not change (their owner moved to the group capi, read-only for it: hack/dev/seed.sh) cannot
// take a new owner. That stops nothing: the network stays Ready with MetadataSynced and LayoutReady Forbidden, the pool
// keeps serving, a new claim gets an address of the new owner, the allocated one stays Ready; nothing leaks.
// Back in the zone, deleting the claims, the pool and the network releases their objects by key.
func TestOwnerChangeForbidden(t *testing.T) {
	g := newG(t)
	z, z2 := newZone(t, g), newZone(t, g)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	nw.Spec.Owner, nw.Spec.DeletionPolicy = &v1alpha1.OwnerRef{Name: z.ownerName}, v1alpha1.DeletionPolicyDelete
	readyLayout(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "cp"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	m1 := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, m1)).To(Succeed())
	address(g, m1)
	g.Eventually(func() string { return claimReady(g, m1) }).Should(Equal(readyTrue))

	moveOwner(g, z.owner, "capi")
	t.Cleanup(func() { moveOwner(g, z.owner, "zones") })
	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) { s.Owner = &v1alpha1.OwnerRef{Name: z2.ownerName} })
	waitTypedCondition(g, nw, v1alpha1.MetadataSyncedCondition, metav1.ConditionFalse, v1alpha1.ReasonForbidden)
	waitTypedCondition(g, nw, v1alpha1.LayoutReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonForbidden)
	g.Expect(meta.IsStatusConditionTrue(nw.Status.Conditions, v1alpha1.ReadyCondition)).To(BeTrue())
	m2 := newClaim(ns, "m2", "worker")
	g.Expect(k8s.Create(ctx, m2)).To(Succeed())
	address(g, m2)
	ips, err := findRef(netbox.IPAddresses, refOf("IPAddressClaim", ns, "m2"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ips).To(HaveLen(1))
	g.Expect(attrsOf(ips[0])).To(Equal(netbox.Attrs{Owner: z2.owner}))
	g.Consistently(func(g Gomega) {
		for path, n := range map[string]int{netbox.Prefixes: 1, netbox.IPRanges: 2, netbox.IPAddresses: 2} {
			g.Expect(ofNamespace(g, path, ns)).To(HaveLen(n), path)
		}
		for _, path := range []string{netbox.Prefixes, netbox.IPRanges} {
			for _, o := range ofNamespace(g, path, ns) {
				g.Expect(attrsOf(o)).To(Equal(netbox.Attrs{Owner: z.owner}), path)
			}
		}
		objs, err := findRef(netbox.IPAddresses, refOf("IPAddressClaim", ns, "m1"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(objs).To(HaveLen(1))
		g.Expect(attrsOf(objs[0])).To(Equal(netbox.Attrs{Owner: z.owner}))
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
		g.Expect(meta.IsStatusConditionTrue(pool.Status.Conditions, v1alpha1.ReadyCondition)).To(BeTrue())
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(m1), m1)).To(Succeed())
		g.Expect(readyOf(m1)).To(Equal(readyTrue))
	}).Should(Succeed())

	moveOwner(g, z.owner, "zones")
	for _, obj := range []client.Object{m1, m2, pool, nw} {
		g.Expect(k8s.Delete(ctx, obj)).To(Succeed())
		g.Eventually(gone(obj)).Should(BeTrue(), obj.GetName())
	}
	for _, path := range []string{netbox.Prefixes, netbox.IPRanges, netbox.IPAddresses} {
		g.Expect(ofNamespace(g, path, ns)).To(BeEmpty(), path)
	}
}

// A network whose owner NetBox does not have stops: its layout waits, its pools serve no new address, the claim it gave
// an address stays Ready. Without spec.owner the network serves again.
func TestHeldNetworkStopsPools(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, _ := readyPool(g, ns, 3)
	m1 := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, m1)).To(Succeed())
	address(g, m1)
	g.Eventually(func() string { return claimReady(g, m1) }).Should(Equal(readyTrue))

	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) { s.Owner = &v1alpha1.OwnerRef{Name: "missing-" + ns} })
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonOwnerNotFound)
	waitTypedCondition(g, nw, v1alpha1.LayoutReadyCondition, metav1.ConditionUnknown, v1alpha1.ReasonWaitingForPrefix)
	other := newPool(ns, "other", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "workers"})
	g.Expect(k8s.Create(ctx, other)).To(Succeed())
	waitTypedCondition(g, other, v1alpha1.RangesReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonNetworkNotReady)
	m2 := newClaim(ns, "m2", "worker")
	g.Expect(k8s.Create(ctx, m2)).To(Succeed())
	g.Consistently(func(g Gomega) {
		g.Expect(noAddress(m2)()).To(BeTrue())
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(m1), m1)).To(Succeed())
		g.Expect(readyOf(m1)).To(Equal(readyTrue))
	}).Should(Succeed())

	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) { s.Owner = nil })
	waitReady(g, other)
	address(g, m2)
}

// attrsOf is the owner and tenant a NetBox object carries.
func attrsOf(o netbox.Object) netbox.Attrs {
	var a netbox.Attrs
	if o.Owner != nil {
		a.Owner = o.Owner.ID
	}
	if o.Tenant != nil {
		a.Tenant = o.Tenant.ID
	}
	return a
}
