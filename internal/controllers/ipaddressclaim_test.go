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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

func testHandler(url string, rangeIDs ...int64) *claimHandler {
	pool := &v1alpha1.NetBoxIPPool{Status: v1alpha1.NetBoxIPPoolStatus{
		Conditions:          []metav1.Condition{{Type: v1alpha1.ReadyCondition, Status: metav1.ConditionTrue}},
		Gateway:             "10.10.0.1",
		AddressPrefixLength: 22,
	}}
	for _, id := range rangeIDs {
		pool.Status.Ranges = append(pool.Status.Ranges, v1alpha1.IPPoolRangeStatus{ID: id, Ready: true})
	}
	pool.Namespace, pool.Name = "ns", "worker"
	return &claimHandler{
		ProviderAdapter: &ProviderAdapter{Client: claimClient(), NetBox: &netbox.Client{URL: url, Token: "t"}, Owner: netbox.Owner{ID: 1}},
		claim:           &ipamv1.IPAddressClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "m1"}, Spec: ipamv1.IPAddressClaimSpec{ClusterName: "c1"}},
		pool:            pool,
	}
}

// claimClient is a client over objs that indexes addresses by pool.
func claimClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	utilruntime.Must(ipamv1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithIndex(&ipamv1.IPAddress{}, poolRefField, func(o client.Object) []string {
			a, ok := asType[*ipamv1.IPAddress](ctx, o)
			if !ok {
				return nil
			}
			return poolRefName(a.Spec.PoolRef)
		}).Build()
}

// withHolder gives h a client in which claim "other" (cluster c2) holds the pool's address 10.10.1.20.
func withHolder(h *claimHandler, otherDeleting bool) {
	other := &ipamv1.IPAddressClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "other"}, Spec: ipamv1.IPAddressClaimSpec{ClusterName: "c2"}}
	if otherDeleting {
		other.DeletionTimestamp, other.Finalizers = &metav1.Time{Time: time.Now()}, []string{"x"}
	}
	held := &ipamv1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "other"},
		Spec: ipamv1.IPAddressSpec{
			Address: "10.10.1.20", ClaimRef: ipamv1.IPAddressClaimReference{Name: "other"},
			PoolRef: ipamv1.IPPoolReference{APIGroup: v1alpha1.GroupVersion.Group, Kind: v1alpha1.NetBoxIPPoolKind, Name: "worker"},
		},
	}
	h.Client = claimClient(other, held)
}

// readyOf is "status/reason" of the claim's Ready condition, empty if it is not set.
func readyOf(c *ipamv1.IPAddressClaim) string {
	r := meta.FindStatusCondition(c.Status.Conditions, ipamv1.IPAddressClaimReadyCondition)
	if r == nil {
		return ""
	}
	return string(r.Status) + "/" + r.Reason
}

func withReady(c *ipamv1.IPAddressClaim, status metav1.ConditionStatus, reason string) {
	c.Status.Conditions = []metav1.Condition{{Type: ipamv1.IPAddressClaimReadyCondition, Status: status, Reason: reason}}
}

func poolScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

const (
	readyTrue       = "True/" + clusterv1.ReadyReason
	allocating      = "Unknown/" + reasonAllocating
	poolNotReady    = "False/" + ipamv1.IPAddressClaimReadyPoolNotReadyReason
	exhausted       = "False/" + ipamv1.IPAddressClaimReadyPoolExhaustedReason
	failed          = "False/" + ipamv1.IPAddressClaimReadyAllocationFailedReason
	addressConflict = "False/" + reasonAddressConflict
)

func TestEnsureAddressTriesReadyRangesInOrder(t *testing.T) {
	var posts []string
	var created string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"results":[]}`)
		case r.URL.Path == "/api/ipam/ip-ranges/1/available-ips/":
			posts = append(posts, r.URL.Path)
			w.WriteHeader(http.StatusConflict)
		default:
			posts = append(posts, r.URL.Path)
			b, _ := io.ReadAll(r.Body)
			created = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":5,"address":"10.10.1.20/22"}`)
		}
	}))
	defer srv.Close()

	a := &ipamv1.IPAddress{}
	h := testHandler(srv.URL, 1, 3, 2)
	h.pool.Status.Ranges[1].Ready = false
	withReady(h.claim, metav1.ConditionFalse, ipamv1.IPAddressClaimReadyPoolExhaustedReason)
	if _, err := h.EnsureAddress(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if got := readyOf(h.claim); got != allocating {
		t.Errorf("ready %q, want %q until ipamutil records the address", got, allocating)
	}
	if a.Spec.Address != "10.10.1.20" || *a.Spec.Prefix != 22 || a.Spec.Gateway != "10.10.0.1" {
		t.Errorf("address %+v", a.Spec)
	}
	if want := []string{"/api/ipam/ip-ranges/1/available-ips/", "/api/ipam/ip-ranges/2/available-ips/"}; !slices.Equal(posts, want) {
		t.Errorf("posts %v, want %v", posts, want)
	}
	for _, want := range []string{`"dns_name":"m1"`, `"description":"c1 worker m1"`} {
		if !strings.Contains(created, want) {
			t.Errorf("post %s lacks %s", created, want)
		}
	}
}

