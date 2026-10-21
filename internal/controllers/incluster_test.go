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
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	inclusterv1 "sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/layout"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

// inClusterNetwork is layoutNetwork with allocator InCluster.
func inClusterNetwork(ns, name string) *v1alpha1.NetBoxNetwork {
	nw := layoutNetwork(ns, name)
	nw.Spec.Allocator = v1alpha1.AllocatorInCluster
	return nw
}

// inClusterSpecOf is the spec of an InClusterIPPool of the network newNetwork makes.
func inClusterSpecOf(addresses string, excluded ...string) inclusterv1.InClusterIPPoolSpec {
	return inclusterv1.InClusterIPPoolSpec{
		Addresses: []string{addresses}, Prefix: 22, Gateway: "10.10.0.1", ExcludedAddresses: excluded,
		AddressReuseGracePeriodSeconds: ptr.To(int32(60)),
	}
}

// inClusterAddress is an IPAddress of the InClusterIPPool pool.
func inClusterAddress(ns, name, pool, address string) *ipamv1.IPAddress {
	return &ipamv1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: ipamv1.IPAddressSpec{
			Address: address, Prefix: ptr.To(int32(22)), Gateway: "10.10.0.1", ClaimRef: ipamv1.IPAddressClaimReference{Name: name},
			PoolRef: ipamv1.IPPoolReference{APIGroup: inclusterv1.GroupVersion.Group, Kind: "InClusterIPPool", Name: pool},
		},
	}
}

func inClusterPool(g Gomega, ns, name string) *inclusterv1.InClusterIPPool {
	pool := &inclusterv1.InClusterIPPool{}
	g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, pool)).To(Succeed())
	return pool
}

// inClusterClient is a client over objs that knows InClusterIPPool, with funcs in front.
func inClusterClient(funcs interceptor.Funcs, objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	utilruntime.Must(ipamv1.AddToScheme(scheme))
	utilruntime.Must(inclusterv1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

func TestInClusterServed(t *testing.T) {
	g := NewWithT(t)
	m := meta.NewDefaultRESTMapper(nil)
	served, err := inClusterServed(m)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(served).To(BeFalse())

	m.Add(inclusterv1.GroupVersion.WithKind("InClusterIPPool"), meta.RESTScopeNamespace)
	served, err = inClusterServed(m)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(served).To(BeTrue())
}

// The pool spans its ranges; the addresses between them and those in use are excluded, adjacent ones as one range.
func TestInClusterSpec(t *testing.T) {
	g := NewWithT(t)
	a := netip.MustParseAddr
	nw := newNetwork("ns", "default")
	pool := func(ranges ...layout.Span) layout.PlacedPool {
		return layout.PlacedPool{Pool: layout.Pool{Name: "workers", Rest: true}, Ranges: ranges}
	}
	g.Expect(inClusterSpec(nw, pool(layout.Span{From: a("10.10.0.2"), To: a("10.10.0.4")}, layout.Span{From: a("10.10.0.6"), To: a("10.10.0.9")}), []netip.Addr{a("10.10.0.8")})).
		To(Equal(inClusterSpecOf("10.10.0.2-10.10.0.9", "10.10.0.5", "10.10.0.8")))
	g.Expect(inClusterSpec(nw, pool(layout.Span{From: a("10.10.0.2"), To: a("10.10.0.4")}, layout.Span{From: a("10.10.0.7"), To: a("10.10.0.9")}), []netip.Addr{a("10.10.0.8"), a("10.10.0.4")})).
		To(Equal(inClusterSpecOf("10.10.0.2-10.10.0.9", "10.10.0.4-10.10.0.6", "10.10.0.8")))
	g.Expect(inClusterSpec(nw, pool(layout.Span{From: a("10.10.0.2"), To: a("10.10.0.9")}), nil)).To(Equal(inClusterSpecOf("10.10.0.2-10.10.0.9")))
}

func TestAddressSpans(t *testing.T) {
	for in, want := range map[string]string{
		"10.10.0.2-10.10.0.4":        "10.10.0.2-10.10.0.4",
		"10.10.0.8/30,10.10.0.2":     "10.10.0.8-10.10.0.11 10.10.0.2",
		"10.10.0.5":                  "10.10.0.5",
		"10.10.0.5,api.example.test": "",
		"":                           "",
	} {
		sps, ok := addressSpans(strings.Split(in, ","))
		var got []string
		for _, sp := range sps {
			got = append(got, spanString(sp))
		}
		if g := map[bool]string{true: strings.Join(got, " ")}[ok]; g != want {
			t.Errorf("%q: %q, want %q", in, g, want)
		}
	}
}

// noInClusterPools fails t on any access to an InClusterIPPool.
func noInClusterPools(t *testing.T) interceptor.Funcs {
	t.Helper()
	deny := func(obj runtime.Object) error {
		switch obj.(type) {
		case *inclusterv1.InClusterIPPool, *inclusterv1.InClusterIPPoolList:
			t.Errorf("access to %T", obj)
			return errors.New("no InClusterIPPool")
		}
		return nil
	}
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			return errors.Join(deny(obj), c.Get(ctx, key, obj, opts...))
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return errors.Join(deny(list), c.List(ctx, list, opts...))
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			return errors.Join(deny(obj), c.Create(ctx, obj, opts...))
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			return errors.Join(deny(obj), c.Update(ctx, obj, opts...))
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return errors.Join(deny(obj), c.Delete(ctx, obj, opts...))
		},
	}
}

