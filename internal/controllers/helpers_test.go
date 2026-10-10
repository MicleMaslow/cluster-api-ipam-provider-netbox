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
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

func newG(t *testing.T) *WithT {
	t.Helper()
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(30 * time.Second)
	g.SetDefaultEventuallyPollingInterval(250 * time.Millisecond)
	g.SetDefaultConsistentlyDuration(3 * time.Second)
	g.SetDefaultConsistentlyPollingInterval(250 * time.Millisecond)
	return g
}

// emitted drains the Events that a FakeRecorder collected.
func emitted(r events.EventRecorder) []string {
	fake, ok := r.(*events.FakeRecorder)
	if !ok {
		panic("emitted: not a FakeRecorder")
	}
	var out []string
	for {
		select {
		case e := <-fake.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// waitEvent waits for a Normal Event with reason about the object name of ns.
func waitEvent(g *WithT, ns, name, reason string) {
	g.Eventually(func(g Gomega) {
		list := &eventsv1.EventList{}
		g.Expect(k8s.List(ctx, list, client.InNamespace(ns))).To(Succeed())
		g.Expect(slices.ContainsFunc(list.Items, func(e eventsv1.Event) bool {
			return e.Type == corev1.EventTypeNormal && e.Reason == reason && e.Regarding.Name == name
		})).To(BeTrue(), "no %s event about %s", reason, name)
	}).Should(Succeed())
}

// returnsWhileLocked reports whether f finishes while netBoxWrites is held, i.e. without waiting for the lock.
// A section that does wait is let through once the time is up, so f always finishes.
func returnsWhileLocked(f func()) bool {
	netBoxWrites.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
		netBoxWrites.Unlock()
		return true
	case <-time.After(500 * time.Millisecond):
	}
	netBoxWrites.Unlock()
	<-done
	return false
}

// writesNeedLock fails t for a NetBox write that arrives while netBoxWrites is free.
func writesNeedLock(t *testing.T, next http.Handler) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && netBoxWrites.TryLock() {
			netBoxWrites.Unlock()
			t.Errorf("%s %s without netBoxWrites", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

func needNetBox(t *testing.T) {
	t.Helper()
	if k8s == nil {
		t.Skip("NETBOX_URL not set: run make test-integration")
	}
}

// newEnv creates a namespace with a Cluster of the same name. On cleanup it pauses the Cluster
// and deletes whatever the test left in NetBox.
func newEnv(t *testing.T, clusterLabels map[string]string) string {
	t.Helper()
	needNetBox(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "t-"}}
	if err := k8s.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: ns.Name, Labels: clusterLabels},
		Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)}, // the v1beta2 CRD rejects an empty spec
	}
	if err := k8s.Create(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		setPaused(t, ns.Name, true)
		time.Sleep(time.Second) // let an in-flight reconcile finish before NetBox is cleaned
		for _, path := range []string{netbox.IPAddresses, netbox.IPRanges, netbox.Prefixes} {
			objs, err := admin.List(ctx, path, url.Values{"cf_" + netbox.FieldCluster: {ns.Name}})
			if err != nil {
				t.Error(err)
				continue
			}
			for _, o := range objs {
				if err := admin.Delete(ctx, path, o.ID, "", "test cleanup"); err != nil {
					t.Error(err)
				}
			}
		}
	})
	return ns.Name
}

func updateCluster(t *testing.T, name string, mutate func(*clusterv1.Cluster)) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		c := &clusterv1.Cluster{}
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: name, Name: name}, c); err != nil {
			return err
		}
		mutate(c)
		return k8s.Update(ctx, c)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func setPaused(t *testing.T, name string, paused bool) {
	t.Helper()
	updateCluster(t, name, func(c *clusterv1.Cluster) { c.Spec.Paused = ptr.To(paused) })
}

func newNetwork(ns, name string) *v1alpha1.NetBoxNetwork {
	return &v1alpha1.NetBoxNetwork{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.NetBoxNetworkSpec{
			ClusterName: ns, ParentPrefix: "10.10.0.0/22", Gateway: "10.10.0.1", PrefixLength: 28,
		},
	}
}

type conditioned interface {
	client.Object
	GetConditions() []metav1.Condition
}

// waitCondition waits until obj's Ready condition has status and reason, refreshing obj.
func waitCondition(g *WithT, obj conditioned, status metav1.ConditionStatus, reason string) {
	waitTypedCondition(g, obj, v1alpha1.ReadyCondition, status, reason)
}

// waitTypedCondition waits until obj's condition of type condType has status and reason, refreshing obj.
func waitTypedCondition(g *WithT, obj conditioned, condType string, status metav1.ConditionStatus, reason string) {
	g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
		c := meta.FindStatusCondition(obj.GetConditions(), condType)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Status).To(Equal(status), c.Message)
		g.Expect(c.Reason).To(Equal(reason), c.Message)
	}).Should(Succeed())
}