func TestEnsureAddressFailsOnUnrenderableFields(t *testing.T) {
	var posts int
	var patches []string
	srv := allocNetBox(t, &posts, &patches)
	a := &ipamv1.IPAddress{}
	h := testHandler(srv.URL, 1)
	h.pool.Spec.IPAddress.DNSName = "{{ .Cluster }}.example." // valid for the pool, not for a claim without a cluster
	h.claim.Spec.ClusterName = ""

	if _, err := h.EnsureAddress(context.Background(), a); err == nil || a.Spec.Address != "" {
		t.Errorf("err %v, address %q, want an error and no address", err, a.Spec.Address)
	}
	if got := readyOf(h.claim); got != failed {
		t.Errorf("ready %q, want %q", got, failed)
	}
	if posts != 0 {
		t.Errorf("%d allocations, want none", posts)
	}
}

func TestEnsureAddressLeavesAddressEmptyOnFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		ready   string
	}{
		"netbox down": {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }, failed},
		"pool exhausted": {func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"results":[]}`)
				return
			}
			w.WriteHeader(http.StatusConflict)
		}, exhausted},
	} {
		srv := httptest.NewServer(tc.handler)
		a := &ipamv1.IPAddress{}
		h := testHandler(srv.URL, 1)
		_, err := h.EnsureAddress(context.Background(), a)
		srv.Close()
		if err == nil || a.Spec.Address != "" {
			t.Errorf("%s: err %v, address %q", name, err, a.Spec.Address)
		}
		if got := readyOf(h.claim); got != tc.ready {
			t.Errorf("%s: ready %q, want %q", name, got, tc.ready)
		}
	}
}

// recheckNetBox answers the recheck of an allocated address: byRef is what the claim's ref finds, taken what the address finds.
// It records the POSTs and the PATCHes it gets.
func recheckNetBox(t *testing.T, byRef, taken string, posts, patches *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			*posts = append(*posts, r.URL.Path+" "+string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":9,"address":"10.10.1.20/22"}`)
		case r.Method == http.MethodPatch:
			b, _ := io.ReadAll(r.Body)
			*patches = append(*patches, r.URL.Path+" "+string(b))
			_, _ = io.WriteString(w, `{}`)
		case r.URL.Query().Has("cf_" + netbox.FieldRef):
			_, _ = io.WriteString(w, byRef)
		case r.URL.Query().Get("address") == "10.10.1.20" && r.URL.Query().Get("vrf_id") == "null":
			_, _ = io.WriteString(w, taken)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEnsureAddressRechecksAllocatedAddress(t *testing.T) {
	ours := `{"results":[{"id":5,"address":"10.10.1.20/22","dns_name":"m1","description":"c1 worker m1","custom_fields":{"capi_ref":"IPAddressClaim/ns/m1"}}]}`
	moved := `{"results":[{"id":5,"address":"10.10.1.21/22","dns_name":"m1","description":"c1 worker m1","custom_fields":{"capi_ref":"IPAddressClaim/ns/m1"}}]}`
	foreign := `{"results":[{"id":6,"address":"10.10.1.20/22"}]}`
	none := `{"results":[]}`
	for name, tc := range map[string]struct {
		addressRef, byRef, taken string
		ready                    string
		conflict                 bool
		posts                    int
	}{
		"in NetBox, recorded":     {"m1", ours, none, readyTrue, false, 0},
		"in NetBox, not recorded": {"", ours, none, allocating, false, 0},
		"deleted, recreated":      {"m1", none, none, readyTrue, false, 1},
		"deleted, taken":          {"m1", none, foreign, addressConflict, true, 0},
		"moved":                   {"m1", moved, none, addressConflict, true, 0},
	} {
		var posts, patches []string
		srv := recheckNetBox(t, tc.byRef, tc.taken, &posts, &patches)
		a := &ipamv1.IPAddress{Spec: ipamv1.IPAddressSpec{Address: "10.10.1.20", Prefix: ptr.To(int32(22))}}
		h := testHandler(srv.URL, 1)
		h.claim.Status.AddressRef.Name = tc.addressRef
		withReady(h.claim, metav1.ConditionFalse, ipamv1.IPAddressClaimReadyAllocationFailedReason)

		_, err := h.EnsureAddress(context.Background(), a)
		if errors.Is(err, errAddressConflict) != tc.conflict || (err != nil) != tc.conflict {
			t.Errorf("%s: err %v, want conflict %t", name, err, tc.conflict)
		}
		if got := readyOf(h.claim); got != tc.ready {
			t.Errorf("%s: ready %q, want %q", name, got, tc.ready)
		}
		if a.Spec.Address != "10.10.1.20" || len(posts) != tc.posts || len(patches) != 0 {
			t.Errorf("%s: address %q, posts %v, patches %v, want %d posts and no patches", name, a.Spec.Address, posts, patches, tc.posts)
		}
		for _, p := range posts {
			for _, want := range []string{"/api/ipam/ip-addresses/ ", `"address":"10.10.1.20/22"`, `"capi_ref":"IPAddressClaim/ns/m1"`, `"owner":1`, `"capi_cluster":"c1"`, `"dns_name":"m1"`, `"description":"c1 worker m1"`} {
				if !strings.Contains(p, want) {
					t.Errorf("%s: post %s lacks %s", name, p, want)
				}
			}
		}
	}
}

