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
	"net/netip"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/layout"
)

// publishing is p publishing in the Secret secret.
func publishing(p v1alpha1.NetworkPool, secret string) v1alpha1.NetworkPool {
	p.Publish = v1alpha1.Publish{Enabled: ptr.To(true), SecretName: secret}
	return p
}

// publishingGroup is group publishing in the Secret secret.
func publishingGroup(group v1alpha1.AddressGroup, secret string) v1alpha1.AddressGroup {
	group.Publish = v1alpha1.Publish{Enabled: ptr.To(true), SecretName: secret}
	return group
}

// secretOf returns the Secret name of ns.
func secretOf(g Gomega, ns, name string) *corev1.Secret {
	s := &corev1.Secret{}
	g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, s)).To(Succeed())
	return s
}

// lines is the value of a published key: the addresses, one per line, no trailing newline.
func lines(addrs ...string) []byte { return []byte(strings.Join(addrs, "\n")) }

func TestPublishData(t *testing.T) {
	g := NewWithT(t)
	a := netip.MustParseAddr
	span := func(from, to string) layout.Span { return layout.Span{From: a(from), To: a(to)} }
	nw := newNetwork("ns", "default")
	nw.Spec.Pools = []v1alpha1.NetworkPool{
		publishing(netPool("cp", 4), "endpoints"), publishing(netPool("ingress", 2), "endpoints"),
		netPool("workers", 0), {Name: "off", Size: netPool("off", 1).Size, Publish: v1alpha1.Publish{Enabled: ptr.To(false), SecretName: "off"}},
		publishing(netPool("other", 1), "other"),
	}
	nw.Spec.Addresses = []v1alpha1.AddressGroup{publishingGroup(metallb(2), "endpoints")}
	l := layout.Layout{
		Pools: []layout.PlacedPool{
			{Pool: layout.Pool{Name: "cp", Size: 4}, Ranges: []layout.Span{span("10.10.0.2", "10.10.0.3"), span("10.10.0.5", "10.10.0.6")}},
			{Pool: layout.Pool{Name: "ingress", Size: 2}, Ranges: []layout.Span{span("10.10.0.8", "10.10.0.9")}},
			{Pool: layout.Pool{Name: "workers", Rest: true}, Ranges: []layout.Span{span("10.10.0.10", "10.10.0.30")}},
			{Pool: layout.Pool{Name: "off", Size: 1}, Ranges: []layout.Span{span("10.10.0.40", "10.10.0.40")}},
			{Pool: layout.Pool{Name: "other", Size: 1}, Ranges: []layout.Span{span("10.10.0.41", "10.10.0.41")}},
		},
		Groups: []layout.PlacedGroup{{Group: layout.Group{Name: "metallb", Count: 2}, Addrs: []netip.Addr{a("10.10.0.62"), a("10.10.0.61")}}},
	}
	want := map[string]map[string][]byte{
		"endpoints": {
			"cp":      lines("10.10.0.2", "10.10.0.3", "10.10.0.5", "10.10.0.6"),
			"ingress": lines("10.10.0.8", "10.10.0.9"),
			"metallb": lines("10.10.0.61", "10.10.0.62"),
		},
		"other": {"other": lines("10.10.0.41")},
	}
	g.Expect(publishData(nw, l, nil)).To(Equal(want))

	// A live exclusion and the VIP drop out of the value of a pool.
	nw.Status.ControlPlaneEndpoint.Host = "10.10.0.5"
	want["endpoints"]["cp"] = lines("10.10.0.2", "10.10.0.6")
	want["endpoints"]["ingress"] = lines("10.10.0.9")
	g.Expect(publishData(nw, l, map[string][]netip.Addr{"cp": {a("10.10.0.3")}, "ingress": {a("10.10.0.8")}})).To(Equal(want))

	nw.Spec.Pools[0].Publish = v1alpha1.Publish{}
	nw.Spec.Pools[1].Publish = v1alpha1.Publish{}
	nw.Spec.Addresses[0].Publish = v1alpha1.Publish{}
	g.Expect(publishData(nw, l, nil)).To(Equal(map[string]map[string][]byte{"other": {"other": lines("10.10.0.41")}}))
}

// publisherOn is a reconciler over a fake API server whose cache must not read Secrets, with the Secret writes recorded.
func publisherOn(t *testing.T, objs ...client.Object) (*NetBoxNetworkReconciler, *[]string) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	var writes []string
	write := func(verb string, obj client.Object) {
		if _, ok := obj.(*corev1.Secret); ok {
			writes = append(writes, verb+" "+obj.GetName())
		}
	}
	return &NetBoxNetworkReconciler{
		Client: interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					t.Errorf("Secret %s read through the cache", key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				write("create", obj)
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				write("update", obj)
				return c.Update(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				write("delete", obj)
				return c.Delete(ctx, obj, opts...)
			},
		}),
		APIReader: base, Recorder: events.NewFakeRecorder(10),
	}, &writes
}

