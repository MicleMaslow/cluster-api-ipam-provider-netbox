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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netmath"
)

func TestNetworkCarvesPrefixInParent(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)

	p := netip.MustParsePrefix(nw.Status.Prefix)
	g.Expect(p.Bits()).To(Equal(28))
	g.Expect(netip.MustParsePrefix("10.10.0.0/22").Contains(p.Addr())).To(BeTrue())
	g.Expect(netip.MustParsePrefix("10.10.0.0/26").Overlaps(p)).To(BeFalse(), "the first block is reserved")
	obj, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(attrsOf(obj)).To(Equal(netbox.Attrs{}))
	g.Expect(obj.Status.Value).To(Equal("active"))
	g.Expect(keys.RefOf(obj)).To(Equal(refOf("NetBoxNetwork", ns, "default")))
	g.Expect(obj.IsPool).To(BeFalse())
	g.Expect(nw.Status.AddressPrefixLength).To(Equal(int32(22)), "the parent mask by default")
}

// A logical parent 10.10.1.0/24 inside the VLAN 10.10.0.0/22: addresses get mask 22, the /24 bounds are plain hosts.
func TestNetworkLogicalParent(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	byHand(t, g, netbox.Prefixes, map[string]any{"prefix": "10.10.1.0/24", "status": "active", "description": "logical parent"})
	nw := newNetwork(ns, "default")
	nw.Spec.ParentPrefix, nw.Spec.AddressPrefixLength, nw.Spec.PrefixLength, nw.Spec.IsPool = "10.10.1.0/24", 22, 28, ptr.To(true)
	nw.Spec.Pools = []v1alpha1.NetworkPool{netPool("all", 0)}
	readyLayout(g, nw)
	g.Expect(nw.Status.Prefix).To(Equal("10.10.1.0/28"))
	g.Expect(nw.Status.AddressPrefixLength).To(Equal(int32(22)))
	obj, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(obj.IsPool).To(BeTrue())

	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "all"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	g.Expect(pool.Status.AddressPrefixLength).To(Equal(int32(22)))
	g.Expect(pool.Status.IPAddresses.Total).To(Equal(int32(16)))
}

func TestNetworkIsPoolFollowsSpec(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	isPool := func() bool {
		obj, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
		g.Expect(err).NotTo(HaveOccurred())
		return obj.IsPool
	}
	setIsPool := func(v *bool) {
		g.Eventually(func(g Gomega) {
			g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
			nw.Spec.IsPool = v
			g.Expect(k8s.Update(ctx, nw)).To(Succeed())
		}).Should(Succeed())
	}
	g.Expect(isPool()).To(BeFalse())

	setIsPool(ptr.To(true))
	g.Eventually(isPool).Should(BeTrue())
	setIsPool(ptr.To(false))
	g.Eventually(isPool).Should(BeFalse())
}

func TestNetworkRecreatedKeepsPrefix(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	first := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, first)).To(Succeed())
	waitReady(g, first)
	forget(g, first)

	again := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, again)).To(Succeed())
	waitReady(g, again)
	g.Expect(again.Status.PrefixID).To(Equal(first.Status.PrefixID))
}

func TestNetworkRetainDeprecatesAndReclaims(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	id := nw.Status.PrefixID

	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	g.Eventually(gone(nw)).Should(BeTrue())
	obj, _, err := admin.Get(ctx, netbox.Prefixes, id)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(obj.Status.Value).To(Equal("deprecated"))

	again := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, again)).To(Succeed())
	waitReady(g, again)
	g.Expect(again.Status.PrefixID).To(Equal(id))
	obj, _, err = admin.Get(ctx, netbox.Prefixes, id)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(obj.Status.Value).To(Equal("active"))
}

func TestNetworkDeletePolicyRemovesPrefix(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	nw.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)

	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	g.Eventually(func() error {
		_, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
		return err
	}).Should(MatchError(netbox.ErrNotFound))
}

func TestNetworkDeleteFindsPrefixWithoutStatus(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	nw.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	id := nw.Status.PrefixID

	setPaused(t, ns, true)
	time.Sleep(time.Second) // let an in-flight reconcile finish before status is cleared
	g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
		nw.Status = v1alpha1.NetBoxNetworkStatus{}
		g.Expect(k8s.Status().Update(ctx, nw)).To(Succeed())
	}).Should(Succeed())
	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	setPaused(t, ns, false)

	g.Eventually(func() error {
		_, _, err := admin.Get(ctx, netbox.Prefixes, id)
		return err
	}).Should(MatchError(netbox.ErrNotFound))
	g.Eventually(gone(nw)).Should(BeTrue())
}

func TestNetworkPrefixNotFound(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	g.Expect(admin.Delete(ctx, netbox.Prefixes, nw.Status.PrefixID, "", "test: deleted by hand")).To(Succeed())

	touch(g, nw)
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonPrefixNotFound)
	g.Consistently(func() ([]netbox.Object, error) {
		return findRef(netbox.Prefixes, refOf("NetBoxNetwork", ns, "default"))
	}).Should(BeEmpty())
}

func TestNetworkCreatesGivenPrefix(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	nw.Spec.PrefixLength, nw.Spec.Prefix = 0, "10.10.3.208/28"
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	g.Expect(nw.Status.Prefix).To(Equal("10.10.3.208/28"))
	obj, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(attrsOf(obj)).To(Equal(netbox.Attrs{}))
}

// A prefix without a key is adopted whatever its owner: only the token's permissions limit what may be taken.
// Its owner, description and comments stay, whatever the spec says.
func TestNetworkAdoptsPrefixOfAnyOwner(t *testing.T) {
	g := newG(t)
	z := newZone(t, g)
	ns := newEnv(t, nil)
	existing := byHand(t, g, netbox.Prefixes, map[string]any{
		"prefix": "10.10.3.224/28", "status": "active", "description": "someone else's", "owner": z.owner,
	})

	nw := adoptedNetwork(ns, "10.10.3.224/28")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	g.Expect(nw.Status.PrefixID).To(Equal(existing.ID))
	obj, _, err := admin.Get(ctx, netbox.Prefixes, existing.ID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(keys.RefOf(obj)).To(Equal(refOf("NetBoxNetwork", ns, "default")))
	g.Expect(attrsOf(obj)).To(Equal(netbox.Attrs{Owner: z.owner}))
	g.Expect(texts{obj.Description, obj.Comments}).To(Equal(texts{"someone else's", ""}))

	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) { s.Description, s.Comments = "mine", "mine" })
	waitReadyAt(g, nw)
	touch(g, nw)
	g.Consistently(func(g Gomega) {
		cur, _, err := admin.Get(ctx, netbox.Prefixes, existing.ID)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cur.LastUpdated).To(Equal(obj.LastUpdated))
	}).Should(Succeed())
}

func TestNetworkPrefixOutsideNewParent(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	first := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, first)).To(Succeed())
	waitReady(g, first)
	forget(g, first)

	moved := newNetwork(ns, "default")
	moved.Spec.ParentPrefix, moved.Spec.Gateway = "10.20.0.0/22", "10.20.0.1"
	g.Expect(k8s.Create(ctx, moved)).To(Succeed())
	waitCondition(g, moved, metav1.ConditionFalse, v1alpha1.ReasonPrefixMismatch)
}

func TestNetworkParentNotFound(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	nw.Spec.ParentPrefix, nw.Spec.Gateway = "10.99.0.0/22", "10.99.0.1"
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonParentNotFound)
	waitTypedCondition(g, nw, v1alpha1.MetadataSyncedCondition, metav1.ConditionUnknown, v1alpha1.ReasonWaitingForPrefix)
}

func TestNetworkPausedCondition(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	setPaused(t, ns, true)
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitTypedCondition(g, nw, clusterv1.PausedCondition, metav1.ConditionTrue, clusterv1.PausedReason)
	g.Consistently(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
		g.Expect(nw.Status.Prefix).To(BeEmpty())
	}).Should(Succeed())

	setPaused(t, ns, false)
	waitTypedCondition(g, nw, clusterv1.PausedCondition, metav1.ConditionFalse, clusterv1.NotPausedReason)
	waitReady(g, nw)
}

// A Paused condition that only lags the generation, as after a spec edit, is written, not dropped.
func TestNetworkPausedObservedGeneration(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	nw := newNetwork("ns", "default")
	nw.Generation = 2
	nw.Finalizers = []string{networkFinalizer}
	nw.Status.Conditions = []metav1.Condition{{Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason, ObservedGeneration: 1}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nw).WithStatusSubresource(nw).Build()
	r := &NetBoxNetworkReconciler{Client: c, APIReader: c}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(nw)})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
	g.Expect(nw.Generation).To(Equal(int64(2)))
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, clusterv1.PausedCondition).ObservedGeneration).To(Equal(int64(2)))
}

// The manager in the tests has no watch filter: Reconcile itself must skip a network without the label.
func TestNetworkWatchFilter(t *testing.T) {
	g := NewWithT(t)
	nw := newNetwork("ns", "default")
	c := claimClient(nw)
	r := &NetBoxNetworkReconciler{Client: c, WatchFilterValue: "x"}

	g.Expect(r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(nw)})).To(Equal(ctrl.Result{}))
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
	g.Expect(nw.Finalizers).To(BeEmpty())
	g.Expect(nw.Status.Conditions).To(BeEmpty())
}