func TestEnsureAddressSyncsAddressFields(t *testing.T) {
	at := func(dns, desc string) string {
		return fmt.Sprintf(`{"results":[{"id":5,"address":"10.10.1.20/22","dns_name":%q,"description":%q,"custom_fields":{"capi_ref":"IPAddressClaim/ns/m1"}}]}`, dns, desc)
	}
	none := `{"results":[]}`
	for name, tc := range map[string]struct {
		byRef   string
		patches int
	}{
		"in sync":         {at("m1", "c1 worker m1"), 0},
		"old description": {at("m1", "IPAddressClaim/ns/m1"), 1},
		"no dns name":     {at("", "c1 worker m1"), 1},
		"edited by hand":  {at("host", "mine"), 1},
	} {
		var posts, patches []string
		srv := recheckNetBox(t, tc.byRef, none, &posts, &patches)
		a := &ipamv1.IPAddress{Spec: ipamv1.IPAddressSpec{Address: "10.10.1.20", Prefix: ptr.To(int32(22))}}
		h := testHandler(srv.URL, 1)
		h.claim.Status.AddressRef.Name = "m1"

		if _, err := h.EnsureAddress(context.Background(), a); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if got := readyOf(h.claim); got != readyTrue {
			t.Errorf("%s: ready %q, want %q", name, got, readyTrue)
		}
		if len(patches) != tc.patches || len(posts) != 0 {
			t.Errorf("%s: patches %v, posts %v, want %d patches", name, patches, posts, tc.patches)
			continue
		}
		for _, p := range patches {
			for _, want := range []string{"/api/ipam/ip-addresses/5/ ", `"dns_name":"m1"`, `"description":"c1 worker m1"`, `"changelog_message":"capi: address fields for IPAddressClaim/ns/m1"`} {
				if !strings.Contains(p, want) {
					t.Errorf("%s: patch %s lacks %s", name, p, want)
				}
			}
		}
	}
}

func TestEnsureAddressKeepsReadyOnUnrenderableFields(t *testing.T) {
	var posts, patches []string
	srv := recheckNetBox(t, `{"results":[{"id":5,"address":"10.10.1.20/22","dns_name":"m1","description":"c1 worker m1","custom_fields":{"capi_ref":"IPAddressClaim/ns/m1"}}]}`, `{"results":[]}`, &posts, &patches)
	a := &ipamv1.IPAddress{Spec: ipamv1.IPAddressSpec{Address: "10.10.1.20", Prefix: ptr.To(int32(22))}}
	h := testHandler(srv.URL, 1)
	h.claim.Status.AddressRef.Name = "m1"
	h.claim.Spec.ClusterName = ""
	h.pool.Spec.IPAddress.DNSName = "{{ .Cluster }}.example."
	withReady(h.claim, metav1.ConditionTrue, clusterv1.ReadyReason)

	if _, err := h.EnsureAddress(context.Background(), a); err == nil {
		t.Error("err nil, want a render error")
	}
	if got := readyOf(h.claim); got != readyTrue {
		t.Errorf("ready %q, want %q", got, readyTrue)
	}
	if len(patches) != 0 {
		t.Errorf("patches %v, want none", patches)
	}
}

func TestEnsureAddressRestoresWithoutUnrenderableFields(t *testing.T) {
	var posts, patches []string
	srv := recheckNetBox(t, `{"results":[]}`, `{"results":[]}`, &posts, &patches)
	a := &ipamv1.IPAddress{Spec: ipamv1.IPAddressSpec{Address: "10.10.1.20", Prefix: ptr.To(int32(22))}}
	h := testHandler(srv.URL, 1)
	h.claim.Status.AddressRef.Name = "m1"
	h.claim.Spec.ClusterName = ""
	h.pool.Spec.IPAddress.DNSName = `{{ if eq .Cluster "" }}a b{{ end }}{{ .Owner }}`
	withReady(h.claim, metav1.ConditionTrue, clusterv1.ReadyReason)

	if _, err := h.EnsureAddress(context.Background(), a); err != nil {
		t.Errorf("err %v, want the address restored", err)
	}
	if got := readyOf(h.claim); got != readyTrue {
		t.Errorf("ready %q, want %q", got, readyTrue)
	}
	if len(posts) != 1 {
		t.Fatalf("posts %v, want one", posts)
	}
	for _, want := range []string{"/api/ipam/ip-addresses/ ", `"address":"10.10.1.20/22"`, `"dns_name":""`, `"description":""`} {
		if !strings.Contains(posts[0], want) {
			t.Errorf("post %s lacks %s", posts[0], want)
		}
	}
}

