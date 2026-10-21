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
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"text/template/parse"
	"unicode"

	"k8s.io/utils/ptr"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

// Default templates.
const (
	defaultDNSName            = "{{ .Owner }}"
	defaultAddressDescription = "{{ .Namespace }}/{{ .Owner }} // {{ .Cluster }} {{ .Pool }}"
	defaultPrefixDescription  = "{{ .Namespace }}/{{ .Name }} // cluster {{ .Cluster }}"
	defaultVIPDescription     = "{{ .Namespace }}/{{ .Name }} // {{ .Cluster }} control plane endpoint"
	defaultRangeDescription   = "{{ .Namespace }}/{{ .Pool }} // pool of cluster {{ .Cluster }}"
	defaultReserveDescription = "{{ .Namespace }}/{{ .Network }} // {{ .Cluster }} {{ .Group }} {{ .Index }}"
	defaultComments           = "// managed by cluster-api-ipam-provider-netbox, please don't edit it in NetBox unless you know what you're doing"
)

const (
	maxDNSName     = 255 // NetBox limits
	maxDescription = 200
	maxRendered    = 1024 // bounds every rendered value
)

var errTooLong = fmt.Errorf("longer than %d bytes", maxRendered)

// boundedFuncs replace the builtins that build strings, so that no value of a template grows past maxRendered.
var boundedFuncs = template.FuncMap{
	"print":    bounded(fmt.Sprint),
	"println":  bounded(fmt.Sprintln),
	"printf":   func(f string, a ...any) (string, error) { return limit(fmt.Sprintf(f, a...)) },
	"html":     bounded(template.HTMLEscaper),
	"js":       bounded(template.JSEscaper),
	"urlquery": bounded(template.URLQueryEscaper),
}

// dnsNamePattern is NetBox's DNSValidator.
var dnsNamePattern = regexp.MustCompile(`^([0-9A-Za-z_-]+|\*)(\.[0-9A-Za-z_-]+)*\.?$`)

// addressData are the variables of the address templates.
type addressData struct{ Owner, Claim, Cluster, Namespace, Pool string }

// networkData are the variables of the prefix and VIP templates.
type networkData struct{ Cluster, Namespace, Name string }

// rangeData are the variables of the range texts.
type rangeData struct{ Cluster, Namespace, Pool string }

// reserveData are the variables of the templates of an address group.
type reserveData struct {
	Cluster, Namespace, Network, Group string
	Index                              int
}

// texts are the description and comments of a NetBox object.
type texts struct{ Description, Comments string }

// on is a with the description and comments t.
func (t texts) on(a netbox.Attrs) netbox.Attrs {
	a.Description, a.Comments = t.Description, t.Comments
	return a
}

// ipFields are the NetBox fields of an address rendered from its pool's templates.
type ipFields struct {
	DNSName string
	texts
}

// addressFields renders the fields of the address claim holds in pool.
func addressFields(pool *v1alpha1.NetBoxIPPool, claim *ipamv1.IPAddressClaim) (ipFields, error) {
	return claimFields(pool.Spec.IPAddress, pool.Name, claim)
}

// claimFields renders t for the address claim holds in pool: .Owner is the first owner of the claim, else the claim.
func claimFields(t v1alpha1.IPAddressFields, pool string, claim *ipamv1.IPAddressClaim) (ipFields, error) {
	owner := claim.Name
	if len(claim.OwnerReferences) > 0 {
		owner = claim.OwnerReferences[0].Name
	}
	return renderFields(t, addressData{Owner: owner, Claim: claim.Name, Cluster: clusterOf(claim), Namespace: claim.Namespace, Pool: pool})
}

// networkTexts are the attributes of the prefix and the VIP of nw: the owner and tenant resolved in its status and
// the texts of its templates. A template that does not render, those of the VIP's dnsName and of the address groups
// included, is InvalidSpec.
func networkTexts(nw *v1alpha1.NetBoxNetwork) (prefix, vip netbox.Attrs, err error) {
	d := networkData{Cluster: nw.Spec.ClusterName, Namespace: nw.Namespace, Name: nw.Name}
	pt, err := renderTexts(nw.Spec.Description, nw.Spec.Comments, defaultPrefixDescription, d)
	if err != nil {
		return prefix, vip, withReason(v1alpha1.ReasonInvalidSpec, err)
	}
	ep := ptr.Deref(nw.Spec.ControlPlaneEndpoint, v1alpha1.ControlPlaneEndpoint{})
	vt, err := renderTexts(ep.Description, ep.Comments, defaultVIPDescription, d)
	if err != nil {
		return prefix, vip, withReason(v1alpha1.ReasonInvalidSpec, fmt.Errorf("controlPlaneEndpoint: %w", err))
	}
	if _, err := dnsNameOf(nw); err != nil {
		return prefix, vip, err
	}
	for _, g := range nw.Spec.Addresses {
		for i := 1; i <= int(g.Count); i++ {
			if _, err := groupFields(nw, g, i); err != nil {
				return prefix, vip, withReason(v1alpha1.ReasonInvalidSpec, fmt.Errorf("addresses %s: %w", g.Name, err))
			}
		}
	}
	return pt.on(networkAttrs(nw)), vt.on(networkAttrs(nw)), nil
}

