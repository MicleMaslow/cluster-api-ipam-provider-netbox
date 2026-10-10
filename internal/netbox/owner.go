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
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Owner is the NetBox Owner that marks objects managed by this provider.
type Owner struct {
	ID int64
}

// LookupOwner resolves an Owner by name.
func (c *Client) LookupOwner(ctx context.Context, name string) (Owner, error) {
	objs, err := c.List(ctx, Owners, url.Values{"name": {name}})
	if err != nil {
		return Owner{}, err
	}
	if len(objs) != 1 {
		return Owner{}, fmt.Errorf("%w: want one owner %q, found %d", ErrMisconfigured, name, len(objs))
	}
	return Owner{ID: objs[0].ID}, nil
}

// Managed returns extra plus the fields every object written by the provider carries.
func (o Owner) Managed(ref, cluster, message string, extra map[string]any) map[string]any {
	f := map[string]any{
		"owner":             o.ID,
		"custom_fields":     map[string]any{FieldRef: ref, FieldCluster: cluster},
		"changelog_message": message,
	}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

// ByRef is the query for the object this provider manages under ref.
func (o Owner) ByRef(ref string) url.Values {
	return url.Values{"cf_" + FieldRef: {ref}, "owner_id": {strconv.FormatInt(o.ID, 10)}}
}

// Owns reports whether obj carries this owner.
func (o Owner) Owns(obj Object) bool { return obj.Owner != nil && obj.Owner.ID == o.ID }

// Ref is the ref obj is managed under, empty if none.
func (obj Object) Ref() string {
	var ref string
	_ = json.Unmarshal(obj.CustomFields[FieldRef], &ref)
	return ref
}

// FindByRef returns the objects at path that o manages under exactly ref.
// NetBox matches text custom fields with icontains unless the field's filter_logic is exact, so the result is re-checked.
func (c *Client) FindByRef(ctx context.Context, path string, o Owner, ref string) ([]Object, error) {
	return c.findRef(ctx, path, o.ByRef(ref), func(got string) bool { return got == ref })
}

// FindAnyByRef returns the objects at path under exactly ref, whatever their owner.
func (c *Client) FindAnyByRef(ctx context.Context, path, ref string) ([]Object, error) {
	return c.findRef(ctx, path, url.Values{"cf_" + FieldRef: {ref}}, func(got string) bool { return got == ref })
}

// FindByRefPrefix returns the objects at path that o manages under a ref starting with prefix.
// NetBox matches the prefix ignoring case, so the result is re-checked.
func (c *Client) FindByRefPrefix(ctx context.Context, path string, o Owner, prefix string) ([]Object, error) {
	q := url.Values{"cf_" + FieldRef + "__isw": {prefix}, "owner_id": {strconv.FormatInt(o.ID, 10)}}
	return c.findRef(ctx, path, q, func(got string) bool { return strings.HasPrefix(got, prefix) })
}

func (c *Client) findRef(ctx context.Context, path string, q url.Values, match func(ref string) bool) ([]Object, error) {
	objs, err := c.List(ctx, path, q)
	return slices.DeleteFunc(objs, func(obj Object) bool { return !match(obj.Ref()) }), err
}