func TestEnsureAddressHandsBackWithoutUnrenderableFields(t *testing.T) {
	var posts int
	var patches []string
	srv := allocNetBox(t, &posts, &patches)
	h := testHandler(srv.URL, 1)
	withHolder(h, false)
	h.pool.Spec.IPAddress.DNSName = `{{ if eq .Cluster "c2" }}a b{{ end }}{{ .Owner }}` // fails for the holder only

	a := &ipamv1.IPAddress{}
	if _, err := h.EnsureAddress(context.Background(), a); err != nil || a.Spec.Address != "10.10.1.21" {
		t.Fatalf("err %v, address %q, want 10.10.1.21", err, a.Spec.Address)
	}
	if len(patches) != 1 {
		t.Fatalf("patches %v, want one", patches)
	}
	for _, want := range []string{"/api/ipam/ip-addresses/5/ ", `"capi_ref":"IPAddressClaim/ns/other"`, `"dns_name":""`, `"description":""`} {
		if !strings.Contains(patches[0], want) {
			t.Errorf("patch %s lacks %s", patches[0], want)
		}
	}
}

func TestPoolReadied(t *testing.T) {
	pool := func(ready metav1.ConditionStatus) *v1alpha1.NetBoxIPPool {
		return &v1alpha1.NetBoxIPPool{Status: v1alpha1.NetBoxIPPoolStatus{
			Conditions:  []metav1.Condition{{Type: v1alpha1.ReadyCondition, Status: ready}},
			IPAddresses: &v1alpha1.NetBoxIPPoolStatusIPAddresses{Total: 1, Used: 1},
		}}
	}
	for name, tc := range map[string]struct {
		old, new metav1.ConditionStatus
		want     bool
	}{
		"turns ready":     {metav1.ConditionFalse, metav1.ConditionTrue, true},
		"stays ready":     {metav1.ConditionTrue, metav1.ConditionTrue, false},
		"turns not ready": {metav1.ConditionTrue, metav1.ConditionFalse, false},
	} {
		if got := poolReadied(ctx).Update(event.UpdateEvent{ObjectOld: pool(tc.old), ObjectNew: pool(tc.new)}); got != tc.want {
			t.Errorf("%s: %t, want %t", name, got, tc.want)
		}
	}
}

func TestAddressFieldsChanged(t *testing.T) {
	pool := func(f v1alpha1.IPAddressFields) *v1alpha1.NetBoxIPPool {
		return &v1alpha1.NetBoxIPPool{Spec: v1alpha1.NetBoxIPPoolSpec{IPAddress: f}}
	}
	base := v1alpha1.IPAddressFields{DNSName: "{{ .Owner }}", Description: "d"}
	for name, tc := range map[string]struct {
		old, new client.Object
		want     bool
	}{
		"same":        {pool(base), pool(base), false},
		"dns name":    {pool(base), pool(v1alpha1.IPAddressFields{DNSName: "{{ .Claim }}", Description: "d"}), true},
		"description": {pool(base), pool(v1alpha1.IPAddressFields{DNSName: base.DNSName}), true},
		"unset":       {pool(base), pool(v1alpha1.IPAddressFields{}), true},
	} {
		got := addressFieldsChanged.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new})
		if got != tc.want {
			t.Errorf("%s: %t, want %t", name, got, tc.want)
		}
	}
}

func TestEnsureAddressKeepsReadyOnNetBoxError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer srv.Close()
	a := &ipamv1.IPAddress{Spec: ipamv1.IPAddressSpec{Address: "10.10.1.20", Prefix: ptr.To(int32(22))}}
	h := testHandler(srv.URL, 1)
	h.claim.Status.AddressRef.Name = "m1"
	withReady(h.claim, metav1.ConditionTrue, clusterv1.ReadyReason)

	if _, err := h.EnsureAddress(context.Background(), a); err == nil || errors.Is(err, errAddressConflict) {
		t.Errorf("err %v, want a NetBox error", err)
	}
	if got := readyOf(h.claim); got != readyTrue {
		t.Errorf("ready %q, want %q: an outage does not undo an allocation", got, readyTrue)
	}
}