func TestNetworkRefusesPrefixHeldByAnotherNetwork(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	first := newNetwork(ns, "first")
	first.Spec.PrefixLength, first.Spec.Prefix = 0, "10.10.3.192/28"
	g.Expect(k8s.Create(ctx, first)).To(Succeed())
	waitReady(g, first)

	second := newNetwork(ns, "second")
	second.Spec.PrefixLength, second.Spec.Prefix = 0, "10.10.3.192/28"
	g.Expect(k8s.Create(ctx, second)).To(Succeed())
	waitCondition(g, second, metav1.ConditionFalse, v1alpha1.ReasonDuplicate)
	obj, _, err := admin.Get(ctx, netbox.Prefixes, first.Status.PrefixID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(keys.RefOf(obj)).To(Equal(refOf("NetBoxNetwork", ns, "first")))
}

// An admin's deprecated status on the adopted prefix of a live network does not free it for another network.
func TestNetworkRefusesDeprecatedPrefixOfLiveNetwork(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	manual := manualPrefix(t, g, "10.10.3.16/28", "deprecated")
	first := newNetwork(ns, "first")
	first.Spec.PrefixLength, first.Spec.Prefix = 0, "10.10.3.16/28"
	g.Expect(k8s.Create(ctx, first)).To(Succeed())
	waitReady(g, first)

	second := newNetwork(ns, "second")
	second.Spec.PrefixLength, second.Spec.Prefix = 0, "10.10.3.16/28"
	g.Expect(k8s.Create(ctx, second)).To(Succeed())
	waitCondition(g, second, metav1.ConditionFalse, v1alpha1.ReasonDuplicate)
	obj, _, err := admin.Get(ctx, netbox.Prefixes, manual.ID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(keys.RefOf(obj)).To(Equal(refOf("NetBoxNetwork", ns, "first")))
	keptByAdmin(g, manual.ID, "deprecated")
	waitReady(g, first)
}

// adoptedNetwork is network "default" of ns adopting cidr.
func adoptedNetwork(ns, cidr string) *v1alpha1.NetBoxNetwork {
	nw := newNetwork(ns, "default")
	nw.Spec.PrefixLength, nw.Spec.Prefix = 0, cidr
	return nw
}

// manualPrefix is a prefix an admin made by hand with status, without an owner.
func manualPrefix(t *testing.T, g *WithT, cidr, status string) netbox.Object {
	t.Helper()
	return byHand(t, g, netbox.Prefixes, map[string]any{"prefix": cidr, "status": status, "description": "manual"})
}

// keptByAdmin checks that the admin's status and description of prefix id are untouched.
func keptByAdmin(g *WithT, id int64, status string) {
	g.Expect(admin.List(ctx, netbox.Prefixes, url.Values{
		"id": {strconv.FormatInt(id, 10)}, "status": {status}, "description": {"manual"},
	})).To(HaveLen(1))
}

func TestNetworkAdoptKeepsAdminFields(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	manual := manualPrefix(t, g, "10.10.3.176/28", "reserved")

	nw := adoptedNetwork(ns, "10.10.3.176/28")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	obj, _, err := admin.Get(ctx, netbox.Prefixes, manual.ID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(keys.RefOf(obj)).To(Equal(refOf("NetBoxNetwork", ns, "default")))
	keptByAdmin(g, manual.ID, "reserved")
}

func TestNetworkAdoptKeepsDeprecatedStatus(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	manual := manualPrefix(t, g, "10.10.3.128/28", "deprecated")

	nw := adoptedNetwork(ns, "10.10.3.128/28")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	touch(g, nw)
	g.Consistently(func(g Gomega) {
		obj, _, err := admin.Get(ctx, netbox.Prefixes, manual.ID)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(keys.RefOf(obj)).To(Equal(refOf("NetBoxNetwork", ns, "default")))
		g.Expect(obj.Status.Value).To(Equal("deprecated"))
	}).Should(Succeed())
	keptByAdmin(g, manual.ID, "deprecated")
}

func TestNetworkAdoptReclaimsRetainedPrefix(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	retained := byHand(t, g, netbox.Prefixes, map[string]any{
		"prefix": "10.10.3.112/28", "status": "deprecated", "custom_fields": map[string]any{keys.Ref: refOf("NetBoxNetwork", ns, "old")},
	})

	nw := adoptedNetwork(ns, "10.10.3.112/28")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	obj, _, err := admin.Get(ctx, netbox.Prefixes, retained.ID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(keys.RefOf(obj)).To(Equal(refOf("NetBoxNetwork", ns, "default")))
	g.Expect(obj.Status.Value).To(Equal("active"))
}

func TestNetworkAdoptReleaseOnDelete(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	manual := manualPrefix(t, g, "10.10.3.160/28", "reserved")
	nw := adoptedNetwork(ns, "10.10.3.160/28")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)

	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	g.Eventually(gone(nw)).Should(BeTrue())
	obj, _, err := admin.Get(ctx, netbox.Prefixes, manual.ID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(keys.RefOf(obj)).To(BeEmpty())
	g.Expect(attrsOf(obj)).To(Equal(netbox.Attrs{}))
	keptByAdmin(g, manual.ID, "reserved")
}

func TestNetworkDeprecatedForeignNamespace(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	foreign := byHand(t, g, netbox.Prefixes, map[string]any{
		"prefix": "10.10.3.144/28", "status": "deprecated", "custom_fields": map[string]any{keys.Ref: refOf("NetBoxNetwork", "elsewhere", "default")},
	})

	nw := adoptedNetwork(ns, "10.10.3.144/28")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonDuplicate)
	obj, _, err := admin.Get(ctx, netbox.Prefixes, foreign.ID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(keys.RefOf(obj)).To(Equal(refOf("NetBoxNetwork", "elsewhere", "default")))
	g.Expect(obj.Status.Value).To(Equal("deprecated"))
}

func TestHeldBy(t *testing.T) {
	const ref = "NetBoxNetwork/ns/a"
	r := &NetBoxNetworkReconciler{Client: claimClient(newNetwork("ns", "b"), newNetwork("ns", "retired")), Keys: netbox.DefaultKeys}
	prefix := func(got, status string) netbox.Object {
		return netbox.Object{Status: netbox.Choice{Value: status}, CustomFields: map[string]json.RawMessage{netbox.DefaultKeys.Ref: json.RawMessage(strconv.Quote(got))}}
	}
	for name, tc := range map[string]struct {
		obj  netbox.Object
		want string
	}{
		"free":                        {prefix("", "active"), ""},
		"ours":                        {prefix(ref, "active"), ""},
		"held":                        {prefix("NetBoxNetwork/ns/b", "active"), "NetBoxNetwork/ns/b"},
		"deprecated, live network":    {prefix("NetBoxNetwork/ns/b", "deprecated"), "NetBoxNetwork/ns/b"},
		"deprecated, gone network":    {prefix("NetBoxNetwork/ns/c", "deprecated"), ""},
		"deprecated, other namespace": {prefix("NetBoxNetwork/other/a", "deprecated"), "NetBoxNetwork/other/a"},
		"retired, ours":               {prefix(ref+"/retired", "deprecated"), ""},
		"retired, same namespace":     {prefix("NetBoxNetwork/ns/b/retired", "deprecated"), ""},
		"retired, other namespace":    {prefix("NetBoxNetwork/other/a/retired", "deprecated"), "NetBoxNetwork/other/a/retired"},
		"retired, active":             {prefix("NetBoxNetwork/ns/b/retired", "active"), "NetBoxNetwork/ns/b/retired"},
		"deprecated, live retired":    {prefix("NetBoxNetwork/ns/retired", "deprecated"), "NetBoxNetwork/ns/retired"},
		"deprecated, other instance":  {prefix("b/NetBoxNetwork/ns/b", "deprecated"), "b/NetBoxNetwork/ns/b"},
	} {
		if got, err := r.heldBy(ctx, "ns", tc.obj, ref); err != nil || got != tc.want {
			t.Errorf("%s: heldBy = %q, %v, want %q", name, got, err, tc.want)
		}
	}
	// Of an instance, the namespace is that of the instance: neither another instance's nor one without is free.
	for _, other := range []string{"b/NetBoxNetwork/ns/c", "NetBoxNetwork/ns/c"} {
		if got, err := r.heldBy(ctx, "ns", prefix(other, "deprecated"), "a/NetBoxNetwork/ns/a"); err != nil || got != other {
			t.Errorf("instance a: heldBy = %q, %v, want %q", got, err, other)
		}
	}
	if got, err := r.heldBy(ctx, "ns", prefix("a/NetBoxNetwork/ns/c", "deprecated"), "a/NetBoxNetwork/ns/a"); err != nil || got != "" {
		t.Errorf("instance a, same namespace: heldBy = %q, %v, want free", got, err)
	}
	broken := &NetBoxNetworkReconciler{Client: interceptor.NewClient(fake.NewClientBuilder().Build(), interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("down")
		},
	}), Keys: netbox.DefaultKeys}
	if _, err := broken.heldBy(ctx, "ns", prefix("NetBoxNetwork/ns/c", "deprecated"), ref); err == nil {
		t.Error("API error: heldBy frees the prefix")
	}
}

func TestNetworkCustomFields(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	nw.Spec.CustomFields = map[string]apiextensionsv1.JSON{"labels": {Raw: []byte(`{"cluster":"` + ns + `","role":"workload"}`)}}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	waitTypedCondition(g, nw, v1alpha1.MetadataSyncedCondition, metav1.ConditionTrue, v1alpha1.ReasonSynced)
	labels := func() map[string]any {
		o, _, _ := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
		return labelsOf(o)
	}
	g.Eventually(labels).Should(Equal(map[string]any{"cluster": ns, "role": "workload"}))

	// An admin edits the JSON value.
	o, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
	g.Expect(err).NotTo(HaveOccurred())
	edited := map[string]any{}
	g.Expect(json.Unmarshal(o.CustomFields["labels"], &edited)).To(Succeed())
	edited["team"] = "x"
	g.Expect(admin.Patch(ctx, netbox.Prefixes, nw.Status.PrefixID, "", map[string]any{"custom_fields": map[string]any{"labels": edited}})).Error().To(Succeed())
	g.Eventually(func() error {
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw); err != nil {
			return err
		}
		nw.Spec.CustomFields = map[string]apiextensionsv1.JSON{"labels": {Raw: []byte(`{"cluster":"` + ns + `"}`)}}
		return k8s.Update(ctx, nw)
	}).Should(Succeed())
	g.Eventually(labels).Should(Equal(map[string]any{"cluster": ns, "team": "x"}))
	g.Eventually(func() map[string][]string {
		_ = k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)
		return nw.Status.CustomFieldKeys
	}).Should(Equal(map[string][]string{"labels": {"cluster"}}))

	o, _, err = admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(keys.RefOf(o)).To(Equal(refOf("NetBoxNetwork", ns, "default")), "patching custom fields keeps the key")
}

// spec.customFields.labels goes next to an admin's labels of an adopted prefix, and the deletion (Retain) takes away
// only our key: an adopted prefix is let go, a carved one keeps it for the clean-up.
func TestSpecLabelsSurviveAdoptAndRetain(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	manual := byHand(t, g, netbox.Prefixes, map[string]any{
		"prefix": "10.10.3.48/28", "status": "active", "custom_fields": map[string]any{"labels": map[string]any{"site": "a"}},
	})
	adopted, carved := adoptedNetwork(ns, "10.10.3.48/28"), newNetwork(ns, "carved")
	for _, nw := range []*v1alpha1.NetBoxNetwork{adopted, carved} {
		nw.Spec.CustomFields = map[string]apiextensionsv1.JSON{"labels": {Raw: []byte(`{"env":"dev"}`)}}
		g.Expect(k8s.Create(ctx, nw)).To(Succeed())
		waitReady(g, nw)
		waitTypedCondition(g, nw, v1alpha1.MetadataSyncedCondition, metav1.ConditionTrue, v1alpha1.ReasonSynced)
	}
	g.Expect(adopted.Status.PrefixID).To(Equal(manual.ID))
	prefix := func(nw *v1alpha1.NetBoxNetwork) netbox.Object {
		o, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
		g.Expect(err).NotTo(HaveOccurred())
		return o
	}
	for nw, want := range map[*v1alpha1.NetBoxNetwork]map[string]any{adopted: {"site": "a", "env": "dev"}, carved: {"env": "dev"}} {
		o := prefix(nw)
		g.Expect(labelsOf(o)).To(Equal(want), nw.Name)
		g.Expect(keys.RefOf(o)).To(Equal(keys.Key("NetBoxNetwork", nw)), nw.Name)
	}

	for _, nw := range []*v1alpha1.NetBoxNetwork{adopted, carved} {
		g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
		g.Eventually(gone(nw)).Should(BeTrue())
	}
	o := prefix(adopted)
	g.Expect(labelsOf(o)).To(Equal(map[string]any{"site": "a", "env": "dev"}))
	g.Expect(keys.RefOf(o)).To(BeEmpty())
	o = prefix(carved)
	g.Expect(o.Status.Value).To(Equal("deprecated"))
	g.Expect(labelsOf(o)).To(Equal(map[string]any{"env": "dev"}))
	g.Expect(keys.RefOf(o)).To(Equal(keys.Key("NetBoxNetwork", carved)))
}