// Without the CRD a network with allocator InCluster is AllocatorUnavailable before it touches an InClusterIPPool or
// NetBox, and its deletion leaves InClusterIPPools alone.
func TestInClusterUnavailable(t *testing.T) {
	g := NewWithT(t)
	var writes []string
	r := networkOn(rangeNetBox(t, "", nil, &writes))
	r.Client = inClusterClient(noInClusterPools(t))
	nw := inClusterNetwork("ns", "default")
	nw.Status.Prefix = "10.10.0.16/28"

	err := r.reconcileLayout(ctx, nw, "")
	g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonAllocatorUnavailable))
	g.Expect(err).To(MatchError(ContainSubstring("install cluster-api-ipam-provider-in-cluster and restart the provider")))
	g.Expect(writes).To(BeEmpty())
	g.Expect(nw.Status.Layout.Pools).To(BeEmpty())

	g.Expect(r.reconcileDelete(ctx, nw, nil)).To(Succeed())
}

// With the CRD served since the start (in-cluster installed later, no restart), the deletion of such a network waits
// for the restart and releases nothing in NetBox: its InClusterIPPools may still hand out addresses.
func TestInClusterDeletionWaitsForRestart(t *testing.T) {
	g := NewWithT(t)
	var writes []string
	r := networkOn(rangeNetBox(t, rangeObject("10.10.0.16", "10.10.0.29", true), nil, &writes))
	r.Client = inClusterClient(interceptor.Funcs{})
	m, ok := r.RESTMapper().(*meta.DefaultRESTMapper)
	g.Expect(ok).To(BeTrue())
	m.Add(inclusterv1.GroupVersion.WithKind("InClusterIPPool"), meta.RESTScopeNamespace)
	nw := inClusterNetwork("ns", "default")
	nw.Status.Prefix = "10.10.0.16/28"

	err := r.reconcileDelete(ctx, nw, nil)
	g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonAllocatorUnavailable))
	g.Expect(err).To(MatchError(ContainSubstring("restart the provider")))
	g.Expect(writes).To(BeEmpty())
}

// reserveObject is the reserved address index of group metallb of network default at a.
func reserveObject(id, index int, a string) string {
	return fmt.Sprintf(`{"id":%d,"address":"%s/22","status":{"value":"active"},"dns_name":"ns-metallb-%d.example.test",%s,"custom_fields":{"capi_ref":"NetBoxNetwork/ns/default/addresses/metallb/%d"}}`,
		id, a, index, textsOf(fmt.Sprintf("ns/default // ns metallb %d", index)), index)
}

// metallbGrows is network default with allocator InCluster laid out in 10.10.0.16/28 as workers rest (range 7) and
// metallb 2, whose spec asks for metallb 3, and its InClusterIPPool workers as that layout made it.
func metallbGrows() (*v1alpha1.NetBoxNetwork, *inclusterv1.InClusterIPPool) {
	nw := newNetwork("ns", "default")
	nw.Spec.Allocator = v1alpha1.AllocatorInCluster
	nw.Spec.Pools = []v1alpha1.NetworkPool{netPool("workers", 0)}
	nw.Spec.Addresses = []v1alpha1.AddressGroup{metallb(3)}
	nw.Status.Prefix = "10.10.0.16/28"
	nw.Status.Layout = v1alpha1.NetworkLayout{
		Pools: []v1alpha1.PoolLayout{{Name: "workers", Size: intstr.FromString("rest"),
			Ranges: []v1alpha1.LayoutRange{{Start: "10.10.0.16", End: "10.10.0.29", ID: 7}}}},
		Addresses: []v1alpha1.AddressGroupLayout{{Name: "metallb", Count: 2, Addresses: []string{"10.10.0.31", "10.10.0.30"}}},
	}
	pool := &inclusterv1.InClusterIPPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "workers",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(nw, v1alpha1.GroupVersion.WithKind("NetBoxNetwork"))}},
		Spec: inClusterSpecOf("10.10.0.16-10.10.0.29"),
	}
	return nw, pool
}