// allocNetBox allocates addresses 10.10.1.20, .21, ... from range 1 and records the PATCHes of addresses.
func allocNetBox(t *testing.T, posts *int, patches *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"results":[]}`)
		case r.Method == http.MethodPatch:
			b, _ := io.ReadAll(r.Body)
			*patches = append(*patches, r.URL.Path+" "+string(b))
			_, _ = io.WriteString(w, `{}`)
		case r.URL.Path == "/api/ipam/ip-ranges/1/available-ips/":
			*posts++
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":%d,"address":"10.10.1.%d/22"}`, 4+*posts, 19+*posts)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEnsureAddressHandsBackAddressHeldByOtherClaim(t *testing.T) {
	var posts int
	var patches []string
	srv := allocNetBox(t, &posts, &patches)
	h := testHandler(srv.URL, 1)
	withHolder(h, false)

	a := &ipamv1.IPAddress{}
	if _, err := h.EnsureAddress(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if a.Spec.Address != "10.10.1.21" || posts != 2 {
		t.Errorf("address %q after %d allocations, want 10.10.1.21 after 2", a.Spec.Address, posts)
	}
	if len(patches) != 1 {
		t.Fatalf("patches %v, want one", patches)
	}
	for _, want := range []string{"/api/ipam/ip-addresses/5/ ", `"capi_ref":"IPAddressClaim/ns/other"`, `"capi_cluster":"c2"`, `"owner":1`, `"dns_name":"other"`, `"description":"c2 worker other"`, `"changelog_message":"capi: restored for IPAddressClaim/ns/other"`} {
		if !strings.Contains(patches[0], want) {
			t.Errorf("patch %s lacks %s", patches[0], want)
		}
	}
}

func TestEnsureAddressIgnoresHolderThatIsGoing(t *testing.T) {
	var posts int
	var patches []string
	srv := allocNetBox(t, &posts, &patches)
	h := testHandler(srv.URL, 1)
	withHolder(h, true)

	a := &ipamv1.IPAddress{}
	if _, err := h.EnsureAddress(context.Background(), a); err != nil || a.Spec.Address != "10.10.1.20" || len(patches) != 0 {
		t.Errorf("err %v, address %q, patches %v: a deleting claim must not get its address back", err, a.Spec.Address, patches)
	}
}

func TestEnsureAddressGivesUpOnHeldAddresses(t *testing.T) {
	var posts int
	var patches []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"results":[]}`)
		case http.MethodPatch:
			patches = append(patches, r.URL.Path)
			_, _ = io.WriteString(w, `{}`)
		default:
			posts++
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":5,"address":"10.10.1.20/22"}`)
		}
	}))
	defer srv.Close()
	h := testHandler(srv.URL, 1)
	withHolder(h, false)

	a := &ipamv1.IPAddress{}
	if _, err := h.EnsureAddress(context.Background(), a); err == nil || a.Spec.Address != "" {
		t.Errorf("err %v, address %q, want an error and no address", err, a.Spec.Address)
	}
	if posts != maxAllocationAttempts || len(patches) != maxAllocationAttempts {
		t.Errorf("%d allocations and %d patches, want %d of each", posts, len(patches), maxAllocationAttempts)
	}
	if got := readyOf(h.claim); got != failed {
		t.Errorf("ready %q, want %q", got, failed)
	}
}

func TestFetchPoolWaitsForPoolStatus(t *testing.T) {
	scheme := poolScheme(t)
	for name, tc := range map[string]struct {
		mutate   func(*claimHandler)
		deleting bool
		wait     bool
	}{
		"ready":            {func(*claimHandler) {}, false, false},
		"not ready":        {func(h *claimHandler) { h.pool.Status.Conditions = nil }, false, true},
		"no ranges":        {func(h *claimHandler) { h.pool.Status.Ranges = nil }, false, true},
		"no ready range":   {func(h *claimHandler) { h.pool.Status.Ranges[0].Ready = false }, false, true},
		"no prefix length": {func(h *claimHandler) { h.pool.Status.AddressPrefixLength = 0 }, false, true},
		"deleting claim":   {func(h *claimHandler) { h.pool.Status.Conditions = nil }, true, false},
	} {
		h := testHandler("", 1)
		tc.mutate(h)
		h.pool.Namespace, h.pool.Name = "ns", "worker"
		h.claim.Spec.PoolRef.Name = "worker"
		if tc.deleting {
			h.claim.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		}
		h.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(h.pool.DeepCopy()).Build()

		pool, res, err := h.FetchPool(context.Background())
		if err != nil || (res != nil) != tc.wait || (pool == nil) != tc.wait {
			t.Errorf("%s: pool %v, result %v, err %v; want wait=%t", name, pool, res, err, tc.wait)
		}
		if res != nil && !res.IsZero() {
			t.Errorf("%s: result %+v, want an empty one (the pool watch wakes the claim)", name, res)
		}
		want := ""
		if tc.wait {
			want = poolNotReady
		}
		if got := readyOf(h.claim); got != want {
			t.Errorf("%s: ready %q, want %q", name, got, want)
		}
	}
}

