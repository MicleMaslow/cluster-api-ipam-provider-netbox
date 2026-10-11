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
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
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

func TestRangeAddresses(t *testing.T) {
	l2 := netip.MustParsePrefix("10.10.0.0/22")
	gw := netip.MustParseAddr("10.10.0.1")
	for name, tc := range map[string]struct {
		prefix     string
		start, end int32
		reserved   []string
		want       string
	}{
		"inside":                {"10.10.1.0/26", 14, 60, nil, "10.10.1.14-10.10.1.60"},
		"negative":              {"10.10.1.0/26", -1, -1, nil, "10.10.1.63-10.10.1.63"},
		"logical parent bounds": {"10.10.1.0/24", 0, -1, nil, "10.10.1.0-10.10.1.255"},
		"l2 network":            {"10.10.0.0/26", 0, 5, nil, "error"},
		"gateway":               {"10.10.0.0/26", 1, 1, nil, "error"},
		"l2 broadcast":          {"10.10.3.192/26", -1, -1, nil, "error"},
		"reserved":              {"10.10.1.0/26", 60, 62, []string{"10.10.1.62"}, "error"},
		"beside reserved":       {"10.10.1.0/26", 2, 61, []string{"10.10.1.62"}, "10.10.1.2-10.10.1.61"},
		"reversed":              {"10.10.1.0/26", 9, 2, nil, "error"},
		"outside":               {"10.10.1.0/26", 2, 64, nil, "error"},
	} {
		reserved := []netip.Addr{gw}
		for _, r := range tc.reserved {
			reserved = append(reserved, netip.MustParseAddr(r))
		}
		s, e, err := rangeAddresses(netip.MustParsePrefix(tc.prefix), v1alpha1.IPPoolRange{NetworkName: "n", Start: tc.start, End: tc.end}, l2, reserved...)
		got := "error"
		if err == nil {
			got = s.String() + "-" + e.String()
		}
		if got != tc.want {
			t.Errorf("%s: got %s (%v), want %s", name, got, err, tc.want)
		}
	}
}