// metallbNetBox serves range 7 (closed to allocation) and the two reserves of metallbGrows, and records the writes.
func metallbNetBox(t *testing.T, writes *[]string) *NetBoxNetworkReconciler {
	t.Helper()
	reserves := reserveObject(1, 1, "10.10.0.31") + "," + reserveObject(2, 2, "10.10.0.30")
	return networkOn(rangeNetBox(t, rangeObject("10.10.0.16", "10.10.0.29", true), func(int32) string { return reserves }, writes))
}

// The in-cluster webhook refuses a pool that gives up an allocated address: RangeInUse with its text, NetBox untouched.
func TestInClusterRejectedUpdateStopsNetBox(t *testing.T) {
	g := NewWithT(t)
	var writes []string
	r := metallbNetBox(t, &writes)
	nw, pool := metallbGrows()
	r.InCluster = true
	r.Client = inClusterClient(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*inclusterv1.InClusterIPPool); ok {
				return apierrors.NewBadRequest("pool addresses do not contain allocated addresses: [10.10.0.29]")
			}
			return c.Update(ctx, obj, opts...)
		},
	}, pool)

	err := r.reconcileLayout(ctx, nw, "")
	g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonRangeInUse))
	g.Expect(err).To(MatchError(ContainSubstring("pool addresses do not contain allocated addresses: [10.10.0.29]")))
	g.Expect(writes).To(BeEmpty())
	g.Expect(nw.Status.Layout.Addresses[0].Addresses).To(HaveLen(2))
}

// A larger count shrinks the InClusterIPPool first, then the range in NetBox, then reserves the address.
func TestInClusterApplyOrder(t *testing.T) {
	g := NewWithT(t)
	var writes []string
	r := metallbNetBox(t, &writes)
	nw, pool := metallbGrows()
	r.InCluster = true
	r.Client = inClusterClient(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			writes = append(writes, fmt.Sprintf("UPDATE %T %s", obj, obj.GetName()))
			return c.Update(ctx, obj, opts...)
		},
	}, pool)

	g.Expect(r.reconcileLayout(ctx, nw, "")).To(Succeed())
	g.Expect(writes).To(HaveLen(3))
	g.Expect(writes[0]).To(Equal("UPDATE *v1alpha2.InClusterIPPool workers"))
	g.Expect(writes[1]).To(Equal(`PATCH /api/ipam/ip-ranges/7/ {"changelog_message":"capi: resized by NetBoxNetwork/ns/default/pool/workers","end_address":"10.10.0.28/22","mark_populated":true,"start_address":"10.10.0.16/22","status":"active"}`))
	g.Expect(writes[2]).To(HavePrefix("POST /api/ipam/ip-addresses/ "))
	g.Expect(writes[2]).To(ContainSubstring(`"address":"10.10.0.29/22"`))
	got := &inclusterv1.InClusterIPPool{}
	g.Expect(r.Get(ctx, client.ObjectKeyFromObject(pool), got)).To(Succeed())
	g.Expect(got.Spec).To(Equal(inClusterSpecOf("10.10.0.16-10.10.0.28")))
	g.Expect(nw.Status.Layout.Addresses[0].Addresses).To(Equal([]string{"10.10.0.31", "10.10.0.30", "10.10.0.29"}))
}

// ownedPool is the InClusterIPPool name of nw over addresses.
func ownedPool(nw *v1alpha1.NetBoxNetwork, name, addresses string) *inclusterv1.InClusterIPPool {
	return &inclusterv1.InClusterIPPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: nw.Namespace, Name: name,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(nw, v1alpha1.GroupVersion.WithKind("NetBoxNetwork"))}},
		Spec: inClusterSpecOf(addresses),
	}
}

