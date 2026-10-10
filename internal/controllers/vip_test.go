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
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

func TestPickVIP(t *testing.T) {
	p := netip.MustParsePrefix
	a := netip.MustParseAddr
	l2 := p("10.10.0.0/22")
	gw := a("10.10.0.1")
	for name, tc := range map[string]struct {
		prefix netip.Prefix
		taken  []netip.Addr
		ranges [][2]netip.Addr
		want   string
	}{
		"last":                     {prefix: p("10.10.0.64/26"), want: "10.10.0.127"},
		"last is l2 broadcast":     {prefix: p("10.10.3.192/26"), want: "10.10.3.254"},
		"last taken":               {prefix: p("10.10.0.64/26"), taken: []netip.Addr{a("10.10.0.127")}, want: "10.10.0.126"},
		"last in a range":          {prefix: p("10.10.0.64/26"), ranges: [][2]netip.Addr{{a("10.10.0.120"), a("10.10.0.127")}}, want: "10.10.0.119"},
		"only gateway and network": {prefix: p("10.10.0.0/31"), want: ""}, // .1 is the gateway, .0 the L2 network
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			got, ok := pickVIP(tc.prefix, l2, gw, tc.taken, tc.ranges)
			if tc.want == "" {
				g.Expect(ok).To(BeFalse())
				return
			}
			g.Expect(ok).To(BeTrue())
			g.Expect(got.String()).To(Equal(tc.want))
		})
	}
}

func TestVIPUsable(t *testing.T) {
	p := netip.MustParsePrefix
	a := netip.MustParseAddr
	l2 := p("10.10.0.0/22")
	gw := a("10.10.0.1")
	ranges := [][2]netip.Addr{{a("10.10.0.20"), a("10.10.0.30")}}
	for name, tc := range map[string]struct {
		prefix string
		addr   string
		want   bool
	}{
		"free":                {"10.10.0.0/26", "10.10.0.40", true},
		"prefix boundary":     {"10.10.0.0/26", "10.10.0.63", true},
		"outside the prefix":  {"10.10.0.0/26", "10.10.0.64", false},
		"l2 network":          {"10.10.0.0/26", "10.10.0.0", false},
		"l2 broadcast":        {"10.10.3.192/26", "10.10.3.255", false},
		"gateway":             {"10.10.0.0/26", "10.10.0.1", false},
		"taken":               {"10.10.0.0/26", "10.10.0.41", false},
		"range start":         {"10.10.0.0/26", "10.10.0.20", false},
		"range end":           {"10.10.0.0/26", "10.10.0.30", false},
		"just outside ranges": {"10.10.0.0/26", "10.10.0.31", true},
	} {
		got := vipUsable(a(tc.addr), p(tc.prefix), l2, gw, []netip.Addr{a("10.10.0.41")}, ranges)
		if got != tc.want {
			t.Errorf("%s: vipUsable = %t, want %t", name, got, tc.want)
		}
	}
}

func TestReservedOf(t *testing.T) {
	g := NewWithT(t)
	gw := netip.MustParseAddr("10.10.0.1")
	nw := newNetwork("ns", "default")
	g.Expect(reservedOf(nw, gw)).To(Equal([]netip.Addr{gw}))

	nw.Status.ControlPlaneEndpoint = v1alpha1.APIEndpoint{Host: "10.10.0.14", Port: 6443}
	g.Expect(reservedOf(nw, gw)).To(Equal([]netip.Addr{gw, netip.MustParseAddr("10.10.0.14")}))
}

// A range that is not in NetBox yet waits for the VIP of its network; nothing is written meanwhile.
func TestPoolNewRangeWaitsForVIP(t *testing.T) {
	g := NewWithT(t)
	nw := vipNetwork("ns", "default")
	nw.Status.Prefix = "10.10.0.16/28"
	nw.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ReadyCondition, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonReady}}
	var writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()
	r := &NetBoxIPPoolReconciler{Client: claimClient(nw), NetBox: &netbox.Client{URL: srv.URL}}

	err := r.reconcileNormal(ctx, newPool("ns", "worker", v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 3}))
	var re reasonError
	g.Expect(errors.As(err, &re)).To(BeTrue(), "%v", err)
	g.Expect(re.reason).To(Equal(v1alpha1.ReasonNetworkNotReady))
	g.Expect(err.Error()).To(ContainSubstring("waiting for the VIP of network default"))
	g.Expect(writes.Load()).To(BeZero())
}

