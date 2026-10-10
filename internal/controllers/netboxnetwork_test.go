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
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
	g.Expect(owner.Owns(obj)).To(BeTrue())
	g.Expect(obj.Status.Value).To(Equal("active"))
	g.Expect(cfString(obj, netbox.FieldRef)).To(Equal("NetBoxNetwork/" + ns + "/default"))
	g.Expect(cfString(obj, netbox.FieldCluster)).To(Equal(ns))
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
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	g.Expect(nw.Status.Prefix).To(Equal("10.10.1.0/28"))
	g.Expect(nw.Status.AddressPrefixLength).To(Equal(int32(22)))
	obj, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(obj.IsPool).To(BeTrue())

	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 0, End: -1})
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
	g.Expect(admin.Delete(ctx, netbox.Prefixes, nw.Status.PrefixID, "test: deleted by hand")).To(Succeed())

	touch(g, nw)
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonPrefixNotFound)
	g.Consistently(func() ([]netbox.Object, error) {
		return admin.List(ctx, netbox.Prefixes, owner.ByRef("NetBoxNetwork/"+ns+"/default"))
	}).Should(BeEmpty())
}

func TestNetworkAdoptsOwnedPrefix(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	existing, err := admin.Create(ctx, netbox.Prefixes, map[string]any{
		"prefix": "10.10.3.240/28", "status": "active", "owner": owner.ID,
		"custom_fields": map[string]any{netbox.FieldCluster: ns},
	})
	g.Expect(err).NotTo(HaveOccurred())

	nw := newNetwork(ns, "default")
	nw.Spec.PrefixLength, nw.Spec.Prefix = 0, "10.10.3.240/28"
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	g.Expect(nw.Status.PrefixID).To(Equal(existing.ID))
	obj, _, err := admin.Get(ctx, netbox.Prefixes, existing.ID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfString(obj, netbox.FieldRef)).To(Equal("NetBoxNetwork/" + ns + "/default"))
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
	g.Expect(owner.Owns(obj)).To(BeTrue())
}

func TestNetworkRefusesPrefixWithoutOurOwner(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	foreign, err := admin.Create(ctx, netbox.Prefixes, map[string]any{
		"prefix": "10.10.3.224/28", "status": "active", "description": "someone else's",
		"custom_fields": map[string]any{netbox.FieldCluster: ns},
	})
	g.Expect(err).NotTo(HaveOccurred())

	nw := newNetwork(ns, "default")
	nw.Spec.PrefixLength, nw.Spec.Prefix = 0, "10.10.3.224/28"
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonNotOwned)
	obj, _, err := admin.Get(ctx, netbox.Prefixes, foreign.ID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(obj.Owner).To(BeNil())
	g.Expect(cfString(obj, netbox.FieldRef)).To(BeEmpty())
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

func TestNetworkSkipsPausedCluster(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	setPaused(t, ns, true)
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	g.Consistently(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
		g.Expect(nw.Status.Prefix).To(BeEmpty())
	}).Should(Succeed())

	setPaused(t, ns, false)
	waitReady(g, nw)
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
	g.Expect(cfString(obj, netbox.FieldRef)).To(Equal("NetBoxNetwork/" + ns + "/first"))
}

// adoptedNetwork is network "default" of ns adopting cidr.
func adoptedNetwork(ns, cidr string) *v1alpha1.NetBoxNetwork {
	nw := newNetwork(ns, "default")
	nw.Spec.PrefixLength, nw.Spec.Prefix = 0, cidr
	return nw
}

// manualPrefix is a prefix an admin made by hand with status and gave our owner.
func manualPrefix(t *testing.T, g *WithT, cidr, status string) netbox.Object {
	t.Helper()
	return byHand(t, g, netbox.Prefixes, map[string]any{"prefix": cidr, "status": status, "description": "manual", "owner": owner.ID})
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
	g.Expect(cfString(obj, netbox.FieldRef)).To(Equal("NetBoxNetwork/" + ns + "/default"))
	g.Expect(cfString(obj, netbox.FieldCluster)).To(Equal(ns))
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
		g.Expect(cfString(obj, netbox.FieldRef)).To(Equal("NetBoxNetwork/" + ns + "/default"))
		g.Expect(obj.Status.Value).To(Equal("deprecated"))
	}).Should(Succeed())
	keptByAdmin(g, manual.ID, "deprecated")
}

