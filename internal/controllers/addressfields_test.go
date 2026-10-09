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
)

func TestAddressFields(t *testing.T) {
	pool := &v1alpha1.NetBoxIPPool{ObjectMeta: metav1.ObjectMeta{Name: "cp"}}
	claim := &ipamv1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "vm-1-0-0", Labels: map[string]string{clusterv1.ClusterNameLabel: "c1"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "VSphereVM", Name: "vm-1"}}},
	}
	orphan := &ipamv1.IPAddressClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "vip", Labels: claim.Labels}}
	long := strings.Repeat("ж", 250)
	for name, tc := range map[string]struct {
		t     v1alpha1.IPAddressFields
		claim *ipamv1.IPAddressClaim
		want  ipFields
		err   string
	}{
		"defaults":      {claim: claim, want: ipFields{DNSName: "vm-1", Description: "c1 cp vm-1"}},
		"no owner":      {claim: orphan, want: ipFields{DNSName: "vip", Description: "c1 cp vip"}},
		"custom":        {t: v1alpha1.IPAddressFields{DNSName: "{{ .Owner }}.Nodes.Example.", Description: " {{ .Namespace }}/{{ .Claim }} "}, claim: claim, want: ipFields{DNSName: "vm-1.nodes.example.", Description: "ns/vm-1-0-0"}},
		"empty dns":     {t: v1alpha1.IPAddressFields{DNSName: `{{ "" }}`}, claim: claim, want: ipFields{Description: "c1 cp vm-1"}},
		"long":          {t: v1alpha1.IPAddressFields{Description: long}, claim: claim, want: ipFields{DNSName: "vm-1", Description: string([]rune(long)[:200])}},
		"unknown field": {t: v1alpha1.IPAddressFields{DNSName: "{{ .Node }}"}, claim: claim, err: "Node"},
		"parse error":   {t: v1alpha1.IPAddressFields{Description: "{{ .Owner"}, claim: claim, err: "unclosed action"},
		"invalid dns":   {t: v1alpha1.IPAddressFields{DNSName: "{{ .Owner }} node"}, claim: claim, err: "not a valid DNS name"},
		"kelvin sign":   {t: v1alpha1.IPAddressFields{DNSName: "\u212A8s"}, claim: claim, err: "not a valid DNS name"},
		"if, with":      {t: v1alpha1.IPAddressFields{DNSName: `{{ if eq .Cluster "" }}x{{ else if .Owner }}{{ with .Owner }}{{ . }}{{ end }}{{ end }}`}, claim: claim, want: ipFields{DNSName: "vm-1", Description: "c1 cp vm-1"}},
		"range":         {t: v1alpha1.IPAddressFields{DNSName: "{{ range 1000000000000 }}{{ end }}{{ .Owner }}"}, claim: claim, err: "range is not allowed"},
		"nested range":  {t: v1alpha1.IPAddressFields{Description: "{{ with .Owner }}{{ else }}{{ range 3 }}{{ end }}{{ end }}"}, claim: claim, err: "range is not allowed"},
		"define":        {t: v1alpha1.IPAddressFields{DNSName: `{{ define "x" }}a{{ end }}{{ .Owner }}`}, claim: claim, err: "define and block are not allowed"},
		"template":      {t: v1alpha1.IPAddressFields{Description: `{{ if true }}{{ template "description" . }}{{ end }}`}, claim: claim, err: "template is not allowed"},
		"output size":   {t: v1alpha1.IPAddressFields{Description: `{{ $x := printf "%*d" 600 1 }}{{ $x }}{{ $x }}`}, claim: claim, err: "longer than 1024 bytes"},
		"value size":    {t: v1alpha1.IPAddressFields{Description: `{{ "\\" | js | js | js | js | js | js | js | js | js | js | js | len }}`}, claim: claim, err: "error calling js"},
		"invalid utf-8": {t: v1alpha1.IPAddressFields{Description: `{{ "a\xff\xfeb" }}`}, claim: claim, want: ipFields{DNSName: "vm-1", Description: "a\uFFFD\uFFFDb"}},
		"separators":    {t: v1alpha1.IPAddressFields{DNSName: "\x1c{{ .Owner }}\x1f", Description: "\x1d\u3000{{ .Owner }}\x1e"}, claim: claim, want: ipFields{DNSName: "vm-1", Description: "vm-1"}},
		"cut on space":  {t: v1alpha1.IPAddressFields{Description: strings.Repeat("ж", 199) + "\x1f tail"}, claim: claim, want: ipFields{DNSName: "vm-1", Description: strings.Repeat("ж", 199)}},
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
	g := NewWithT(t)
	g.Expect(validateAddressFields(v1alpha1.IPAddressFields{})).To(Succeed())
	g.Expect(validateAddressFields(v1alpha1.IPAddressFields{DNSName: "a/{{ .Owner }}"})).To(MatchError(ContainSubstring("not a valid DNS name")))
}