// reserveVIP with a preferred address checks only that address and never moves an existing VIP.
func TestReserveVIPPreferred(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	rec := events.NewFakeRecorder(10)
	r := &NetBoxNetworkReconciler{NetBox: admin, Owner: owner, Recorder: rec}
	nw := vipNetwork(ns, "default")
	prefix := netip.MustParsePrefix("10.10.3.80/28")
	byHand(t, g, netbox.IPAddresses, map[string]any{"address": "10.10.3.90/22", "status": "active"})

	for name, preferred := range map[string]string{"taken": "10.10.3.90", "outside": "10.10.3.200", "service address": "10.10.3.255"} {
		_, err := r.reserveVIP(ctx, nw, prefix, netip.MustParseAddr(preferred), true)
		g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonEndpointConflict), name)
		g.Expect(vipsOf(g, ns, "default")).To(BeEmpty(), name)
	}

	vip, err := r.reserveVIP(ctx, nw, prefix, netip.MustParseAddr("10.10.3.85"), true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(vip.String()).To(Equal("10.10.3.85"))
	g.Expect(vipsOf(g, ns, "default")).To(HaveLen(1))
	g.Expect(emitted(rec)).To(ConsistOf("Normal VIPReserved Reserved the control plane VIP 10.10.3.85/22"))

	vip, err = r.reserveVIP(ctx, nw, prefix, netip.MustParseAddr("10.10.3.86"), true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(vip.String()).To(Equal("10.10.3.85"))
	g.Expect(vipsOf(g, ns, "default")).To(HaveLen(1))
	g.Expect(emitted(rec)).To(BeEmpty(), "no change in NetBox, no event")
}

func TestPreferredVIP(t *testing.T) {
	g := NewWithT(t)
	prefix := netip.MustParsePrefix("10.10.0.64/26")
	nw := vipNetwork("ns", "default")
	g.Expect(preferredVIP(nw, "", prefix).IsValid()).To(BeFalse())
	g.Expect(preferredVIP(nw, "10.10.0.100", prefix)).To(Equal(netip.MustParseAddr("10.10.0.100")))
	g.Expect(preferredVIP(nw, "10.10.0.200", prefix).IsValid()).To(BeFalse(), "outside the prefix")
	g.Expect(preferredVIP(nw, "api.example", prefix).IsValid()).To(BeFalse(), "not an address")

	nw.Status.ControlPlaneEndpoint.Host = "10.10.0.70"
	g.Expect(preferredVIP(nw, "10.10.0.100", prefix)).To(Equal(netip.MustParseAddr("10.10.0.70")), "the status wins")
}

func TestSyncClusterEndpoint(t *testing.T) {
	vip := v1alpha1.APIEndpoint{Host: "10.10.0.126", Port: 6443}
	cluster := func(ep clusterv1.APIEndpoint) *clusterv1.Cluster {
		return &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ns"}, Spec: clusterv1.ClusterSpec{ControlPlaneEndpoint: ep}}
	}
	for name, tc := range map[string]struct {
		cluster *clusterv1.Cluster
		vipErr  error
		reason  string
		want    clusterv1.APIEndpoint
	}{
		"no cluster":    {reason: v1alpha1.ReasonWaitingForCluster},
		"vip failed":    {cluster: cluster(clusterv1.APIEndpoint{}), vipErr: retryLater(v1alpha1.ReasonExhausted, errors.New("none")), reason: v1alpha1.ReasonExhausted},
		"empty":         {cluster: cluster(clusterv1.APIEndpoint{}), want: clusterv1.APIEndpoint{Host: vip.Host, Port: vip.Port}},
		"port only":     {cluster: cluster(clusterv1.APIEndpoint{Port: 8443}), want: clusterv1.APIEndpoint{Host: vip.Host, Port: 8443}},
		"host only":     {cluster: cluster(clusterv1.APIEndpoint{Host: vip.Host}), want: clusterv1.APIEndpoint{Host: vip.Host, Port: 6443}},
		"already there": {cluster: cluster(clusterv1.APIEndpoint{Host: vip.Host, Port: vip.Port}), want: clusterv1.APIEndpoint{Host: vip.Host, Port: vip.Port}},
		"other host":    {cluster: cluster(clusterv1.APIEndpoint{Host: "10.10.0.5", Port: 6443}), reason: v1alpha1.ReasonEndpointConflict, want: clusterv1.APIEndpoint{Host: "10.10.0.5", Port: 6443}},
		"other port":    {cluster: cluster(clusterv1.APIEndpoint{Host: vip.Host, Port: 8443}), want: clusterv1.APIEndpoint{Host: vip.Host, Port: 8443}},
		"dns name":      {cluster: cluster(clusterv1.APIEndpoint{Host: "api.example", Port: 6443}), reason: v1alpha1.ReasonEndpointConflict, want: clusterv1.APIEndpoint{Host: "api.example", Port: 6443}},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			nw := vipNetwork("ns", "default")
			nw.Spec.ClusterName = "ns"
			nw.Status.ControlPlaneEndpoint = vip
			scheme := runtime.NewScheme()
			g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
			b := fake.NewClientBuilder().WithScheme(scheme)
			if tc.cluster != nil {
				b = b.WithObjects(tc.cluster)
			}
			c := b.Build()
			r := &NetBoxNetworkReconciler{Client: c}

			err := r.syncClusterEndpoint(ctx, nw, tc.cluster, tc.vipErr)
			if tc.reason == "" {
				g.Expect(err).NotTo(HaveOccurred())
			} else {
				g.Expect(errReason(err)).To(Equal(tc.reason))
			}
			if tc.cluster == nil {
				return
			}
			got := &clusterv1.Cluster{}
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(tc.cluster), got)).To(Succeed())
			g.Expect(got.Spec.ControlPlaneEndpoint).To(Equal(tc.want))
		})
	}
}

