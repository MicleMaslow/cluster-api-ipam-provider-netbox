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

package v1alpha1

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestValidation(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("envtest assets missing: run make test")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	network := func(name string, mutate func(*NetBoxNetworkSpec)) *NetBoxNetwork {
		n := &NetBoxNetwork{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
			Spec:       NetBoxNetworkSpec{ClusterName: "c1", ParentPrefix: "10.10.0.0/22", Gateway: "10.10.0.1", PrefixLength: 27},
		}
		if mutate != nil {
			mutate(&n.Spec)
		}
		return n
	}
	wantErr := func(t *testing.T, err error, msg string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("got %v, want an error with %q", err, msg)
		}
	}

	t.Run("network create rules", func(t *testing.T) {
		for name, tc := range map[string]struct {
			mutate func(*NetBoxNetworkSpec)
			msg    string
		}{
			"both":        {func(s *NetBoxNetworkSpec) { s.Prefix = "10.10.1.0/27" }, "exactly one of prefix or prefixLength"},
			"neither":     {func(s *NetBoxNetworkSpec) { s.PrefixLength = 0 }, "exactly one of prefix or prefixLength"},
			"bad-parent":  {func(s *NetBoxNetworkSpec) { s.ParentPrefix = "10.10.0.0" }, "must be a CIDR"},
			"bad-gateway": {func(s *NetBoxNetworkSpec) { s.Gateway = "gw" }, "must be an IP address"},

			"gateway-outside-parent":          {func(s *NetBoxNetworkSpec) { s.Gateway = "10.20.0.1" }, "gateway must be inside parentPrefix"},
			"prefix-outside-parent":           {func(s *NetBoxNetworkSpec) { s.PrefixLength, s.Prefix = 0, "10.20.1.0/27" }, "prefix must be inside parentPrefix"},
			"prefix-ipv6-in-ipv4-parent":      {func(s *NetBoxNetworkSpec) { s.PrefixLength, s.Prefix = 0, "fd00::/80" }, "prefix must be inside parentPrefix"},
			"prefix-length-equals-mask":       {func(s *NetBoxNetworkSpec) { s.PrefixLength = 22 }, "prefixLength must be longer than the parentPrefix mask"},
			"prefix-length-shorter-than-mask": {func(s *NetBoxNetworkSpec) { s.PrefixLength = 16 }, "prefixLength must be longer than the parentPrefix mask"},
			"ipv6-parent":                     {func(s *NetBoxNetworkSpec) { s.ParentPrefix = "fd00::/64" }, "spec.parentPrefix: Invalid value"},
			"ipv6-gateway":                    {func(s *NetBoxNetworkSpec) { s.Gateway = "fd00::1" }, "only IPv4 is supported"},
			"adopt-with-delete": {func(s *NetBoxNetworkSpec) {
				s.PrefixLength, s.Prefix, s.DeletionPolicy = 0, "10.10.1.0/27", DeletionPolicyDelete
			}, "an adopted prefix cannot use deletionPolicy Delete"},
		} {
			wantErr(t, c.Create(ctx, network(name, tc.mutate)), tc.msg)
		}
	})

	t.Run("network defaults and immutability", func(t *testing.T) {
		n := network("immutable", nil)
		if err := c.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		if n.Spec.DeletionPolicy != DeletionPolicyRetain {
			t.Errorf("deletionPolicy defaulted to %q", n.Spec.DeletionPolicy)
		}
		n.Spec.ParentPrefix = "10.20.0.0/22"
		wantErr(t, c.Update(ctx, n), "parentPrefix is immutable")
	})

	t.Run("network accepted", func(t *testing.T) {
		for name, mutate := range map[string]func(*NetBoxNetworkSpec){
			"carve-delete": func(s *NetBoxNetworkSpec) { s.DeletionPolicy = DeletionPolicyDelete },
			"carve-23":     func(s *NetBoxNetworkSpec) { s.PrefixLength = 23 },
			"adopt-retain": func(s *NetBoxNetworkSpec) { s.PrefixLength, s.Prefix = 0, "10.10.1.0/27" },
		} {
			if err := c.Create(ctx, network("ok-"+name, mutate)); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("network gateway is immutable", func(t *testing.T) {
		n := network("gateway", nil)
		if err := c.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		n.Spec.Gateway = "10.10.0.2"
		wantErr(t, c.Update(ctx, n), "gateway is immutable")
	})

	t.Run("network cannot switch mode", func(t *testing.T) {
		n := network("switch", nil)
		if err := c.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		n.Spec.PrefixLength = 0
		n.Spec.Prefix = "10.10.1.0/27"
		wantErr(t, c.Update(ctx, n), "cannot switch between prefix and prefixLength")
	})

	t.Run("custom fields", func(t *testing.T) {
		ok := network("cf-ok", func(s *NetBoxNetworkSpec) {
			s.CustomFields = map[string]apiextensionsv1.JSON{"labels": {Raw: []byte(`{"cluster":"c1"}`)}, "site": {Raw: []byte(`"a"`)}}
		})
		if err := c.Create(ctx, ok); err != nil {
			t.Errorf("create: %v", err)
		}
	})

	t.Run("tags", func(t *testing.T) {
		if err := c.Create(ctx, network("tags-ok", func(s *NetBoxNetworkSpec) { s.Tags = []string{"capi", "k8s_2"} })); err != nil {
			t.Errorf("create: %v", err)
		}
		wantErr(t, c.Create(ctx, network("tags-bad", func(s *NetBoxNetworkSpec) { s.Tags = []string{"has space"} })), "spec.tags[0]")
		wantErr(t, c.Create(ctx, network("tags-dup", func(s *NetBoxNetworkSpec) { s.Tags = []string{"a", "a"} })), "Duplicate value")
	})

	t.Run("spec is required", func(t *testing.T) {
		for _, kind := range []string{"NetBoxNetwork", "NetBoxIPPool"} {
			u := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "nospec", "namespace": "default"}}}
			u.SetGroupVersionKind(GroupVersion.WithKind(kind))
			wantErr(t, c.Create(ctx, u), "spec: Required value")
		}
	})

	t.Run("pool ranges", func(t *testing.T) {
		pool := func(name string, ranges ...IPPoolRange) *NetBoxIPPool {
			return &NetBoxIPPool{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}, Spec: NetBoxIPPoolSpec{Ranges: ranges}}
		}
		a := IPPoolRange{NetworkName: "a", Start: 1, End: 6}
		wantErr(t, c.Create(ctx, pool("dup", a, a)), "one range per network")
		wantErr(t, c.Create(ctx, pool("reversed", IPPoolRange{NetworkName: "a", Start: 5, End: 2})), "start must not be after end")
		wantErr(t, c.Create(ctx, pool("reversed-neg", IPPoolRange{NetworkName: "a", Start: -2, End: -5})), "start must not be after end")
		if err := c.Create(ctx, pool("mixed-signs", IPPoolRange{NetworkName: "a", Start: 2, End: -1})); err != nil {
			t.Errorf("start 2, end -1: %v", err)
		}

		withFields := func(name string, f IPAddressFields) *NetBoxIPPool {
			p := pool(name, a)
			p.Spec.IPAddress = f
			return p
		}
		for name, f := range map[string]IPAddressFields{
			"dns-only":         {DNSName: "{{ .Owner }}"},
			"description-only": {Description: "{{ .Cluster }}"},
			"both":             {DNSName: "{{ .Owner }}", Description: "{{ .Pool }}"},
		} {
			if err := c.Create(ctx, withFields("fields-"+name, f)); err != nil {
				t.Errorf("ipAddress %s: %v", name, err)
			}
		}
		// the typed client drops empty values, so the invalid ones go raw
		raw := func(name string, ipAddress map[string]any) *unstructured.Unstructured {
			u := &unstructured.Unstructured{Object: map[string]any{
				"metadata": map[string]any{"name": name, "namespace": "default"},
				"spec": map[string]any{
					"ranges":    []any{map[string]any{"networkName": "a", "start": int64(1), "end": int64(6)}},
					"ipAddress": ipAddress,
				},
			}}
			u.SetGroupVersionKind(GroupVersion.WithKind("NetBoxIPPool"))
			return u
		}
		wantErr(t, c.Create(ctx, raw("fields-empty", map[string]any{})), "should have at least 1 properties")
		wantErr(t, c.Create(ctx, raw("fields-empty-dns", map[string]any{"dnsName": ""})), "spec.ipAddress.dnsName")
		wantErr(t, c.Create(ctx, raw("fields-empty-desc", map[string]any{"dnsName": "x", "description": ""})), "spec.ipAddress.description")
		wantErr(t, c.Create(ctx, raw("fields-long", map[string]any{"description": strings.Repeat("x", 1025)})), "spec.ipAddress.description")

		p := pool("edit", a)
		if err := c.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		p.Spec.Ranges = append(p.Spec.Ranges, IPPoolRange{NetworkName: "b", Start: -1, End: -1})
		if err := c.Update(ctx, p); err != nil {
			t.Errorf("append: %v", err)
		}
		p.Spec.Ranges[0].End = 7
		if err := c.Update(ctx, p); err != nil {
			t.Errorf("change end: %v", err)
		}
		p.Spec.Ranges = p.Spec.Ranges[1:]
		if err := c.Update(ctx, p); err != nil {
			t.Errorf("remove: %v", err)
		}
	})
}