// waitReady waits for Ready=True and the status written with it: the patch helper writes conditions first.
func waitReady(g *WithT, obj conditioned) {
	waitCondition(g, obj, metav1.ConditionTrue, v1alpha1.ReasonReady)
	eventually(g, obj, func(g Gomega) {
		switch o := obj.(type) {
		case *v1alpha1.NetBoxNetwork:
			g.Expect(o.Status.Prefix).NotTo(BeEmpty())
			if o.Spec.ControlPlaneEndpoint != nil {
				g.Expect(o.Status.ControlPlaneEndpoint.Host).NotTo(BeEmpty())
			}
		case *v1alpha1.NetBoxIPPool:
			g.Expect(o.Status.Ranges).NotTo(BeEmpty())
			g.Expect(o.Status.IPAddresses).NotTo(BeNil())
		}
	})
}

// eventually refreshes obj until check passes.
func eventually(g *WithT, obj client.Object, check func(g Gomega)) {
	g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
		check(g)
	}).Should(Succeed())
}

// moveAnnotate marks obj the way clusterctl move does before it deletes it from the source cluster.
func moveAnnotate(g *WithT, obj client.Object) {
	g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
		annotations.AddAnnotations(obj, map[string]string{clusterctlv1.DeleteForMoveAnnotation: ""})
		g.Expect(k8s.Update(ctx, obj)).To(Succeed())
	}).Should(Succeed())
}

// forget drops obj the way clusterctl move does on the source cluster: annotated, deleted, finalizers stripped if still there.
func forget(g *WithT, obj client.Object) {
	moveAnnotate(g, obj)
	g.Expect(k8s.Delete(ctx, obj)).To(Succeed())
	g.Eventually(func(g Gomega) {
		err := k8s.Get(ctx, client.ObjectKeyFromObject(obj), obj)
		if apierrors.IsNotFound(err) {
			return
		}
		g.Expect(err).NotTo(HaveOccurred())
		obj.SetFinalizers(nil)
		g.Expect(client.IgnoreNotFound(k8s.Update(ctx, obj))).To(Succeed())
	}).Should(Succeed())
	g.Eventually(gone(obj)).Should(BeTrue())
}

// touch updates an annotation on obj, which makes its controller reconcile it.
func touch(g *WithT, obj client.Object) {
	g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
		annotations.AddAnnotations(obj, map[string]string{"test/touched": time.Now().Format(time.RFC3339Nano)})
		g.Expect(k8s.Update(ctx, obj)).To(Succeed())
	}).Should(Succeed())
}

func gone(obj client.Object) func() bool {
	return func() bool { return apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKeyFromObject(obj), obj)) }
}

// byHand creates an object in NetBox as an admin would and deletes it after the test.
func byHand(t *testing.T, g *WithT, path string, fields map[string]any) netbox.Object {
	t.Helper()
	o, err := admin.Create(ctx, path, fields)
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(func() { _ = admin.Delete(ctx, path, o.ID, "", "test cleanup") })
	return o
}

// otherOwner is the Owner of another management plane, seeded by hack/dev/seed.sh.
func otherOwner(g *WithT) netbox.Owner {
	o, err := admin.LookupOwner(ctx, "other-owner")
	g.Expect(err).NotTo(HaveOccurred(), "re-run hack/dev/seed.sh")
	return o
}

func cfString(o netbox.Object, name string) string {
	var s string
	_ = json.Unmarshal(o.CustomFields[name], &s)
	return s
}

func setTags(g *WithT, nw *v1alpha1.NetBoxNetwork, tags []string) {
	g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
		nw.Spec.Tags = tags
		g.Expect(k8s.Update(ctx, nw)).To(Succeed())
	}).Should(Succeed())
}

// forbiddenNetBox is a NetBox that finds every object by ref (a ref prefix finds network "default") as object 7,
// and answers 403 to every write.
func forbiddenNetBox(t *testing.T) *netbox.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		q := r.URL.Query()
		ref := q.Get("cf_" + netbox.FieldRef)
		if p := q.Get("cf_" + netbox.FieldRef + "__isw"); p != "" {
			ref = p + "default"
		}
		obj, _ := json.Marshal(map[string]any{"id": 7, "start_address": "10.10.0.2/22", "end_address": "10.10.0.3/22", "custom_fields": map[string]string{netbox.FieldRef: ref}})
		if strings.HasSuffix(r.URL.Path, "/7/") {
			_, _ = w.Write(obj)
			return
		}
		_, _ = fmt.Fprintf(w, `{"results":[%s]}`, obj)
	}))
	t.Cleanup(srv.Close)
	return &netbox.Client{URL: srv.URL, Token: "t"}
}