// TestMetadataSteadyState checks that NetBox's form of select, multiselect, object, multi-object, url and date
// values is not taken for a change: a reconcile of a synced network writes nothing.
func TestMetadataSteadyState(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	for _, name := range []string{"site_tier", "vlan"} {
		g.Expect(admin.List(ctx, "extras/custom-fields/", url.Values{"name": {name}})).To(HaveLen(1), "custom field %s is not seeded: run hack/dev/seed.sh", name)
	}
	sets, err := admin.List(ctx, "extras/custom-field-choice-sets/", url.Values{"name": {"site-tiers"}})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sets).To(HaveLen(1), "choice set site-tiers is not seeded: run hack/dev/seed.sh")

	vlan1 := byHand(t, g, "ipam/vlans/", map[string]any{"vid": 3901, "name": ns + "-1"})
	vlan2 := byHand(t, g, "ipam/vlans/", map[string]any{"vid": 3902, "name": ns + "-2"})
	field := func(suffix, typ string, extra map[string]any) string {
		name := strings.ReplaceAll(ns, "-", "_") + "_" + suffix
		fields := map[string]any{"name": name, "label": name, "type": typ, "object_types": []string{"ipam.prefix"}}
		maps.Copy(fields, extra)
		byHand(t, g, "extras/custom-fields/", fields)
		return name
	}
	tiers := field("tiers", "multiselect", map[string]any{"choice_set": sets[0].ID})
	vlans := field("vlans", "multiobject", map[string]any{"related_object_type": "ipam.vlan"})
	link := field("link", "url", nil)
	day := field("day", "date", nil)

	raw := func(s string, args ...any) apiextensionsv1.JSON {
		return apiextensionsv1.JSON{Raw: fmt.Appendf(nil, s, args...)}
	}
	nw := newNetwork(ns, "default")
	nw.Spec.CustomFields = map[string]apiextensionsv1.JSON{
		"site_tier": raw(`"standard"`),
		"vlan":      raw(`%d`, vlan1.ID),
		tiers:       raw(`["premium","standard"]`),
		vlans:       raw(`[%d,%d]`, vlan2.ID, vlan1.ID),
		link:        raw(`"https://example.com/a b?x=1"`),
		day:         raw(`"2026-01-02"`),
	}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	waitTypedCondition(g, nw, v1alpha1.MetadataSyncedCondition, metav1.ConditionTrue, v1alpha1.ReasonSynced)

	prefix := func() netbox.Object {
		o, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
		g.Expect(err).NotTo(HaveOccurred())
		return o
	}
	o := prefix()
	for field := range nw.Spec.CustomFields {
		g.Expect(string(o.CustomFields[field])).NotTo(Or(BeEmpty(), Equal("null")), "custom field %s", field)
	}
	touch(g, nw)
	g.Consistently(func() string { return prefix().LastUpdated }, "5s").Should(Equal(o.LastUpdated))
}

func TestNetworkRejectsKeyField(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	nw.Spec.CustomFields = map[string]apiextensionsv1.JSON{keys.Ref: {Raw: []byte(`"x"`)}}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	waitTypedCondition(g, nw, v1alpha1.MetadataSyncedCondition, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec)
	g.Expect(nw.Status.PrefixID).NotTo(BeZero())

	o, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(keys.RefOf(o)).To(Equal(refOf("NetBoxNetwork", ns, "default")))
}

func TestNetworkTags(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	nw.Spec.Tags = []string{"capi"}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	waitTypedCondition(g, nw, v1alpha1.MetadataSyncedCondition, metav1.ConditionTrue, v1alpha1.ReasonSynced)
	tags := func() []string {
		o, _, _ := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
		return tagSlugs(o.Tags)
	}
	g.Eventually(tags).Should(Equal([]string{"capi"}))
	eventually(g, nw, func(g Gomega) { g.Expect(nw.Status.Tags).To(Equal([]string{"capi"})) })

	g.Expect(admin.Patch(ctx, netbox.Prefixes, nw.Status.PrefixID, "", map[string]any{
		"tags": []map[string]string{{"slug": "capi"}, {"slug": "k8s"}},
	})).Error().To(Succeed())
	setTags(g, nw, nil)
	g.Eventually(tags).Should(Equal([]string{"k8s"}), "the tag set by an admin stays, the provider's goes")
	g.Eventually(func() []string {
		_ = k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)
		return nw.Status.Tags
	}).Should(BeEmpty())

	setTags(g, nw, []string{"missing-tag"})
	waitTypedCondition(g, nw, v1alpha1.MetadataSyncedCondition, metav1.ConditionFalse, v1alpha1.ReasonNetBoxError)
	waitReady(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "cp"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
}

// A NetBox write the token may not make, a 403 or the 404 of an object outside the permission constraint of the
// action, keeps the finalizer with reason Forbidden and reports no release.
func TestNetworkDeleteForbidden(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(ipamv1.AddToScheme(scheme)).To(Succeed())
	for _, write := range []int{http.StatusForbidden, http.StatusNotFound} {
		for _, policy := range []v1alpha1.DeletionPolicy{v1alpha1.DeletionPolicyDelete, v1alpha1.DeletionPolicyRetain} {
			name := fmt.Sprintf("%d %s", write, policy)
			nw := newNetwork("ns", "default")
			nw.Spec.DeletionPolicy = policy
			nw.Finalizers = []string{networkFinalizer}
			nw.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			nw.Status.Conditions = []metav1.Condition{{Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nw).WithStatusSubresource(nw).Build()
			r := &NetBoxNetworkReconciler{Client: c, APIReader: c, NetBox: forbiddenNetBox(t, write), Keys: netbox.DefaultKeys, Recorder: events.NewFakeRecorder(10)}

			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(nw)})
			g.Expect(err).NotTo(HaveOccurred(), name)
			g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: netBoxRequeueAfter}), name)
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed(), name)
			g.Expect(nw.Finalizers).To(ConsistOf(networkFinalizer), name)
			cond := meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.ReadyCondition)
			g.Expect(cond).NotTo(BeNil(), name)
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse), name)
			g.Expect(cond.Reason).To(Equal(v1alpha1.ReasonForbidden), name)
			g.Expect(emitted(r.Recorder)).To(BeEmpty(), name)

			err = r.releaseVIP(ctx, vipNetwork("ns", "default"))
			g.Expect(err).To(MatchError(netbox.ErrForbidden), name)
			g.Expect(emitted(r.Recorder)).To(BeEmpty(), name)
		}
	}
}

// vipNetwork is newNetwork asking for a VIP.
func vipNetwork(ns, name string) *v1alpha1.NetBoxNetwork {
	nw := newNetwork(ns, name)
	nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{}
	return nw
}

// vipsOf are the NetBox addresses held as the VIP of network name.
func vipsOf(g Gomega, ns, name string) []netbox.Object {
	vips, err := findRef(netbox.IPAddresses, refOf("NetBoxNetwork", ns, name, "vip"))
	g.Expect(err).NotTo(HaveOccurred())
	return vips
}

func TestNetworkReservesVIP(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	nw.Spec.ControlPlaneEndpoint.DNSName = "API.Cluster.Example"
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)

	want := netmath.Last(netip.MustParsePrefix(nw.Status.Prefix)).String()
	g.Expect(nw.Status.ControlPlaneEndpoint).To(Equal(v1alpha1.APIEndpoint{Host: want, Port: 6443}))
	vips := vipsOf(g, ns, "default")
	g.Expect(vips).To(HaveLen(1))
	g.Expect(vips[0].Address).To(Equal(want + "/22"))
	g.Expect(vips[0].Role.Value).To(Equal("vip"))
	g.Expect(vips[0].Status.Value).To(Equal("active"))
	g.Expect(vips[0].DNSName).To(Equal("api.cluster.example"), "NetBox stores dns_name in lower case")
	g.Expect(vips[0].Description).To(Equal(ns + "/default // " + ns + " control plane endpoint"))

	// Steady state writes nothing: the dns_name comparison must agree with NetBox's lower case.
	// Every save bumps last_updated, a no-op one too.
	updated := func() string { return vipsOf(g, ns, "default")[0].LastUpdated }
	touch(g, nw)
	g.Consistently(updated, "5s").Should(Equal(vips[0].LastUpdated))

	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) { s.ControlPlaneEndpoint.DNSName = "api2.cluster.example" })
	g.Eventually(func() string { return vipsOf(g, ns, "default")[0].DNSName }).Should(Equal("api2.cluster.example"))
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Expect(nw.Status.ControlPlaneEndpoint).To(Equal(v1alpha1.APIEndpoint{Host: want, Port: 6443}))
	g.Expect(vipsOf(g, ns, "default")[0].ID).To(Equal(vips[0].ID))
}

func TestNetworkWithoutEndpointReservesNoVIP(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	g.Expect(nw.Status.ControlPlaneEndpoint).To(Equal(v1alpha1.APIEndpoint{}))
	g.Expect(vipsOf(g, ns, "default")).To(BeEmpty())
}

func TestNetworkVIPSkipsAddressesInUse(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	byHand(t, g, netbox.IPAddresses, map[string]any{"address": "10.10.3.111/22", "status": "active"})
	byHand(t, g, netbox.IPRanges, map[string]any{"start_address": "10.10.3.109/22", "end_address": "10.10.3.120/22", "status": "active"}) // reaches past the prefix

	nw := adoptedNetwork(ns, "10.10.3.96/28")
	nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	g.Expect(nw.Status.ControlPlaneEndpoint.Host).To(Equal("10.10.3.108"))
}

func TestNetworkVIPExhausted(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	byHand(t, g, netbox.IPAddresses, map[string]any{"address": "10.10.3.80/22", "status": "active"})

	nw := adoptedNetwork(ns, "10.10.3.80/32")
	nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonExhausted)
	waitCondition(g, nw, metav1.ConditionTrue, v1alpha1.ReasonReady)
	eventually(g, nw, func(g Gomega) { g.Expect(nw.Status.Prefix).To(Equal("10.10.3.80/32")) }) // conditions are written first
	g.Expect(nw.Status.ControlPlaneEndpoint).To(Equal(v1alpha1.APIEndpoint{}))
	g.Expect(vipsOf(g, ns, "default")).To(BeEmpty())
}

func TestNetworkVIPSurvivesLostStatus(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	vip := nw.Status.ControlPlaneEndpoint
	id := vipsOf(g, ns, "default")[0].ID

	forget(g, nw)
	g.Expect(vipsOf(g, ns, "default")[0].Status.Value).To(Equal("active"), "a move releases nothing")
	again := vipNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, again)).To(Succeed())
	waitReady(g, again)
	g.Expect(again.Status.ControlPlaneEndpoint).To(Equal(vip))
	vips := vipsOf(g, ns, "default")
	g.Expect(vips).To(HaveLen(1))
	g.Expect(vips[0].ID).To(Equal(id))

	setPaused(t, ns, true)
	time.Sleep(time.Second) // let an in-flight reconcile finish before status is cleared
	g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(again), again)).To(Succeed())
		again.Status = v1alpha1.NetBoxNetworkStatus{}
		g.Expect(k8s.Status().Update(ctx, again)).To(Succeed())
	}).Should(Succeed())
	setPaused(t, ns, false)
	touch(g, again)
	waitReady(g, again)
	g.Expect(again.Status.ControlPlaneEndpoint).To(Equal(vip))
	vips = vipsOf(g, ns, "default")
	g.Expect(vips).To(HaveLen(1))
	g.Expect(vips[0].ID).To(Equal(id))
}

