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
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
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