// A pool that gives up addresses goes before the one that takes them: no address is in two pools at once.
func TestInClusterShrinksFirst(t *testing.T) {
	g := NewWithT(t)
	a := netip.MustParseAddr
	nw := inClusterNetwork("ns", "default")
	owned := func(name, addresses string) *inclusterv1.InClusterIPPool { return ownedPool(nw, name, addresses) }
	var updates []string
	r := &NetBoxNetworkReconciler{Recorder: events.NewFakeRecorder(10), Client: inClusterClient(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			updates = append(updates, obj.GetName())
			return c.Update(ctx, obj, opts...)
		},
	}, owned("cp", "10.10.0.16-10.10.0.18"), owned("workers", "10.10.0.19-10.10.0.31"))}
	l := layout.Layout{Pools: []layout.PlacedPool{
		{Pool: layout.Pool{Name: "cp", Size: 4}, Ranges: []layout.Span{{From: a("10.10.0.16"), To: a("10.10.0.19")}}},
		{Pool: layout.Pool{Name: "workers", Rest: true}, Ranges: []layout.Span{{From: a("10.10.0.20"), To: a("10.10.0.31")}}},
	}}
	g.Expect(r.applyInClusterPools(ctx, nw, l, layout.Space{}, nil)).To(Succeed())
	g.Expect(updates).To(Equal([]string{"workers", "cp"}))
}

// A network created again under its name takes its pool: the controller reference gets its UID, which the garbage
// collector goes by, in one update.
func TestInClusterPoolTakesNewUID(t *testing.T) {
	g := NewWithT(t)
	old := inClusterNetwork("ns", "default")
	old.UID = "00000000-0000-0000-0000-00000000000a"
	nw := inClusterNetwork("ns", "default")
	nw.UID = "00000000-0000-0000-0000-00000000000b"
	var updates int
	r := &NetBoxNetworkReconciler{Recorder: events.NewFakeRecorder(10), Client: inClusterClient(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			updates++
			return c.Update(ctx, obj, opts...)
		},
	}, ownedPool(old, "workers", "10.10.0.16-10.10.0.31"))}
	l := layout.Layout{Pools: []layout.PlacedPool{{Pool: layout.Pool{Name: "workers", Rest: true},
		Ranges: []layout.Span{{From: netip.MustParseAddr("10.10.0.16"), To: netip.MustParseAddr("10.10.0.31")}}}}}

	for range 2 {
		g.Expect(r.applyInClusterPools(ctx, nw, l, layout.Space{}, nil)).To(Succeed())
	}
	g.Expect(updates).To(Equal(1))
	pool := &inclusterv1.InClusterIPPool{}
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "workers"}, pool)).To(Succeed())
	g.Expect(metav1.IsControlledBy(pool, nw)).To(BeTrue())
	g.Expect(pool.OwnerReferences).To(HaveLen(1))
	g.Expect(pool.Spec).To(Equal(inClusterSpecOf("10.10.0.16-10.10.0.31")))
}

// A pool without a controller is adopted in one update: the controller reference, the spec, and the watch label of
// the network, which the watch filters of this provider and of in-cluster go by. Its other labels stay.
func TestInClusterPoolAdoptedTakesWatchLabel(t *testing.T) {
	g := NewWithT(t)
	nw := inClusterNetwork("ns", "default")
	nw.UID = "00000000-0000-0000-0000-00000000000b"
	nw.Labels = map[string]string{clusterv1.WatchLabel: "mine"}
	chart := chartPool("ns", "workers", "10.10.0.16-10.10.0.31")
	chart.Labels = map[string]string{"app": "chart"}
	var updates int
	r := &NetBoxNetworkReconciler{Recorder: events.NewFakeRecorder(10), Client: inClusterClient(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			updates++
			return c.Update(ctx, obj, opts...)
		},
	}, chart)}
	l := layout.Layout{Pools: []layout.PlacedPool{{Pool: layout.Pool{Name: "workers", Rest: true},
		Ranges: []layout.Span{{From: netip.MustParseAddr("10.10.0.16"), To: netip.MustParseAddr("10.10.0.31")}}}}}

	for range 2 {
		g.Expect(r.applyInClusterPools(ctx, nw, l, layout.Space{}, nil)).To(Succeed())
	}
	g.Expect(updates).To(Equal(1))
	pool := &inclusterv1.InClusterIPPool{}
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "workers"}, pool)).To(Succeed())
	g.Expect(metav1.IsControlledBy(pool, nw)).To(BeTrue())
	g.Expect(pool.Labels).To(Equal(map[string]string{"app": "chart", clusterv1.WatchLabel: "mine"}))
	g.Expect(pool.Spec).To(Equal(inClusterSpecOf("10.10.0.16-10.10.0.31")))
	g.Expect(emitted(r.Recorder)).To(ConsistOf("Normal PoolAdopted Adopted the InClusterIPPool workers"))
}

