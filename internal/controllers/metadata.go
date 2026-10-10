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
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

// mergeCustomFields returns the custom_fields patch that brings current in line with desired and
// the keys owned afterwards per JSON-object field (see NetBoxNetworkSpec.CustomFields).
// ponytail: values decode to float64, integers above 2^53 lose precision.
func mergeCustomFields(current map[string]json.RawMessage, desired map[string]apiextensionsv1.JSON, owned map[string][]string) (map[string]any, map[string][]string, error) {
	patch, newOwned := map[string]any{}, map[string][]string{}
	for field, v := range desired {
		var want any
		if err := json.Unmarshal(v.Raw, &want); err != nil {
			return nil, nil, fmt.Errorf("custom field %s: %w", field, err)
		}
		obj, isObj := want.(map[string]any)
		if !isObj {
			if !jsonEqual(current[field], want) {
				patch[field] = want
			}
			continue
		}
		merged, changed, err := mergeObject(current[field], obj, owned[field])
		if err != nil {
			return nil, nil, fmt.Errorf("custom field %s: %w", field, err)
		}
		if changed {
			patch[field] = merged
		}
		newOwned[field] = slices.Sorted(maps.Keys(obj))
	}
	for field, keys := range owned {
		if _, ok := desired[field]; ok {
			continue
		}
		merged, changed, err := mergeObject(current[field], nil, keys)
		if err != nil {
			return nil, nil, fmt.Errorf("custom field %s: %w", field, err)
		}
		if changed {
			patch[field] = merged
		}
	}
	return patch, newOwned, nil
}

// mergeObject writes want's keys into the JSON object cur and drops owned keys want no longer has.
func mergeObject(cur json.RawMessage, want map[string]any, owned []string) (map[string]any, bool, error) {
	obj := map[string]any{}
	if len(cur) > 0 && string(cur) != "null" {
		if err := json.Unmarshal(cur, &obj); err != nil {
			return nil, false, fmt.Errorf("current value is not a JSON object: %w", err)
		}
	}
	changed := false
	for _, k := range owned {
		if _, keep := want[k]; !keep {
			if _, ok := obj[k]; ok {
				delete(obj, k)
				changed = true
			}
		}
	}
	for k, v := range want {
		if old, ok := obj[k]; !ok || !reflect.DeepEqual(old, v) {
			obj[k] = v
			changed = true
		}
	}
	return obj, changed, nil
}

// jsonEqual reports whether the value NetBox returned is want, compared in the form want was written in.
func jsonEqual(raw json.RawMessage, want any) bool {
	var got any
	if len(raw) > 0 && json.Unmarshal(raw, &got) != nil {
		return false
	}
	return reflect.DeepEqual(writtenForm(got, want), want)
}

// writtenForm turns a value as NetBox returns it into the form it is written in, by the shape of want:
// an object reference {id,…} into its id, a choice {value,…} (select and multiselect in NetBox 4.7) into its value,
// a list element by element.
func writtenForm(got, want any) any {
	switch w := want.(type) {
	case float64:
		if m, ok := got.(map[string]any); ok && m["id"] != nil {
			return m["id"]
		}
	case string:
		if m, ok := got.(map[string]any); ok && m["value"] != nil {
			return m["value"]
		}
	case []any:
		l, ok := got.([]any)
		if !ok {
			return got
		}
		out, refs := make([]any, len(l)), len(l) > 0
		for i, e := range l {
			var we any
			if i < len(w) {
				we = w[i]
			}
			out[i] = writtenForm(e, we)
			m, isObj := e.(map[string]any)
			refs = refs && isObj && m["id"] != nil
		}
		// Multi-object values come back sorted by id.
		if refs && sameElements(out, w) {
			return want
		}
		return out
	}
	return got
}

func sameElements(a, b []any) bool {
	return len(a) == len(b) && !slices.ContainsFunc(a, func(x any) bool {
		return !slices.ContainsFunc(b, func(y any) bool { return reflect.DeepEqual(x, y) })
	})
}

// mergeTags returns the sorted tag slugs the prefix should carry: current ones minus those the provider
// set earlier (owned), plus desired. changed says whether that differs from current.
func mergeTags(current, desired, owned []string) ([]string, bool) {
	keep := map[string]bool{}
	for _, s := range current {
		keep[s] = true
	}
	for _, s := range owned {
		delete(keep, s)
	}
	for _, s := range desired {
		keep[s] = true
	}
	result := slices.AppendSeq([]string{}, maps.Keys(keep))
	slices.Sort(result)
	return result, !slices.Equal(result, slices.Sorted(slices.Values(current)))
}

func tagSlugs(tags []netbox.Tag) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		out = append(out, t.Slug)
	}
	return out
}

// tagRefs is the write form of tags in the NetBox API.
func tagRefs(slugs []string) []map[string]string {
	out := make([]map[string]string, 0, len(slugs))
	for _, s := range slugs {
		out = append(out, map[string]string{"slug": s})
	}
	return out
}