func TestFetchPoolKeepsReadyOfAllocatedClaim(t *testing.T) {
	for name, pool := range map[string]*v1alpha1.NetBoxIPPool{
		"pool not ready": {ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "worker"}},
		"no pool":        nil,
	} {
		h := testHandler("", 1)
		h.claim.Spec.PoolRef.Name = "worker"
		h.claim.Status.AddressRef.Name = "m1"
		withReady(h.claim, metav1.ConditionTrue, clusterv1.ReadyReason)
		b := fake.NewClientBuilder().WithScheme(poolScheme(t))
		if pool != nil {
			b = b.WithObjects(pool)
		}
		h.Client = b.Build()

		_, _, _ = h.FetchPool(context.Background())
		if got := readyOf(h.claim); got != readyTrue {
			t.Errorf("%s: ready %q, want %q", name, got, readyTrue)
		}
	}
}

func TestFetchPoolReportsMissingPool(t *testing.T) {
	scheme := poolScheme(t)
	h := testHandler("", 1)
	h.claim.Spec.PoolRef.Name = "worker"
	h.Client = fake.NewClientBuilder().WithScheme(scheme).Build()
	if _, _, err := h.FetchPool(context.Background()); !apierrors.IsNotFound(err) {
		t.Errorf("err %v, want NotFound", err)
	}
	if got := readyOf(h.claim); got != poolNotReady {
		t.Errorf("ready %q, want %q", got, poolNotReady)
	}
}

func newClaim(ns, name, pool string) *ipamv1.IPAddressClaim {
	return &ipamv1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: ipamv1.IPAddressClaimSpec{
			ClusterName: ns,
			PoolRef:     ipamv1.IPPoolReference{APIGroup: v1alpha1.GroupVersion.Group, Kind: v1alpha1.NetBoxIPPoolKind, Name: pool},
		},
	}
}

// readyPool creates network "default" (/28) and pool "worker" over it and waits for both.
func readyPool(g *WithT, ns string, ranges ...v1alpha1.IPPoolRange) (*v1alpha1.NetBoxNetwork, *v1alpha1.NetBoxIPPool) {
	nw := newNetwork(ns, "default")
	g.Expect(k8s.Create(ctx, nw)).To(Succeed())
	waitReady(g, nw)
	pool := newPool(ns, "worker", ranges...)
	g.Expect(k8s.Create(ctx, pool)).To(Succeed())
	waitReady(g, pool)
	return nw, pool
}

func address(g *WithT, claim *ipamv1.IPAddressClaim) *ipamv1.IPAddress {
	a := &ipamv1.IPAddress{}
	g.Eventually(func() error { return k8s.Get(ctx, client.ObjectKeyFromObject(claim), a) }).Should(Succeed())
	return a
}

func noAddress(claim *ipamv1.IPAddressClaim) func() bool {
	return func() bool {
		return apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKeyFromObject(claim), &ipamv1.IPAddress{}))
	}
}

func TestClaimGetsAddress(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, pool := readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 4})
	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())

	a := address(g, claim)
	p := netip.MustParsePrefix(nw.Status.Prefix)
	g.Expect([]string{addr(p, 2), addr(p, 3), addr(p, 4)}).To(ContainElement(a.Spec.Address))
	g.Expect(*a.Spec.Prefix).To(Equal(int32(22)))
	g.Expect(a.Spec.Gateway).To(Equal("10.10.0.1"))
	g.Eventually(func() string {
		_ = k8s.Get(ctx, client.ObjectKeyFromObject(claim), claim)
		return claim.Status.AddressRef.Name
	}).Should(Equal("m1"))
	g.Eventually(func() string { return claimReady(g, claim) }).Should(Equal(readyTrue))

	ips, err := admin.List(ctx, netbox.IPAddresses, owner.ByRef("IPAddressClaim/"+ns+"/m1"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ips).To(HaveLen(1))
	g.Expect(ips[0].Address).To(Equal(a.Spec.Address + "/22"))
	g.Expect(cfString(ips[0], netbox.FieldCluster)).To(Equal(ns))
	g.Eventually(func() int32 {
		_ = k8s.Get(ctx, client.ObjectKeyFromObject(pool), pool)
		if pool.Status.IPAddresses == nil {
			return -1
		}
		return pool.Status.IPAddresses.Free
	}).Should(Equal(int32(2)))
}

func TestClaimReleaseDeletesNetBoxAddress(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 4})
	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	address(g, claim)

	g.Expect(k8s.Delete(ctx, claim)).To(Succeed())
	g.Eventually(func() ([]netbox.Object, error) {
		return admin.List(ctx, netbox.IPAddresses, owner.ByRef("IPAddressClaim/"+ns+"/m1"))
	}).Should(BeEmpty())
	g.Eventually(noAddress(claim)).Should(BeTrue())
}