// The in-cluster webhook refuses to delete a pool with allocated addresses: the network keeps WaitingForPools with
// its message, checked again every minute, deletes the other pools meanwhile and releases nothing in NetBox.
func TestInClusterDeletionRefused(t *testing.T) {
	g := NewWithT(t)
	var writes []string
	r := networkOn(rangeNetBox(t, "", nil, &writes))
	r.InCluster = true
	nw := inClusterNetwork("ns", "default")
	nw.Status.Prefix = "10.10.0.16/28"
	nw.Finalizers = []string{networkFinalizer}
	const refusal = "Pool has IPAddresses allocated. Cannot delete Pool until all IPAddresses have been removed."
	r.Client = inClusterClient(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*inclusterv1.InClusterIPPool); ok && obj.GetName() == "cp" {
				return apierrors.NewBadRequest(refusal)
			}
			return c.Delete(ctx, obj, opts...)
		},
	}, ownedPool(nw, "cp", "10.10.0.16-10.10.0.18"), ownedPool(nw, "workers", "10.10.0.19-10.10.0.31"))

	err := r.reconcileDelete(ctx, nw, nil)
	g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonWaitingForPools))
	g.Expect(err).To(MatchError("InClusterIPPool cp: " + refusal))
	g.Expect(toResult(err)).To(Equal(ctrl.Result{RequeueAfter: netBoxRequeueAfter}))
	g.Expect(apierrors.IsNotFound(r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "workers"}, &inclusterv1.InClusterIPPool{}))).To(BeTrue())
	g.Expect(r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "cp"}, &inclusterv1.InClusterIPPool{})).To(Succeed())
	g.Expect(writes).To(BeEmpty())
	g.Expect(nw.Finalizers).To(ConsistOf(networkFinalizer))
}

func TestInClusterPoolsCreated(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := inClusterNetwork(ns, "default")
	p := readyLayout(g, nw)
	for name, want := range map[string]inclusterv1.InClusterIPPoolSpec{
		"cp":      inClusterSpecOf(span(p, 0, 2)),
		"workers": inClusterSpecOf(span(p, 3, 15)),
	} {
		pool := inClusterPool(g, ns, name)
		g.Expect(pool.Spec).To(Equal(want), name)
		g.Expect(metav1.IsControlledBy(pool, nw)).To(BeTrue(), name)
		for _, lr := range rangesIn(g, nw, name) {
			rng, _, err := admin.Get(ctx, netbox.IPRanges, lr.ID)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(rng.MarkPopulated).To(BeTrue(), name)
			g.Expect(rng.Status.Value).To(Equal("active"), name)
		}
	}
	waitEvent(g, ns, "default", "InClusterIPPoolCreated")
}

// An address taken in NetBox inside a placed pool is excluded from it; once allocated it stays in it, AddressConflict.
func TestInClusterLiveExclusion(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := inClusterNetwork(ns, "default")
	p := readyLayout(g, nw)
	ranges := rangesIn(g, nw, "workers")

	// NetBox refuses to create an address inside a populated range, not to move one there.
	ip := byHand(t, g, netbox.IPAddresses, map[string]any{"address": "198.51.100.10/24", "status": "active"})
	_, err := admin.Patch(ctx, netbox.IPAddresses, ip.ID, "", map[string]any{"address": addr(p, 8) + "/22"})
	g.Expect(err).NotTo(HaveOccurred())
	touch(g, nw)
	g.Eventually(func(g Gomega) {
		g.Expect(inClusterPool(g, ns, "workers").Spec).To(Equal(inClusterSpecOf(span(p, 3, 15), addr(p, 8))))
	}).Should(Succeed())
	g.Expect(rangesIn(g, nw, "workers")).To(Equal(ranges))

	g.Expect(k8s.Create(ctx, inClusterAddress(ns, "m1", "workers", addr(p, 8)))).To(Succeed())
	touch(g, nw)
	waitTypedCondition(g, nw, v1alpha1.LayoutReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonAddressConflict)
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.LayoutReadyCondition).Message).To(ContainSubstring(addr(p, 8)))
	g.Eventually(func(g Gomega) {
		g.Expect(inClusterPool(g, ns, "workers").Spec).To(Equal(inClusterSpecOf(span(p, 3, 15))))
	}).Should(Succeed())
}