func getSecret(t *testing.T, r *NetBoxNetworkReconciler, name string) (*corev1.Secret, error) {
	t.Helper()
	s := &corev1.Secret{}
	return s, r.APIReader.Get(ctx, client.ObjectKey{Namespace: "ns", Name: name}, s)
}

// Secrets are written only when their data differs; the ones the network published and no longer asks for are deleted,
// a Secret that is not the network's is left alone and not recorded.
func TestPublishWritesOnChange(t *testing.T) {
	g := NewWithT(t)
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "foreign"}, Data: map[string][]byte{"cp": []byte("keep")}}
	r, writes := publisherOn(t, foreign)
	nw := newNetwork("ns", "default")
	nw.UID = types.UID("00000000-0000-0000-0000-000000000001")

	g.Expect(r.publish(ctx, nw, map[string]map[string][]byte{"endpoints": {"cp": lines("10.10.0.2"), "metallb": lines("10.10.0.61")}})).To(Succeed())
	s, err := getSecret(t, r, "endpoints")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(s.Type).To(Equal(corev1.SecretTypeOpaque))
	g.Expect(s.Data).To(Equal(map[string][]byte{"cp": lines("10.10.0.2"), "metallb": lines("10.10.0.61")}))
	g.Expect(metav1.GetControllerOf(s)).NotTo(BeNil())
	g.Expect(metav1.GetControllerOf(s).UID).To(Equal(nw.UID))
	g.Expect(nw.Status.PublishedSecrets).To(Equal([]string{"endpoints"}))
	g.Expect(*writes).To(Equal([]string{"create endpoints"}))

	g.Expect(r.publish(ctx, nw, map[string]map[string][]byte{"endpoints": {"cp": lines("10.10.0.2"), "metallb": lines("10.10.0.61")}})).To(Succeed())
	g.Expect(*writes).To(HaveLen(1), "same data written again")

	g.Expect(r.publish(ctx, nw, map[string]map[string][]byte{"endpoints": {"cp": lines("10.10.0.2", "10.10.0.3")}, "second": {"x": lines("10.10.0.9")}})).To(Succeed())
	s, _ = getSecret(t, r, "endpoints")
	g.Expect(s.Data).To(Equal(map[string][]byte{"cp": lines("10.10.0.2", "10.10.0.3")}))
	g.Expect(*writes).To(Equal([]string{"create endpoints", "update endpoints", "create second"}))
	g.Expect(nw.Status.PublishedSecrets).To(Equal([]string{"endpoints", "second"}))

	err = r.publish(ctx, nw, map[string]map[string][]byte{"foreign": {"cp": lines("10.10.0.2")}, "second": {"x": lines("10.10.0.9")}})
	g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonSecretConflict))
	g.Expect(err).To(MatchError(ContainSubstring("secret foreign exists and is not managed by network default")))
	s, _ = getSecret(t, r, "foreign")
	g.Expect(s.Data).To(Equal(foreign.Data))
	_, err = getSecret(t, r, "endpoints")
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "endpoints not asked for any more")
	g.Expect(nw.Status.PublishedSecrets).To(Equal([]string{"second"}))

	// A Secret in the status that someone else owns now is dropped from it, not deleted.
	nw.Status.PublishedSecrets = []string{"foreign", "second"}
	g.Expect(r.publish(ctx, nw, nil)).To(Succeed())
	_, err = getSecret(t, r, "foreign")
	g.Expect(err).NotTo(HaveOccurred())
	_, err = getSecret(t, r, "second")
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	g.Expect(nw.Status.PublishedSecrets).To(BeEmpty())
}

// The Secret of a network created again of the same name is the network's: it takes the new UID.
func TestPublishTakesNewUID(t *testing.T) {
	g := NewWithT(t)
	nw := newNetwork("ns", "default")
	nw.UID = types.UID("00000000-0000-0000-0000-000000000002")
	old := newNetwork("ns", "default")
	old.UID = types.UID("00000000-0000-0000-0000-000000000001")
	r, writes := publisherOn(t)
	data := map[string]map[string][]byte{"endpoints": {"cp": lines("10.10.0.2")}}
	g.Expect(r.publish(ctx, old, data)).To(Succeed())

	g.Expect(r.publish(ctx, nw, data)).To(Succeed())
	s, _ := getSecret(t, r, "endpoints")
	g.Expect(metav1.GetControllerOf(s).UID).To(Equal(nw.UID))
	g.Expect(*writes).To(Equal([]string{"create endpoints", "update endpoints"}))
}

func TestPublishTooManySecrets(t *testing.T) {
	g := NewWithT(t)
	r, writes := publisherOn(t)
	data := map[string]map[string][]byte{}
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p", "q"} {
		data[n] = map[string][]byte{"x": lines("10.10.0.2")}
	}
	err := r.publish(ctx, newNetwork("ns", "default"), data)
	g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonInvalidSpec))
	g.Expect(*writes).To(BeEmpty())
}