func TestClaimWaitsForFreeAddress(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 2})
	m1 := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, m1)).To(Succeed())
	first := address(g, m1)

	m2 := newClaim(ns, "m2", "worker")
	g.Expect(k8s.Create(ctx, m2)).To(Succeed())
	g.Consistently(noAddress(m2)).Should(BeTrue())
	g.Expect(claimReady(g, m2)).To(Equal(exhausted))

	g.Expect(k8s.Delete(ctx, m1)).To(Succeed())
	g.Expect(address(g, m2).Spec.Address).To(Equal(first.Spec.Address))
	g.Eventually(func() string { return claimReady(g, m2) }).Should(Equal(readyTrue))
}

func claimReady(g *WithT, claim *ipamv1.IPAddressClaim) string {
	g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(claim), claim)).To(Succeed())
	return readyOf(claim)
}

func TestClaimReusesNetBoxAddressAfterCrash(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, _ := readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 4})
	p := netip.MustParsePrefix(nw.Status.Prefix)
	ref := "IPAddressClaim/" + ns + "/m1"
	_, err := admin.Create(ctx, netbox.IPAddresses, map[string]any{
		"address": addr(p, 4) + "/22", "status": "active", "owner": owner.ID,
		"custom_fields": map[string]any{netbox.FieldRef: ref, netbox.FieldCluster: ns},
	})
	g.Expect(err).NotTo(HaveOccurred())

	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	g.Expect(address(g, claim).Spec.Address).To(Equal(addr(p, 4)))
	g.Expect(admin.List(ctx, netbox.IPAddresses, owner.ByRef(ref))).To(HaveLen(1))
}

// ownedIPs lists the NetBox addresses of the claim.
func ownedIPs(ns, name string) func() ([]netbox.Object, error) {
	return func() ([]netbox.Object, error) {
		return admin.List(ctx, netbox.IPAddresses, owner.ByRef("IPAddressClaim/"+ns+"/"+name))
	}
}

// allocatedClaim creates a claim and waits until it holds an address and is Ready.
func allocatedClaim(g *WithT, ns string) (*ipamv1.IPAddressClaim, netbox.Object) {
	readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 4})
	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	address(g, claim)
	g.Eventually(func() string { return claimReady(g, claim) }).Should(Equal(readyTrue))
	ips, err := ownedIPs(ns, "m1")()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ips).To(HaveLen(1))
	return claim, ips[0]
}

func TestClaimRestoresDeletedAddress(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	claim, ip := allocatedClaim(g, ns)
	g.Expect(admin.Delete(ctx, netbox.IPAddresses, ip.ID, "test: deleted by hand")).To(Succeed())

	touch(g, claim)
	g.Eventually(ownedIPs(ns, "m1")).Should(HaveLen(1))
	ips, err := ownedIPs(ns, "m1")()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ips[0].Address).To(Equal(ip.Address))
	g.Expect(owner.Owns(ips[0])).To(BeTrue())
	g.Expect(cfString(ips[0], netbox.FieldRef)).To(Equal("IPAddressClaim/" + ns + "/m1"))
	g.Expect(cfString(ips[0], netbox.FieldCluster)).To(Equal(ns))
	g.Expect(claimReady(g, claim)).To(Equal(readyTrue))
}

func TestClaimAddressConflict(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	claim, ip := allocatedClaim(g, ns)
	g.Expect(admin.Delete(ctx, netbox.IPAddresses, ip.ID, "test: deleted by hand")).To(Succeed())
	foreign, err := admin.Create(ctx, netbox.IPAddresses, map[string]any{"address": ip.Address, "status": "active", "description": "someone else's"})
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(func() { _ = admin.Delete(ctx, netbox.IPAddresses, foreign.ID, "test cleanup") })

	touch(g, claim)
	g.Eventually(func() string { return claimReady(g, claim) }).Should(Equal(addressConflict))
	g.Consistently(ownedIPs(ns, "m1")).Should(BeEmpty())
	got, _, err := admin.Get(ctx, netbox.IPAddresses, foreign.ID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.Owner).To(BeNil())
	g.Expect(cfString(got, netbox.FieldRef)).To(BeEmpty())
}

