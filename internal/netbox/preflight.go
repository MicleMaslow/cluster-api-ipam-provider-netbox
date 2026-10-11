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
	"time"

	utilversion "k8s.io/apimachinery/pkg/util/version"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// minVersion is the oldest NetBox the provider supports (conditional updates need ETag/If-Match).
var minVersion = utilversion.MustParseGeneric("4.6.0")

const customFields = "extras/custom-fields/"

// preflightBackoff is the wait between preflight attempts: it starts at Initial and doubles up to Max.
var preflightBackoff = struct{ Initial, Max time.Duration }{time.Second, time.Minute}

var requiredObjectTypes = []string{"ipam.prefix", "ipam.iprange", "ipam.ipaddress"}

// Preflight checks that NetBox is new enough and set up as the provider expects for k.
// What the token may not view is not checked: it is warned about.
// A setup the provider cannot work with is ErrMisconfigured; any other error may be transient.
func (c *Client) Preflight(ctx context.Context, k Keys) error {
	if err := k.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrMisconfigured, err)
	}
	var status struct {
		Version string `json:"netbox-version"`
	}
	if _, err := c.do(ctx, http.MethodGet, "status/", nil, "", nil, &status); err != nil {
		return err
	}
	v, err := utilversion.ParseGeneric(status.Version)
	if err != nil {
		return fmt.Errorf("%w: version %q: %w", ErrMisconfigured, status.Version, err)
	}
	if !v.AtLeast(minVersion) {
		return fmt.Errorf("%w: version %s: NetBox ≥ 4.6 required", ErrMisconfigured, status.Version)
	}
	return c.checkField(ctx, k.Ref)
}

// WaitPreflight runs Preflight until it passes, fails with ErrMisconfigured or ctx ends.
// retry is called with the error and the wait before each new attempt.
func (c *Client) WaitPreflight(ctx context.Context, k Keys, retry func(err error, after time.Duration)) error {
	wait := preflightBackoff.Initial
	for {
		err := c.Preflight(ctx, k)
		if err == nil || errors.Is(err, ErrMisconfigured) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		retry(err, wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, preflightBackoff.Max)
	}
}

// checkField checks the definition of the text custom field name; without the right to view it, by behavior.
// filter_logic loose is only warned about (lookups are re-checked on the client); disabled is ErrMisconfigured.
func (c *Client) checkField(ctx context.Context, name string) error {
	var page struct {
		Results []struct {
			Type        Choice   `json:"type"`
			FilterLogic Choice   `json:"filter_logic"`
			Status      Choice   `json:"status"` // NetBox ≥ 4.7
			ObjectTypes []string `json:"object_types"`
		} `json:"results"`
	}
	_, err := c.do(ctx, http.MethodGet, customFields, url.Values{"name": {name}}, "", nil, &page)
	if errors.Is(err, ErrForbidden) {
		log.FromContext(ctx).Info("WARNING: the token may not view custom field definitions (extras.customfield), "+name+" is not checked", "field", name)
		return c.probeFilter(ctx, name)
	}
	if err != nil {
		return fmt.Errorf("custom field %s: %w", name, err)
	}
	if len(page.Results) != 1 {
		return fmt.Errorf("%w: custom field %s: not found", ErrMisconfigured, name)
	}
	d := page.Results[0]
	if d.Type.Value != "text" {
		return fmt.Errorf("%w: custom field %s: type is %s, want text", ErrMisconfigured, name, d.Type.Value)
	}
	switch d.FilterLogic.Value {
	case "exact":
	case "disabled":
		return fmt.Errorf("%w: custom field %s: NetBox ignores cf_ filters with filter_logic disabled", ErrMisconfigured, name)
	default:
		log.FromContext(ctx).Info("WARNING: custom field "+name+": filter_logic is "+d.FilterLogic.Value+", not exact: lookups read more objects", "field", name)
	}
	switch d.Status.Value {
	case "", "active":
	case "provisioning": // NetBox brings the field live by itself
		return fmt.Errorf("custom field %s: status is provisioning, waiting for active", name)
	default:
		return fmt.Errorf("%w: custom field %s: status is %s, want active", ErrMisconfigured, name, d.Status.Value)
	}
	var missing []string
	for _, t := range requiredObjectTypes {
		if !slices.Contains(d.ObjectTypes, t) {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: custom field %s: not enabled for %s", ErrMisconfigured, name, strings.Join(missing, ", "))
	}
	return nil
}

// probeFilter asks for the prefixes with a ref nobody has: a custom field NetBox can filter by matches none.
// NetBox ignores a filter it does not know instead of failing, so that is only warned about.
func (c *Client) probeFilter(ctx context.Context, name string) error {
	var page struct {
		Count int `json:"count"`
	}
	q := url.Values{"cf_" + name: {"preflight-probe"}, "limit": {"1"}}
	_, err := c.do(ctx, http.MethodGet, Prefixes, q, "", nil, &page)
	switch {
	case errors.Is(err, ErrForbidden):
		return fmt.Errorf("custom field %s: %w (the token needs view on ipam.prefix)", name, err)
	case errors.Is(err, errBadRequest):
		return fmt.Errorf("%w: custom field %s: NetBox rejects the filter cf_%[2]s, the field is not text: %w", ErrMisconfigured, name, err)
	case err != nil:
		return fmt.Errorf("custom field %s: %w", name, err)
	}
	if page.Count > 0 {
		log.FromContext(ctx).Info("WARNING: NetBox ignores the filter cf_"+name+": the custom field is missing or not filterable, lookups read more objects", "field", name)
	}
	return nil
}