// dnsNameOf is the dns_name of the VIP of nw; one that does not render is InvalidSpec.
func dnsNameOf(nw *v1alpha1.NetBoxNetwork) (string, error) {
	ep := ptr.Deref(nw.Spec.ControlPlaneEndpoint, v1alpha1.ControlPlaneEndpoint{})
	dns, err := renderDNSName(ep.DNSName, networkData{Cluster: nw.Spec.ClusterName, Namespace: nw.Namespace, Name: nw.Name})
	if err != nil {
		return "", withReason(v1alpha1.ReasonInvalidSpec, fmt.Errorf("controlPlaneEndpoint: %w", err))
	}
	return dns, nil
}

// groupFields renders the fields of address index of group g in nw.
func groupFields(nw *v1alpha1.NetBoxNetwork, g v1alpha1.AddressGroup, index int) (ipFields, error) {
	d := reserveData{Cluster: nw.Spec.ClusterName, Namespace: nw.Namespace, Network: nw.Name, Group: g.Name, Index: index}
	dns, err := renderDNSName(g.DNSName, d)
	if err != nil {
		return ipFields{}, err
	}
	tx, err := renderTexts(g.Description, "", defaultReserveDescription, d)
	return ipFields{DNSName: dns, texts: tx}, err
}

// rangeTexts are the texts of the ranges of pool in nw: the defaults, which always render.
func rangeTexts(nw *v1alpha1.NetBoxNetwork, pool string) texts {
	t, _ := renderTexts("", "", defaultRangeDescription, rangeData{Cluster: nw.Spec.ClusterName, Namespace: nw.Namespace, Pool: pool})
	return t
}

// validatePoolTemplates renders the templates of pool with sample values.
func validatePoolTemplates(pool *v1alpha1.NetBoxIPPool) error {
	if _, err := renderFields(pool.Spec.IPAddress, addressData{Owner: "owner", Claim: "claim", Cluster: "cluster", Namespace: "namespace", Pool: "pool"}); err != nil {
		return fmt.Errorf("ipAddress: %w", err)
	}
	return nil
}

func renderFields(t v1alpha1.IPAddressFields, d addressData) (ipFields, error) {
	dns, err := renderDNSName(cmp.Or(t.DNSName, defaultDNSName), d)
	if err != nil {
		return ipFields{}, err
	}
	tx, err := renderTexts(t.Description, t.Comments, defaultAddressDescription, d)
	if err != nil {
		return ipFields{}, err
	}
	return ipFields{DNSName: dns, texts: tx}, nil
}

// renderDNSName renders the dnsName template src on d, in lower case as NetBox stores it; "" renders "".
func renderDNSName(src string, d any) (string, error) {
	dns, err := render("dnsName", src, d)
	if err != nil {
		return "", err
	}
	if dns != "" && (len(dns) > maxDNSName || !dnsNamePattern.MatchString(dns)) {
		return "", fmt.Errorf("dnsName %q is not a valid DNS name", dns)
	}
	return strings.ToLower(dns), nil
}

// renderTexts renders the description and comments templates on d, "" taking the default.
// The description is cut to what NetBox allows; comments are not cut.
func renderTexts(description, comments, defaultDescription string, d any) (texts, error) {
	desc, err := render("description", cmp.Or(description, defaultDescription), d)
	if err != nil {
		return texts{}, err
	}
	if r := []rune(desc); len(r) > maxDescription {
		desc = trim(string(r[:maxDescription]))
	}
	comm, err := render("comments", cmp.Or(comments, defaultComments), d)
	if err != nil {
		return texts{}, err
	}
	return texts{Description: desc, Comments: comm}, nil
}

// render executes src on d. Without loops and calls of other templates, and with bounded values,
// it ends quickly and in little memory. A NUL in the result is an error: NetBox answers a permanent 400.
func render(name, src string, d any) (string, error) {
	t, err := template.New(name).Option("missingkey=error").Funcs(boundedFuncs).Parse(src)
	if err == nil && len(t.Templates()) > 1 {
		err = errors.New("define and block are not allowed")
	}
	if err == nil {
		err = checkNode(t.Root)
	}
	var w limitWriter
	if err == nil {
		err = t.Execute(&w, d)
	}
	if err == nil && strings.ContainsRune(w.String(), 0) {
		err = errors.New("contains a NUL character")
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return trim(w.String()), nil
}

// checkNode rejects the nodes that can run without bound.
func checkNode(n parse.Node) error {
	switch n := n.(type) {
	case *parse.ListNode:
		for _, c := range n.Nodes {
			if err := checkNode(c); err != nil {
				return err
			}
		}
	case *parse.IfNode:
		return checkBranch(n.BranchNode)
	case *parse.WithNode:
		return checkBranch(n.BranchNode)
	case *parse.RangeNode:
		return errors.New("range is not allowed")
	case *parse.TemplateNode:
		return errors.New("template is not allowed")
	}
	return nil
}

func checkBranch(b parse.BranchNode) error {
	if err := checkNode(b.List); err != nil || b.ElseList == nil {
		return err
	}
	return checkNode(b.ElseList)
}

// trim strips what NetBox strips (Python's str.strip) and replaces invalid UTF-8 bytes as encoding/json does,
// so that the result compares equal to the value NetBox stores.
func trim(s string) string {
	return strings.TrimFunc(string([]rune(s)), func(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f })
}

// limitWriter fails past maxRendered bytes.
type limitWriter struct{ strings.Builder }

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.Len()+len(p) > maxRendered {
		return 0, errTooLong
	}
	return w.Builder.Write(p)
}

func limit(s string) (string, error) {
	if len(s) > maxRendered {
		return "", errTooLong
	}
	return s, nil
}

func bounded(f func(...any) string) func(...any) (string, error) {
	return func(a ...any) (string, error) { return limit(f(a...)) }
}
