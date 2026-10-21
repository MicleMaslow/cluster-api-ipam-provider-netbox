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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	inclusterv1 "sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

// syncNetwork is inClusterNetwork with the addresses of workers synced to NetBox.
func syncNetwork(ns, name string) *v1alpha1.NetBoxNetwork {
	nw := inClusterNetwork(ns, name)
	nw.Spec.Pools[1].Sync = v1alpha1.PoolSync{Enabled: ptr.To(true)}
	return nw
}

// syncedClaim is the claim name of the InClusterIPPool workers, owned by the machine owner.
func syncedClaim(ns, name, owner string) *ipamv1.IPAddressClaim {
	return &ipamv1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1", Kind: "VSphereVM", Name: owner, UID: "00000000-0000-0000-0000-0000000000f1",
		}}},
		Spec: ipamv1.IPAddressClaimSpec{ClusterName: ns, PoolRef: ipamv1.IPPoolReference{APIGroup: inclusterv1.GroupVersion.Group, Kind: "InClusterIPPool", Name: "workers"}},
	}
}

// allocate creates the claim name of owner and its IPAddress at address in workers, as in-cluster does.
func allocate(g *WithT, ns, name, owner, address string) *ipamv1.IPAddress {
	g.Expect(k8s.Create(ctx, syncedClaim(ns, name, owner))).To(Succeed())
	a := inClusterAddress(ns, name, "workers", address)
	g.Expect(k8s.Create(ctx, a)).To(Succeed())
	return a
}

// syncedAt are the global-VRF NetBox addresses at a.
func syncedAt(g Gomega, a string) []netbox.Object {
	found, err := admin.List(ctx, netbox.IPAddresses, url.Values{"address": {a}, "vrf_id": {"null"}})
	g.Expect(err).NotTo(HaveOccurred())
	return found
}

// syncedObject waits for the one NetBox address under the sync key of IPAddress name of workers in nw.
func syncedObject(g *WithT, nw *v1alpha1.NetBoxNetwork, name string) netbox.Object {
	var ip netbox.Object
	g.Eventually(func(g Gomega) {
		found, err := findRef(netbox.IPAddresses, syncRef(keys, nw, "workers", name))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(HaveLen(1))
		ip = found[0]
	}).Should(Succeed())
	return ip
}

// noSynced waits until no NetBox address is under a sync key of nw.
func noSynced(g *WithT, nw *v1alpha1.NetBoxNetwork) {
	g.Eventually(func(g Gomega) {
		found, err := admin.FindByRefPrefix(ctx, keys, netbox.IPAddresses, syncRef(keys, nw, "", ""))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeEmpty())
	}).Should(Succeed())
}

func TestSyncRef(t *testing.T) {
	g := NewWithT(t)
	nw := newNetwork("ns", "default")
	g.Expect(syncRef(netbox.DefaultKeys, nw, "workers", "m1")).To(Equal("NetBoxNetwork/ns/default/sync/workers/m1"))
	g.Expect(syncRef(netbox.DefaultKeys, nw, "workers", "")).To(Equal("NetBoxNetwork/ns/default/sync/workers/"))
	g.Expect(syncRef(netbox.DefaultKeys, nw, "", "")).To(Equal("NetBoxNetwork/ns/default/sync/"))
	g.Expect(syncRef(netbox.Keys{Instance: "ci", Ref: "capi_ref"}, nw, "workers", "m1")).To(Equal("ci/NetBoxNetwork/ns/default/sync/workers/m1"))
}

