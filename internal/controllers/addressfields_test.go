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
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

func TestAddressFields(t *testing.T) {
	pool := &v1alpha1.NetBoxIPPool{ObjectMeta: metav1.ObjectMeta{Name: "cp"}}
	claim := &ipamv1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "vm-1-0-0", Labels: map[string]string{clusterv1.ClusterNameLabel: "c1"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "VSphereVM", Name: "vm-1"}}},
	}
	orphan := &ipamv1.IPAddressClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "vip", Labels: claim.Labels}}
	long := strings.Repeat("ж", 250)
	fields := func(dns, desc string) ipFields {
		return ipFields{DNSName: dns, texts: texts{Description: desc, Comments: managedComments}}
	}
	for name, tc := range map[string]struct {
		t     v1alpha1.IPAddressFields
		claim *ipamv1.IPAddressClaim
		want  ipFields
		err   string
	}{
		"defaults":      {claim: claim, want: fields("vm-1", "ns/vm-1 // c1 cp")},
		"no owner":      {claim: orphan, want: fields("vip", "ns/vip // c1 cp")},
		"custom":        {t: v1alpha1.IPAddressFields{DNSName: "{{ .Owner }}.Nodes.Example.", Description: " {{ .Namespace }}/{{ .Claim }} ", Comments: "{{ .Pool }}\n{{ .Cluster }} "}, claim: claim, want: ipFields{DNSName: "vm-1.nodes.example.", texts: texts{Description: "ns/vm-1-0-0", Comments: "cp\nc1"}}},
		"empty dns":     {t: v1alpha1.IPAddressFields{DNSName: `{{ "" }}`}, claim: claim, want: fields("", "ns/vm-1 // c1 cp")},
		"empty texts":   {t: v1alpha1.IPAddressFields{Description: `{{ "" }}`, Comments: `{{ "" }}`}, claim: claim, want: ipFields{DNSName: "vm-1"}},
		"long":          {t: v1alpha1.IPAddressFields{Description: long, Comments: long}, claim: claim, want: ipFields{DNSName: "vm-1", texts: texts{Description: string([]rune(long)[:200]), Comments: long}}},
		"unknown field": {t: v1alpha1.IPAddressFields{DNSName: "{{ .Node }}"}, claim: claim, err: "Node"},
		"parse error":   {t: v1alpha1.IPAddressFields{Description: "{{ .Owner"}, claim: claim, err: "unclosed action"},
		"comments":      {t: v1alpha1.IPAddressFields{Comments: "{{ .Network }}"}, claim: claim, err: "comments: "},
		"invalid dns":   {t: v1alpha1.IPAddressFields{DNSName: "{{ .Owner }} node"}, claim: claim, err: "not a valid DNS name"},
		"kelvin sign":   {t: v1alpha1.IPAddressFields{DNSName: "\u212A8s"}, claim: claim, err: "not a valid DNS name"},
		"if, with":      {t: v1alpha1.IPAddressFields{DNSName: `{{ if eq .Cluster "" }}x{{ else if .Owner }}{{ with .Owner }}{{ . }}{{ end }}{{ end }}`}, claim: claim, want: fields("vm-1", "ns/vm-1 // c1 cp")},
		"range":         {t: v1alpha1.IPAddressFields{DNSName: "{{ range 1000000000000 }}{{ end }}{{ .Owner }}"}, claim: claim, err: "range is not allowed"},
		"nested range":  {t: v1alpha1.IPAddressFields{Description: "{{ with .Owner }}{{ else }}{{ range 3 }}{{ end }}{{ end }}"}, claim: claim, err: "range is not allowed"},
		"define":        {t: v1alpha1.IPAddressFields{DNSName: `{{ define "x" }}a{{ end }}{{ .Owner }}`}, claim: claim, err: "define and block are not allowed"},
		"template":      {t: v1alpha1.IPAddressFields{Description: `{{ if true }}{{ template "description" . }}{{ end }}`}, claim: claim, err: "template is not allowed"},
		"output size":   {t: v1alpha1.IPAddressFields{Description: `{{ $x := printf "%*d" 600 1 }}{{ $x }}{{ $x }}`}, claim: claim, err: "longer than 1024 bytes"},
		"comments size": {t: v1alpha1.IPAddressFields{Comments: `{{ $x := printf "%*d" 600 1 }}{{ $x }}{{ $x }}`}, claim: claim, err: "longer than 1024 bytes"},
		"value size":    {t: v1alpha1.IPAddressFields{Description: `{{ "\\" | js | js | js | js | js | js | js | js | js | js | js | len }}`}, claim: claim, err: "error calling js"},
		"invalid utf-8": {t: v1alpha1.IPAddressFields{Description: `{{ "a\xff\xfeb" }}`}, claim: claim, want: fields("vm-1", "a\uFFFD\uFFFDb")},
		"nul dns":       {t: v1alpha1.IPAddressFields{DNSName: `a{{ "\x00" }}`}, claim: claim, err: "dnsName: contains a NUL"},
		"nul text":      {t: v1alpha1.IPAddressFields{Description: "a\x00b"}, claim: claim, err: "description: contains a NUL"},
		"nul comments":  {t: v1alpha1.IPAddressFields{Comments: `{{ "\x00" }}`}, claim: claim, err: "comments: contains a NUL"},
		"separators":    {t: v1alpha1.IPAddressFields{DNSName: "\x1c{{ .Owner }}\x1f", Description: "\x1d\u3000{{ .Owner }}\x1e"}, claim: claim, want: fields("vm-1", "vm-1")},
		"cut on space":  {t: v1alpha1.IPAddressFields{Description: strings.Repeat("ж", 199) + "\x1f tail"}, claim: claim, want: fields("vm-1", strings.Repeat("ж", 199))},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			p := pool.DeepCopy()
			p.Spec.IPAddress = tc.t
			got, err := addressFields(p, tc.claim)
			if tc.err != "" {
				g.Expect(err).To(MatchError(ContainSubstring(tc.err)))
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

// The prefix and the VIP render the network's templates, the range the pool's ipRange ones; the defaults are those of
// the spec. The description is cut to 200 characters, comments are not; a network template that does not render is InvalidSpec.
func TestTexts(t *testing.T) {
	g := NewWithT(t)
	const comments = managedComments
	nw := newNetwork("ns", "default")
	nw.Spec.ClusterName, nw.Status.OwnerID, nw.Status.TenantID = "c1", 1, 7

	prefix, vip, err := networkTexts(nw)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(prefix).To(Equal(netbox.Attrs{Owner: 1, Tenant: 7, Description: "ns/default // cluster c1", Comments: comments}))
	g.Expect(vip).To(Equal(netbox.Attrs{Owner: 1, Tenant: 7, Description: "ns/default // c1 control plane endpoint", Comments: comments}))

	long := strings.Repeat("x", 1000)
	custom := nw.DeepCopy()
	custom.Spec.Description, custom.Spec.Comments = "{{ .Name }} of {{ .Cluster }}", long
	custom.Spec.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{Description: long, Comments: "{{ .Namespace }}"}
	prefix, vip, err = networkTexts(custom)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(prefix).To(Equal(netbox.Attrs{Owner: 1, Tenant: 7, Description: "default of c1", Comments: long}))
	g.Expect(vip).To(Equal(netbox.Attrs{Owner: 1, Tenant: 7, Description: long[:200], Comments: "ns"}))

	for name, mutate := range map[string]func(*v1alpha1.NetBoxNetworkSpec){
		"description": func(s *v1alpha1.NetBoxNetworkSpec) { s.Description = "{{ .Pool }}" },
		"comments":    func(s *v1alpha1.NetBoxNetworkSpec) { s.Comments = "{{ range 3 }}{{ end }}" },
		"endpoint description": func(s *v1alpha1.NetBoxNetworkSpec) {
			s.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{Description: "{{ .Owner }}"}
		},
		"endpoint comments": func(s *v1alpha1.NetBoxNetworkSpec) {
			s.ControlPlaneEndpoint = &v1alpha1.ControlPlaneEndpoint{Comments: "{{"}
		},
	} {
		bad := nw.DeepCopy()
		mutate(&bad.Spec)
		_, _, err := networkTexts(bad)
		g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonInvalidSpec), name)
		g.Expect(err).To(MatchError(ContainSubstring(strings.TrimPrefix(name, "endpoint "))), name)
		g.Expect(strings.HasPrefix(err.Error(), "controlPlaneEndpoint: ")).To(Equal(strings.HasPrefix(name, "endpoint")), name)
	}

	nul := nw.DeepCopy()
	nul.Spec.Comments = "a\x00b"
	_, _, err = networkTexts(nul)
	g.Expect(errReason(err)).To(Equal(v1alpha1.ReasonInvalidSpec))
	g.Expect(err).To(MatchError(ContainSubstring("NUL")))

	pool := newPool("ns", "worker")
	got, err := rangeTexts(pool, nw)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(Equal(texts{Description: "ns/worker // pool of cluster c1", Comments: comments}))
	pool.Spec.IPRange = v1alpha1.IPRangeFields{Description: "{{ .Pool }} in {{ .Network }}", Comments: long}
	got, err = rangeTexts(pool, nw)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(Equal(texts{Description: "worker in default", Comments: long}))
	pool.Spec.IPRange.Description = "{{ .Owner }}"
	_, err = rangeTexts(pool, nw)
	g.Expect(err).To(MatchError(ContainSubstring("description: ")))

	g.Expect(validatePoolTemplates(newPool("ns", "worker"))).To(Succeed())
	pool = newPool("ns", "worker")
	pool.Spec.IPAddress.DNSName = "a/{{ .Owner }}"
	g.Expect(validatePoolTemplates(pool)).To(MatchError(ContainSubstring("ipAddress: dnsName")))
	pool = newPool("ns", "worker")
	pool.Spec.IPRange.Comments = "{{ .Claim }}"
	g.Expect(validatePoolTemplates(pool)).To(MatchError(ContainSubstring("ipRange: comments")))
}
