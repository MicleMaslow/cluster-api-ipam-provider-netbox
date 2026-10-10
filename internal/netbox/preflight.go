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
)

// minVersion is the oldest NetBox the provider supports (conditional updates need ETag/If-Match).
var minVersion = utilversion.MustParseGeneric("4.6.0")

const customFields = "extras/custom-fields/"

// preflightBackoff is the wait between preflight attempts: it starts at Initial and doubles up to Max.
var preflightBackoff = struct{ Initial, Max time.Duration }{time.Second, time.Minute}

var requiredObjectTypes = []string{"ipam.prefix", "ipam.iprange", "ipam.ipaddress"}

// Preflight checks that NetBox is new enough and set up as the provider expects, then resolves the Owner.
// A setup the provider cannot work with is ErrMisconfigured; any other error may be transient.
func (c *Client) Preflight(ctx context.Context, ownerName string) (Owner, error) {
	var status struct {
		Version string `json:"netbox-version"`
	}
	if _, err := c.do(ctx, http.MethodGet, "status/", nil, "", nil, &status); err != nil {
		return Owner{}, err
	}
	v, err := utilversion.ParseGeneric(status.Version)
	if err != nil {
		return Owner{}, fmt.Errorf("%w: version %q: %w", ErrMisconfigured, status.Version, err)
	}
	if !v.AtLeast(minVersion) {
		return Owner{}, fmt.Errorf("%w: version %s: NetBox ≥ 4.6 required", ErrMisconfigured, status.Version)
	}
	refID, err := c.checkField(ctx, FieldRef)
	if err != nil {
		return Owner{}, err
	}
	if _, err := c.checkField(ctx, FieldCluster); err != nil {
		return Owner{}, err
	}
	// Owners have no last_updated and so no ETag; a custom field stands for the objects the provider updates.
	if etag, err := c.do(ctx, http.MethodGet, objectPath(customFields, refID), nil, "", nil, nil); err != nil {
		return Owner{}, fmt.Errorf("custom field %s: %w", FieldRef, err)
	} else if etag == "" {
		return Owner{}, fmt.Errorf("%w: NetBox answers without ETag (a proxy strips it?); conditional updates need it", ErrMisconfigured)
	}
	return c.LookupOwner(ctx, ownerName)
}

// WaitPreflight runs Preflight until it passes, fails with ErrMisconfigured or ctx ends.
// retry is called with the error and the wait before each new attempt.
func (c *Client) WaitPreflight(ctx context.Context, ownerName string, retry func(err error, after time.Duration)) (Owner, error) {
	wait := preflightBackoff.Initial
	for {
		o, err := c.Preflight(ctx, ownerName)
		if err == nil || errors.Is(err, ErrMisconfigured) {
			return o, err
		}
		if ctx.Err() != nil {
			return Owner{}, ctx.Err()
		}
		retry(err, wait)
		select {
		case <-ctx.Done():
			return Owner{}, ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, preflightBackoff.Max)
	}
}

func (c *Client) checkField(ctx context.Context, name string) (int64, error) {
	var page struct {
		Results []struct {
			ID          int64    `json:"id"`
			Type        Choice   `json:"type"`
			FilterLogic Choice   `json:"filter_logic"`
			Status      Choice   `json:"status"` // NetBox ≥ 4.7
			ObjectTypes []string `json:"object_types"`
		} `json:"results"`
	}
	if _, err := c.do(ctx, http.MethodGet, customFields, url.Values{"name": {name}}, "", nil, &page); err != nil {
		if errors.Is(err, ErrForbidden) {
			err = fmt.Errorf("%w (the token needs view on extras.customfield)", err)
		}
		return 0, fmt.Errorf("custom field %s: %w", name, err)
	}
	if len(page.Results) != 1 {
		return 0, fmt.Errorf("%w: custom field %s: not found", ErrMisconfigured, name)
	}
	f := page.Results[0]
	if f.Type.Value != "text" {
		return 0, fmt.Errorf("%w: custom field %s: type is %s, want text", ErrMisconfigured, name, f.Type.Value)
	}
	if f.FilterLogic.Value != "exact" {
		return 0, fmt.Errorf("%w: custom field %s: filter_logic is %s, want exact", ErrMisconfigured, name, f.FilterLogic.Value)
	}
	switch f.Status.Value {
	case "", "active":
	case "provisioning": // NetBox brings the field live by itself
		return 0, fmt.Errorf("custom field %s: status is provisioning, waiting for active", name)
	default:
		return 0, fmt.Errorf("%w: custom field %s: status is %s, want active", ErrMisconfigured, name, f.Status.Value)
	}
	var missing []string
	for _, t := range requiredObjectTypes {
		if !slices.Contains(f.ObjectTypes, t) {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return 0, fmt.Errorf("%w: custom field %s: not enabled for %s", ErrMisconfigured, name, strings.Join(missing, ", "))
	}
	return f.ID, nil
}