func TestNetworkVIPRetainDeprecatesAndReclaims(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	id := vipsOf(g, ns, "default")[0].ID

	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	g.Eventually(gone(nw)).Should(BeTrue())
	vips := vipsOf(g, ns, "default")
	g.Expect(vips).To(HaveLen(1))
	g.Expect(vips[0].ID).To(Equal(id))
	g.Expect(vips[0].Status.Value).To(Equal("deprecated"))

	again := vipNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, again)).To(Succeed())
	waitReady(g, again)
	vips = vipsOf(g, ns, "default")
	g.Expect(vips).To(HaveLen(1))
	g.Expect(vips[0].ID).To(Equal(id))
	g.Expect(vips[0].Status.Value).To(Equal("active"))
}

// Under deletionPolicy Delete the VIP is not deleted while a Cluster has it as its endpoint.
func TestNetworkDeleteWaitsForClusterOnVIP(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	nw.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	waitReady(g, nw)
	g.Expect(clusterEndpointOf(g, ns).Host).To(Equal(nw.Status.ControlPlaneEndpoint.Host))

	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonWaitingForCluster)
	g.Expect(vipsOf(g, ns, "default")).To(HaveLen(1))

	deleteCluster(t, g, ns)
	g.Eventually(gone(nw)).Should(BeTrue())
	g.Expect(vipsOf(g, ns, "default")).To(BeEmpty())
}

// Under Retain the VIP stays deprecated and is never handed out, so the deletion does not wait for the Cluster.
func TestNetworkDeleteRetainNoWait(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	vip := nw.Status.ControlPlaneEndpoint.Host

	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	g.Eventually(gone(nw)).Should(BeTrue())
	g.Expect(clusterEndpointOf(g, ns).Host).To(Equal(vip))
	vips := vipsOf(g, ns, "default")
	g.Expect(vips).To(HaveLen(1))
	g.Expect(vips[0].Status.Value).To(Equal("deprecated"))
}

// A network that lost its status finds its VIP but not its prefix: the prefix is reported lost, not carved again.
func TestPrefixNotFoundViaVIP(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	g.Expect(admin.Delete(ctx, netbox.Prefixes, nw.Status.PrefixID, "", "test: deleted by hand")).To(Succeed())
	forget(g, nw)

	again := vipNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, again)).To(Succeed())
	waitCondition(g, again, metav1.ConditionFalse, v1alpha1.ReasonPrefixNotFound)
	g.Consistently(func() ([]netbox.Object, error) {
		return findRef(netbox.Prefixes, refOf("NetBoxNetwork", ns, "default"))
	}).Should(BeEmpty())
}

// A retained prefix and VIP that do not fit the spec of the new network are re-keyed, and new ones are made.
func TestRetainedMismatchRetired(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	ref := refOf("NetBoxNetwork", ns, "default")
	nw := vipNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	oldPrefix, oldVIP := nw.Status.PrefixID, vipsOf(g, ns, "default")[0]
	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	g.Eventually(gone(nw)).Should(BeTrue())

	again := vipNetwork(ns, "default")
	again.Spec.PrefixLength = 27
	g.Expect(k8s.Create(ctx, again)).To(Succeed())
	waitReady(g, again)
	g.Expect(netip.MustParsePrefix(again.Status.Prefix).Bits()).To(Equal(27))
	g.Expect(again.Status.PrefixID).NotTo(Equal(oldPrefix))
	g.Expect(netip.MustParsePrefix(again.Status.Prefix).Contains(netip.MustParseAddr(again.Status.ControlPlaneEndpoint.Host))).To(BeTrue())

	for path, id := range map[string]int64{netbox.Prefixes: oldPrefix, netbox.IPAddresses: oldVIP.ID} {
		old, _, err := admin.Get(ctx, path, id)
		g.Expect(err).NotTo(HaveOccurred(), path)
		g.Expect(old.Status.Value).To(Equal("deprecated"), path)
	}
	old, _, _ := admin.Get(ctx, netbox.Prefixes, oldPrefix)
	g.Expect(keys.RefOf(old)).To(Equal(retiredRef(ref)))
	old, _, _ = admin.Get(ctx, netbox.IPAddresses, oldVIP.ID)
	g.Expect(keys.RefOf(old)).To(Equal(retiredRef(ref + "/vip")))
	vips := vipsOf(g, ns, "default")
	g.Expect(vips).To(HaveLen(1))
	g.Expect(vips[0].ID).NotTo(Equal(oldVIP.ID))
	waitEvent(g, ns, "default", "PrefixRetired")
	waitEvent(g, ns, "default", "VIPRetired")
}

// retainedPrefix is the prefix that network "default" of ns carved and retained on deletion.
func retainedPrefix(g *WithT, ns string) int64 {
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	id := nw.Status.PrefixID
	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	g.Eventually(gone(nw)).Should(BeTrue())
	return id
}

// A retained prefix other than spec.prefix of the new network is retired, and spec.prefix adopted.
func TestRetainedMismatchAdopts(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	old := retainedPrefix(g, ns)
	manual := manualPrefix(t, g, "10.10.3.32/28", "reserved")

	nw := adoptedNetwork(ns, "10.10.3.32/28")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	g.Expect(nw.Status.PrefixID).To(Equal(manual.ID))
	obj, _, err := admin.Get(ctx, netbox.Prefixes, old)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(keys.RefOf(obj)).To(Equal(retiredRef(refOf("NetBoxNetwork", ns, "default"))))
	g.Expect(obj.Status.Value).To(Equal("deprecated"))
	waitEvent(g, ns, "default", "PrefixRetired")
}

// A retained prefix that is spec.prefix of the new network is taken back and active again.
func TestRetainedPrefixTakenBackBySpecPrefix(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	old := retainedPrefix(g, ns)
	obj, _, err := admin.Get(ctx, netbox.Prefixes, old)
	g.Expect(err).NotTo(HaveOccurred())

	nw := adoptedNetwork(ns, obj.Prefix)
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	g.Expect(nw.Status.PrefixID).To(Equal(old))
	obj, _, err = admin.Get(ctx, netbox.Prefixes, old)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(keys.RefOf(obj)).To(Equal(refOf("NetBoxNetwork", ns, "default")))
	g.Expect(obj.Status.Value).To(Equal("active"))
}

func TestNetworkVIPAdoptedPrefixDeprecatesVIP(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	manual := manualPrefix(t, g, "10.10.3.64/28", "reserved")
	nw := adoptedNetwork(ns, "10.10.3.64/28")
	nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	g.Expect(nw.Status.PrefixID).To(Equal(manual.ID))
	g.Expect(nw.Status.ControlPlaneEndpoint.Host).To(Equal("10.10.3.79"))

	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	g.Eventually(gone(nw)).Should(BeTrue())
	vips := vipsOf(g, ns, "default")
	g.Expect(vips).To(HaveLen(1))
	g.Expect(vips[0].Status.Value).To(Equal("deprecated"))
}

// A VIP lost from NetBox comes back at its address; if the address was taken meanwhile, the network
// reports EndpointConflict and stays Ready.
func TestNetworkVIPLostFromNetBoxKeepsItsAddress(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	above := byHand(t, g, netbox.IPAddresses, map[string]any{"address": "10.10.3.111/22", "status": "active"})
	nw := adoptedNetwork(ns, "10.10.3.96/28")
	nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	g.Expect(nw.Status.ControlPlaneEndpoint.Host).To(Equal("10.10.3.110"))

	// The higher address frees up: a new pick would move the VIP there.
	g.Expect(admin.Delete(ctx, netbox.IPAddresses, above.ID, "", "test: freed")).To(Succeed())
	g.Expect(admin.Delete(ctx, netbox.IPAddresses, vipsOf(g, ns, "default")[0].ID, "", "test: deleted by hand")).To(Succeed())
	touch(g, nw)
	g.Eventually(func(g Gomega) {
		vips := vipsOf(g, ns, "default")
		g.Expect(vips).To(HaveLen(1))
		g.Expect(vips[0].Address).To(Equal("10.10.3.110/22"))
	}).Should(Succeed())
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)

	g.Expect(admin.Delete(ctx, netbox.IPAddresses, vipsOf(g, ns, "default")[0].ID, "", "test: deleted by hand")).To(Succeed())
	byHand(t, g, netbox.IPAddresses, map[string]any{"address": "10.10.3.110/22", "status": "active"})
	touch(g, nw)
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonEndpointConflict)
	waitReady(g, nw)
	g.Expect(nw.Status.ControlPlaneEndpoint.Host).To(Equal("10.10.3.110"))
	g.Expect(vipsOf(g, ns, "default")).To(BeEmpty())
}

// A VIP moved by hand in NetBox is reported, not followed: the status keeps the reserved address.
func TestNetworkVIPMovedByHand(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := adoptedNetwork(ns, "10.10.3.96/28")
	nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Expect(nw.Status.ControlPlaneEndpoint.Host).To(Equal("10.10.3.111"))

	_, err := admin.Patch(ctx, netbox.IPAddresses, vipsOf(g, ns, "default")[0].ID, "", map[string]any{"address": "10.10.3.105/22"})
	g.Expect(err).NotTo(HaveOccurred())
	touch(g, nw)
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonEndpointConflict)
	msg := meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.ControlPlaneEndpointReadyCondition).Message
	g.Expect(msg).To(And(ContainSubstring("10.10.3.105"), ContainSubstring("10.10.3.111")))
	g.Consistently(func(g Gomega) string {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
		return nw.Status.ControlPlaneEndpoint.Host
	}, "3s").Should(Equal("10.10.3.111"))
	waitCondition(g, nw, metav1.ConditionTrue, v1alpha1.ReasonReady)
}

func clusterEndpointOf(g Gomega, ns string) clusterv1.APIEndpoint {
	c := &clusterv1.Cluster{}
	g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: ns}, c)).To(Succeed())
	return c.Spec.ControlPlaneEndpoint
}

func setClusterEndpoint(t *testing.T, ns, host string, port int32) {
	t.Helper()
	updateCluster(t, ns, func(c *clusterv1.Cluster) {
		c.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: host, Port: port}
	})
}

func TestNetworkWritesVIPToCluster(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	nw.Spec.ControlPlaneEndpoint.Port = 8443
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Expect(clusterEndpointOf(g, ns)).To(Equal(clusterv1.APIEndpoint{Host: nw.Status.ControlPlaneEndpoint.Host, Port: 8443}))
}

// deleteCluster deletes the Cluster of newEnv and returns a func that creates it again with an endpoint.
// The cleanup creates it if the test did not, so that newEnv's cleanup can pause it.
func deleteCluster(t *testing.T, g *WithT, ns string) func(clusterv1.APIEndpoint) error {
	t.Helper()
	cluster := &clusterv1.Cluster{}
	g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: ns}, cluster)).To(Succeed())
	g.Expect(k8s.Delete(ctx, cluster)).To(Succeed())
	g.Eventually(gone(cluster)).Should(BeTrue())
	recreate := func(ep clusterv1.APIEndpoint) error {
		return k8s.Create(ctx, &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: ns},
			Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false), ControlPlaneEndpoint: ep},
		})
	}
	t.Cleanup(func() { _ = recreate(clusterv1.APIEndpoint{}) })
	return recreate
}