// syncNetBox serves found as the NetBox addresses under the sync keys, at as those at any address, and records the
// writes; a create answers failCreate if it is not 0.
func syncNetBox(t *testing.T, found, at string, failCreate int, writes *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(writesNeedLock(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method != http.MethodGet:
			b, _ := io.ReadAll(r.Body)
			*writes = append(*writes, r.Method+" "+r.URL.Path+" "+string(b))
			if r.Method == http.MethodPost && failCreate != 0 {
				w.WriteHeader(failCreate)
				_, _ = io.WriteString(w, `{"address":["Cannot create IP address 10.10.0.20/22 inside range 10.10.0.19-31/22."]}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":9}`)
		case r.URL.Path == "/api/ipam/ip-addresses/" && r.URL.Query().Has("cf_capi_ref__isw"):
			_, _ = io.WriteString(w, `{"results":[`+found+`]}`)
		case r.URL.Path == "/api/ipam/ip-addresses/" && r.URL.Query().Has("address"):
			_, _ = io.WriteString(w, `{"results":[`+at+`]}`)
		default:
			_, _ = io.WriteString(w, `{"results":[]}`)
		}
	})))
	t.Cleanup(srv.Close)
	return srv
}

// syncedOn is network default synced on workers, laid out in 10.10.0.16/28 with workers on range 7, and a reconciler
// on srv that sees IPAddress m1 of workers at 10.10.0.20, claimed for vm-1.
func syncedOn(srv *httptest.Server) (*NetBoxNetworkReconciler, *v1alpha1.NetBoxNetwork) {
	nw := syncNetwork("ns", "default")
	nw.Status.Prefix = "10.10.0.16/28"
	nw.Status.Layout.Pools = []v1alpha1.PoolLayout{
		{Name: "cp", Size: intstr.FromInt32(3), Ranges: []v1alpha1.LayoutRange{{Start: "10.10.0.16", End: "10.10.0.18", ID: 6}}},
		{Name: "workers", Size: intstr.FromString("rest"), Ranges: []v1alpha1.LayoutRange{{Start: "10.10.0.19", End: "10.10.0.31", ID: 7}}},
	}
	r := networkOn(srv)
	r.Client = inClusterClient(interceptor.Funcs{}, ownedPool(nw, "workers", "10.10.0.19-10.10.0.31"), syncedClaim("ns", "m1", "vm-1"), inClusterAddress("ns", "m1", "workers", "10.10.0.20"))
	r.APIReader = r.Client
	return r, nw
}

const (
	openRange  = `PATCH /api/ipam/ip-ranges/7/ {"changelog_message":"capi: opened to sync addresses by NetBoxNetwork/ns/default","mark_populated":false}`
	closeRange = `PATCH /api/ipam/ip-ranges/7/ {"changelog_message":"capi: closed to allocation by NetBoxNetwork/ns/default","mark_populated":true}`
)

// syncedIP is the synced NetBox address of m1 at a with dns_name dns.
func syncedIP(a, dns string) string {
	return `{"id":30,"address":"` + a + `/22","status":{"value":"active"},"dns_name":"` + dns + `",` + textsOf("ns/vm-1 // ns workers") +
		`,"custom_fields":{"capi_ref":"NetBoxNetwork/ns/default/sync/workers/m1"}}`
}

