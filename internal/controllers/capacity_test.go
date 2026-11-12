/*
Copyright The cluster-api-ipam-provider-netbox Authors.

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
	"net/netip"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestFits(t *testing.T) {
	free := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.32/27"), // too small for a /26
		netip.MustParsePrefix("10.0.0.192/26"),
		netip.MustParsePrefix("10.0.1.0/24"),
		netip.MustParsePrefix("10.0.2.0/23"),
	}
	for _, c := range []struct {
		bits int
		want float64
	}{{26, 1 + 4 + 8}, {27, 1 + 2 + 8 + 16}, {24, 1 + 2}, {22, 0}} {
		if got := fits(free, c.bits); got != c.want {
			t.Errorf("fits /%d: %v, want %v", c.bits, got, c.want)
		}
	}
	if got := sum(free); got != 32+64+256+512 {
		t.Errorf("sum %v", got)
	}
	if got := size(netip.MustParsePrefix("10.0.0.0/22")); got != 1024 {
		t.Errorf("size %v", got)
	}
}

// The parent of the suite's networks gets its metrics, by the length the networks carve.
func TestParentCapacity(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)

	g.Expect((&parentCapacity{Reader: k8s, NetBox: admin.ReadOnly()}).collect(ctx)).To(Succeed())
	parent := nw.Spec.ParentPrefix
	g.Expect(testutil.ToFloat64(parentSize.WithLabelValues(parent))).To(Equal(1024.0))
	free := testutil.ToFloat64(parentFree.WithLabelValues(parent))
	g.Expect(free).To(BeNumerically("<=", 1024-16), "the /28 of the network is taken")
	g.Expect(testutil.ToFloat64(parentFreePrefixes.WithLabelValues(parent, "28"))).To(And(BeNumerically(">", 0), BeNumerically("<=", free/16)))
}