func TestEndpointFor(t *testing.T) {
	const vip = "10.10.0.126"
	ep := func(host string, port int32) clusterv1.APIEndpoint {
		return clusterv1.APIEndpoint{Host: host, Port: port}
	}
	for name, tc := range map[string]struct {
		cluster  clusterv1.APIEndpoint
		specPort int32
		patch    clusterv1.APIEndpoint // zero: the Cluster is left as it is
		port     int32                 // of the status
		conflict bool
	}{
		"empty":                    {patch: ep(vip, 6443), port: 6443},
		"empty, spec port":         {specPort: 443, patch: ep(vip, 443), port: 443},
		"port only":                {cluster: ep("", 443), patch: ep(vip, 443), port: 443},
		"port only, spec port":     {cluster: ep("", 443), specPort: 6443, port: 6443, conflict: true},
		"host only":                {cluster: ep(vip, 0), patch: ep(vip, 6443), port: 6443},
		"other host only":          {cluster: ep("10.10.0.5", 0), port: 6443, conflict: true},
		"the VIP":                  {cluster: ep(vip, 6443), port: 6443},
		"other host":               {cluster: ep("10.10.0.5", 6443), port: 6443, conflict: true},
		"dns name, port only kept": {cluster: ep("api.example", 443), port: 443, conflict: true},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			nw := vipNetwork("ns", "default")
			nw.Spec.ControlPlaneEndpoint.Port = tc.specPort
			cluster := &clusterv1.Cluster{Spec: clusterv1.ClusterSpec{ControlPlaneEndpoint: tc.cluster}}
			want, err := endpointFor(nw, cluster, netip.MustParseAddr(vip))
			g.Expect(want.Port).To(Equal(tc.port))
			if tc.conflict {
				g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonEndpointConflict))
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			if tc.patch.IsZero() {
				g.Expect(want).To(Equal(tc.cluster))
			} else {
				g.Expect(want).To(Equal(tc.patch))
			}
		})
	}
	g := NewWithT(t)
	want, err := endpointFor(vipNetwork("ns", "default"), nil, netip.MustParseAddr(vip))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(want).To(Equal(ep(vip, 6443)), "no Cluster yet")
}