// NetBox refuses to create an address inside a populated range, not to change or delete one: the range of the address
// is opened for the create alone, and closed again whatever the create answers.
func TestSyncOpensRangeOnlyToCreate(t *testing.T) {
	g := NewWithT(t)
	create := `POST /api/ipam/ip-addresses/ {"address":"10.10.0.20/22","changelog_message":"capi: address for NetBoxNetwork/ns/default/sync/workers/m1",` +
		`"comments":"` + managedComments + `","custom_fields":{"capi_ref":"NetBoxNetwork/ns/default/sync/workers/m1"},"description":"ns/vm-1 // ns workers",` +
		`"dns_name":"vm-1","status":"active"}`
	for name, tc := range map[string]struct {
		found      string
		failCreate int
		writes     []string
		fails      bool
	}{
		"created":       {writes: []string{openRange, create, closeRange}},
		"create fails":  {failCreate: http.StatusBadRequest, writes: []string{openRange, create, closeRange}, fails: true},
		"in sync":       {found: syncedIP("10.10.0.20", "vm-1")},
		"dns_name":      {found: syncedIP("10.10.0.20", "old"), writes: []string{`PATCH /api/ipam/ip-addresses/30/ {"changelog_message":"capi: dns_name of NetBoxNetwork/ns/default/sync/workers/m1","dns_name":"vm-1"}`}},
		"moved by hand": {found: syncedIP("10.10.0.21", "vm-1"), writes: []string{`PATCH /api/ipam/ip-addresses/30/ {"address":"10.10.0.20/22","changelog_message":"capi: reclaimed by NetBoxNetwork/ns/default/sync/workers/m1","status":"active"}`}},
	} {
		var writes []string
		r, nw := syncedOn(syncNetBox(t, tc.found, "", tc.failCreate, &writes))
		err := r.syncAddresses(ctx, nw)
		g.Expect(err != nil).To(Equal(tc.fails), "%s: %v", name, err)
		g.Expect(writes).To(Equal(tc.writes), name)
	}
}

// The range is closed again when the reconcile's ctx ends during the create (the section's deadline, a shutdown), and
// when the create panics.
func TestSyncClosesRangeOnCancelAndPanic(t *testing.T) {
	g := NewWithT(t)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var writes []string
	srv := httptest.NewServer(writesNeedLock(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"results":[]}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		writes = append(writes, r.Method+" "+r.URL.Path+" "+string(b))
		if r.Method == http.MethodPost {
			cancel()
			select { // the client gives up on its ctx
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		_, _ = io.WriteString(w, `{"id":9}`)
	})))
	t.Cleanup(srv.Close)
	r, nw := syncedOn(srv)

	err := r.syncAddresses(cctx, nw)
	g.Expect(err).To(MatchError(context.Canceled))
	g.Expect(writes).To(HaveLen(3))
	g.Expect(writes[0]).To(Equal(openRange))
	g.Expect(writes[1]).To(HavePrefix("POST /api/ipam/ip-addresses/ "))
	g.Expect(writes[2]).To(Equal(closeRange))

	writes = nil
	g.Expect(func() {
		netBoxWrites.Lock()
		defer netBoxWrites.Unlock()
		_ = r.unfenced(ctx, nw, 7, func() error { panic("create") })
	}).To(PanicWith("create"))
	g.Expect(writes).To(Equal([]string{openRange, closeRange}))
}

// An address under a sync key that no IPAddress has any more, or of a pool that no longer syncs, is deleted.
func TestSyncDeletesLeftovers(t *testing.T) {
	g := NewWithT(t)
	gone := `{"id":31,"address":"10.10.0.22/22","status":{"value":"active"},"custom_fields":{"capi_ref":"NetBoxNetwork/ns/default/sync/workers/m2"}}`
	var writes []string
	r, nw := syncedOn(syncNetBox(t, syncedIP("10.10.0.20", "vm-1")+","+gone, "", 0, &writes))
	g.Expect(r.syncAddresses(ctx, nw)).To(Succeed())
	g.Expect(writes).To(Equal([]string{`DELETE /api/ipam/ip-addresses/31/ {"changelog_message":"capi: released by NetBoxNetwork/ns/default"}`}))

	writes = nil
	nw.Spec.Pools[1].Sync.Enabled = ptr.To(false)
	g.Expect(r.syncAddresses(ctx, nw)).To(Succeed())
	g.Expect(writes).To(ConsistOf(
		`DELETE /api/ipam/ip-addresses/30/ {"changelog_message":"capi: released by NetBoxNetwork/ns/default"}`,
		`DELETE /api/ipam/ip-addresses/31/ {"changelog_message":"capi: released by NetBoxNetwork/ns/default"}`,
	))
}