// A Secret shared by a pool and a group of the network holds both, is created with an owner reference, follows a count
// and is deleted with the last enabled flag.
func TestPublishSecret(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := reserveNetwork(ns, "default")
	nw.Spec.Pools[0] = publishing(nw.Spec.Pools[0], "endpoints")
	nw.Spec.Addresses[0] = publishingGroup(nw.Spec.Addresses[0], "endpoints")
	p := readyLayout(g, nw)

	want := map[string][]byte{"cp": lines(addr(p, 0), addr(p, 1), addr(p, 2)), "metallb": lines(addr(p, 13), addr(p, 14))}
	g.Eventually(func(g Gomega) {
		s := secretOf(g, ns, "endpoints")
		g.Expect(s.Data).To(Equal(want))
	}).Should(Succeed())
	s := secretOf(g, ns, "endpoints")
	g.Expect(s.Type).To(Equal(corev1.SecretTypeOpaque))
	g.Expect(metav1.IsControlledBy(s, nw)).To(BeTrue())
	eventually(g, nw, func(g Gomega) { g.Expect(nw.Status.PublishedSecrets).To(Equal([]string{"endpoints"})) })

	setGroups(g, nw, publishingGroup(metallb(3), "endpoints"))
	waitLayoutReady(g, nw)
	want["metallb"] = lines(addr(p, 12), addr(p, 13), addr(p, 14))
	g.Eventually(func(g Gomega) { g.Expect(secretOf(g, ns, "endpoints").Data).To(Equal(want)) }).Should(Succeed())

	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) {
		s.Pools[0].Publish = v1alpha1.Publish{Enabled: ptr.To(false), SecretName: "endpoints"}
	})
	delete(want, "cp")
	g.Eventually(func(g Gomega) { g.Expect(secretOf(g, ns, "endpoints").Data).To(Equal(want)) }).Should(Succeed())

	setGroups(g, nw, metallb(3))
	g.Eventually(gone(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "endpoints"}})).Should(BeTrue())
	eventually(g, nw, func(g Gomega) { g.Expect(nw.Status.PublishedSecrets).To(BeEmpty()) })
	waitLayoutReady(g, nw)
}

// A Secret of the name that the network does not control is not touched: LayoutReady says SecretConflict, the layout
// itself is applied.
func TestPublishSkipsForeignSecret(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "endpoints"}, Data: map[string][]byte{"cp": []byte("keep")}}
	g.Expect(k8s.Create(ctx, foreign)).To(Succeed())
	nw := layoutNetwork(ns, "default")
	nw.Spec.Pools[0] = publishing(nw.Spec.Pools[0], "endpoints")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())

	waitTypedCondition(g, nw, v1alpha1.LayoutReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonSecretConflict)
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.LayoutReadyCondition).Message).To(ContainSubstring("secret endpoints exists and is not managed by network default"))
	g.Expect(rangesIn(g, nw, "cp")).NotTo(BeEmpty())
	g.Expect(nw.Status.PublishedSecrets).To(BeEmpty())
	g.Consistently(func(g Gomega) {
		s := secretOf(g, ns, "endpoints")
		g.Expect(s.Data).To(Equal(foreign.Data))
		g.Expect(s.ResourceVersion).To(Equal(foreign.ResourceVersion))
	}).Should(Succeed())
}

// A change that is refused is not published: the Secret keeps the addresses that are applied.
func TestPublishWaitsForApply(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	nw.Spec.Pools[0] = publishing(nw.Spec.Pools[0], "endpoints")
	p := readyLayout(g, nw)
	old := map[string][]byte{"cp": lines(addr(p, 0), addr(p, 1), addr(p, 2))}
	g.Eventually(func(g Gomega) { g.Expect(secretOf(g, ns, "endpoints").Data).To(Equal(old)) }).Should(Succeed())

	pool := newPool(ns, "workers", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "workers"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	m1 := newClaim(ns, "m1", "workers")
	g.Expect(k8s.Create(ctx, m1)).To(Succeed())
	g.Expect(address(g, m1).Spec.Address).To(Equal(addr(p, 3)))

	setPools(g, nw, publishing(netPool("cp", 4), "endpoints"), netPool("workers", 0))
	waitTypedCondition(g, nw, v1alpha1.LayoutReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonRangeInUse)
	rv := secretOf(g, ns, "endpoints").ResourceVersion
	g.Consistently(func(g Gomega) {
		s := secretOf(g, ns, "endpoints")
		g.Expect(s.Data).To(Equal(old))
		g.Expect(s.ResourceVersion).To(Equal(rv))
	}).Should(Succeed())

	setPools(g, nw, publishing(netPool("cp", 3), "endpoints"), netPool("workers", 0))
	waitLayoutReady(g, nw)
	g.Expect(secretOf(g, ns, "endpoints").Data).To(Equal(old))
}
