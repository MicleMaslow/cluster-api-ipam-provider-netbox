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

	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
)

const (
	defaultDNSName     = "{{ .Owner }}"
	defaultDescription = "{{ .Cluster }} {{ .Pool }} {{ .Owner }}"
	maxDNSName         = 255 // NetBox limits
	maxDescription     = 200
	maxRendered        = 1024 // bounds every rendered value
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

// addressData are the variables of the address field templates.
type addressData struct{ Owner, Claim, Cluster, Namespace, Pool string }

// ipFields are the NetBox fields of an address rendered from its pool's templates.
type ipFields struct{ DNSName, Description string }

// addressFields renders the fields of the address claim holds in pool.
func addressFields(pool *v1alpha1.NetBoxIPPool, claim *ipamv1.IPAddressClaim) (ipFields, error) {
	owner := claim.Name
	if len(claim.OwnerReferences) > 0 {
		owner = claim.OwnerReferences[0].Name
	}
	return renderFields(pool.Spec.IPAddress, addressData{Owner: owner, Claim: claim.Name, Cluster: clusterOf(claim), Namespace: claim.Namespace, Pool: pool.Name})
}

// validateAddressFields renders t with sample values.
func validateAddressFields(t v1alpha1.IPAddressFields) error {
	_, err := renderFields(t, addressData{Owner: "owner", Claim: "claim", Cluster: "cluster", Namespace: "namespace", Pool: "pool"})
	return err
}

func renderFields(t v1alpha1.IPAddressFields, d addressData) (ipFields, error) {
	dns, err := render("dnsName", cmp.Or(t.DNSName, defaultDNSName), d)
	if err != nil {
		return ipFields{}, err
	}
	if dns != "" && (len(dns) > maxDNSName || !dnsNamePattern.MatchString(dns)) {
		return ipFields{}, fmt.Errorf("dnsName %q is not a valid DNS name", dns)
	}
	dns = strings.ToLower(dns) // NetBox stores it lowercased
	desc, err := render("description", cmp.Or(t.Description, defaultDescription), d)
	if err != nil {
		return ipFields{}, err
	}
	if r := []rune(desc); len(r) > maxDescription {
		desc = trim(string(r[:maxDescription]))
	}
	return ipFields{DNSName: dns, Description: desc}, nil
}

// render executes src on d. Without loops and calls of other templates, and with bounded values,
// it ends quickly and in little memory.
func render(name, src string, d addressData) (string, error) {
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