// A larger count takes the top of workers while its start is allocated; not while its end is.
func TestInClusterCountChange(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := inClusterNetwork(ns, "default")
	nw.Spec.Addresses = []v1alpha1.AddressGroup{metallb(2)}
	p := readyLayout(g, nw)
	g.Expect(inClusterPool(g, ns, "workers").Spec).To(Equal(inClusterSpecOf(span(p, 3, 13))))
	g.Expect(k8s.Create(ctx, inClusterAddress(ns, "m1", "workers", addr(p, 3)))).To(Succeed())

	setGroups(g, nw, metallb(3))
	waitLayoutReady(g, nw)
	g.Expect(inClusterPool(g, ns, "workers").Spec).To(Equal(inClusterSpecOf(span(p, 3, 12))))
	g.Expect(spans(rangesIn(g, nw, "workers"))).To(Equal([]string{span(p, 3, 12)}))
	g.Expect(reserveAddrs(g, nw, "metallb")).To(Equal(at(p, 15, 14, 13)))

	g.Expect(k8s.Create(ctx, inClusterAddress(ns, "m2", "workers", addr(p, 12)))).To(Succeed())
	before := inClusterPool(g, ns, "workers")
	setGroups(g, nw, metallb(4))
	waitTypedCondition(g, nw, v1alpha1.LayoutReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonRangeInUse)
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.LayoutReadyCondition).Message).To(ContainSubstring(addr(p, 12)))
	g.Expect(inClusterPool(g, ns, "workers").ResourceVersion).To(Equal(before.ResourceVersion))
	g.Expect(reservesOf(g, nw, "metallb")).To(HaveLen(3))
}

func TestInClusterPoolOfAnotherNetwork(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	other := &inclusterv1.InClusterIPPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cp", OwnerReferences: []metav1.OwnerReference{{
			APIVersion: v1alpha1.GroupVersion.String(), Kind: "NetBoxNetwork", Name: "other", UID: types.UID("00000000-0000-0000-0000-000000000001"), Controller: ptr.To(true),
		}}},
		Spec: inClusterSpecOf("10.10.3.250-10.10.3.251"),
	}
	g.Expect(k8s.Create(ctx, other)).To(Succeed())

	nw := inClusterNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitTypedCondition(g, nw, v1alpha1.LayoutReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonDuplicate)
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.LayoutReadyCondition).Message).To(ContainSubstring("InClusterIPPool cp exists and is not managed by network default"))
	g.Consistently(func(g Gomega) {
		g.Expect(inClusterPool(g, ns, "cp").ResourceVersion).To(Equal(other.ResourceVersion))
	}).Should(Succeed())
}

// A network created again after its status was lost finds its layout in NetBox and keeps its pools: the same spec
// and NetBox ranges, the controller reference on the new UID.
func TestInClusterPoolsKeptAfterStatusLoss(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := inClusterNetwork(ns, "default")
	readyLayout(g, nw)
	specs, ranges := map[string]inclusterv1.InClusterIPPoolSpec{}, map[string][]v1alpha1.LayoutRange{}
	for _, name := range []string{"cp", "workers"} {
		specs[name], ranges[name] = inClusterPool(g, ns, name).Spec, rangesIn(g, nw, name)
	}
	oldUID := nw.UID

	forget(g, nw)
	nw = inClusterNetwork(ns, "default")
	readyLayout(g, nw)
	g.Expect(nw.UID).NotTo(Equal(oldUID))
	for name, spec := range specs {
		pool := inClusterPool(g, ns, name)
		g.Expect(pool.Spec).To(Equal(spec), name)
		g.Expect(metav1.GetControllerOf(pool).UID).To(Equal(nw.UID), name)
		g.Expect(rangesIn(g, nw, name)).To(Equal(ranges[name]), name)
	}
}