func TestPoolTreatsNetworkWithoutPrefixAsNotReady(t *testing.T) {
	nw := newNetwork("ns", "default")
	nw.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ReadyCondition, Status: metav1.ConditionTrue}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"results":[]}`) }))
	defer srv.Close()
	r := &NetBoxIPPoolReconciler{Client: claimClient(nw), NetBox: &netbox.Client{URL: srv.URL}, Keys: netbox.DefaultKeys}

	err := r.reconcileNormal(ctx, newPool("ns", "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3}))
	var re reasonError
	if !errors.As(err, &re) || re.reason != v1alpha1.ReasonNetworkNotReady {
		t.Errorf("got %v, want reason %s", err, v1alpha1.ReasonNetworkNotReady)
	}
}

// TestPoolOfAnotherWatchFilterIsSkipped: the IPAddress watch has no label filter, so it can enqueue such a pool.
func TestPoolOfAnotherWatchFilterIsSkipped(t *testing.T) {
	g := NewWithT(t)
	pool := newPool("ns", "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3})
	pool.Labels = nil
	c := claimClient(pool)
	r := &NetBoxIPPoolReconciler{Client: c, WatchFilterValue: "w"}

	g.Expect(r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})).To(Equal(ctrl.Result{}))
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
	g.Expect(pool.Finalizers).To(BeEmpty())
}

// The ranges of a deleted pool are found by their key alone, also without any network in the namespace.
func TestPoolDeleteFindsRangesByKey(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1alpha1.AddToScheme, clusterv1.AddToScheme, ipamv1.AddToScheme} {
		g.Expect(add(scheme)).To(Succeed())
	}
	rng := `{"id":7,"start_address":"10.10.0.18/22","end_address":"10.10.0.19/22","status":{"value":"active"},"owner":{"id":1},"custom_fields":{"capi_ref":"NetBoxIPPool/ns/worker/default"}}`
	for name, withNetwork := range map[string]bool{"networks": true, "none": false} {
		pool := newPool("ns", "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3})
		pool.Finalizers = []string{poolFinalizer}
		pool.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		pool.Status.Conditions = []metav1.Condition{{Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason}}
		objs := []client.Object{pool}
		if withNetwork {
			nw := newNetwork("ns", "default")
			nw.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete
			nw.Status.Prefix = "10.10.0.16/28"
			objs = append(objs, nw)
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(pool).
			WithIndex(&ipamv1.IPAddress{}, poolRefField, func(o client.Object) []string {
				a, ok := o.(*ipamv1.IPAddress)
				if !ok {
					return nil
				}
				return poolRefName(a.Spec.PoolRef)
			}).Build()
		var reqs []string
		r := &NetBoxIPPoolReconciler{Client: ignoreGone{c}, NetBox: objNetBox(t, netbox.IPRanges, rng, &reqs), Keys: netbox.DefaultKeys, Recorder: events.NewFakeRecorder(10)}

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
		g.Expect(err).NotTo(HaveOccurred(), name)
		g.Expect(apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(pool), pool))).To(BeTrue(), name+": the pool is gone")
		g.Expect(reqs[0]).To(Equal("GET /api/ipam/ip-ranges/?cf_capi_ref__isw=NetBoxIPPool%2Fns%2Fworker%2F&limit=1000&ordering=id"), name)
		if withNetwork {
			g.Expect(reqs).To(ContainElement("DELETE /api/ipam/ip-ranges/7/?"), name)
		} else {
			g.Expect(reqs).To(ContainElement(HavePrefix("PATCH /api/ipam/ip-ranges/7/")), name+": deprecated without the network")
		}
	}
}

// A range release the token may not make, a 403 or the 404 of a range outside the permission constraint of the
// action, keeps the finalizer with reason Forbidden and reports no release.
func TestPoolDeleteForbidden(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1alpha1.AddToScheme, clusterv1.AddToScheme, ipamv1.AddToScheme} {
		g.Expect(add(scheme)).To(Succeed())
	}
	for _, write := range []int{http.StatusForbidden, http.StatusNotFound} {
		for _, policy := range []v1alpha1.DeletionPolicy{v1alpha1.DeletionPolicyDelete, v1alpha1.DeletionPolicyRetain} {
			name := fmt.Sprintf("%d %s", write, policy)
			nw := newNetwork("ns", "default")
			nw.Spec.DeletionPolicy = policy
			pool := newPool("ns", "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3})
			pool.Finalizers = []string{poolFinalizer}
			pool.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			pool.Status.Conditions = []metav1.Condition{{Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nw, pool).WithStatusSubresource(pool).
				WithIndex(&ipamv1.IPAddress{}, poolRefField, func(o client.Object) []string {
					a, ok := o.(*ipamv1.IPAddress)
					if !ok {
						return nil
					}
					return poolRefName(a.Spec.PoolRef)
				}).Build()
			r := &NetBoxIPPoolReconciler{Client: c, NetBox: forbiddenNetBox(t, write), Keys: netbox.DefaultKeys, Recorder: events.NewFakeRecorder(10)}

			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
			g.Expect(err).NotTo(HaveOccurred(), name)
			g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: netBoxRequeueAfter}), name)
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed(), name)
			g.Expect(pool.Finalizers).To(ConsistOf(poolFinalizer), name)
			cond := meta.FindStatusCondition(pool.Status.Conditions, v1alpha1.ReadyCondition)
			g.Expect(cond).NotTo(BeNil(), name)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse), name)
			g.Expect(cond.Reason).To(Equal(v1alpha1.ReasonForbidden), name)
			g.Expect(emitted(r.Recorder)).To(BeEmpty(), name)
		}
	}
}

func TestPoolCreatesRanges(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, extra := newNetwork(ns, "default"), newNetwork(ns, "extra")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	g.Expect(k8s.Create(ctx, extra)).To(Succeed())
	waitReady(g, nw)
	waitReady(g, extra)

	pool := newPool(ns, "worker",
		v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 5},
		v1alpha1.IPPoolRange{NetworkName: "extra", Start: -1, End: -1})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)

	p, e := netip.MustParsePrefix(nw.Status.Prefix), netip.MustParsePrefix(extra.Status.Prefix)
	eventually(g, pool, func(g Gomega) {
		g.Expect(pool.Status.Ranges).To(HaveLen(2))
		g.Expect(pool.Status.Ranges[0]).To(gstruct.MatchFields(gstruct.IgnoreExtras, gstruct.Fields{"StartAddress": Equal(addr(p, 2)), "EndAddress": Equal(addr(p, 5)), "Ready": BeTrue()}))
		g.Expect(pool.Status.Ranges[1]).To(gstruct.MatchFields(gstruct.IgnoreExtras, gstruct.Fields{"StartAddress": Equal(addr(e, -1)), "EndAddress": Equal(addr(e, -1)), "Ready": BeTrue()}))
		g.Expect(pool.Status.Gateway).To(Equal("10.10.0.1"))
		g.Expect(pool.Status.AddressPrefixLength).To(Equal(int32(22)))
		g.Expect(pool.Status.IPAddresses).To(Equal(&v1alpha1.NetBoxIPPoolStatusIPAddresses{Total: 5, Used: 0, Free: 5}))
	})
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)

	rng, _, err := admin.Get(ctx, netbox.IPRanges, pool.Status.Ranges[0].ID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rng.StartAddress).To(Equal(addr(p, 2) + "/22"))
	g.Expect(rng.EndAddress).To(Equal(addr(p, 5) + "/22"))
	g.Expect(attrsOf(rng)).To(Equal(netbox.Attrs{}))
	g.Expect(keys.RefOf(rng)).To(Equal(refOf("NetBoxIPPool", ns, "worker/default")))
}

// Every change the provider makes in NetBox is recorded as an Event on the object behind it.
func TestChangesAreRecordedAsEvents(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	nw.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 5})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	waitEvent(g, ns, "default", "PrefixAcquired")
	waitEvent(g, ns, "default", "VIPReserved")
	waitEvent(g, ns, "worker", "RangeCreated")

	setRanges(g, pool, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 6})
	waitEvent(g, ns, "worker", "RangeUpdated")

	g.Expect(k8s.Delete(ctx, pool)).To(Succeed())
	waitEvent(g, ns, "worker", "RangeReleased")
	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	deleteCluster(t, g, ns) // under Delete the VIP waits for the Cluster that uses it
	waitEvent(g, ns, "default", "VIPReleased")
	waitEvent(g, ns, "default", "PrefixReleased")
}

func TestPoolRejectsMixedNetworks(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	other := newNetwork(ns, "other")
	other.Spec.ParentPrefix, other.Spec.Gateway = "10.20.0.0/22", "10.20.0.1"
	g.Expect(k8s.Create(ctx, other)).To(Succeed())
	waitReady(g, other)

	pool := newPool(ns, "worker",
		v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3},
		v1alpha1.IPPoolRange{NetworkName: "other", Start: 2, End: 3})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitCondition(g, pool, metav1.ConditionFalse, v1alpha1.ReasonNetworkNotReady)
	g.Consistently(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
		g.Expect(pool.Status.Ranges).To(BeEmpty(), "the network of the first range sets the L2")
	}).Should(Succeed())

	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	p := netip.MustParsePrefix(nw.Status.Prefix)
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonMixedNetworks)
	waitReady(g, pool)
	waitRange(g, pool, "default", addr(p, 2), addr(p, 3))
	g.Expect(pool.Status.Ranges).To(HaveLen(1))
	g.Expect(findRef(netbox.IPRanges, refOf("NetBoxIPPool", ns, "worker/other"))).To(BeEmpty())
}

// TestPoolSpansLogicalParents: networks carved from two logical parents of one VLAN share an L2, so one pool serves both.
func TestPoolSpansLogicalParents(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	var networks []*v1alpha1.NetBoxNetwork
	pool := newPool(ns, "worker")
	for i, parent := range []string{"10.10.1.0/24", "10.10.2.0/24"} {
		name := fmt.Sprintf("n%d", i)
		byHand(t, g, netbox.Prefixes, map[string]any{"prefix": parent, "status": "active", "description": "logical parent"})
		nw := newNetwork(ns, name)
		nw.Spec.ParentPrefix, nw.Spec.AddressPrefixLength = parent, 22
		g.Expect(k8s.Create(ctx, nw)).To(Succeed())
		waitReady(g, nw)
		networks = append(networks, nw)
		pool.Spec.Ranges = append(pool.Spec.Ranges, v1alpha1.IPPoolRange{NetworkName: name, Start: 0, End: -1})
	}
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Expect(pool.Status.Ranges).To(HaveLen(2))
	for i, rs := range pool.Status.Ranges {
		p := netip.MustParsePrefix(networks[i].Status.Prefix)
		g.Expect(rs.Ready).To(BeTrue(), rs.Message)
		g.Expect(rs.StartAddress).To(Equal(addr(p, 0)))
		g.Expect(rs.EndAddress).To(Equal(addr(p, -1)))
	}
	g.Expect(pool.Status.AddressPrefixLength).To(Equal(int32(22)))
	g.Expect(pool.Status.IPAddresses.Total).To(Equal(int32(32)))
}

func TestPoolRejectsInvalidAddressTemplate(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 4})
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
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "late", Start: 2, End: 3})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitCondition(g, pool, metav1.ConditionFalse, v1alpha1.ReasonNetworkNotReady)

	g.Expect(k8s.Create(ctx, newNetwork(ns, "late"))).To(Succeed())
	waitReady(g, pool)
}

func TestPoolRetainDeprecatesRange(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)

	g.Expect(k8s.Delete(ctx, pool)).To(Succeed())
	g.Eventually(gone(pool)).Should(BeTrue())
	rng, _, err := admin.Get(ctx, netbox.IPRanges, pool.Status.Ranges[0].ID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rng.Status.Value).To(Equal("deprecated"))
}

func TestPoolDeletePolicyRemovesRange(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	nw.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)

	g.Expect(k8s.Delete(ctx, pool)).To(Succeed())
	g.Eventually(func() error {
		_, _, err := admin.Get(ctx, netbox.IPRanges, pool.Status.Ranges[0].ID)
		return err
	}).Should(MatchError(netbox.ErrNotFound))
}

func TestPoolDeleteFindsRangesWithoutStatus(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	nw.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	id := pool.Status.Ranges[0].ID

	setPaused(t, ns, true)
	time.Sleep(time.Second) // let an in-flight reconcile finish before status is cleared
	g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
		pool.Status = v1alpha1.NetBoxIPPoolStatus{}
		g.Expect(k8s.Status().Update(ctx, pool)).To(Succeed())
	}).Should(Succeed())
	g.Expect(k8s.Delete(ctx, pool)).To(Succeed())
	setPaused(t, ns, false)

	g.Eventually(func() error {
		_, _, err := admin.Get(ctx, netbox.IPRanges, id)
		return err
	}).Should(MatchError(netbox.ErrNotFound))
	g.Eventually(gone(pool)).Should(BeTrue())
}

func TestNetworkDeletionWaitsForPool(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)

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
}

func TestPoolDeletionWaitsForAddresses(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	_, pool := readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 4})
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
	nw, pool := readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 4})

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
	nw := newNetwork(ns, "default")
	nw.Spec.CustomFields = map[string]apiextensionsv1.JSON{"undefined_field_xyz": {Raw: []byte(`"v"`)}}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	waitTypedCondition(g, nw, v1alpha1.MetadataSyncedCondition, metav1.ConditionFalse, v1alpha1.ReasonNetBoxError)

	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 5})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
}

func rangeOf(pool *v1alpha1.NetBoxIPPool, network string) *v1alpha1.IPPoolRangeStatus {
	for i := range pool.Status.Ranges {
		if pool.Status.Ranges[i].NetworkName == network {
			return &pool.Status.Ranges[i]
		}
	}
	return nil
}

// waitRange waits until the pool serves its range in network from start to end and returns it.
func waitRange(g *WithT, pool *v1alpha1.NetBoxIPPool, network, start, end string) v1alpha1.IPPoolRangeStatus {
	var rs v1alpha1.IPPoolRangeStatus
	eventually(g, pool, func(g Gomega) {
		got := rangeOf(pool, network)
		g.Expect(got).NotTo(BeNil())
		g.Expect(got.Ready).To(BeTrue(), got.Message)
		g.Expect(got.StartAddress + "-" + got.EndAddress).To(Equal(start + "-" + end))
		rs = *got
	})
	return rs
}

func setRanges(g *WithT, pool *v1alpha1.NetBoxIPPool, ranges ...v1alpha1.IPPoolRange) {
	g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
		pool.Spec.Ranges = ranges
		g.Expect(k8s.Update(ctx, pool)).To(Succeed())
	}).Should(Succeed())
}

// boundsInNetBox is "start-end" of a NetBox IP range, without the mask.
func boundsInNetBox(g *WithT, id int64) string {
	rng, _, err := admin.Get(ctx, netbox.IPRanges, id)
	g.Expect(err).NotTo(HaveOccurred())
	return netip.MustParsePrefix(rng.StartAddress).Addr().String() + "-" + netip.MustParsePrefix(rng.EndAddress).Addr().String()
}

func TestPoolRangeGrowsAndShrinks(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, pool := readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 5})
	p := netip.MustParsePrefix(nw.Status.Prefix)
	id := waitRange(g, pool, "default", addr(p, 2), addr(p, 5)).ID

	for _, end := range []int{7, 3} {
		setRanges(g, pool, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: int32(end)})
		g.Expect(waitRange(g, pool, "default", addr(p, 2), addr(p, end)).ID).To(Equal(id))
		g.Expect(boundsInNetBox(g, id)).To(Equal(addr(p, 2) + "-" + addr(p, end)))
		g.Expect(markPopulated(g, id)).To(BeFalse())
		eventually(g, pool, func(g Gomega) { g.Expect(pool.Status.IPAddresses.Total).To(BeEquivalentTo(end - 1)) })
	}
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)

	g.Expect(admin.Patch(ctx, netbox.IPRanges, id, "", map[string]any{"mark_populated": true})).Error().To(Succeed())
	touch(g, pool)
	g.Eventually(func() bool { return markPopulated(g, id) }).Should(BeFalse(), "a fence left behind is lifted")
}

func markPopulated(g *WithT, id int64) bool {
	rng, _, err := admin.Get(ctx, netbox.IPRanges, id)
	g.Expect(err).NotTo(HaveOccurred())
	return rng.MarkPopulated
}

func TestPoolShrinkBlockedByAllocatedAddress(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, pool := readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 5})
	p := netip.MustParsePrefix(nw.Status.Prefix)
	id := waitRange(g, pool, "default", addr(p, 2), addr(p, 5)).ID
	m1 := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, m1)).To(Succeed())
	g.Expect(address(g, m1).Spec.Address).To(Equal(addr(p, 2)))

	setRanges(g, pool, v1alpha1.IPPoolRange{NetworkName: "default", Start: 3, End: 5})
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonRangeInUse)
	g.Expect(meta.FindStatusCondition(pool.Status.Conditions, v1alpha1.RangesReadyCondition).Message).To(ContainSubstring(addr(p, 2)))
	waitReady(g, pool)
	eventually(g, pool, func(g Gomega) {
		g.Expect(rangeOf(pool, "default")).To(gstruct.PointTo(gstruct.MatchFields(gstruct.IgnoreExtras, gstruct.Fields{
			"StartAddress": Equal(addr(p, 2)), "EndAddress": Equal(addr(p, 5)), "Ready": BeTrue(), "Reason": Equal(v1alpha1.ReasonRangeInUse),
		})))
	})
	g.Expect(boundsInNetBox(g, id)).To(Equal(addr(p, 2) + "-" + addr(p, 5)))

	m2 := newClaim(ns, "m2", "worker")
	g.Expect(k8s.Create(ctx, m2)).To(Succeed())
	g.Expect(address(g, m2).Spec.Address).To(Equal(addr(p, 3)))

	g.Expect(k8s.Delete(ctx, m1)).To(Succeed())
	waitRange(g, pool, "default", addr(p, 3), addr(p, 5))
	g.Expect(boundsInNetBox(g, id)).To(Equal(addr(p, 3) + "-" + addr(p, 5)))
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Expect(rangeOf(pool, "default").Reason).To(BeEmpty())
}

func TestPoolRemovesRange(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, extra := newNetwork(ns, "default"), newNetwork(ns, "extra")
	extra.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	g.Expect(k8s.Create(ctx, extra)).To(Succeed())
	waitReady(g, nw)
	waitReady(g, extra)
	p, e := netip.MustParsePrefix(nw.Status.Prefix), netip.MustParsePrefix(extra.Status.Prefix)
	pool := newPool(ns, "worker",
		v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 2},
		v1alpha1.IPPoolRange{NetworkName: "extra", Start: -2, End: -2})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitRange(g, pool, "default", addr(p, 2), addr(p, 2))
	id := waitRange(g, pool, "extra", addr(e, -2), addr(e, -2)).ID
	m1, m2 := newClaim(ns, "m1", "worker"), newClaim(ns, "m2", "worker")
	g.Expect(k8s.Create(ctx, m1)).To(Succeed())
	g.Expect(address(g, m1).Spec.Address).To(Equal(addr(p, 2)))
	g.Expect(k8s.Create(ctx, m2)).To(Succeed())
	g.Expect(address(g, m2).Spec.Address).To(Equal(addr(e, -2)))

	g.Expect(k8s.Delete(ctx, extra)).To(Succeed())
	waitCondition(g, extra, metav1.ConditionFalse, v1alpha1.ReasonWaitingForPools)

	setRanges(g, pool, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 2})
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonRangeInUse)
	eventually(g, pool, func(g Gomega) {
		g.Expect(rangeOf(pool, "extra")).To(gstruct.PointTo(gstruct.MatchFields(gstruct.IgnoreExtras, gstruct.Fields{
			"ID": Equal(id), "Ready": BeFalse(), "Reason": Equal(v1alpha1.ReasonRangeInUse), "Message": ContainSubstring(addr(e, -2)),
		})))
	})
	waitReady(g, pool)
	g.Expect(boundsInNetBox(g, id)).To(Equal(addr(e, -2) + "-" + addr(e, -2)))

	m3 := newClaim(ns, "m3", "worker")
	g.Expect(k8s.Create(ctx, m3)).To(Succeed())
	g.Consistently(noAddress(m3)).Should(BeTrue(), "a removed range hands out no addresses")
	g.Eventually(func() string { return claimReady(g, m3) }).Should(Equal(exhausted))
	g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(extra), extra)).To(Succeed(), "the network waits for the range")

	g.Expect(k8s.Delete(ctx, m2)).To(Succeed())
	eventually(g, pool, func(g Gomega) { g.Expect(rangeOf(pool, "extra")).To(BeNil()) })
	g.Eventually(func() error {
		_, _, err := admin.Get(ctx, netbox.IPRanges, id)
		return err
	}).Should(MatchError(netbox.ErrNotFound))
	g.Eventually(gone(extra)).Should(BeTrue())
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Expect(noAddress(m3)()).To(BeTrue())
}

func TestPoolRangeCannotCoverVIP(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: -1})
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonInvalidRange)
	waitReady(g, nw)
	g.Expect(findRef(netbox.IPRanges, refOf("NetBoxIPPool", ns, "worker/default"))).To(BeEmpty())

	setRanges(g, pool, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: -2})
	waitReady(g, pool)
	p := netip.MustParsePrefix(nw.Status.Prefix)
	waitRange(g, pool, "default", addr(p, 2), addr(p, -2))
	g.Expect(nw.Status.ControlPlaneEndpoint.Host).To(Equal(addr(p, -1)))
}

// Removing controlPlaneEndpoint leaves the VIP in NetBox and in the status, so pools keep off it.
func TestPoolRangeCannotCoverVIPAfterFieldRemoved(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	vip := nw.Status.ControlPlaneEndpoint

	g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
		nw.Spec.ControlPlaneEndpoint = nil
		g.Expect(k8s.Update(ctx, nw)).To(Succeed())
	}).Should(Succeed())
	eventually(g, nw, func(g Gomega) {
		g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.ControlPlaneEndpointReadyCondition)).To(BeNil())
	})
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: -1})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonInvalidRange)
	g.Expect(findRef(netbox.IPRanges, refOf("NetBoxIPPool", ns, "worker/default"))).To(BeEmpty())
	g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
	g.Expect(nw.Status.ControlPlaneEndpoint).To(Equal(vip))
	g.Expect(vipsOf(g, ns, "default")).To(HaveLen(1))
}

// A VIP added to a live network that has no room for it leaves the network Ready and its pool serving.
func TestPoolKeepsServingWhenVIPHasNoRoom(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, pool := readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 0, End: -1})
	first := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, first)).To(Succeed())
	address(g, first)

	g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
		nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{}
		g.Expect(k8s.Update(ctx, nw)).To(Succeed())
	}).Should(Succeed())
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonExhausted)
	waitCondition(g, nw, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Expect(nw.Status.ControlPlaneEndpoint).To(Equal(v1alpha1.APIEndpoint{}))
	g.Consistently(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
		g.Expect(meta.IsStatusConditionTrue(pool.Status.Conditions, v1alpha1.ReadyCondition)).To(BeTrue())
		g.Expect(rangeOf(pool, "default")).To(gstruct.PointTo(gstruct.MatchFields(gstruct.IgnoreExtras, gstruct.Fields{"Ready": BeTrue()})))
	}).Should(Succeed())

	second := newClaim(ns, "m2", "worker")
	g.Expect(k8s.Create(ctx, second)).To(Succeed())
	g.Expect(address(g, second).Spec.Address).NotTo(BeEmpty())
}

func TestPoolBadRangeDoesNotBlockOthers(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	other := newNetwork(ns, "other")
	g.Expect(k8s.Create(ctx, other)).To(Succeed())
	nw, pool := readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3})
	waitReady(g, other)
	p := netip.MustParsePrefix(nw.Status.Prefix)

	setRanges(g, pool,
		v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3},
		v1alpha1.IPPoolRange{NetworkName: "other", Start: 2, End: 40})
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonInvalidRange)
	waitReady(g, pool)
	g.Expect(pool.Status.Ranges).To(HaveLen(1))
	g.Expect(findRef(netbox.IPRanges, refOf("NetBoxIPPool", ns, "worker/other"))).To(BeEmpty())

	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	g.Expect([]string{addr(p, 2), addr(p, 3)}).To(ContainElement(address(g, claim).Spec.Address))
}

func TestPoolFailedEditKeepsServing(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, pool := readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3})
	p := netip.MustParsePrefix(nw.Status.Prefix)
	id := waitRange(g, pool, "default", addr(p, 2), addr(p, 3)).ID
	other := newPool(ns, "other", v1alpha1.IPPoolRange{NetworkName: "default", Start: 5, End: 6})
	g.Expect(k8s.Create(ctx, other)).To(Succeed())
	waitReady(g, other)

	for i, step := range []struct {
		end    int32
		reason string
	}{{6, v1alpha1.ReasonRangeOverlap}, {40, v1alpha1.ReasonInvalidRange}} {
		setRanges(g, pool, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: step.end})
		waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionFalse, step.reason)
		waitReady(g, pool)
		eventually(g, pool, func(g Gomega) {
			g.Expect(rangeOf(pool, "default")).To(gstruct.PointTo(gstruct.MatchFields(gstruct.IgnoreExtras, gstruct.Fields{
				"ID": Equal(id), "StartAddress": Equal(addr(p, 2)), "EndAddress": Equal(addr(p, 3)), "Ready": BeTrue(), "Reason": Equal(step.reason),
			})))
		})
		g.Expect(boundsInNetBox(g, id)).To(Equal(addr(p, 2) + "-" + addr(p, 3)))
		claim := newClaim(ns, fmt.Sprintf("m%d", i), "worker")
		g.Expect(k8s.Create(ctx, claim)).To(Succeed())
		g.Expect(address(g, claim).Spec.Address).To(Equal(addr(p, 2+i)), "the range serves its old bounds")
	}
}

func TestPoolBlockedShrinkReclaimsDeprecatedRange(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, pool := readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 5})
	p := netip.MustParsePrefix(nw.Status.Prefix)
	id := waitRange(g, pool, "default", addr(p, 2), addr(p, 5)).ID
	m1 := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, m1)).To(Succeed())
	g.Expect(address(g, m1).Spec.Address).To(Equal(addr(p, 2)))

	setPaused(t, ns, true)
	time.Sleep(time.Second) // let an in-flight reconcile finish
	g.Expect(admin.Patch(ctx, netbox.IPRanges, id, "", map[string]any{"status": "deprecated", "mark_populated": true})).Error().To(Succeed())
	setRanges(g, pool, v1alpha1.IPPoolRange{NetworkName: "default", Start: 3, End: 5})
	setPaused(t, ns, false)

	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonRangeInUse)
	g.Expect(waitRange(g, pool, "default", addr(p, 2), addr(p, 5)).ID).To(Equal(id))
	rng, _, err := admin.Get(ctx, netbox.IPRanges, id)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rng.Status.Value).To(Equal("active"))
	g.Expect(rng.MarkPopulated).To(BeFalse())
	m2 := newClaim(ns, "m2", "worker")
	g.Expect(k8s.Create(ctx, m2)).To(Succeed())
	g.Expect(address(g, m2).Spec.Address).To(Equal(addr(p, 3)))
}

func TestPoolRemovesRangeRetain(t *testing.T) {
	for name, clearStatus := range map[string]bool{"with status": false, "without status": true} {
		t.Run(name, func(t *testing.T) {
			g := newG(t)
			ns := newEnv(t, nil)
			nw, extra := newNetwork(ns, "default"), newNetwork(ns, "extra")
			g.Expect(k8s.Create(ctx, nw)).To(Succeed())
			g.Expect(k8s.Create(ctx, extra)).To(Succeed())
			waitReady(g, nw)
			waitReady(g, extra)
			p, e := netip.MustParsePrefix(nw.Status.Prefix), netip.MustParsePrefix(extra.Status.Prefix)
			pool := newPool(ns, "worker",
				v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3},
				v1alpha1.IPPoolRange{NetworkName: "extra", Start: 2, End: 3})
			g.Expect(k8s.Create(ctx, pool)).To(Succeed())
			id := waitRange(g, pool, "extra", addr(e, 2), addr(e, 3)).ID

			setPaused(t, ns, true)
			time.Sleep(time.Second) // let an in-flight reconcile finish
			setRanges(g, pool, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3})
			if clearStatus { // as clusterctl move leaves it
				g.Eventually(func(g Gomega) {
					g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
					pool.Status = v1alpha1.NetBoxIPPoolStatus{}
					g.Expect(k8s.Status().Update(ctx, pool)).To(Succeed())
				}).Should(Succeed())
			}
			setPaused(t, ns, false)

			g.Eventually(func(g Gomega) {
				rng, _, err := admin.Get(ctx, netbox.IPRanges, id)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(rng.Status.Value).To(Equal("deprecated"))
				g.Expect(rng.MarkPopulated).To(BeTrue())
			}).Should(Succeed())
			waitRange(g, pool, "default", addr(p, 2), addr(p, 3))
			eventually(g, pool, func(g Gomega) { g.Expect(rangeOf(pool, "extra")).To(BeNil()) })
		})
	}
}

func TestPoolRecreatedWithOtherOffsets(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, pool := readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 5})
	p := netip.MustParsePrefix(nw.Status.Prefix)
	id := waitRange(g, pool, "default", addr(p, 2), addr(p, 5)).ID
	g.Expect(k8s.Delete(ctx, pool)).To(Succeed())
	g.Eventually(gone(pool)).Should(BeTrue())

	pool = newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 3, End: 6})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	g.Expect(waitRange(g, pool, "default", addr(p, 3), addr(p, 6)).ID).To(Equal(id), "the deprecated range is taken back")
	rng, _, err := admin.Get(ctx, netbox.IPRanges, id)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rng.Status.Value).To(Equal("active"))
	g.Expect(boundsInNetBox(g, id)).To(Equal(addr(p, 3) + "-" + addr(p, 6)))
	g.Expect(k8s.Delete(ctx, pool)).To(Succeed())
	g.Eventually(gone(pool)).Should(BeTrue())

	other := newPool(ns, "other", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 7})
	g.Expect(k8s.Create(ctx, other)).To(Succeed())
	g.Expect(waitRange(g, other, "default", addr(p, 2), addr(p, 7)).ID).NotTo(Equal(id))
	_, _, err = admin.Get(ctx, netbox.IPRanges, id)
	g.Expect(err).To(MatchError(netbox.ErrNotFound), "our deprecated range in the way is deleted")
}

func TestPoolMetallbCarveOut(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	p := netip.MustParsePrefix(nw.Status.Prefix)
	workers := newPool(ns, "workers", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: -2})
	g.Expect(k8s.Create(ctx, workers)).To(Succeed())
	wid := waitRange(g, workers, "default", addr(p, 2), addr(p, -2)).ID

	metallb := newPool(ns, "metallb", v1alpha1.IPPoolRange{NetworkName: "default", Start: -4, End: -2})
	g.Expect(k8s.Create(ctx, metallb)).To(Succeed())
	waitCondition(g, metallb, metav1.ConditionFalse, v1alpha1.ReasonRangeOverlap)

	setRanges(g, workers, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: -5})
	mid := waitRange(g, metallb, "default", addr(p, -4), addr(p, -2)).ID
	waitReady(g, metallb)
	waitRange(g, workers, "default", addr(p, 2), addr(p, -5))
	g.Expect(boundsInNetBox(g, wid)).To(Equal(addr(p, 2) + "-" + addr(p, -5)))
	g.Expect(boundsInNetBox(g, mid)).To(Equal(addr(p, -4) + "-" + addr(p, -2)))
}

// A pool without the cluster-name label pauses with the Cluster of its network.
func TestPoolPausedByNetworkCluster(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	p := netip.MustParsePrefix(nw.Status.Prefix)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3})
	pool.Labels = nil
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	id := waitRange(g, pool, "default", addr(p, 2), addr(p, 3)).ID

	setPaused(t, ns, true)
	waitTypedCondition(g, pool, clusterv1.PausedCondition, metav1.ConditionTrue, clusterv1.PausedReason)
	setRanges(g, pool, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 5})
	g.Consistently(func() string { return boundsInNetBox(g, id) }).Should(Equal(addr(p, 2) + "-" + addr(p, 3)))

	setPaused(t, ns, false)
	waitTypedCondition(g, pool, clusterv1.PausedCondition, metav1.ConditionFalse, clusterv1.NotPausedReason)
	waitRange(g, pool, "default", addr(p, 2), addr(p, 5))
	g.Expect(boundsInNetBox(g, id)).To(Equal(addr(p, 2) + "-" + addr(p, 5)))
}

func TestOmitGateway(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 4})
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

// TestPoolsRaceForOverlappingRanges creates two pools at once: NetBox checks range overlap without a lock.
func TestPoolsRaceForOverlappingRanges(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	pools := []*v1alpha1.NetBoxIPPool{
		newPool(ns, "a", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 5}),
		newPool(ns, "b", v1alpha1.IPPoolRange{NetworkName: "default", Start: 4, End: 7}),
	}
	errs := make([]error, len(pools))
	var wg sync.WaitGroup
	for i, p := range pools {
		wg.Go(func() { errs[i] = k8s.Create(ctx, p) })
	}
	wg.Wait()
	g.Expect(errors.Join(errs...)).To(Succeed())

	g.Eventually(func(g Gomega) {
		var reasons []string
		for _, p := range pools {
			g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(p), p)).To(Succeed())
			c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.RangesReadyCondition)
			g.Expect(c).NotTo(BeNil())
			reasons = append(reasons, c.Reason)
		}
		g.Expect(reasons).To(ConsistOf(v1alpha1.ReasonReady, v1alpha1.ReasonRangeOverlap))
	}).Should(Succeed())
	g.Expect(admin.List(ctx, netbox.IPRanges, url.Values{"parent": {nw.Status.Prefix}})).To(HaveLen(1))
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

// rangeNetBox serves the NetBox range 7 of pool worker in network default, if o is not empty, and records the writes.
func rangeNetBox(t *testing.T, o string, writes *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(writesNeedLock(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method != http.MethodGet:
			b, _ := io.ReadAll(r.Body)
			*writes = append(*writes, r.Method+" "+r.URL.Path+" "+string(b))
			_, _ = io.WriteString(w, `{"id":8}`)
		case r.URL.Path == "/api/ipam/ip-ranges/7/" && o == "":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"detail":"Not found."}`)
		case r.URL.Path == "/api/ipam/ip-ranges/7/":
			w.Header().Set("ETag", `"e1"`)
			_, _ = io.WriteString(w, o)
		case r.URL.Path == "/api/ipam/ip-ranges/" && o != "":
			_, _ = io.WriteString(w, `{"results":[`+o+`]}`)
		default:
			_, _ = io.WriteString(w, `{"results":[]}`)
		}
	})))
	t.Cleanup(srv.Close)
	return srv
}