// A pool whose InClusterIPPool another network controls (Duplicate) is not synced: its IPAddresses are not this
// network's.
func TestSyncSkipsPoolOfAnotherNetwork(t *testing.T) {
	g := NewWithT(t)
	var writes []string
	r, nw := syncedOn(syncNetBox(t, "", "", 0, &writes))
	r.Client = inClusterClient(interceptor.Funcs{}, ownedPool(inClusterNetwork("ns", "other"), "workers", "10.10.0.19-10.10.0.31"),
		syncedClaim("ns", "m1", "vm-1"), inClusterAddress("ns", "m1", "workers", "10.10.0.20"))
	r.APIReader = r.Client
	g.Expect(r.syncAddresses(ctx, nw)).To(Succeed())
	g.Expect(writes).To(BeEmpty())
}

// A NetBox address of the sync at the address to create, under another key, is deleted right before the create: one
// address is held by one object (the cache can still show the IPAddress that had it).
func TestSyncReusedAddressDeletedBeforeCreate(t *testing.T) {
	g := NewWithT(t)
	old := `{"id":29,"address":"10.10.0.20/22","status":{"value":"active"},"dns_name":"vm-0",` + textsOf("ns/vm-0 // ns workers") +
		`,"custom_fields":{"capi_ref":"NetBoxNetwork/ns/default/sync/workers/m0"}}`
	var writes []string
	r, nw := syncedOn(syncNetBox(t, old, old, 0, &writes))
	r.Client = inClusterClient(interceptor.Funcs{}, ownedPool(nw, "workers", "10.10.0.19-10.10.0.31"), syncedClaim("ns", "m0", "vm-0"), inClusterAddress("ns", "m0", "workers", "10.10.0.20"),
		syncedClaim("ns", "m1", "vm-1"), inClusterAddress("ns", "m1", "workers", "10.10.0.20"))
	r.APIReader = r.Client
	g.Expect(r.syncAddresses(ctx, nw)).To(Succeed())
	g.Expect(writes).To(HaveLen(4))
	g.Expect(writes[0]).To(Equal(openRange))
	g.Expect(writes[1]).To(Equal(`DELETE /api/ipam/ip-addresses/29/ {"changelog_message":"capi: released by NetBoxNetwork/ns/default"}`))
	g.Expect(writes[2]).To(And(HavePrefix("POST /api/ipam/ip-addresses/ "), ContainSubstring(`"capi_ref":"NetBoxNetwork/ns/default/sync/workers/m1"`)))
	g.Expect(writes[3]).To(Equal(closeRange))
}

// Every IPAddress of a synced pool is a NetBox address under its sync key: the mask of the L2 network, dns_name and
// texts from the templates for the owner of its claim, owner and tenant of the network. It goes with the IPAddress,
// and the range stays closed to allocation.
func TestSyncAddresses(t *testing.T) {
	g := newG(t)
	z := newZone(t, g)
	ns := newEnv(t, nil)
	nw := syncNetwork(ns, "default")
	nw.Spec.Owner, nw.Spec.Tenant = &v1alpha1.OwnerRef{ID: z.owner}, &v1alpha1.TenantRef{ID: z.tenant}
	p := readyLayout(g, nw)
	a := allocate(g, ns, "m1", "vm-1", addr(p, 5))

	ip := syncedObject(g, nw, "m1")
	g.Expect(ip.Address).To(Equal(addr(p, 5) + "/22"))
	g.Expect(ip.Status.Value).To(Equal("active"))
	g.Expect(ip.DNSName).To(Equal("vm-1"))
	g.Expect(texts{Description: ip.Description, Comments: ip.Comments}).To(Equal(texts{ns + "/vm-1 // " + ns + " workers", managedComments}))
	g.Expect(ip.Owner).To(Equal(&netbox.Ref{ID: z.owner}))
	g.Expect(ip.Tenant).To(Equal(&netbox.Ref{ID: z.tenant}))
	for _, lr := range rangesIn(g, nw, "workers") {
		rng, _, err := admin.Get(ctx, netbox.IPRanges, lr.ID)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(rng.MarkPopulated).To(BeTrue())
	}
	waitEvent(g, ns, "default", "AddressSynced")

	g.Expect(k8s.Delete(ctx, a)).To(Succeed())
	noSynced(g, nw)
}