// Nothing reaches NetBox before the Cluster exists: no prefix, and no VIP, as the Cluster may bring an endpoint of its own.
func TestNetworkWaitsForCluster(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	recreate := deleteCluster(t, g, ns)

	nw := vipNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonWaitingForCluster)
	g.Consistently(func() ([]netbox.Object, error) {
		return findRef(netbox.Prefixes, refOf("NetBoxNetwork", ns, "default"))
	}, "3s").Should(BeEmpty())
	g.Expect(vipsOf(g, ns, "default")).To(BeEmpty())
	g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
	g.Expect(nw.Status.Prefix).To(BeEmpty())

	g.Expect(recreate(clusterv1.APIEndpoint{})).To(Succeed())
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	waitReady(g, nw)
	vip := nw.Status.ControlPlaneEndpoint
	g.Expect(vip.Host).To(Equal(netmath.Last(netip.MustParsePrefix(nw.Status.Prefix)).String()))
	g.Expect(vipsOf(g, ns, "default")).To(HaveLen(1))
	g.Expect(clusterEndpointOf(g, ns)).To(Equal(clusterv1.APIEndpoint{Host: vip.Host, Port: vip.Port}))
}

// A Cluster created after its network with an endpoint inside the prefix keeps that endpoint as the VIP.
func TestNetworkTakesEndpointOfLateCluster(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	recreate := deleteCluster(t, g, ns)
	nw := adoptedNetwork(ns, "10.10.3.96/28")
	nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonWaitingForCluster)

	g.Expect(recreate(clusterv1.APIEndpoint{Host: "10.10.3.100", Port: 6443})).To(Succeed())
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	waitReady(g, nw)
	g.Expect(nw.Status.ControlPlaneEndpoint).To(Equal(v1alpha1.APIEndpoint{Host: "10.10.3.100", Port: 6443}))
	vips := vipsOf(g, ns, "default")
	g.Expect(vips).To(HaveLen(1))
	g.Expect(vips[0].Address).To(Equal("10.10.3.100/22"))
}

func TestNetworkRespectsPausedCluster(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	setPaused(t, ns, true)
	nw := vipNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	g.Consistently(func() clusterv1.APIEndpoint { return clusterEndpointOf(g, ns) }).Should(BeZero())

	setPaused(t, ns, false)
	waitReady(g, nw)
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Expect(clusterEndpointOf(g, ns).Host).To(Equal(nw.Status.ControlPlaneEndpoint.Host))
}

// A Cluster that already has an endpoint inside the prefix keeps it: the VIP is that address.
func TestNetworkTakesEndpointOfCluster(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	setClusterEndpoint(t, ns, "10.10.3.100", 6443)
	nw := adoptedNetwork(ns, "10.10.3.96/28")
	nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Expect(nw.Status.ControlPlaneEndpoint).To(Equal(v1alpha1.APIEndpoint{Host: "10.10.3.100", Port: 6443}))
	vips := vipsOf(g, ns, "default")
	g.Expect(vips).To(HaveLen(1))
	g.Expect(vips[0].Address).To(Equal("10.10.3.100/22"))
	g.Expect(clusterEndpointOf(g, ns)).To(Equal(clusterv1.APIEndpoint{Host: "10.10.3.100", Port: 6443}))
}

// An endpoint outside the prefix is left as it is; the network and its VIP stay.
func TestNetworkClusterEndpointOutsidePrefix(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	setClusterEndpoint(t, ns, "10.99.0.5", 6443)
	nw := vipNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonEndpointConflict)
	g.Expect(vipsOf(g, ns, "default")).To(HaveLen(1))
	g.Consistently(func() clusterv1.APIEndpoint { return clusterEndpointOf(g, ns) }).Should(Equal(clusterv1.APIEndpoint{Host: "10.99.0.5", Port: 6443}))
}

// An endpoint inside the prefix on an address NetBox gave to something else cannot be the VIP.
func TestNetworkClusterEndpointTaken(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	byHand(t, g, netbox.IPAddresses, map[string]any{"address": "10.10.3.100/22", "status": "active", "custom_fields": map[string]any{keys.Ref: "elsewhere"}})
	setClusterEndpoint(t, ns, "10.10.3.100", 6443)
	nw := adoptedNetwork(ns, "10.10.3.96/28")
	nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonEndpointConflict)
	waitCondition(g, nw, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Expect(nw.Status.ControlPlaneEndpoint).To(Equal(v1alpha1.APIEndpoint{}))
	g.Expect(vipsOf(g, ns, "default")).To(BeEmpty())
	g.Expect(clusterEndpointOf(g, ns)).To(Equal(clusterv1.APIEndpoint{Host: "10.10.3.100", Port: 6443}))
}

// An endpoint that was set to the VIP and then changed is reported, not overwritten.
func TestNetworkClusterEndpointChanged(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	vip := nw.Status.ControlPlaneEndpoint

	setClusterEndpoint(t, ns, "api.example", 6443)
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonEndpointConflict)
	waitCondition(g, nw, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Expect(nw.Status.ControlPlaneEndpoint).To(Equal(vip))
	g.Consistently(func() clusterv1.APIEndpoint { return clusterEndpointOf(g, ns) }).Should(Equal(clusterv1.APIEndpoint{Host: "api.example", Port: 6443}))
}

// The prefix and the VIP of network default, with owner 1, tenant 7 and the default texts as TestNetworkCreatesWithAttrs creates them.
var (
	networkPrefix = `{"id":2,"prefix":"10.10.0.16/28","status":{"value":"active"},"owner":{"id":1},"tenant":{"id":7},` +
		textsOf("ns/default // cluster ns") + `,"custom_fields":{"capi_ref":"NetBoxNetwork/ns/default"}}`
	networkVIP = `{"id":3,"address":"10.10.0.31/22","status":{"value":"active"},"owner":{"id":1},"tenant":{"id":7},` +
		textsOf("ns/default // ns control plane endpoint") + `,"custom_fields":{"capi_ref":"NetBoxNetwork/ns/default/vip"}}`
)

// networkNetBox serves the parent prefix 10.10.0.0/22. Its prefix and VIP are there as long as prefixLookups
// have not passed the number given; every write is recorded and answered with the prefix and the VIP.
// The prefix itself cannot be read, so a metadata sync fails.
func networkNetBox(t *testing.T, prefixLookups int32, writes *[]string) *httptest.Server {
	t.Helper()
	var lookups atomic.Int32
	srv := httptest.NewServer(writesNeedLock(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/available-prefixes/"):
			*writes = append(*writes, r.URL.Path)
			_, _ = io.WriteString(w, networkPrefix)
		case r.Method != http.MethodGet:
			*writes = append(*writes, r.URL.Path)
			_, _ = io.WriteString(w, networkVIP)
		case r.URL.Path == "/api/ipam/prefixes/2/":
			http.Error(w, "unavailable", http.StatusInternalServerError)
		case r.URL.Path == "/api/ipam/prefixes/" && q.Has("cf_capi_ref") && lookups.Add(1) <= prefixLookups:
			_, _ = io.WriteString(w, `{"results":[`+networkPrefix+`]}`)
		case r.URL.Path == "/api/ipam/prefixes/" && q.Get("prefix") == "10.10.0.0/22":
			_, _ = io.WriteString(w, `{"results":[{"id":1,"prefix":"10.10.0.0/22"}]}`)
		case r.URL.Path == "/api/ipam/ip-addresses/" && q.Has("cf_capi_ref") && prefixLookups > 1:
			_, _ = io.WriteString(w, `{"results":[`+networkVIP+`]}`)
		default:
			_, _ = io.WriteString(w, `{"results":[]}`)
		}
	})))
	t.Cleanup(srv.Close)
	return srv
}

func networkOn(srv *httptest.Server) *NetBoxNetworkReconciler {
	return &NetBoxNetworkReconciler{Client: claimClient(), NetBox: &netbox.Client{URL: srv.URL, Token: "t"}, Keys: netbox.DefaultKeys, Recorder: events.NewFakeRecorder(10)}
}

// A prefix and a VIP that are there as they should be are found without the lock.
func TestNetworkReconcileTakesNoLockWithoutWrites(t *testing.T) {
	g := NewWithT(t)
	var writes []string
	r := networkOn(networkNetBox(t, 1<<30, &writes))
	nw := vipNetwork("ns", "default")
	var vipErr, err error
	g.Expect(returnsWhileLocked(func() { vipErr, _, err = r.reconcileNormal(ctx, nw, &clusterv1.Cluster{}) })).To(BeTrue(), "waited for netBoxWrites")
	g.Expect(vipErr).NotTo(HaveOccurred())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(writes).To(BeEmpty())
	g.Expect(nw.Status.Prefix).To(Equal("10.10.0.16/28"))
	g.Expect(nw.Status.ControlPlaneEndpoint.Host).To(Equal("10.10.0.31"))
}

// The prefix is found in the first pass, which stops at the VIP it must create. The second pass starts from the status
// the network had, not from the one the first left: now the prefix is gone and is carved again, not reported lost.
func TestNetworkSecondPassStartsFromOriginalStatus(t *testing.T) {
	g := NewWithT(t)
	var writes []string
	r := networkOn(networkNetBox(t, 1, &writes))
	nw := vipNetwork("ns", "default")
	var vipErr, err error
	g.Expect(returnsWhileLocked(func() { vipErr, _, err = r.reconcileNormal(ctx, nw, &clusterv1.Cluster{}) })).To(BeFalse(), "did not wait for netBoxWrites")
	g.Expect(vipErr).NotTo(HaveOccurred())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(writes).To(Equal([]string{"/api/ipam/prefixes/1/available-prefixes/", "/api/ipam/ip-addresses/"}))
	g.Expect(nw.Status.Prefix).To(Equal("10.10.0.16/28"))
	g.Expect(nw.Status.PrefixID).To(Equal(int64(2)))
	g.Expect(nw.Status.ControlPlaneEndpoint.Host).To(Equal("10.10.0.31"))
}

// The cache has the conditions of the last reconcile but not its status: the status is read live, so a prefix deleted
// by hand is not carved again and a VIP moved by hand is not taken.
func TestNetworkStaleCacheReadsLiveStatus(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	for name, tc := range map[string]struct {
		prefixLookups int32
		vip           string
		cond, reason  string
	}{
		"prefix deleted": {0, "10.10.0.31", v1alpha1.ReadyCondition, v1alpha1.ReasonPrefixNotFound},
		"VIP moved":      {1 << 30, "10.10.0.29", v1alpha1.ControlPlaneEndpointReadyCondition, v1alpha1.ReasonEndpointConflict},
	} {
		nw := vipNetwork("ns", "default")
		nw.Finalizers = []string{networkFinalizer}
		nw.Status = v1alpha1.NetBoxNetworkStatus{Prefix: "10.10.0.16/28", PrefixID: 2, ControlPlaneEndpoint: v1alpha1.APIEndpoint{Host: tc.vip, Port: 6443},
			Conditions: append(readyAt(0), metav1.Condition{Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason})}
		cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ns"}, Spec: clusterv1.ClusterSpec{Paused: ptr.To(false)}}
		live := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nw, cluster).WithStatusSubresource(nw).Build()
		var writes []string
		r := networkOn(networkNetBox(t, tc.prefixLookups, &writes))
		r.APIReader = live
		r.Client = interceptor.NewClient(live, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if n, ok := obj.(*v1alpha1.NetBoxNetwork); ok {
				n.Status = v1alpha1.NetBoxNetworkStatus{Conditions: n.Status.Conditions}
			}
			return err
		}})

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(nw)})
		g.Expect(err).NotTo(HaveOccurred(), name)
		g.Expect(writes).To(BeEmpty(), name)
		g.Expect(live.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
		g.Expect(meta.FindStatusCondition(nw.Status.Conditions, tc.cond).Reason).To(Equal(tc.reason), name)
		g.Expect(nw.Status.ControlPlaneEndpoint.Host).To(Equal(tc.vip), name)
	}
}