func rangeObject(start, end string, closed bool) string {
	return `{"id":7,"start_address":"` + start + `/22","end_address":"` + end + `/22","mark_populated":` + strconv.FormatBool(closed) +
		`,"status":{"value":"active"},` + textsOf("ns/worker // pool of cluster ns") + `,"custom_fields":{"capi_ref":"NetBoxIPPool/ns/worker/default"}}`
}

func poolOn(srv *httptest.Server) *NetBoxIPPoolReconciler {
	return &NetBoxIPPoolReconciler{Client: claimClient(), NetBox: &netbox.Client{URL: srv.URL, Token: "t"}, Keys: netbox.DefaultKeys, Recorder: events.NewFakeRecorder(10)}
}

// Only a range that is to change, or is not there yet, waits for the lock; one that is as it should be does not.
func TestPoolReconcileRangeTakesLockToWrite(t *testing.T) {
	nw := newNetwork("ns", "default")
	nw.Status.Prefix = "10.10.0.16/28"
	for name, tc := range map[string]struct {
		existing string
		end      int32
		writes   int
	}{
		"unchanged": {rangeObject("10.10.0.18", "10.10.0.19", false), 3, 0},
		"resized":   {rangeObject("10.10.0.18", "10.10.0.19", false), 4, 1},
		"created":   {"", 3, 1},
	} {
		var writes []string
		r := poolOn(rangeNetBox(t, tc.existing, &writes))
		var (
			rs  *v1alpha1.IPPoolRangeStatus
			err error
		)
		pool := newPool("ns", "worker")
		rg := v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: tc.end}
		if got := returnsWhileLocked(func() { rs, err = r.reconcileRange(ctx, pool, nw, rg, nil, nil) }); got != (tc.writes == 0) {
			t.Errorf("%s: finished while locked %t, want %t", name, got, tc.writes == 0)
		}
		if err != nil || rs == nil || !rs.Ready || len(writes) != tc.writes {
			t.Errorf("%s: status %+v, err %v, writes %v, want a ready range and %d writes", name, rs, err, writes, tc.writes)
		}
		if tc.writes > 0 && rs != nil && rs.EndAddress != "10.10.0."+map[int32]string{3: "19", 4: "20"}[tc.end] {
			t.Errorf("%s: range ends at %s", name, rs.EndAddress)
		}
	}
}