// An address of a deleted IPAddress that a new one takes at once ends up with one NetBox address, under the new key.
func TestSyncReuseKeepsOneObject(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := syncNetwork(ns, "default")
	p := readyLayout(g, nw)
	a := allocate(g, ns, "m1", "vm-1", addr(p, 5))
	syncedObject(g, nw, "m1")

	g.Expect(k8s.Delete(ctx, a)).To(Succeed())
	allocate(g, ns, "m2", "vm-2", addr(p, 5))
	g.Eventually(func(g Gomega) {
		at := syncedAt(g, addr(p, 5))
		g.Expect(at).To(HaveLen(1))
		g.Expect(keys.RefOf(at[0])).To(Equal(syncRef(keys, nw, "workers", "m2")))
		g.Expect(at[0].DNSName).To(Equal("vm-2"))
	}).Should(Succeed())
}

// A pool whose sync is turned off loses its NetBox addresses; its IPAddresses stay.
func TestSyncDisabledDeletes(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := syncNetwork(ns, "default")
	p := readyLayout(g, nw)
	a := allocate(g, ns, "m1", "vm-1", addr(p, 5))
	syncedObject(g, nw, "m1")

	updateNetwork(g, nw, func(s *v1alpha1.NetBoxNetworkSpec) { s.Pools[1].Sync.Enabled = ptr.To(false) })
	noSynced(g, nw)
	g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(a), a)).To(Succeed())
}

// A NetBox address of someone else at the address of an IPAddress is AddressConflict and is left as it is.
func TestSyncForeignObjectConflict(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := syncNetwork(ns, "default")
	p := readyLayout(g, nw)
	// NetBox refuses to create an address inside a populated range, not to move one there.
	foreign := byHand(t, g, netbox.IPAddresses, map[string]any{"address": "198.51.100.11/24", "status": "active"})
	_, err := admin.Patch(ctx, netbox.IPAddresses, foreign.ID, "", map[string]any{"address": addr(p, 5) + "/22"})
	g.Expect(err).NotTo(HaveOccurred())
	foreign, _, err = admin.Get(ctx, netbox.IPAddresses, foreign.ID)
	g.Expect(err).NotTo(HaveOccurred())
	allocate(g, ns, "m1", "vm-1", addr(p, 5))

	g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(nw), nw)).To(Succeed())
		c := meta.FindStatusCondition(nw.Status.Conditions, v1alpha1.LayoutReadyCondition)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Reason).To(Equal(v1alpha1.ReasonAddressConflict), c.Message)
		g.Expect(c.Message).To(ContainSubstring("IPAddress m1 (" + addr(p, 5) + ")"))
	}).Should(Succeed())
	g.Consistently(func(g Gomega) {
		at := syncedAt(g, addr(p, 5))
		g.Expect(at).To(HaveLen(1))
		g.Expect(at[0].ID).To(Equal(foreign.ID))
		g.Expect(at[0].LastUpdated).To(Equal(foreign.LastUpdated))
	}).Should(Succeed())
}

// The NetBox addresses of the sync are the network's own: the InClusterIPPool does not exclude them.
func TestSyncedObjectsNotExcluded(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw := syncNetwork(ns, "default")
	p := readyLayout(g, nw)
	allocate(g, ns, "m1", "vm-1", addr(p, 5))
	syncedObject(g, nw, "m1")

	touch(g, nw)
	waitLayoutReady(g, nw)
	g.Consistently(func(g Gomega) {
		g.Expect(inClusterPool(g, ns, "workers").Spec).To(Equal(inClusterSpecOf(span(p, 3, 15))))
	}).Should(Succeed())
}