// The prefix and the VIP are created with the owner and tenant the network resolved; ids need no lookup.
func TestNetworkCreatesWithAttrs(t *testing.T) {
	g := NewWithT(t)
	var writes, bodies []string
	back := networkNetBox(t, 1, &writes)
	target, err := url.Parse(back.URL)
	g.Expect(err).NotTo(HaveOccurred())
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			bodies = append(bodies, r.URL.Path+" "+string(b))
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	nw := vipNetwork("ns", "default")
	nw.Spec.Owner, nw.Spec.Tenant = &v1alpha1.OwnerRef{ID: 1}, &v1alpha1.TenantRef{ID: 7}
	nw.Status.OwnerID, nw.Status.TenantID, nw.Status.Conditions = 1, 7, readyAt(nw.Generation) // checked before
	vipErr, _, err := networkOn(front).reconcileNormal(ctx, nw, &clusterv1.Cluster{})
	g.Expect(vipErr).NotTo(HaveOccurred())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(bodies).To(HaveLen(2))
	for _, b := range bodies {
		g.Expect(b).To(And(ContainSubstring(`"owner":1`), ContainSubstring(`"tenant":7`), ContainSubstring(`"comments":"`+managedComments+`"`)))
	}
	g.Expect(bodies[0]).To(ContainSubstring(`"description":"ns/default // cluster ns"`))
	g.Expect(bodies[1]).To(ContainSubstring(`"description":"ns/default // ns control plane endpoint"`))
}

// A network template that does not render is InvalidSpec: nothing reaches NetBox and nothing is retried until the spec changes.
func TestNetworkInvalidTemplate(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	for name, mutate := range map[string]func(*v1alpha1.NetBoxNetworkSpec){
		"description":       func(s *v1alpha1.NetBoxNetworkSpec) { s.Description = "{{ .Pool }}" },
		"endpoint comments": func(s *v1alpha1.NetBoxNetworkSpec) { s.ControlPlaneEndpoint.Comments = "{{ range 2 }}{{ end }}" },
	} {
		var reqs []string
		r := attrsReconciler(t, &reqs)
		nw := vipNetwork("ns", "default")
		mutate(&nw.Spec)
		nw.Spec.ClusterName, nw.Finalizers = "c1", []string{networkFinalizer}
		nw.Status = v1alpha1.NetBoxNetworkStatus{Prefix: "10.10.0.16/28", PrefixID: 2, Conditions: append(readyAt(0),
			metav1.Condition{Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason})}
		cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c1"}, Spec: clusterv1.ClusterSpec{Paused: ptr.To(false)}}
		r.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(nw, cluster).WithStatusSubresource(nw).Build()
		r.APIReader = r.Client

		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(nw)})
		g.Expect(err).NotTo(HaveOccurred(), name)
		g.Expect(res).To(Equal(ctrl.Result{}), name)
		g.Expect(r.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
		ready := meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.ReadyCondition)
		g.Expect(ready.Status).To(Equal(metav1.ConditionFalse), name)
		g.Expect(ready.Reason).To(Equal(v1alpha1.ReasonInvalidSpec), name)
		g.Expect(ready.Message).To(ContainSubstring(strings.TrimPrefix(name, "endpoint ")), name)
		g.Expect(reqs).To(BeEmpty(), name)
	}
}

// attrsSyncNetBox serves the prefix and the VIP of network default with owner 5 and tenant 6 and records every write with its body.
func attrsSyncNetBox(t *testing.T, writes *[]string) *httptest.Server {
	t.Helper()
	const attrs = `"owner":{"id":5},"tenant":{"id":6},"status":{"value":"active"}`
	srv := httptest.NewServer(writesNeedLock(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method != http.MethodGet:
			b, _ := io.ReadAll(r.Body)
			*writes = append(*writes, r.Method+" "+r.URL.Path+" "+string(b))
			_, _ = io.WriteString(w, `{}`)
		case r.URL.Path == "/api/ipam/prefixes/" && r.URL.Query().Has("cf_capi_ref"):
			_, _ = io.WriteString(w, `{"results":[{"id":2,"prefix":"10.10.0.16/28",`+attrs+`,`+textsOf("ns/default // cluster ns")+`,"custom_fields":{"capi_ref":"NetBoxNetwork/ns/default"}}]}`)
		case r.URL.Path == "/api/ipam/ip-addresses/" && r.URL.Query().Has("cf_capi_ref"):
			_, _ = io.WriteString(w, `{"results":[{"id":3,"address":"10.10.0.31/22",`+attrs+`,`+textsOf("ns/default // ns control plane endpoint")+`,"custom_fields":{"capi_ref":"NetBoxNetwork/ns/default/vip"}}]}`)
		default:
			_, _ = io.WriteString(w, `{"results":[]}`)
		}
	})))
	t.Cleanup(srv.Close)
	return srv
}

// The owner, tenant, description and comments the spec sets are synced onto the prefix and the VIP, under the lock, each
// change recorded as an Event. An unset owner or tenant is neither written nor cleared; an adopted prefix keeps its own
// attributes, only the VIP follows the spec.
func TestNetworkSyncsAttrs(t *testing.T) {
	g := NewWithT(t)
	const (
		prefix = "PATCH /api/ipam/prefixes/2/ "
		vip    = "PATCH /api/ipam/ip-addresses/3/ "
		msg    = `"changelog_message":"capi: attributes of NetBoxNetwork/ns/default"`
	)
	for name, tc := range map[string]struct {
		owner, tenant int64
		adopted       bool
		spec          func(*v1alpha1.NetBoxNetworkSpec)
		writes        []string
	}{
		"unset":       {},
		"in sync":     {owner: 5, tenant: 6},
		"both":        {owner: 1, tenant: 7, writes: []string{prefix + `{` + msg + `,"owner":1,"tenant":7}`, vip + `{` + msg + `,"owner":1,"tenant":7}`}},
		"tenant only": {tenant: 7, writes: []string{prefix + `{` + msg + `,"tenant":7}`, vip + `{` + msg + `,"tenant":7}`}},
		"adopted":     {owner: 1, adopted: true, writes: []string{vip + `{` + msg + `,"owner":1}`}},
		"description": {spec: func(s *v1alpha1.NetBoxNetworkSpec) { s.Description = "{{ .Name }} of {{ .Cluster }}" },
			writes: []string{prefix + `{` + msg + `,"description":"default of ns"}`}},
		"endpoint texts": {spec: func(s *v1alpha1.NetBoxNetworkSpec) {
			s.ControlPlaneEndpoint.Description, s.ControlPlaneEndpoint.Comments = "vip", `{{ "" }}`
		},
			writes: []string{vip + `{` + msg + `,"comments":"","description":"vip"}`}},
		"adopted texts": {adopted: true, spec: func(s *v1alpha1.NetBoxNetworkSpec) { s.Description, s.Comments = "a", "b" }},
	} {
		var writes []string
		r := networkOn(attrsSyncNetBox(t, &writes))
		nw := vipNetwork("ns", "default")
		if tc.spec != nil {
			tc.spec(&nw.Spec)
		}
		if tc.owner != 0 {
			nw.Spec.Owner = &v1alpha1.OwnerRef{ID: tc.owner}
		}
		if tc.tenant != 0 {
			nw.Spec.Tenant = &v1alpha1.TenantRef{ID: tc.tenant}
		}
		if tc.adopted {
			nw.Spec.Prefix, nw.Spec.PrefixLength = "10.10.0.16/28", 0
		}
		nw.Status.OwnerID, nw.Status.TenantID, nw.Status.Conditions = tc.owner, tc.tenant, readyAt(nw.Generation)
		vipErr, attrsErr, err := r.reconcileNormal(ctx, nw, &clusterv1.Cluster{})
		g.Expect(attrsErr).NotTo(HaveOccurred(), name)
		g.Expect(vipErr).NotTo(HaveOccurred(), name)
		g.Expect(err).NotTo(HaveOccurred(), name)
		g.Expect(writes).To(Equal(tc.writes), name)
		ev := emitted(r.Recorder)
		g.Expect(ev).To(HaveLen(len(tc.writes)), name)
		for _, e := range ev {
			g.Expect(e).To(HavePrefix("Normal AttributesSynced "), name)
		}
	}
}