// A Cluster with a host only gets the port; the host is never changed.
func TestEndpointPartialHostOnly(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	setClusterEndpoint(t, ns, "10.10.3.100", 0)
	nw := adoptedNetwork(ns, "10.10.3.96/28")
	nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Expect(nw.Status.ControlPlaneEndpoint).To(Equal(v1alpha1.APIEndpoint{Host: "10.10.3.100", Port: 6443}))
	g.Expect(clusterEndpointOf(g, ns)).To(Equal(clusterv1.APIEndpoint{Host: "10.10.3.100", Port: 6443}))
}

// A Cluster with a port only, as CAPI copies it from the infrastructure cluster, gets the VIP and keeps its port.
func TestEndpointPartialPortOnly(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	setClusterEndpoint(t, ns, "", 443)
	nw := vipNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Expect(nw.Status.ControlPlaneEndpoint.Port).To(Equal(int32(443)))
	g.Expect(clusterEndpointOf(g, ns)).To(Equal(clusterv1.APIEndpoint{Host: nw.Status.ControlPlaneEndpoint.Host, Port: 443}))
}

// A VIP whose address an older NetBox object took at the same time is deleted; the next pick avoids it.
func TestVIPDuplicate(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	var once sync.Once
	beforeVIPCreate = func() {
		once.Do(func() {
			// The highest usable address of 10.10.3.96/28: the one the VIP takes.
			_, err := admin.Create(ctx, netbox.IPAddresses, map[string]any{
				"address": "10.10.3.111/22", "status": "active", "role": "vip", "custom_fields": map[string]any{netbox.FieldCluster: ns},
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(func() { beforeVIPCreate = func() {} })

	nw := adoptedNetwork(ns, "10.10.3.96/28")
	nw.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{}
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionFalse, v1alpha1.ReasonEndpointConflict)
	g.Expect(meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.ControlPlaneEndpointReadyCondition).Message).To(ContainSubstring("10.10.3.111 is also held by NetBox address #"))
	g.Expect(vipsOf(g, ns, "default")).To(BeEmpty())
	g.Expect(nw.Status.ControlPlaneEndpoint).To(BeZero())

	touch(g, nw)
	waitTypedCondition(g, nw, v1alpha1.ControlPlaneEndpointReadyCondition, metav1.ConditionTrue, v1alpha1.ReasonReady)
	g.Expect(nw.Status.ControlPlaneEndpoint.Host).To(Equal("10.10.3.110"))
	vips := vipsOf(g, ns, "default")
	g.Expect(vips).To(HaveLen(1))
	g.Expect(vips[0].Address).To(Equal("10.10.3.110/22"))
}

// A Cluster changed under the endpoint patch is read again at once; the condition is left as it was.
// Metadata is synced as usual, and an unexpected error of it goes back to controller-runtime.
func TestClusterPatchConflictQuiet(t *testing.T) {
	for name, tags := range map[string][]string{"metadata synced": nil, "metadata fails": {"capi"}} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			scheme := runtime.NewScheme()
			g.Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())
			g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
			nw := vipNetwork("ns", "default")
			nw.Spec.ClusterName, nw.Spec.Tags = "ns", tags
			nw.Finalizers = []string{networkFinalizer}
			nw.Status.Conditions = []metav1.Condition{
				{Type: clusterv1.PausedCondition, Status: metav1.ConditionFalse, Reason: clusterv1.NotPausedReason},
				{Type: v1alpha1.ControlPlaneEndpointReadyCondition, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonWaitingForCluster, Message: "before"},
				{Type: v1alpha1.MetadataSyncedCondition, Status: metav1.ConditionUnknown, Reason: v1alpha1.ReasonWaitingForPrefix},
			}
			cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ns"}, Spec: clusterv1.ClusterSpec{Paused: ptr.To(false)}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nw, cluster).WithStatusSubresource(nw).
				WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
					if _, ok := obj.(*clusterv1.Cluster); ok {
						return apierrors.NewConflict(clusterv1.GroupVersion.WithResource("clusters").GroupResource(), obj.GetName(), errors.New("changed"))
					}
					return c.Patch(ctx, obj, p, opts...)
				}}).Build()
			var writes []string
			r := networkOn(networkNetBox(t, 1<<30, &writes))
			r.Client = c

			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(nw)})
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
			synced := meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.MetadataSyncedCondition)
			if tags == nil {
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(res).To(Equal(ctrl.Result{Requeue: true})) //nolint:staticcheck // the result under test
				g.Expect(synced.Status).To(Equal(metav1.ConditionTrue))
			} else {
				g.Expect(err).To(MatchError(ContainSubstring("HTTP 500")), "requeued with back-off and logged")
				g.Expect(res).To(BeZero())
				g.Expect(synced.Reason).To(Equal(v1alpha1.ReasonNetBoxError))
			}
			cond := meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.ControlPlaneEndpointReadyCondition)
			g.Expect(cond.Reason).To(Equal(v1alpha1.ReasonWaitingForCluster))
			g.Expect(cond.Message).To(Equal("before"))
			g.Expect(meta.IsStatusConditionTrue(nw.Status.Conditions, v1alpha1.ReadyCondition)).To(BeTrue())
		})
	}
}