// chartPool is the InClusterIPPool name over addresses as a chart renders it: no owner, nothing excluded.
func chartPool(ns, name, addresses string) *inclusterv1.InClusterIPPool {
	return &inclusterv1.InClusterIPPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: inclusterv1.InClusterIPPoolSpec{
			Addresses: []string{addresses}, Prefix: 22, Gateway: "10.10.0.1", AllocateReservedIPAddresses: true,
			AddressReuseGracePeriodSeconds: ptr.To(int32(0)),
		},
	}
}

// migratedPrefix is the prefix of the cluster migrateNetwork takes over.
var migratedPrefix = netip.MustParsePrefix("10.10.3.0/28")

// migrateNetwork renders the pools cp (offsets 2-4) and workers (5-12) of a running cluster in migratedPrefix, changed
// by edit, and creates network default of allocator InCluster over it, with pools cp and workers of the sizes given.
func migrateNetwork(t *testing.T, g *WithT, ns string, cp, workers int32, edit ...func(map[string]*inclusterv1.InClusterIPPool)) (*v1alpha1.NetBoxNetwork, map[string]*inclusterv1.InClusterIPPool) {
	t.Helper()
	p := migratedPrefix
	pools := map[string]*inclusterv1.InClusterIPPool{"cp": chartPool(ns, "cp", span(p, 2, 4)), "workers": chartPool(ns, "workers", span(p, 5, 12))}
	for _, f := range edit {
		f(pools)
	}
	for _, pool := range pools {
		g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	}
	nw := inClusterNetwork(ns, "default")
	nw.Spec.PrefixLength, nw.Spec.Prefix = 0, p.String()
	nw.Spec.Pools = []v1alpha1.NetworkPool{netPool("cp", cp), netPool("workers", workers)}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	return nw, pools
}

// The InClusterIPPools of a running cluster are adopted where they are: the network controls them, rewrites their
// spec, creates their ranges in NetBox and records their positions in status.layout.
func TestInClusterPoolAdopted(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	p := migratedPrefix
	nw, _ := migrateNetwork(t, g, ns, 3, 8)
	waitLayoutReady(g, nw)
	cp, workers := rangesIn(g, nw, "cp"), rangesIn(g, nw, "workers")
	g.Expect(spans(cp)).To(Equal([]string{span(p, 2, 4)}))
	g.Expect(spans(workers)).To(Equal([]string{span(p, 5, 12)}))
	g.Expect(nw.Status.Layout.Pools).To(Equal([]v1alpha1.PoolLayout{
		{Name: "cp", Size: intstr.FromInt32(3), Ranges: cp},
		{Name: "workers", Size: intstr.FromInt32(8), Ranges: workers},
	}))
	for name, want := range map[string]string{"cp": span(p, 2, 4), "workers": span(p, 5, 12)} {
		pool := inClusterPool(g, ns, name)
		g.Expect(metav1.IsControlledBy(pool, nw)).To(BeTrue(), name)
		g.Expect(pool.Spec).To(Equal(inClusterSpecOf(want)), name)
	}
	waitEvent(g, ns, "default", "PoolAdopted")
}

// Sizes other than those of the InClusterIPPools found are InvalidSpec with the sizes found; the pools stay as they are.
func TestInClusterAdoptSizeMismatch(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, pools := migrateNetwork(t, g, ns, 3, 9)
	waitTypedCondition(g, nw, v1alpha1.LayoutReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec)
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.LayoutReadyCondition).Message).To(ContainSubstring("cp 3, workers 8"))
	g.Consistently(func(g Gomega) {
		for name, pool := range pools {
			g.Expect(inClusterPool(g, ns, name).ResourceVersion).To(Equal(pool.ResourceVersion), name)
		}
		g.Expect(ofNamespace(g, netbox.IPRanges, ns)).To(BeEmpty())
	}).Should(Succeed())
}

// A NetBox address without a key inside an adopted pool is excluded from it: its range splits around it and the
// size of the pool counts without it.
func TestInClusterAdoptTakenInsideSpan(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	p := migratedPrefix
	byHand(t, g, netbox.IPAddresses, map[string]any{"address": addr(p, 8) + "/22", "status": "active"})
	nw, _ := migrateNetwork(t, g, ns, 3, 7)
	waitLayoutReady(g, nw)
	g.Expect(spans(rangesIn(g, nw, "workers"))).To(Equal([]string{span(p, 5, 7), span(p, 9, 12)}))
	g.Expect(inClusterPool(g, ns, "workers").Spec).To(Equal(inClusterSpecOf(span(p, 5, 12), addr(p, 8))))
}