func TestNetworkAdoptReclaimsRetainedPrefix(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	retained := byHand(t, g, netbox.Prefixes, map[string]any{
		"prefix": "10.10.3.112/28", "status": "deprecated", "owner": owner.ID,
		"custom_fields": map[string]any{netbox.FieldRef: "NetBoxNetwork/" + ns + "/old", netbox.FieldCluster: ns},
	})

	nw := adoptedNetwork(ns, "10.10.3.112/28")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	obj, _, err := admin.Get(ctx, netbox.Prefixes, retained.ID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfString(obj, netbox.FieldRef)).To(Equal("NetBoxNetwork/" + ns + "/default"))
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
	g.Expect(cfString(obj, netbox.FieldRef)).To(BeEmpty())
	g.Expect(cfString(obj, netbox.FieldCluster)).To(BeEmpty())
	g.Expect(owner.Owns(obj)).To(BeTrue())
	keptByAdmin(g, manual.ID, "reserved")
}

func TestNetworkForeignOwnerGuard(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	ref := "NetBoxNetwork/" + ns + "/default"
	byHand(t, g, netbox.Prefixes, map[string]any{
		"prefix": "10.20.3.240/28", "status": "active", "owner": otherOwner(g).ID,
		"custom_fields": map[string]any{netbox.FieldRef: ref, netbox.FieldCluster: ns},
	})

	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonOwnedByOtherOwner)
	g.Consistently(func() ([]netbox.Object, error) {
		return admin.List(ctx, netbox.Prefixes, owner.ByRef(ref))
	}).Should(BeEmpty())
}

func TestNetworkDeprecatedForeignNamespace(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	foreign := byHand(t, g, netbox.Prefixes, map[string]any{
		"prefix": "10.10.3.144/28", "status": "deprecated", "owner": owner.ID,
		"custom_fields": map[string]any{netbox.FieldRef: "NetBoxNetwork/elsewhere/default", netbox.FieldCluster: ns},
	})

	nw := adoptedNetwork(ns, "10.10.3.144/28")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitCondition(g, nw, metav1.ConditionFalse, v1alpha1.ReasonDuplicate)
	obj, _, err := admin.Get(ctx, netbox.Prefixes, foreign.ID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfString(obj, netbox.FieldRef)).To(Equal("NetBoxNetwork/elsewhere/default"))
	g.Expect(obj.Status.Value).To(Equal("deprecated"))
}

func TestHeldBy(t *testing.T) {
	const ref = "NetBoxNetwork/ns/a"
	prefix := func(got, status string) netbox.Object {
		return netbox.Object{Status: netbox.Choice{Value: status}, CustomFields: map[string]json.RawMessage{netbox.FieldRef: json.RawMessage(strconv.Quote(got))}}
	}
	for name, tc := range map[string]struct {
		obj  netbox.Object
		want string
	}{
		"free":                        {prefix("", "active"), ""},
		"ours":                        {prefix(ref, "active"), ""},
		"held":                        {prefix("NetBoxNetwork/ns/b", "active"), "NetBoxNetwork/ns/b"},
		"deprecated, same namespace":  {prefix("NetBoxNetwork/ns/b", "deprecated"), ""},
		"deprecated, other namespace": {prefix("NetBoxNetwork/other/a", "deprecated"), "NetBoxNetwork/other/a"},
	} {
		if got := heldBy(tc.obj, ref); got != tc.want {
			t.Errorf("%s: heldBy = %q, want %q", name, got, tc.want)
		}
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
		m := map[string]any{}
		_ = json.Unmarshal(o.CustomFields["labels"], &m)
		return m
	}
	g.Eventually(labels).Should(Equal(map[string]any{"cluster": ns, "role": "workload"}))

	g.Expect(admin.Patch(ctx, netbox.Prefixes, nw.Status.PrefixID, "", map[string]any{"custom_fields": map[string]any{
		"labels": map[string]any{"cluster": ns, "role": "workload", "team": "x"},
	}})).Error().To(Succeed())
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

	o, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfString(o, netbox.FieldRef)).To(Equal("NetBoxNetwork/"+ns+"/default"), "patching custom fields keeps the bookkeeping fields")
}

func TestNetworkRejectsBookkeepingCustomFields(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	for _, field := range []string{netbox.FieldRef, netbox.FieldCluster} {
		name := strings.ReplaceAll(field, "_", "-")
		nw := newNetwork(ns, name)
		nw.Spec.CustomFields = map[string]apiextensionsv1.JSON{field: {Raw: []byte(`"x"`)}}
		g.Expect(k8s.Create(ctx, nw)).To(Succeed())
		waitReady(g, nw)
		waitTypedCondition(g, nw, v1alpha1.MetadataSyncedCondition, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec)
		g.Expect(nw.Status.PrefixID).NotTo(BeZero())

		o, _, err := admin.Get(ctx, netbox.Prefixes, nw.Status.PrefixID)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cfString(o, netbox.FieldRef)).To(Equal("NetBoxNetwork/" + ns + "/" + name))
		g.Expect(cfString(o, netbox.FieldCluster)).To(Equal(ns))
	}
}