// A range the provider creates carries the owner and tenant of its network.
func TestPoolCreatesRangeWithNetworkTenant(t *testing.T) {
	nw := newNetwork("ns", "default")
	nw.Status.Prefix, nw.Status.OwnerID, nw.Status.TenantID = "10.10.0.16/28", 1, 7
	var writes []string
	r := poolOn(rangeNetBox(t, "", &writes))
	rg := v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3}
	if _, err := r.reconcileRange(ctx, newPool("ns", "worker"), nw, rg, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || !strings.Contains(writes[0], "POST /api/ipam/ip-ranges/ ") || !strings.Contains(writes[0], `"owner":1`) || !strings.Contains(writes[0], `"tenant":7`) ||
		!strings.Contains(writes[0], `"description":"ns/worker // pool of cluster ns"`) || !strings.Contains(writes[0], `"comments":"`+managedComments+`"`) {
		t.Errorf("writes %q, want one POST with owner 1, tenant 7 and the default description and comments", writes)
	}
}

// A new range whose templates do not render is created without texts and serves; the error is InvalidSpec.
func TestPoolCreatesRangeWithoutTexts(t *testing.T) {
	g := NewWithT(t)
	nw := newNetwork("ns", "default")
	nw.Status.Prefix = "10.10.0.16/28"
	var writes []string
	r := poolOn(rangeNetBox(t, "", &writes))
	pool := newPool("ns", "worker")
	pool.Spec.IPRange.Comments = failsOnDefault
	rs, err := r.reconcileRange(ctx, pool, nw, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3}, nil, nil)
	g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonInvalidSpec))
	g.Expect(err).To(MatchError(ContainSubstring("attributes of the range 10.10.0.18-10.10.0.19: ipRange: comments: ")))
	g.Expect(rs.Ready).To(BeTrue())
	g.Expect(writes).To(HaveLen(1))
	g.Expect(writes[0]).To(HavePrefix("POST /api/ipam/ip-ranges/ "))
	g.Expect(writes[0]).NotTo(Or(ContainSubstring(`"description"`), ContainSubstring(`"comments"`)))
}

