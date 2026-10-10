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
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
	r := &NetBoxNetworkReconciler{NetBox: admin, Owner: owner}
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

	vip, err = r.reserveVIP(ctx, nw, prefix, netip.MustParseAddr("10.10.3.86"), true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(vip.String()).To(Equal("10.10.3.85"))
	g.Expect(vipsOf(g, ns, "default")).To(HaveLen(1))
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
	paused := cluster(clusterv1.APIEndpoint{})
	paused.Spec.Paused = ptr.To(true)
	for name, tc := range map[string]struct {
		cluster *clusterv1.Cluster
		vipErr  error
		reason  string
		want    clusterv1.APIEndpoint
	}{
		"no cluster":    {reason: v1alpha1.ReasonWaitingForCluster},
		"vip failed":    {cluster: cluster(clusterv1.APIEndpoint{}), vipErr: retryLater(v1alpha1.ReasonExhausted, errors.New("none")), reason: v1alpha1.ReasonExhausted},
		"empty":         {cluster: cluster(clusterv1.APIEndpoint{}), want: clusterv1.APIEndpoint{Host: vip.Host, Port: vip.Port}},
		"port only":     {cluster: cluster(clusterv1.APIEndpoint{Port: 8443}), want: clusterv1.APIEndpoint{Host: vip.Host, Port: vip.Port}},
		"paused":        {cluster: paused, reason: v1alpha1.ReasonWaitingForCluster},
		"already there": {cluster: cluster(clusterv1.APIEndpoint{Host: vip.Host, Port: vip.Port}), want: clusterv1.APIEndpoint{Host: vip.Host, Port: vip.Port}},
		"other host":    {cluster: cluster(clusterv1.APIEndpoint{Host: "10.10.0.5", Port: 6443}), reason: v1alpha1.ReasonEndpointConflict, want: clusterv1.APIEndpoint{Host: "10.10.0.5", Port: 6443}},
		"other port":    {cluster: cluster(clusterv1.APIEndpoint{Host: vip.Host, Port: 8443}), reason: v1alpha1.ReasonEndpointConflict, want: clusterv1.APIEndpoint{Host: vip.Host, Port: 8443}},
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
