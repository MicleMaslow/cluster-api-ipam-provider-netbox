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
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

// The Cluster stays unpaused, as when clusterctl move deletes it before our objects: nothing may reach NetBox.
func TestMoveKeepsNetBoxObjects(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := layoutNetwork(ns, "default")
	nw.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete
	readyLayout(g, nw)
	pool := newPool(ns, "worker", v1alpha1.IPPoolRange{NetworkName: "default", Pool: "cp"})
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	address(g, claim)
	ips, err := findRef(netbox.IPAddresses, refOf("IPAddressClaim", ns, "m1"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ips).To(HaveLen(1))

	objs := []client.Object{claim, pool, nw}
	for _, o := range objs {
		moveAnnotate(g, o)
	}
	for _, o := range objs {
		g.Expect(k8s.Delete(ctx, o)).To(Succeed())
	}
	for _, o := range objs {
		g.Eventually(gone(o)).Should(BeTrue())
	}

	for path, id := range map[string]int64{
		netbox.Prefixes:    nw.Status.PrefixID,
		netbox.IPRanges:    pool.Status.Ranges[0].ID,
		netbox.IPAddresses: ips[0].ID,
	} {
		obj, _, err := admin.Get(ctx, path, id)
		g.Expect(err).NotTo(HaveOccurred(), path)
		g.Expect(obj.Status.Value).To(Equal("active"), path)
	}
}
