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
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Keys says where the provider keeps its key on NetBox objects.
// An object is ours when its ref field holds exactly our key.
type Keys struct {
	Instance string // management-zone prefix of every key, "" for none
	Ref      string // text custom field of the key
}

// DefaultKeys are capi_ref, without an instance.
var DefaultKeys = Keys{Ref: "capi_ref"}

// fieldName is NetBox's rule for custom field names, with no "__".
var fieldName = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func validField(name string) bool {
	return fieldName.MatchString(name) && !strings.Contains(name, "__")
}

// Validate rejects keys that cannot work.
func (k Keys) Validate() error {
	if !validField(k.Ref) {
		return fmt.Errorf("ref field %q: name must match %s without __", k.Ref, fieldName)
	}
	if errs := validation.IsDNS1123Label(k.Instance); k.Instance != "" && len(errs) > 0 {
		return fmt.Errorf("instance name %q: %s", k.Instance, strings.Join(errs, "; "))
	}
	return nil
}

// Key is the key of o in NetBox: [<instance>/]<kind>/<namespace>/<name>[/<suffix>...].
// Unlike the UID it survives clusterctl move.
func (k Keys) Key(kind string, o client.Object, suffix ...string) string {
	return k.KindPrefix(kind) + strings.Join(append([]string{o.GetNamespace(), o.GetName()}, suffix...), "/")
}

// KindPrefix starts every key of kind.
func (k Keys) KindPrefix(kind string) string {
	if k.Instance == "" {
		return kind + "/"
	}
	return k.Instance + "/" + kind + "/"
}

// RefOf is the key obj carries, "" if none.
func (k Keys) RefOf(obj Object) string {
	var ref string
	_ = json.Unmarshal(obj.CustomFields[k.Ref], &ref)
	return ref
}

// Attrs are the plain attributes of the objects the provider writes. A zero owner or tenant is unset: never written
// nor cleared. Empty description and comments are not written on create (NetBox's default), but Changes clears them.
type Attrs struct {
	Owner, Tenant         int64
	Description, Comments string
}

// Managed returns extra plus the fields every object written by the provider carries.
func (k Keys) Managed(a Attrs, ref, message string, extra map[string]any) map[string]any {
	f := map[string]any{"custom_fields": map[string]any{k.Ref: ref}, "changelog_message": message}
	if a.Owner != 0 {
		f["owner"] = a.Owner
	}
	if a.Tenant != 0 {
		f["tenant"] = a.Tenant
	}
	if a.Description != "" {
		f["description"] = a.Description
	}
	if a.Comments != "" {
		f["comments"] = a.Comments
	}
	maps.Copy(f, extra)
	return f
}

// Changes are the attributes of a that o does not carry: the fields that bring o to the spec.
func (a Attrs) Changes(o Object) map[string]any {
	f := map[string]any{}
	if a.Owner != 0 && (o.Owner == nil || o.Owner.ID != a.Owner) {
		f["owner"] = a.Owner
	}
	if a.Tenant != 0 && (o.Tenant == nil || o.Tenant.ID != a.Tenant) {
		f["tenant"] = a.Tenant
	}
	if a.Description != o.Description {
		f["description"] = a.Description
	}
	if a.Comments != o.Comments {
		f["comments"] = a.Comments
	}
	return f
}

// FindByRef returns the objects at path under exactly ref.
// NetBox matches text custom fields with icontains unless the field's filter_logic is exact, so the result is re-checked.
func (c *Client) FindByRef(ctx context.Context, k Keys, path, ref string) ([]Object, error) {
	return c.findRef(ctx, k, path, url.Values{"cf_" + k.Ref: {ref}}, func(got string) bool { return got == ref })
}

// FindByRefPrefix returns the objects at path under a ref starting with prefix.
// NetBox matches the prefix ignoring case, so the result is re-checked.
func (c *Client) FindByRefPrefix(ctx context.Context, k Keys, path, prefix string) ([]Object, error) {
	return c.findRef(ctx, k, path, url.Values{"cf_" + k.Ref + "__isw": {prefix}}, func(got string) bool { return strings.HasPrefix(got, prefix) })
}

func (c *Client) findRef(ctx context.Context, k Keys, path string, q url.Values, match func(ref string) bool) ([]Object, error) {
	objs, err := c.List(ctx, path, q)
	return slices.DeleteFunc(objs, func(obj Object) bool { return !match(k.RefOf(obj)) }), err
}