func TestClaimDoesNotTakeDeletedAddress(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	first, ip := allocatedClaim(g, ns)
	g.Expect(admin.Delete(ctx, netbox.IPAddresses, ip.ID, "test: deleted by hand")).To(Succeed())

	second := newClaim(ns, "m2", "worker")
	g.Expect(k8s.Create(ctx, second)).To(Succeed())
	got := address(g, second)
	g.Expect(got.Spec.Address + "/22").NotTo(Equal(ip.Address))

	g.Eventually(ownedIPs(ns, "m1")).Should(HaveLen(1))
	ips, err := ownedIPs(ns, "m1")()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ips[0].Address).To(Equal(ip.Address))
	g.Expect(owner.Owns(ips[0])).To(BeTrue())
	g.Expect(cfString(ips[0], netbox.FieldCluster)).To(Equal(ns))
	ips, err = ownedIPs(ns, "m2")()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ips).To(HaveLen(1))
	g.Expect(ips[0].Address).To(Equal(got.Spec.Address + "/22"))

	touch(g, first)
	g.Consistently(func() string { return claimReady(g, first) }).Should(Equal(readyTrue))
	g.Expect(ownedIPs(ns, "m1")()).To(HaveLen(1))
}

func TestClaimForeignOwnerGuard(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	nw, _ := readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 4})
	p := netip.MustParsePrefix(nw.Status.Prefix)
	byHand(t, g, netbox.IPAddresses, map[string]any{
		"address": addr(p, 10) + "/22", "status": "active", "owner": otherOwner(g).ID,
		"custom_fields": map[string]any{netbox.FieldRef: "IPAddressClaim/" + ns + "/m1", netbox.FieldCluster: ns},
	})

	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	g.Eventually(func() string { return claimReady(g, claim) }).Should(Equal(failed))
	g.Expect(meta.FindStatusCondition(claim.Status.Conditions, ipamv1.IPAddressClaimReadyCondition).Message).To(ContainSubstring("another owner"))
	g.Consistently(noAddress(claim)).Should(BeTrue())
	g.Expect(ownedIPs(ns, "m1")()).To(BeEmpty())
}

func TestClaimSkipsPausedCluster(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 4})
	setPaused(t, ns, true)
	claim := newClaim(ns, "m1", "worker")
	g.Expect(k8s.Create(ctx, claim)).To(Succeed())
	g.Consistently(noAddress(claim)).Should(BeTrue())

	setPaused(t, ns, false)
	address(g, claim)
}

func TestClaimAddressFields(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	readyPool(g, ns, v1alpha1.IPPoolRange{NetworkName: "default", Start: 2, End: 5})
	node := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "node-a"}}
	g.Expect(k8s.Create(ctx, node)).To(Succeed())
	owned := newClaim(ns, "node-a-0-0", "worker")
	owned.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: node.Name, UID: node.UID}}
	g.Expect(k8s.Create(ctx, owned)).To(Succeed())
	bare := newClaim(ns, "vip", "worker")
	g.Expect(k8s.Create(ctx, bare)).To(Succeed())
	address(g, owned)
	address(g, bare)

	ip := func(name string) netbox.Object {
		ips, err := ownedIPs(ns, name)()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ips).To(HaveLen(1))
		return ips[0]
	}
	g.Expect(ip("node-a-0-0").DNSName).To(Equal("node-a"))
	g.Expect(ip("node-a-0-0").Description).To(Equal(ns + " worker node-a"))
	g.Expect(ip("vip").DNSName).To(Equal("vip"))
}

func TestClaimAddressFieldsFollowPool(t *testing.T) {
	g := newG(t)
	ns := newEnv(t, nil)
	claim, before := allocatedClaim(g, ns)
	pool := &v1alpha1.NetBoxIPPool{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "worker"}}
	eventually(g, pool, func(g Gomega) {
		pool.Spec.IPAddress = v1alpha1.IPAddressFields{DNSName: "{{ .Claim }}.Nodes.Example", Description: "{{ .Claim }}"}
		g.Expect(k8s.Update(ctx, pool)).To(Succeed())
	})
	g.Eventually(func(g Gomega) {
		ips, err := ownedIPs(ns, "m1")()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ips).To(HaveLen(1))
		g.Expect(ips[0].DNSName).To(Equal("m1.nodes.example"))
		g.Expect(ips[0].Description).To(Equal("m1"))
	}).Should(Succeed())

	// Steady state writes nothing: NetBox lowercases dns_name, and the comparison must agree.
	// Every save bumps last_updated, a no-op one too.
	updated := func() string {
		ip, _, err := admin.Get(ctx, netbox.IPAddresses, before.ID)
		g.Expect(err).NotTo(HaveOccurred())
		return ip.LastUpdated
	}
	last := updated()
	touch(g, claim)
	g.Consistently(updated, "5s").Should(Equal(last))

	// An invalid template stops new allocations but keeps allocated claims ready.
	eventually(g, pool, func(g Gomega) {
		pool.Spec.IPAddress.DNSName = "{{ .Claim }} node"
		g.Expect(k8s.Update(ctx, pool)).To(Succeed())
	})
	waitCondition(g, pool, metav1.ConditionFalse, v1alpha1.ReasonInvalidSpec)
	g.Consistently(func() string { return claimReady(g, claim) }, "5s").Should(Equal(readyTrue))
}