// An adopted pool of several addresses takes the free ones of each: those between them stay excluded and do not count.
func TestInClusterAdoptSeveralAddresses(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	p := migratedPrefix
	nw, _ := migrateNetwork(t, g, ns, 3, 6, func(pools map[string]*inclusterv1.InClusterIPPool) {
		pools["workers"].Spec.Addresses = []string{addr(p, 12), span(p, 5, 7), addr(p, 10) + "/31"}
	})
	waitLayoutReady(g, nw)
	g.Expect(spans(rangesIn(g, nw, "workers"))).To(Equal([]string{span(p, 5, 7), span(p, 10, 12)}))
	g.Expect(inClusterPool(g, ns, "workers").Spec).To(Equal(inClusterSpecOf(span(p, 5, 12), span(p, 8, 9))))
}

// The excludedAddresses of an adopted pool stay excluded: its ranges split around them and the size counts without them.
func TestInClusterAdoptKeepsExcluded(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	p := migratedPrefix
	nw, _ := migrateNetwork(t, g, ns, 3, 7, func(pools map[string]*inclusterv1.InClusterIPPool) {
		pools["workers"].Spec.ExcludedAddresses = []string{addr(p, 8)}
	})
	waitLayoutReady(g, nw)
	g.Expect(spans(rangesIn(g, nw, "workers"))).To(Equal([]string{span(p, 5, 7), span(p, 9, 12)}))
	g.Expect(inClusterPool(g, ns, "workers").Spec).To(Equal(inClusterSpecOf(span(p, 5, 12), addr(p, 8))))
}

// InClusterIPPools whose addresses overlap are InvalidSpec, whatever the sizes: two pools would hand out one address.
// Nothing changes.
func TestInClusterAdoptOverlapRefused(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	p := migratedPrefix
	nw, pools := migrateNetwork(t, g, ns, 4, 8, func(pools map[string]*inclusterv1.InClusterIPPool) {
		pools["cp"].Spec.Addresses = []string{span(p, 2, 5)}
	})
	waitTypedCondition(g, nw, v1alpha1.LayoutReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec)
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.LayoutReadyCondition).Message).To(ContainSubstring("InClusterIPPools cp and workers overlap"))
	g.Consistently(func(g Gomega) {
		for name, pool := range pools {
			g.Expect(inClusterPool(g, ns, name).ResourceVersion).To(Equal(pool.ResourceVersion), name)
		}
		g.Expect(ofNamespace(g, netbox.IPRanges, ns)).To(BeEmpty())
	}).Should(Succeed())
}

// A network waits for its InClusterIPPools to go, then releases its NetBox objects as deletionPolicy says.
func TestInClusterDeletion(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := inClusterNetwork(ns, "default")
	readyLayout(g, nw)
	ranges := append(rangesIn(g, nw, "cp"), rangesIn(g, nw, "workers")...)
	held := inClusterPool(g, ns, "workers")
	held.Finalizers = []string{"test.example.test/hold"}
	g.Expect(k8s.Update(ctx, held)).To(Succeed())

	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonWaitingForPools)
	g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
		g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.ReadyCondition).Message).To(Equal("InClusterIPPool workers still exists"))
	}).Should(Succeed())
	g.Eventually(gone(&inclusterv1.InClusterIPPool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cp"}})).Should(BeTrue())
	for _, lr := range ranges {
		rng, _, err := admin.Get(ctx, netbox.IPRanges, lr.ID)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(rng.Status.Value).To(Equal("active"))
	}

	held = inClusterPool(g, ns, "workers")
	held.Finalizers = nil
	g.Expect(k8s.Update(ctx, held)).To(Succeed())
	g.Eventually(gone(nw)).Should(BeTrue())
	for _, lr := range ranges {
		rng, _, err := admin.Get(ctx, netbox.IPRanges, lr.ID)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(rng.Status.Value).To(Equal("deprecated"))
	}
}

func TestPoolRangeOfInClusterNetwork(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	readyLayout(g, inClusterNetwork(ns, "default"))
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "workers"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitCondition(g, pool, metav1.ConditionFalse, v1alpha1.ReasonInvalidRange)
	g.Expect(meta.FindStatusCondition(pool.Status.Conditions, v1alpha1.ReadyCondition).Message).To(ContainSubstring("network default hands out addresses through InClusterIPPools"))
}
