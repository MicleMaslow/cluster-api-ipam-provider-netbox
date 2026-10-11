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
	"fmt"
	"net/url"
	"strconv"
)

// LookupOwner returns the ID of the owner named name; ErrNotFound if there is none.
func (c *Client) LookupOwner(ctx context.Context, name string) (int64, error) {
	return c.lookupOne(ctx, Owners, "owner", "name", name)
}

// LookupTenant returns the ID of the tenant with slug; ErrNotFound if there is none.
func (c *Client) LookupTenant(ctx context.Context, slug string) (int64, error) {
	return c.lookupOne(ctx, Tenants, "tenant", "slug", slug)
}

func (c *Client) lookupOne(ctx context.Context, path, what, filter, value string) (int64, error) {
	objs, err := c.List(ctx, path, url.Values{filter: {value}})
	switch {
	case err != nil:
		return 0, err
	case len(objs) == 0:
		return 0, fmt.Errorf("%w: %s %q", ErrNotFound, what, value)
	case len(objs) > 1:
		return 0, fmt.Errorf("%w: %s %q: found %d, want one", ErrMisconfigured, what, value, len(objs))
	}
	return objs[0].ID, nil
}

// CheckOwner returns ErrNotFound if NetBox has no owner id.
func (c *Client) CheckOwner(ctx context.Context, id int64) error {
	return c.exists(ctx, Owners, "owner", id)
}

// CheckTenant returns ErrNotFound if NetBox has no tenant id.
func (c *Client) CheckTenant(ctx context.Context, id int64) error {
	return c.exists(ctx, Tenants, "tenant", id)
}

// exists checks that the object id is there; ErrNotFound if not.
func (c *Client) exists(ctx context.Context, path, what string, id int64) error {
	objs, err := c.List(ctx, path, url.Values{"id": {strconv.FormatInt(id, 10)}})
	if err == nil && len(objs) == 0 {
		err = fmt.Errorf("%w: %s %d", ErrNotFound, what, id)
	}
	return err
}