func TestNetworkTags(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
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
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 5})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
}

func TestNetworkDeleteForbidden(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	nw := newNetwork("ns", "default")
	nw.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete
	nw.Finalizers = []string{networkFinalizer}
	nw.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nw).WithStatusSubresource(nw).Build()
	r := &NetBoxNetworkReconciler{Client: c, NetBox: forbiddenNetBox(t)}

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(nw)})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{RequeueAfter: netBoxRequeueAfter}))
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
	g.Expect(nw.Finalizers).To(ConsistOf(networkFinalizer))
	cond := meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.ReadyCondition)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(v1alpha1.ReasonForbidden))
}

// vipNetwork is newNetwork asking for a VIP.
func vipNetwork(ns, name string) *v1alpha1.NetBoxNetwork {
	nw := newNetwork(ns, name)
	nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{}
	return nw
}

// vipsOf are the NetBox addresses held as the VIP of network name.
func vipsOf(g Gomega, ns, name string) []netbox.Object {
	vips, err := admin.FindByRef(ctx, netbox.IPAddresses, owner, "NetBoxNetwork/"+ns+"/"+name+"/vip")
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
	g.Expect(vips[0].Description).To(Equal(ns + " control plane endpoint"))
	g.Expect(cfString(vips[0], netbox.FieldCluster)).To(Equal(ns))

	// Steady state writes nothing: the dns_name comparison must agree with NetBox's lower case.
	// Every save bumps last_updated, a no-op one too.
	updated := func() string { return vipsOf(g, ns, "default")[0].LastUpdated }
	touch(g, nw)
	g.Consistently(updated, "5s").Should(Equal(vips[0].LastUpdated))

	g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
		nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{Port: 8443, DNSName: "api2.cluster.example"}
		g.Expect(k8s.Update(ctx, nw)).To(Succeed())
	}).Should(Succeed())
	eventually(g, nw, func(g Gomega) {
		g.Expect(nw.Status.ControlPlaneEndpoint).To(Equal(v1alpha1.APIEndpoint{Host: want, Port: 8443}))
	})
	// The Cluster keeps the port it was given.
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonEndpointConflict)
	g.Eventually(func() string { return vipsOf(g, ns, "default")[0].DNSName }).Should(Equal("api2.cluster.example"))
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
	g.Expect(nw.Status.Prefix).To(Equal("10.10.3.80/32"))
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

func TestNetworkVIPDeletedWithNetwork(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := vipNetwork(ns, "default")
	nw.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	g.Expect(vipsOf(g, ns, "default")).To(HaveLen(1))

	g.Expect(k8s.Delete(ctx, nw)).To(Succeed())
	g.Eventually(gone(nw)).Should(BeTrue())
	g.Expect(vipsOf(g, ns, "default")).To(BeEmpty())
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
	g.Expect(admin.Delete(ctx, netbox.IPAddresses, above.ID, "test: freed")).To(Succeed())
	g.Expect(admin.Delete(ctx, netbox.IPAddresses, vipsOf(g, ns, "default")[0].ID, "test: deleted by hand")).To(Succeed())
	touch(g, nw)
	g.Eventually(func(g Gomega) {
		vips := vipsOf(g, ns, "default")
		g.Expect(vips).To(HaveLen(1))
		g.Expect(vips[0].Address).To(Equal("10.10.3.110/22"))
	}).Should(Succeed())
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)

	g.Expect(admin.Delete(ctx, netbox.IPAddresses, vipsOf(g, ns, "default")[0].ID, "test: deleted by hand")).To(Succeed())
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

// No VIP is picked before the Cluster exists: it may bring an endpoint of its own.
func TestNetworkWaitsForCluster(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	recreate := deleteCluster(t, g, ns)

	nw := vipNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonWaitingForCluster)
	waitCondition(g, nw, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Consistently(func() []netbox.Object { return vipsOf(g, ns, "default") }, "3s").Should(BeEmpty())
	g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
	g.Expect(nw.Status.ControlPlaneEndpoint).To(BeZero())

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
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonWaitingForCluster)

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
	byHand(t, g, netbox.IPAddresses, map[string]any{"address": "10.10.3.100/22", "status": "active"})
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