// A VIP found by ref is checked for a duplicate while the status does not have it yet, as when the check after
// its create failed or after a move; once the status has it, it is not checked again. Only an older object
// makes it give way: a long-lived VIP is never deleted for a newer one.
func TestVIPDuplicateCheckedUntilInStatus(t *testing.T) {
	g := NewWithT(t)
	var lists, deletes, other atomic.Int32
	other.Store(9)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case q.Has("address"):
			lists.Add(1)
			_, _ = io.WriteString(w, `{"results":[`+networkVIP+`,{"id":`+strconv.Itoa(int(other.Load()))+`,"address":"10.10.0.31/22","role":{"value":"vip"}}]}`)
		case q.Has("owner_id"):
			_, _ = io.WriteString(w, `{"results":[`+networkVIP+`]}`)
		default:
			_, _ = io.WriteString(w, `{"results":[]}`)
		}
	}))
	defer srv.Close()
	r := networkOn(srv)
	prefix := netip.MustParsePrefix("10.10.0.16/28")

	nw := vipNetwork("ns", "default")
	nw.Status.ControlPlaneEndpoint = v1alpha1.APIEndpoint{Host: "10.10.0.31", Port: 6443}
	vip, err := r.reserveVIP(ctx, nw, prefix, netip.Addr{}, true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(vip.String()).To(Equal("10.10.0.31"))
	g.Expect(lists.Load()).To(BeZero(), "the status has the VIP: no check")

	nw.Status.ControlPlaneEndpoint = v1alpha1.APIEndpoint{}
	vip, err = r.reserveVIP(ctx, nw, prefix, netip.Addr{}, true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(vip.String()).To(Equal("10.10.0.31"))
	g.Expect(lists.Load()).To(Equal(int32(1)))
	g.Expect(deletes.Load()).To(BeZero(), "the other object is newer: ours is kept")

	other.Store(1)
	_, err = r.reserveVIP(ctx, nw, prefix, netip.Addr{}, true)
	g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonEndpointConflict))
	g.Expect(err.Error()).To(ContainSubstring("10.10.0.31 is also held by NetBox address #1"))
	g.Expect(lists.Load()).To(Equal(int32(2)))
	g.Expect(deletes.Load()).To(Equal(int32(1)), "the other object is older: ours is deleted")
}