// A range that is already gone from NetBox is released without the lock.
func TestPoolReleaseRangeTakesLockToWrite(t *testing.T) {
	for _, gone := range []bool{true, false} {
		o := ""
		if !gone {
			o = rangeObject("10.10.0.18", "10.10.0.19", false)
		}
		var writes []string
		r := poolOn(rangeNetBox(t, o, &writes))
		var err error
		listed := netbox.Object{ID: 7, Status: netbox.Choice{Value: "active"}}
		if got := returnsWhileLocked(func() { err = r.releaseRange(ctx, newPool("ns", "worker"), "default", listed, nil) }); got != gone {
			t.Errorf("gone %t: finished while locked %t", gone, got)
		}
		if want := map[bool]int{true: 0, false: 2}[gone]; err != nil || len(writes) != want {
			t.Errorf("gone %t: err %v, writes %q, want nil and %d", gone, err, writes, want)
		}
		if !gone && (!strings.Contains(writes[0], `"mark_populated":true`) || !strings.Contains(writes[1], `"status":"deprecated"`)) {
			t.Errorf("writes %q, want the range closed, then deprecated", writes)
		}
	}
}

// RangeReleased is recorded for a range the provider released, not for one that vanished in between.
func TestPoolReleaseRangeEventOnlyOnChange(t *testing.T) {
	for name, tc := range map[string]struct {
		policy v1alpha1.DeletionPolicy
		status int
		events int
	}{
		"deleted":    {v1alpha1.DeletionPolicyDelete, http.StatusOK, 1},
		"deprecated": {v1alpha1.DeletionPolicyRetain, http.StatusOK, 1},
		"gone":       {v1alpha1.DeletionPolicyRetain, http.StatusNotFound, 0},
	} {
		var vanished bool // the range is gone once the deprecating PATCH 404s
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			switch {
			case r.Method == http.MethodDelete:
				w.WriteHeader(http.StatusNoContent)
			case vanished || r.Method == http.MethodPatch && strings.Contains(string(b), `"status":"deprecated"`) && tc.status == http.StatusNotFound:
				vanished = true
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"detail":"Not found."}`)
			case r.Method == http.MethodPatch:
				_, _ = io.WriteString(w, `{"id":7}`)
			case r.URL.Path == "/api/ipam/ip-ranges/7/":
				w.Header().Set("ETag", `"e1"`)
				_, _ = io.WriteString(w, rangeObject("10.10.0.18", "10.10.0.19", false))
			default:
				_, _ = io.WriteString(w, `{"results":[]}`)
			}
		}))
		nw := newNetwork("ns", "default")
		nw.Spec.DeletionPolicy = tc.policy
		r := poolOn(srv)
		r.Client = claimClient(nw)

		err := r.releaseRange(ctx, newPool("ns", "worker"), "default", netbox.Object{ID: 7, Status: netbox.Choice{Value: "active"}}, nil)
		if ev := emitted(r.Recorder); err != nil || len(ev) != tc.events {
			t.Errorf("%s: err %v, events %q, want nil and %d", name, err, ev, tc.events)
		}
		srv.Close()
	}
}

// Whichever way a range section goes about its write, ErrWriteNeeded of the first pass comes out of it as it is:
// no wrapper turns it into another reason, a conflict or a success.
func TestPoolWriteNeededSurvivesTheSection(t *testing.T) {
	nw := newNetwork("ns", "default")
	nw.Status.Prefix = "10.10.0.16/28"
	start, end := netip.MustParseAddr("10.10.0.18"), netip.MustParseAddr("10.10.0.19")
	for name, tc := range map[string]struct {
		existing string
		end      netip.Addr
		held     []netip.Addr
	}{
		"create":                 {"", end, nil},
		"resize":                 {rangeObject("10.10.0.18", "10.10.0.19", false), start.Next().Next(), nil},
		"close before shrinking": {rangeObject("10.10.0.18", "10.10.0.19", false), start, nil},
		"keep a closed range":    {rangeObject("10.10.0.18", "10.10.0.19", true), start, []netip.Addr{end}},
	} {
		var writes []string
		r := poolOn(rangeNetBox(t, tc.existing, &writes))
		r.NetBox = r.NetBox.ReadOnly()
		rs, err := r.syncRange(ctx, newPool("ns", "worker"), nw, nil, start, tc.end, 22, tc.held)
		if !errors.Is(err, netbox.ErrWriteNeeded) || errReason(err) != v1alpha1.ReasonNetBoxError || len(writes) != 0 {
			t.Errorf("%s: status %+v, err %v (reason %s), writes %q, want ErrWriteNeeded and no writes", name, rs, err, errReason(err), writes)
		}
	}
}

// The range of a network is found by its key alone: no owner, no parent, so one moved by hand is found too.
func TestSyncRangeFindsByKey(t *testing.T) {
	g := NewWithT(t)
	var reqs []string
	rng := `{"id":7,"start_address":"10.10.0.18/22","end_address":"10.10.0.19/22","status":{"value":"active"},"owner":{"id":1},` +
		textsOf("ns/worker // pool of cluster ns") + `,"custom_fields":{"capi_ref":"NetBoxIPPool/ns/worker/default"}}`
	r := &NetBoxIPPoolReconciler{NetBox: objNetBox(t, netbox.IPRanges, rng, &reqs), Keys: netbox.DefaultKeys, Recorder: events.NewFakeRecorder(10)}
	nw := newNetwork("ns", "default")
	nw.Status.Prefix, nw.Status.OwnerID = "10.10.0.16/28", 1
	pool := newPool("ns", "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3})
	rs, err := r.syncRange(ctx, pool, nw, nil, netip.MustParseAddr("10.10.0.18"), netip.MustParseAddr("10.10.0.19"), 22, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs.ID).To(Equal(int64(7)))
	g.Expect(rs.Ready).To(BeTrue())
	g.Expect(reqs).To(Equal([]string{"GET /api/ipam/ip-ranges/?cf_capi_ref=NetBoxIPPool%2Fns%2Fworker%2Fdefault&limit=1000&ordering=id"}))
}

// failsOnDefault renders the sample values of validatePoolTemplates, but not network "default".
const failsOnDefault = `{{ if eq .Network "default" }}{{ slice .Network 0 99 }}{{ end }}`

// A range takes the owner and tenant of its network's spec and the description and comments of the pool's ipRange
// templates, under the lock, the change recorded as an Event; an unset owner or tenant is neither written nor cleared.
// A range that cannot be shrunk yet is synced too; templates that do not render leave the texts as they are and are
// InvalidSpec, the range serving.
func TestSyncRangeSyncsAttrs(t *testing.T) {
	g := NewWithT(t)
	pool := newPool("ns", "worker")
	pool.Spec.IPRange.Description = failsOnDefault
	g.Expect(validatePoolTemplates(pool)).To(Succeed())
	const msg = `"changelog_message":"capi: attributes of NetBoxIPPool/ns/worker/default"`
	for name, tc := range map[string]struct {
		owner, tenant int64
		ipRange       v1alpha1.IPRangeFields
		end           int32
		held          []netip.Addr
		writes        []string
		reason        string
	}{
		"unset":   {end: 3},
		"in sync": {owner: 5, tenant: 6, end: 3},
		"both":    {owner: 1, tenant: 7, end: 3, writes: []string{"PATCH /api/ipam/ip-ranges/7/ {" + msg + `,"owner":1,"tenant":7}`}},
		"shrink blocked": {owner: 1, end: 2, held: []netip.Addr{netip.MustParseAddr("10.10.0.19")}, reason: v1alpha1.ReasonRangeInUse,
			writes: []string{"PATCH /api/ipam/ip-ranges/7/ {" + msg + `,"owner":1}`}},
		"templates": {end: 3, ipRange: v1alpha1.IPRangeFields{Description: "{{ .Pool }} in {{ .Network }}", Comments: "c"},
			writes: []string{"PATCH /api/ipam/ip-ranges/7/ {" + msg + `,"comments":"c","description":"worker in default"}`}},
		"templates do not render": {owner: 1, end: 3, ipRange: v1alpha1.IPRangeFields{Description: "{{ .Owner }}"}, reason: v1alpha1.ReasonInvalidSpec,
			writes: []string{"PATCH /api/ipam/ip-ranges/7/ {" + msg + `,"owner":1}`}},
		"templates fail on real values": {end: 3, ipRange: v1alpha1.IPRangeFields{Description: failsOnDefault}, reason: v1alpha1.ReasonInvalidSpec},
	} {
		var writes []string
		o := `{"id":7,"start_address":"10.10.0.18/22","end_address":"10.10.0.19/22","status":{"value":"active"},"owner":{"id":5},"tenant":{"id":6},` +
			textsOf("ns/worker // pool of cluster ns") + `,"custom_fields":{"capi_ref":"NetBoxIPPool/ns/worker/default"}}`
		r := poolOn(rangeNetBox(t, o, &writes))
		nw := newNetwork("ns", "default")
		nw.Status.Prefix, nw.Status.OwnerID, nw.Status.TenantID = "10.10.0.16/28", tc.owner, tc.tenant
		rg := v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: tc.end}
		pool := newPool("ns", "worker")
		pool.Spec.IPRange = tc.ipRange
		rs, err := r.reconcileRange(ctx, pool, nw, rg, nil, tc.held)
		if tc.reason == "" {
			g.Expect(err).NotTo(HaveOccurred(), name)
		} else {
			g.Expect(errReason(err)).To(Equal(tc.reason), name)
		}
		if tc.reason == v1alpha1.ReasonInvalidSpec {
			g.Expect(err).To(MatchError(ContainSubstring("attributes of the range 10.10.0.18/22-10.10.0.19/22: ipRange: description: ")), name)
		}
		g.Expect(rs.Ready).To(BeTrue(), name)
		g.Expect(writes).To(Equal(tc.writes), name)
		ev := emitted(r.Recorder)
		g.Expect(ev).To(HaveLen(len(tc.writes)), name)
		for _, e := range ev {
			g.Expect(e).To(HavePrefix("Normal AttributesSynced "), name)
		}
	}
}

// A range whose owner and tenant the token may not set keeps serving: RangesReady is Forbidden and says so, Ready stays True.
func TestPoolRangeAttrsForbiddenKeepsServing(t *testing.T) {
	g := NewWithT(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"detail":"x"}`)
			return
		}
		if r.URL.Path == "/api/ipam/ip-ranges/" {
			_, _ = io.WriteString(w, `{"results":[{"id":7,"start_address":"10.10.0.18/22","end_address":"10.10.0.19/22","status":{"value":"active"},"owner":{"id":5},"custom_fields":{"capi_ref":"NetBoxIPPool/ns/worker/default"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	t.Cleanup(srv.Close)
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1alpha1.AddToScheme, clusterv1.AddToScheme, ipamv1.AddToScheme} {
		g.Expect(add(scheme)).To(Succeed())
	}
	nw := newNetwork("ns", "default")
	nw.Status.Prefix, nw.Status.OwnerID, nw.Status.Conditions = "10.10.0.16/28", 1, readyAt(0)
	pool := newPool("ns", "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3})
	pool.Finalizers = []string{poolFinalizer}
	pool.Status.Conditions = []metav1.Condition{{Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nw, pool).WithStatusSubresource(pool).
		WithIndex(&ipamv1.IPAddress{}, poolRefField, func(o client.Object) []string {
			a, ok := o.(*ipamv1.IPAddress)
			if !ok {
				return nil
			}
			return poolRefName(a.Spec.PoolRef)
		}).Build()
	r := poolOn(srv)
	r.Client = c

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: netBoxRequeueAfter}))
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
	g.Expect(pool.Status.Ranges).To(HaveLen(1))
	g.Expect(pool.Status.Ranges[0].Ready).To(BeTrue())
	g.Expect(pool.Status.Ranges[0].Reason).To(Equal(v1alpha1.ReasonForbidden))
	g.Expect(meta.IsStatusConditionTrue(pool.Status.Conditions, v1alpha1.ReadyCondition)).To(BeTrue())
	rr := meta.FindStatusCondition(pool.Status.Conditions, v1alpha1.RangesReadyCondition)
	g.Expect(rr).NotTo(BeNil())
	g.Expect(rr.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(rr.Reason).To(Equal(v1alpha1.ReasonForbidden))
	g.Expect(rr.Message).To(ContainSubstring("attributes of the range 10.10.0.18/22-10.10.0.19/22"))
}

// A network that cannot resolve its owner or tenant takes the ranges of its pools out of service like any network that is
// not Ready: nothing is written through them and a new claim gets no address.
func TestPoolRangeOfHeldNetworkLeavesService(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1alpha1.AddToScheme, clusterv1.AddToScheme, ipamv1.AddToScheme} {
		g.Expect(add(scheme)).To(Succeed())
	}
	for _, reason := range []string{v1alpha1.ReasonOwnerNotFound, v1alpha1.ReasonTenantNotFound} {
		nw := newNetwork("ns", "default")
		nw.Status.Prefix, nw.Status.OwnerID = "10.10.0.16/28", 1
		nw.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ReadyCondition, Status: metav1.ConditionFalse, Reason: reason}}
		pool := newPool("ns", "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3})
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
		var writes []string
		r := poolOn(rangeNetBox(t, rangeObject("10.10.0.18", "10.10.0.19", false), &writes))
		r.Client = c

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
		g.Expect(err).NotTo(HaveOccurred(), reason)
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(pool), pool)).To(Succeed())
		g.Expect(pool.Status.Ranges).To(HaveLen(1), reason)
		g.Expect(pool.Status.Ranges[0].Ready).To(BeFalse(), reason)
		g.Expect(writes).To(BeEmpty(), reason)

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
	a, b := newNetwork(ns, "a"), newNetwork(ns, "b")
	b.Spec.Owner, b.Spec.Tenant = &v1alpha1.OwnerRef{Name: z.ownerName}, &v1alpha1.TenantRef{Slug: z.tenantSlug}
	for _, nw := range []*v1alpha1.NetBoxNetwork{a, b} {
		g.Expect(k8s.Create(ctx, nw)).To(Succeed())
		waitReady(g, nw)
	}
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "a", Start: 2, End: 2}, v1alpha1.IPPoolRange{NetworkName: "b", Start: 2, End: 3})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	eventually(g, pool, func(g Gomega) { g.Expect(pool.Status.Ranges).To(HaveLen(2)) })
	for i, nw := range []*v1alpha1.NetBoxNetwork{a, b} {
		rng, _, err := admin.Get(ctx, netbox.IPRanges, pool.Status.Ranges[i].ID)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(attrsOf(rng)).To(Equal(networkAttrs(nw)), nw.Name)
	}
	g.Expect(networkAttrs(b)).To(Equal(netbox.Attrs{Owner: z.owner, Tenant: z.tenant}))
	g.Expect(networkAttrs(a)).To(Equal(netbox.Attrs{}), "no spec.owner, no spec.tenant: none written")

	m1 := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, m1)).To(Succeed())
	g.Expect(address(g, m1).Spec.Address).To(Equal(addr(netip.MustParsePrefix(a.Status.Prefix), 2)))
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

// The prefix, the VIP, the range and the address are created with the owner and tenant of the network's spec.
func TestAttrsOnEverything(t *testing.T) {
	g := newG(t)
	z := newZone(t, g)
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	nw.Spec.Owner, nw.Spec.Tenant = &v1alpha1.OwnerRef{Name: z.ownerName}, &v1alpha1.TenantRef{ID: z.tenant}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 4})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	address(g, claim)

	want := netbox.Attrs{Owner: z.owner, Tenant: z.tenant}
	g.Expect(networkAttrs(nw)).To(Equal(want))
	prefix, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
	g.Expect(err).NotTo(HaveOccurred())
	rng, _, err := admin.Get(ctx, netbox.IPRanges, pool.Status.Ranges[0].ID)
	g.Expect(err).NotTo(HaveOccurred())
	ips, err := findRef(netbox.IPAddresses, refOf("IPAddressClaim", ns, "m1"))
	g.Expect(err).NotTo(HaveOccurred())
	objs := append([]netbox.Object{prefix, rng}, append(vipsOf(g, ns, "default"), ips...)...)
	g.Expect(objs).To(HaveLen(4))
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
	nw := vipNetwork(ns, "default")
	nw.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3})
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
	seed(netbox.IPRanges, netbox.IPRanges, rangeRef(other, pool, "default"), map[string]any{"start_address": addr(p, 2) + "/22", "end_address": addr(p, 3) + "/22", "status": "active"})
	seed(netbox.IPAddresses, netbox.IPAddresses, other.Key("IPAddressClaim", claim), map[string]any{"address": addr(p, 2) + "/22", "status": "active"})

	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	address(g, claim)
	g.Eventually(func() string { return claimReady(g, claim) }).Should(Equal(readyTrue))
	ours := []struct{ path, ref string }{
		{netbox.Prefixes, keys.Key("NetBoxNetwork", nw)},
		{netbox.IPAddresses, vipRef(keys, nw)},
		{netbox.IPRanges, rangeRef(keys, pool, "default")},
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

// A new spec.tenant reaches the prefix, the VIP, the range and the address, each recorded as an Event; the owner stays.
func TestTenantChangeSynced(t *testing.T) {
	g := newG(t)
	z := newZone(t, g)
	next := byHand(t, g, netbox.Tenants, map[string]any{"name": z.tenantSlug + "-next", "slug": z.tenantSlug + "-next"}).ID
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	nw.Spec.Owner, nw.Spec.Tenant = &v1alpha1.OwnerRef{Name: z.ownerName}, &v1alpha1.TenantRef{Slug: z.tenantSlug}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 4})
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
		g.Expect(objs).To(HaveLen(4), "prefix, range, VIP and address")
		for _, o := range objs {
			g.Expect(attrsOf(o)).To(Equal(want), "object %d", o.ID)
		}
	}).Should(Succeed())
	for _, name := range []string{"default", "worker", "m1"} {
		waitEvent(g, ns, name, "AttributesSynced")
	}
}

// The prefix, the VIP, the range and the address get the default description and comments. Changed templates reach them,
// an edit by hand in NetBox is overwritten by the next reconcile. A template that does not render is InvalidSpec on the
// network or the pool; the claim stays Ready.
func TestTextsSynced(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 4})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	address(g, claim)
	g.Eventually(func() string { return claimReady(g, claim) }).Should(Equal(readyTrue))

	objects := []struct{ path, ref string }{
		{netbox.Prefixes, keys.Key("NetBoxNetwork", nw)},
		{netbox.IPAddresses, vipRef(keys, nw)},
		{netbox.IPRanges, rangeRef(keys, pool, "default")},
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
	textsAre(
		texts{ns + "/default // cluster " + ns, managedComments},
		texts{ns + "/default // " + ns + " control plane endpoint", managedComments},
		texts{ns + "/worker // pool of cluster " + ns, managedComments},
		texts{ns + "/m1 // " + ns + " worker", managedComments},
	)

	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) {
		s.Description, s.Comments = "{{ .Name }} prefix", "net {{ .Cluster }}"
		s.ControlPlaneEndpoint.Description, s.ControlPlaneEndpoint.Comments = "vip of {{ .Cluster }}", `{{ "" }}`
	})
	eventually(g, pool, func(g Gomega) {
		pool.Spec.IPRange = v1alpha1.IPRangeFields{Description: "{{ .Pool }} in {{ .Network }}", Comments: "range"}
		pool.Spec.IPAddress.Comments = "claim {{ .Claim }}"
		g.Expect(k8s.Update(ctx, pool)).To(Succeed())
	})
	want := []texts{{"default prefix", "net " + ns}, {"vip of " + ns, ""}, {"worker in default", "range"}, {ns + "/m1 // " + ns + " worker", "claim m1"}}
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
		pool.Spec.IPRange.Comments = "{{ .Owner }}"
		g.Expect(k8s.Update(ctx, pool)).To(Succeed())
	})
	waitCondition(g, pool, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec)
	g.Expect(meta.FindStatusCondition(pool.Status.Conditions, v1alpha1.ReadyCondition).Message).To(ContainSubstring("ipRange: comments"))
	g.Consistently(func() string { return claimReady(g, claim) }).Should(Equal(readyTrue))
	textsAre(want...)
}

// A description of more than 200 characters is cut to what NetBox stores and then compares equal: once the prefix, the VIP,
// the range and the address are in place, reconciling them again writes nothing.
func TestTextsSteadyState(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	long := strings.Repeat("ж", 250)
	nw := vipNetwork(ns, "default")
	nw.Spec.Description, nw.Spec.ControlPlaneEndpoint.Description = long, long
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 4})
	pool.Spec.IPRange.Description, pool.Spec.IPAddress.Description = long, long
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	address(g, claim)
	g.Eventually(func() string { return claimReady(g, claim) }).Should(Equal(readyTrue))

	objects := []struct{ path, ref string }{
		{netbox.Prefixes, keys.Key("NetBoxNetwork", nw)},
		{netbox.IPAddresses, vipRef(keys, nw)},
		{netbox.IPRanges, rangeRef(keys, pool, "default")},
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
		for _, o := range current(g) {
			g.Expect(o.Description).To(Equal(string([]rune(long)[:200])), "object %d", o.ID)
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
// take a new owner. That stops nothing: the network stays Ready with MetadataSynced Forbidden, the range keeps serving
// with RangesReady Forbidden, a new claim gets an address of the new owner, the allocated one stays Ready; nothing leaks.
// Back in the zone, deleting the claims, the pool and the network releases their objects by key.
func TestOwnerChangeForbidden(t *testing.T) {
	g := newG(t)
	z, z2 := newZone(t, g), newZone(t, g)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	nw.Spec.Owner, nw.Spec.DeletionPolicy = &v1alpha1.OwnerRef{Name: z.ownerName}, v1alpha1.DeletionPolicyDelete
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 4})
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
	g.Expect(meta.IsStatusConditionTrue(nw.Status.Conditions, v1alpha1.ReadyCondition)).To(BeTrue())
	waitTypedCondition(g, pool, v1alpha1.RangesReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonForbidden)
	g.Expect(meta.IsStatusConditionTrue(pool.Status.Conditions, v1alpha1.ReadyCondition)).To(BeTrue())
	m2 := newClaim(ns, "m2", "worker")
	g.Expect(k8s.Create(ctx, m2)).To(Succeed())
	address(g, m2)
	ips, err := findRef(netbox.IPAddresses, refOf("IPAddressClaim", ns, "m2"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ips).To(HaveLen(1))
	g.Expect(attrsOf(ips[0])).To(Equal(netbox.Attrs{Owner: z2.owner}))
	g.Consistently(func(g Gomega) {
		for path, n := range map[string]int{netbox.Prefixes: 1, netbox.IPRanges: 1, netbox.IPAddresses: 2} {
			g.Expect(ofNamespace(g, path, ns)).To(HaveLen(n), path)
		}
		for _, ref := range []string{refOf("NetBoxNetwork", ns, "default"), refOf("NetBoxIPPool", ns, "worker/default"), refOf("IPAddressClaim", ns, "m1")} {
			for _, path := range []string{netbox.Prefixes, netbox.IPRanges, netbox.IPAddresses} {
				objs, err := findRef(path, ref)
				g.Expect(err).NotTo(HaveOccurred())
				for _, o := range objs {
					g.Expect(attrsOf(o)).To(Equal(netbox.Attrs{Owner: z.owner}), ref)
				}
			}
		}
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

// A network whose owner NetBox does not have stops: a new range in it waits, a new claim waits, the claim it gave an
// address stays Ready. Without spec.owner the network serves again.
func TestHeldNetworkStopsNewRanges(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, _ := readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3})
	m1 := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, m1)).To(Succeed())
	address(g, m1)
	g.Eventually(func() string { return claimReady(g, m1) }).Should(Equal(readyTrue))

	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) { s.Owner = &v1alpha1.OwnerRef{Name: "missing-" + ns} })
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonOwnerNotFound)
	other := newPool(ns, "other", v1alpha1.IPPoolRange{NetworkName: "default", Start: 5, End: 6})
	g.Expect(k8s.Create(ctx, other)).To(Succeed())
	waitTypedCondition(g, other, v1alpha1.RangesReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonNetworkNotReady)
	m2 := newClaim(ns, "m2", "worker")
	g.Expect(k8s.Create(ctx, m2)).To(Succeed())
	g.Consistently(func(g Gomega) {
		ranges, err := findRef(netbox.IPRanges, refOf("NetBoxIPPool", ns, "other/default"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ranges).To(BeEmpty())
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