// Owner and tenant the token may not set on the prefix and the VIP stop nothing: Ready stays True, MetadataSynced is
// Forbidden and names both, the network is checked again later.
func TestNetworkAttrsForbiddenKeepsReady(t *testing.T) {
	g := NewWithT(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		const attrs = `"owner":{"id":5},"status":{"value":"active"}`
		switch {
		case r.Method != http.MethodGet:
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"detail":"x"}`)
		case r.URL.Path == "/api/ipam/prefixes/" && r.URL.Query().Has("cf_capi_ref"):
			_, _ = io.WriteString(w, `{"results":[{"id":2,"prefix":"10.10.0.16/28",`+attrs+`,"custom_fields":{"capi_ref":"NetBoxNetwork/ns/default"}}]}`)
		case r.URL.Path == "/api/ipam/ip-addresses/" && r.URL.Query().Has("cf_capi_ref"):
			_, _ = io.WriteString(w, `{"results":[{"id":3,"address":"10.10.0.31/22",`+attrs+`,"custom_fields":{"capi_ref":"NetBoxNetwork/ns/default/vip"}}]}`)
		default:
			_, _ = io.WriteString(w, `{"results":[]}`)
		}
	}))
	t.Cleanup(srv.Close)
	scheme := runtime.NewScheme()
	g.Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	nw := vipNetwork("ns", "default")
	nw.Spec.ClusterName, nw.Spec.Owner, nw.Finalizers = "c1", &v1alpha1.OwnerRef{ID: 1}, []string{networkFinalizer}
	nw.Status = v1alpha1.NetBoxNetworkStatus{OwnerID: 1, Prefix: "10.10.0.16/28", PrefixID: 2, ControlPlaneEndpoint: v1alpha1.APIEndpoint{Host: "10.10.0.31", Port: 6443},
		Conditions: append(readyAt(0), metav1.Condition{Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason})}
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c1"}, Spec: clusterv1.ClusterSpec{
		Paused: ptr.To(false), ControlPlaneEndpoint: clusterv1.APIEndpoint{Host: "10.10.0.31", Port: 6443}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nw, cluster).WithStatusSubresource(nw).Build()
	r := networkOn(srv)
	r.Client = c

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(nw)})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: netBoxRequeueAfter}))
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
	g.Expect(meta.IsStatusConditionTrue(nw.Status.Conditions, v1alpha1.ReadyCondition)).To(BeTrue())
	g.Expect(meta.IsStatusConditionTrue(nw.Status.Conditions, v1alpha1.ControlPlaneEndpointReadyCondition)).To(BeTrue())
	m := meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.MetadataSyncedCondition)
	g.Expect(m).NotTo(BeNil())
	g.Expect(m.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(m.Reason).To(Equal(v1alpha1.ReasonForbidden))
	g.Expect(m.Message).To(And(ContainSubstring("attributes of the prefix 10.10.0.16/28"), ContainSubstring("attributes of the VIP 10.10.0.31/22")))
	g.Expect(emitted(r.Recorder)).To(BeEmpty())
}

// retire moves the key of a retained object to retiredRef.
func TestRetireMovesKey(t *testing.T) {
	g := NewWithT(t)
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqs = append(reqs, r.Method+" "+r.URL.Path+" "+string(b))
		_, _ = io.WriteString(w, `{"id":9}`)
	}))
	t.Cleanup(srv.Close)
	r := &NetBoxNetworkReconciler{NetBox: &netbox.Client{URL: srv.URL, Token: "t"}, Keys: netbox.Keys{Instance: "a", Ref: "capi_ref"}, Recorder: events.NewFakeRecorder(10)}
	g.Expect(r.retire(ctx, newNetwork("ns", "default"), netbox.Prefixes, netbox.Object{ID: 9, Prefix: "10.10.0.16/28"}, "a/NetBoxNetwork/ns/default", "PrefixRetired")).To(Succeed())
	g.Expect(reqs).To(Equal([]string{
		`PATCH /api/ipam/prefixes/9/ {"changelog_message":"capi: retired by a/NetBoxNetwork/ns/default","custom_fields":{"capi_ref":"a/NetBoxNetwork/ns/default/retired"}}`,
	}))
}

// attrsNetBox has owners a (1) and b (2) and tenants a (7) and b (8), found by name or slug and by id, and nothing else;
// it records every request. The name and slug "forbidden" and the id 3 may not be viewed; "down" and the id 4 fail.
func attrsNetBox(t *testing.T, reqs *[]string) *netbox.Client {
	t.Helper()
	ids := map[string]string{
		"owners?a": "1", "owners?b": "2", "tenants?a": "7", "tenants?b": "8",
		"owners?1": "1", "owners?2": "2", "tenants?7": "7", "tenants?8": "8",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reqs = append(*reqs, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		q := r.URL.Query()
		if q.Get("name") == "forbidden" || q.Get("slug") == "forbidden" || q.Get("id") == "3" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if q.Get("name") == "down" || q.Get("slug") == "down" || q.Get("id") == "4" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if id, ok := ids[path.Base(r.URL.Path)+"?"+q.Get("name")+q.Get("slug")+q.Get("id")]; ok && r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"results":[{"id":`+id+`}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	t.Cleanup(srv.Close)
	return &netbox.Client{URL: srv.URL, Token: "t"}
}

// attrsReconciler is a network reconciler on attrsNetBox.
func attrsReconciler(t *testing.T, reqs *[]string) *NetBoxNetworkReconciler {
	t.Helper()
	return &NetBoxNetworkReconciler{Client: claimClient(), NetBox: attrsNetBox(t, reqs), Keys: netbox.DefaultKeys, Recorder: events.NewFakeRecorder(10)}
}

func readyAt(generation int64) []metav1.Condition {
	return []metav1.Condition{{Type: v1alpha1.ReadyCondition, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonReady, ObservedGeneration: generation}}
}

// spec.owner and spec.tenant are cached as ids in the status. NetBox is asked (a name or slug looked up, an id checked)
// only when the generation changed or nothing is cached; a Ready of this generation set while waiting for the Cluster,
// for an invalid spec or after a failed lookup did not resolve. Unset is 0 without a request, also over an id cached
// before. Unknown is OwnerNotFound or TenantNotFound and drops the cache; any other failure keeps it, so the claims are
// not woken by a flap. An id the token may not view is taken as given.
func TestResolveIdentityCache(t *testing.T) {
	g := NewWithT(t)
	notReady := func(reason string) []metav1.Condition {
		return []metav1.Condition{{Type: v1alpha1.ReadyCondition, Status: metav1.ConditionFalse, Reason: reason, ObservedGeneration: 2}}
	}
	waiting := notReady(v1alpha1.ReasonWaitingForCluster)
	for name, tc := range map[string]struct {
		owner   *v1alpha1.OwnerRef
		tenant  *v1alpha1.TenantRef
		status  v1alpha1.NetBoxNetworkStatus
		want    netbox.Attrs
		lookups int
		reason  string
	}{
		"unset":                        {},
		"unset drops cache":            {status: v1alpha1.NetBoxNetworkStatus{OwnerID: 1, TenantID: 7, PrefixID: 3, Conditions: readyAt(2)}},
		"name and slug":                {owner: &v1alpha1.OwnerRef{Name: "b"}, tenant: &v1alpha1.TenantRef{Slug: "a"}, want: netbox.Attrs{Owner: 2, Tenant: 7}, lookups: 2},
		"ids checked":                  {owner: &v1alpha1.OwnerRef{ID: 2}, tenant: &v1alpha1.TenantRef{ID: 8}, want: netbox.Attrs{Owner: 2, Tenant: 8}, lookups: 2},
		"ids cached":                   {owner: &v1alpha1.OwnerRef{ID: 2}, tenant: &v1alpha1.TenantRef{ID: 8}, status: v1alpha1.NetBoxNetworkStatus{OwnerID: 2, TenantID: 8, Conditions: readyAt(2)}, want: netbox.Attrs{Owner: 2, Tenant: 8}},
		"id other than cached":         {owner: &v1alpha1.OwnerRef{ID: 1}, status: v1alpha1.NetBoxNetworkStatus{OwnerID: 2, Conditions: readyAt(2)}, want: netbox.Attrs{Owner: 1}, lookups: 1},
		"cached":                       {owner: &v1alpha1.OwnerRef{Name: "b"}, tenant: &v1alpha1.TenantRef{Slug: "b"}, status: v1alpha1.NetBoxNetworkStatus{OwnerID: 2, TenantID: 8, Conditions: readyAt(2)}, want: netbox.Attrs{Owner: 2, Tenant: 8}},
		"cached, not ready":            {owner: &v1alpha1.OwnerRef{Name: "b"}, status: v1alpha1.NetBoxNetworkStatus{OwnerID: 2, Conditions: []metav1.Condition{{Type: v1alpha1.ReadyCondition, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonExhausted, ObservedGeneration: 2}}}, want: netbox.Attrs{Owner: 2}},
		"generation changed":           {owner: &v1alpha1.OwnerRef{Name: "b"}, status: v1alpha1.NetBoxNetworkStatus{OwnerID: 1, Conditions: readyAt(1)}, want: netbox.Attrs{Owner: 2}, lookups: 1},
		"nothing cached":               {tenant: &v1alpha1.TenantRef{Slug: "b"}, status: v1alpha1.NetBoxNetworkStatus{Conditions: readyAt(2)}, want: netbox.Attrs{Tenant: 8}, lookups: 1},
		"waited for cluster":           {owner: &v1alpha1.OwnerRef{Name: "b"}, status: v1alpha1.NetBoxNetworkStatus{OwnerID: 1, Conditions: waiting}, want: netbox.Attrs{Owner: 2}, lookups: 1},
		"unknown owner name":           {owner: &v1alpha1.OwnerRef{Name: "x"}, status: v1alpha1.NetBoxNetworkStatus{OwnerID: 1, Conditions: readyAt(1)}, lookups: 1, reason: v1alpha1.ReasonOwnerNotFound},
		"unknown owner id":             {owner: &v1alpha1.OwnerRef{ID: 9}, lookups: 1, reason: v1alpha1.ReasonOwnerNotFound},
		"unknown tenant slug":          {tenant: &v1alpha1.TenantRef{Slug: "x"}, lookups: 1, reason: v1alpha1.ReasonTenantNotFound},
		"unknown tenant id":            {tenant: &v1alpha1.TenantRef{ID: 9}, status: v1alpha1.NetBoxNetworkStatus{TenantID: 9, Conditions: readyAt(1)}, lookups: 1, reason: v1alpha1.ReasonTenantNotFound},
		"ids not viewable":             {owner: &v1alpha1.OwnerRef{ID: 3}, tenant: &v1alpha1.TenantRef{ID: 3}, want: netbox.Attrs{Owner: 3, Tenant: 3}, lookups: 2},
		"unknown owner, tenant cached": {owner: &v1alpha1.OwnerRef{Name: "x"}, tenant: &v1alpha1.TenantRef{Slug: "b"}, status: v1alpha1.NetBoxNetworkStatus{TenantID: 8, Conditions: readyAt(2)}, want: netbox.Attrs{Tenant: 8}, lookups: 1, reason: v1alpha1.ReasonOwnerNotFound},
		"lookup fails, cache kept":     {owner: &v1alpha1.OwnerRef{Name: "down"}, tenant: &v1alpha1.TenantRef{Slug: "down"}, status: v1alpha1.NetBoxNetworkStatus{OwnerID: 2, TenantID: 8, Conditions: readyAt(1)}, want: netbox.Attrs{Owner: 2, Tenant: 8}, lookups: 2, reason: v1alpha1.ReasonNetBoxError},
		"check fails, cache kept":      {owner: &v1alpha1.OwnerRef{ID: 4}, status: v1alpha1.NetBoxNetworkStatus{OwnerID: 4, Conditions: readyAt(1)}, want: netbox.Attrs{Owner: 4}, lookups: 1, reason: v1alpha1.ReasonNetBoxError},
		"lookup forbidden, cache kept": {owner: &v1alpha1.OwnerRef{Name: "forbidden"}, status: v1alpha1.NetBoxNetworkStatus{OwnerID: 2, Conditions: readyAt(1)}, want: netbox.Attrs{Owner: 2}, lookups: 1, reason: v1alpha1.ReasonForbidden},
		"after a failed lookup":        {owner: &v1alpha1.OwnerRef{Name: "a"}, status: v1alpha1.NetBoxNetworkStatus{OwnerID: 2, Conditions: notReady(v1alpha1.ReasonNetBoxError)}, want: netbox.Attrs{Owner: 1}, lookups: 1},
		"after a forbidden lookup":     {owner: &v1alpha1.OwnerRef{Name: "a"}, status: v1alpha1.NetBoxNetworkStatus{OwnerID: 2, Conditions: notReady(v1alpha1.ReasonForbidden)}, want: netbox.Attrs{Owner: 1}, lookups: 1},
		"after an invalid spec":        {owner: &v1alpha1.OwnerRef{Name: "a"}, status: v1alpha1.NetBoxNetworkStatus{OwnerID: 2, Conditions: notReady(v1alpha1.ReasonInvalidSpec)}, want: netbox.Attrs{Owner: 1}, lookups: 1},
		"after a tenant not found":     {tenant: &v1alpha1.TenantRef{Slug: "a"}, status: v1alpha1.NetBoxNetworkStatus{TenantID: 8, Conditions: notReady(v1alpha1.ReasonTenantNotFound)}, want: netbox.Attrs{Tenant: 7}, lookups: 1},
	} {
		var reqs []string
		r := attrsReconciler(t, &reqs)
		nw := newNetwork("ns", "default")
		nw.Generation, nw.Spec.Owner, nw.Spec.Tenant, nw.Status = 2, tc.owner, tc.tenant, tc.status
		err := r.resolveAttrs(ctx, nw)
		if tc.reason == "" {
			g.Expect(err).NotTo(HaveOccurred(), name)
		} else {
			g.Expect(errReason(err)).To(Equal(tc.reason), name)
		}
		g.Expect(networkAttrs(nw)).To(Equal(tc.want), name)
		g.Expect(reqs).To(HaveLen(tc.lookups), name)
	}
}

// The owner is resolved, but no prefix is acquired; the Cluster goes away, spec.owner changes meanwhile. The Ready of the new
// generation, set while waiting for the Cluster, does not make the old owner look fresh: back with the Cluster, the new one is resolved.
func TestResolveAfterWaitingForCluster(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	nw := newNetwork("ns", "default")
	nw.Spec.ClusterName = "c1"
	nw.Generation, nw.Spec.Owner, nw.Finalizers = 2, &v1alpha1.OwnerRef{Name: "b"}, []string{networkFinalizer}
	nw.Status = v1alpha1.NetBoxNetworkStatus{OwnerID: 1, Conditions: []metav1.Condition{
		{Type: v1alpha1.ReadyCondition, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonParentNotFound, ObservedGeneration: 1},
		{Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason, ObservedGeneration: 2},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nw).WithStatusSubresource(nw).Build()
	var reqs []string
	r := attrsReconciler(t, &reqs)
	r.Client, r.APIReader = c, c
	key := client.ObjectKeyFromObject(nw)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(c.Get(ctx, key, nw)).To(Succeed())
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.ReadyCondition).Reason).To(Equal(v1alpha1.ReasonWaitingForCluster))
	g.Expect(reqs).To(BeEmpty())

	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c1"}, Spec: clusterv1.ClusterSpec{Paused: ptr.To(false)}}
	g.Expect(c.Create(ctx, cluster)).To(Succeed())
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(c.Get(ctx, key, nw)).To(Succeed())
	g.Expect(nw.Status.OwnerID).To(Equal(int64(2)), "the owner of the current spec")
	g.Expect(reqs).To(ContainElement("GET /api/users/owners/?limit=1000&name=b&ordering=id"))
}

// The owner is not found while the tenant lookup fails and keeps the old id. The OwnerNotFound of this generation does
// not make that id look fresh: the next reconcile looks the tenant up again.
func TestResolveTenantAfterOwnerNotFound(t *testing.T) {
	g := NewWithT(t)
	var tenantDown atomic.Bool
	tenantDown.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/owners/"):
			_, _ = io.WriteString(w, `{"results":[]}`)
		case tenantDown.Load():
			w.WriteHeader(http.StatusBadGateway)
		default:
			_, _ = io.WriteString(w, `{"results":[{"id":8}]}`)
		}
	}))
	t.Cleanup(srv.Close)
	r := &NetBoxNetworkReconciler{NetBox: &netbox.Client{URL: srv.URL, Token: "t"}}
	nw := newNetwork("ns", "default")
	nw.Generation, nw.Spec.Owner, nw.Spec.Tenant = 2, &v1alpha1.OwnerRef{Name: "x"}, &v1alpha1.TenantRef{Slug: "b"}
	nw.Status = v1alpha1.NetBoxNetworkStatus{TenantID: 7, Conditions: readyAt(1)}

	err := r.resolveAttrs(ctx, nw)
	setReady(&nw.Status.Conditions, nw.Generation, err)
	g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonOwnerNotFound))
	g.Expect(nw.Status.TenantID).To(Equal(int64(7)), "a failed lookup keeps the old id")

	tenantDown.Store(false)
	g.Expect(errReason(r.resolveAttrs(ctx, nw))).To(Equal(v1alpha1.ReasonOwnerNotFound))
	g.Expect(nw.Status.TenantID).To(Equal(int64(8)))
}

// An owner or a tenant NetBox does not have stops the network before NetBox, drops the cached id and is looked up
// again later; a lookup the token may not make is Forbidden and keeps it.
func TestNetworkOwnerNotFound(t *testing.T) {
	g := NewWithT(t)
	for name, tc := range map[string]struct {
		owner  *v1alpha1.OwnerRef
		tenant *v1alpha1.TenantRef
		reason string
		cached netbox.Attrs
	}{
		"owner":            {&v1alpha1.OwnerRef{Name: "x"}, nil, v1alpha1.ReasonOwnerNotFound, netbox.Attrs{}},
		"owner id":         {&v1alpha1.OwnerRef{ID: 9}, nil, v1alpha1.ReasonOwnerNotFound, netbox.Attrs{}},
		"tenant":           {nil, &v1alpha1.TenantRef{Slug: "x"}, v1alpha1.ReasonTenantNotFound, netbox.Attrs{}},
		"tenant id":        {nil, &v1alpha1.TenantRef{ID: 9}, v1alpha1.ReasonTenantNotFound, netbox.Attrs{}},
		"owner forbidden":  {&v1alpha1.OwnerRef{Name: "forbidden"}, nil, v1alpha1.ReasonForbidden, netbox.Attrs{Owner: 1}},
		"tenant forbidden": {nil, &v1alpha1.TenantRef{Slug: "forbidden"}, v1alpha1.ReasonForbidden, netbox.Attrs{Tenant: 7}},
	} {
		var reqs []string
		r := attrsReconciler(t, &reqs)
		nw := newNetwork("ns", "default")
		nw.Generation, nw.Spec.Owner, nw.Spec.Tenant = 2, tc.owner, tc.tenant
		nw.Status = v1alpha1.NetBoxNetworkStatus{OwnerID: 1, TenantID: 7, Prefix: "10.10.0.16/28", PrefixID: 3, Conditions: readyAt(1)}
		_, _, err := r.reconcileNormal(ctx, nw, &clusterv1.Cluster{})
		g.Expect(errReason(err)).To(Equal(tc.reason), name)
		g.Expect(toResult(err)).To(Equal(ctrl.Result{RequeueAfter: netBoxRequeueAfter}), name)
		g.Expect(reqs).To(HaveLen(1), name)
		g.Expect(networkAttrs(nw)).To(Equal(tc.cached), name)
	}
}

// stamps are the last_updated of the NetBox prefix and VIP: they change with every write.
func stamps(g Gomega, prefix, vip int64) [2]string {
	p, _, err := admin.Get(ctx, netbox.Prefixes, prefix)
	g.Expect(err).NotTo(HaveOccurred())
	v, _, err := admin.Get(ctx, netbox.IPAddresses, vip)
	g.Expect(err).NotTo(HaveOccurred())
	return [2]string{p.LastUpdated, v.LastUpdated}
}

// spec.owner and spec.tenant by name and slug reach the prefix and the VIP and are cached as ids. The same ones
// by id change nothing in NetBox; another owner changes the cache and moves the prefix and the VIP to it.
func TestNetworkSpecOwnerTenant(t *testing.T) {
	g := newG(t)
	z, z2 := newZone(t, g), newZone(t, g)
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	nw.Spec.Owner, nw.Spec.Tenant = &v1alpha1.OwnerRef{Name: z.ownerName}, &v1alpha1.TenantRef{Slug: z.tenantSlug}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	g.Expect(networkAttrs(nw)).To(Equal(netbox.Attrs{Owner: z.owner, Tenant: z.tenant}))
	vips, err := findRef(netbox.IPAddresses, vipRef(keys, nw))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(vips).To(HaveLen(1))
	prefix, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
	g.Expect(err).NotTo(HaveOccurred())
	for _, o := range []netbox.Object{prefix, vips[0]} {
		g.Expect(o.Owner).To(Equal(&netbox.Ref{ID: z.owner}))
		g.Expect(o.Tenant).To(Equal(&netbox.Ref{ID: z.tenant}))
	}
	before := stamps(g, prefix.ID, vips[0].ID)

	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) {
		s.Owner, s.Tenant = &v1alpha1.OwnerRef{ID: z.owner}, &v1alpha1.TenantRef{ID: z.tenant}
	})
	waitReadyAt(g, nw)
	g.Expect(stamps(g, prefix.ID, vips[0].ID)).To(Equal(before))

	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) { s.Owner = &v1alpha1.OwnerRef{Name: z2.ownerName} })
	waitReadyAt(g, nw)
	eventually(g, nw, func(g Gomega) { g.Expect(networkAttrs(nw)).To(Equal(netbox.Attrs{Owner: z2.owner, Tenant: z.tenant})) })
	g.Eventually(func(g Gomega) {
		for _, o := range append(vipsOf(g, ns, "default"), ofNamespace(g, netbox.Prefixes, ns)...) {
			g.Expect(attrsOf(o)).To(Equal(netbox.Attrs{Owner: z2.owner, Tenant: z.tenant}), "object %d", o.ID)
		}
	}).Should(Succeed())
}

// An object outside the permission constraint of change answers 404: deleting its network is Forbidden, keeps the
// finalizer and leaves the prefix active. Back in the zone, the deletion goes through.
func TestNetworkDeleteOutsideConstraint(t *testing.T) {
	g := newG(t)
	z := newZone(t, g)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	nw.Spec.Owner, nw.Spec.Tenant = &v1alpha1.OwnerRef{ID: z.owner}, &v1alpha1.TenantRef{ID: z.tenant}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)

	moveOwner(g, z.owner, "capi") // outside the token's write constraint (hack/dev/seed.sh)
	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonForbidden)
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.ReadyCondition).Message).To(ContainSubstring("NetBox hides ipam/prefixes/"))
	g.Expect(nw.Finalizers).To(ContainElement(networkFinalizer))
	p, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(p.Status.Value).To(Equal("active"))

	moveOwner(g, z.owner, "zones")
	touch(g, nw)
	g.Eventually(gone(nw)).Should(BeTrue())
}

// Deleting a network never resolves its owner or tenant: the ranges, the prefix and the VIP are found by their keys alone.
func TestNetworkDeleteFindsByKey(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	for name, tc := range map[string]struct {
		owner  *v1alpha1.OwnerRef
		tenant *v1alpha1.TenantRef
		status v1alpha1.NetBoxNetworkStatus
	}{
		"nothing":           {},
		"no prefix":         {owner: &v1alpha1.OwnerRef{Name: "b"}},
		"typos":             {owner: &v1alpha1.OwnerRef{Name: "x"}, tenant: &v1alpha1.TenantRef{Slug: "x"}, status: v1alpha1.NetBoxNetworkStatus{Prefix: "10.10.0.16/28"}},
		"lookups forbidden": {owner: &v1alpha1.OwnerRef{Name: "forbidden"}, tenant: &v1alpha1.TenantRef{Slug: "forbidden"}, status: v1alpha1.NetBoxNetworkStatus{Prefix: "10.10.0.16/28"}},
		"cached":            {owner: &v1alpha1.OwnerRef{Name: "b"}, status: v1alpha1.NetBoxNetworkStatus{Prefix: "10.10.0.16/28", OwnerID: 3}},
	} {
		nw := vipNetwork("ns", "default")
		nw.Spec.Owner, nw.Spec.Tenant, nw.Status = tc.owner, tc.tenant, tc.status
		nw.Finalizers = []string{networkFinalizer}
		nw.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		nw.Status.Conditions = []metav1.Condition{{Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason}}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nw).WithStatusSubresource(nw).Build()
		var reqs []string
		r := attrsReconciler(t, &reqs)
		r.Client, r.APIReader = c, c

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(nw)})
		g.Expect(err).NotTo(HaveOccurred(), name)
		g.Expect(apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(nw), nw))).To(BeTrue(), name+": the finalizer is gone")
		g.Expect(reqs).To(Equal([]string{
			"GET /api/ipam/ip-ranges/?cf_capi_ref__isw=NetBoxNetwork%2Fns%2Fdefault%2Fpool%2F&limit=1000&ordering=id",
			"GET /api/ipam/prefixes/?cf_capi_ref=NetBoxNetwork%2Fns%2Fdefault&limit=1000&ordering=id",
			"GET /api/ipam/ip-addresses/?cf_capi_ref=NetBoxNetwork%2Fns%2Fdefault%2Fvip&limit=1000&ordering=id",
			"GET /api/ipam/ip-addresses/?cf_capi_ref__isw=NetBoxNetwork%2Fns%2Fdefault%2Faddresses%2F&limit=1000&ordering=id",
		}), name)
	}
}
