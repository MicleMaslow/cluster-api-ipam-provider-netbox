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

package netbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	utilversion "k8s.io/apimachinery/pkg/util/version"
)

// minVersion is the oldest NetBox the provider supports (conditional updates need ETag/If-Match).
var minVersion = utilversion.MustParseGeneric("4.6.0")

var requiredObjectTypes = []string{"ipam.prefix", "ipam.iprange", "ipam.ipaddress"}

// Preflight checks that NetBox is new enough and set up as the provider expects, then resolves the Owner.
func (c *Client) Preflight(ctx context.Context, ownerName string) (Owner, error) {
	var status struct {
		Version string `json:"netbox-version"`
	}
	if _, err := c.do(ctx, http.MethodGet, "status/", nil, "", nil, &status); err != nil {
		return Owner{}, err
	}
	v, err := utilversion.ParseGeneric(status.Version)
	if err != nil {
		return Owner{}, fmt.Errorf("netbox: version %q: %w", status.Version, err)
	}
	if !v.AtLeast(minVersion) {
		return Owner{}, fmt.Errorf("netbox: version %s: NetBox ≥ 4.6 required", status.Version)
	}
	for _, name := range []string{FieldRef, FieldCluster} {
		if err := c.checkField(ctx, name); err != nil {
			return Owner{}, err
		}
	}
	return c.LookupOwner(ctx, ownerName)
}

func (c *Client) checkField(ctx context.Context, name string) error {
	var page struct {
		Results []struct {
			Type        Choice   `json:"type"`
			FilterLogic Choice   `json:"filter_logic"`
			ObjectTypes []string `json:"object_types"`
		} `json:"results"`
	}
	if _, err := c.do(ctx, http.MethodGet, "extras/custom-fields/", url.Values{"name": {name}}, "", nil, &page); err != nil {
		if errors.Is(err, ErrForbidden) {
			err = fmt.Errorf("%w (the token needs view on extras.customfield)", err)
		}
		return fmt.Errorf("custom field %s: %w", name, err)
	}
	if len(page.Results) != 1 {
		return fmt.Errorf("custom field %s: not found", name)
	}
	f := page.Results[0]
	if f.Type.Value != "text" {
		return fmt.Errorf("custom field %s: type is %s, want text", name, f.Type.Value)
	}
	if f.FilterLogic.Value != "exact" {
		return fmt.Errorf("custom field %s: filter_logic is %s, want exact", name, f.FilterLogic.Value)
	}
	var missing []string
	for _, t := range requiredObjectTypes {
		if !slices.Contains(f.ObjectTypes, t) {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("custom field %s: not enabled for %s", name, strings.Join(missing, ", "))
	}
	return nil
}
