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
	"reflect"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

func TestMergeCustomFields(t *testing.T) {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	want := func(s string) apiextensionsv1.JSON { return apiextensionsv1.JSON{Raw: []byte(s)} }
	for name, tc := range map[string]struct {
		current   map[string]json.RawMessage
		desired   map[string]apiextensionsv1.JSON
		owned     map[string][]string
		wantPatch map[string]any
		wantOwned map[string][]string
	}{
		"object merged, others' keys kept": {
			current:   map[string]json.RawMessage{"labels": raw(`{"team":"x","role":"old"}`)},
			desired:   map[string]apiextensionsv1.JSON{"labels": want(`{"role":"lb","cluster":"c"}`)},
			wantPatch: map[string]any{"labels": map[string]any{"team": "x", "role": "lb", "cluster": "c"}},
			wantOwned: map[string][]string{"labels": {"cluster", "role"}},
		},
		"dropped key removed": {
			current:   map[string]json.RawMessage{"labels": raw(`{"team":"x","role":"lb","cluster":"c"}`)},
			desired:   map[string]apiextensionsv1.JSON{"labels": want(`{"cluster":"c"}`)},
			owned:     map[string][]string{"labels": {"cluster", "role"}},
			wantPatch: map[string]any{"labels": map[string]any{"team": "x", "cluster": "c"}},
			wantOwned: map[string][]string{"labels": {"cluster"}},
		},
		"field dropped from spec": {
			current:   map[string]json.RawMessage{"labels": raw(`{"team":"x","cluster":"c"}`)},
			owned:     map[string][]string{"labels": {"cluster"}},
			wantPatch: map[string]any{"labels": map[string]any{"team": "x"}},
			wantOwned: map[string][]string{},
		},
		"unchanged": {
			current:   map[string]json.RawMessage{"labels": raw(`{"cluster":"c"}`), "site": raw(`"a"`)},
			desired:   map[string]apiextensionsv1.JSON{"labels": want(`{"cluster":"c"}`), "site": want(`"a"`)},
			owned:     map[string][]string{"labels": {"cluster"}},
			wantPatch: map[string]any{},
			wantOwned: map[string][]string{"labels": {"cluster"}},
		},
		"scalar replaced": {
			current:   map[string]json.RawMessage{"site": raw(`"old"`), "rack": raw(`7`)},
			desired:   map[string]apiextensionsv1.JSON{"site": want(`"new"`), "rack": want(`null`)},
			wantPatch: map[string]any{"site": "new", "rack": nil},
			wantOwned: map[string][]string{},
		},
		"provider keys in the field kept": {
			current:   map[string]json.RawMessage{"labels": raw(`{"capi_ref":"NetBoxNetwork/ns/a","role":"old"}`)},
			desired:   map[string]apiextensionsv1.JSON{"labels": want(`{"team":"x"}`)},
			owned:     map[string][]string{"labels": {"role"}},
			wantPatch: map[string]any{"labels": map[string]any{"capi_ref": "NetBoxNetwork/ns/a", "team": "x"}},
			wantOwned: map[string][]string{"labels": {"team"}},
		},
		"provider keys kept when the field leaves the spec": {
			current:   map[string]json.RawMessage{"labels": raw(`{"capi_ref":"NetBoxNetwork/ns/a","team":"x"}`)},
			owned:     map[string][]string{"labels": {"team"}},
			wantPatch: map[string]any{"labels": map[string]any{"capi_ref": "NetBoxNetwork/ns/a"}},
			wantOwned: map[string][]string{},
		},
		"empty current object": {
			current:   map[string]json.RawMessage{"labels": raw(`null`)},
			desired:   map[string]apiextensionsv1.JSON{"labels": want(`{"cluster":"c"}`)},
			wantPatch: map[string]any{"labels": map[string]any{"cluster": "c"}},
			wantOwned: map[string][]string{"labels": {"cluster"}},
		},
	} {
		patch, owned, err := mergeCustomFields(tc.current, tc.desired, tc.owned)
		if err != nil || !reflect.DeepEqual(patch, tc.wantPatch) || !reflect.DeepEqual(owned, tc.wantOwned) {
			t.Errorf("%s: patch %v owned %v err %v; want %v %v", name, patch, owned, err, tc.wantPatch, tc.wantOwned)
		}
	}
	if _, _, err := mergeCustomFields(
		map[string]json.RawMessage{"labels": json.RawMessage(`[1]`)},
		map[string]apiextensionsv1.JSON{"labels": {Raw: []byte(`{"a":"b"}`)}}, nil); err == nil {
		t.Error("non-object current value: want error")
	}
}

func TestNormalizeCustomField(t *testing.T) {
	const (
		vlan5    = `{"id":5,"url":"http://nb/api/ipam/vlans/5/","display":"v5","vid":50,"name":"v5"}`
		vlan7    = `{"id":7,"url":"http://nb/api/ipam/vlans/7/","display":"v7","vid":70,"name":"v7"}`
		choiceA  = `{"value":"a","label":"A"}`
		choiceB  = `{"value":"b","label":"B"}`
		urlValue = `"https://example.com/a b?x=1#f"`
	)
	for name, tc := range map[string]struct {
		got, want string
		equal     bool
	}{
		"object":                {vlan5, `5`, true},
		"object, other id":      {vlan5, `6`, false},
		"object, wrong shape":   {vlan5, `"5"`, false},
		"object unset":          {``, `5`, false},
		"object cleared":        {`null`, `null`, true},
		"multi-object":          {`[` + vlan5 + `,` + vlan7 + `]`, `[5,7]`, true},
		"multi-object unsorted": {`[` + vlan5 + `,` + vlan7 + `]`, `[7,5]`, true},
		"multi-object, subset":  {`[` + vlan5 + `]`, `[5,7]`, false},
		"multi-object, other":   {`[` + vlan5 + `,` + vlan7 + `]`, `[5,8]`, false},
		"multi-object emptied":  {`[]`, `[]`, true},
		"select 4.7":            {choiceA, `"a"`, true},
		"select 4.7, other":     {choiceA, `"b"`, false},
		"select 4.6":            {`"a"`, `"a"`, true},
		"multiselect 4.7":       {`[` + choiceA + `,` + choiceB + `]`, `["a","b"]`, true},
		"multiselect, order":    {`[` + choiceA + `,` + choiceB + `]`, `["b","a"]`, false},
		"multiselect 4.6":       {`["a","b"]`, `["a","b"]`, true},
		"multiselect, longer":   {`[` + choiceA + `,` + choiceB + `]`, `["a"]`, false},
		"url":                   {urlValue, urlValue, true},
		"url, other":            {urlValue, `"https://example.com"`, false},
		"number":                {`5`, `5`, true},
		"json list kept":        {`[3,1]`, `[3,1]`, true},
		"json list, order":      {`[3,1]`, `[1,3]`, false},
	} {
		var want any
		if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := jsonEqual(json.RawMessage(tc.got), want); got != tc.equal {
			t.Errorf("%s: jsonEqual(%s, %s) = %v, want %v", name, tc.got, tc.want, got, tc.equal)
		}
	}
}

func TestMergeTags(t *testing.T) {
	for name, tc := range map[string]struct {
		current, desired, owned []string
		want                    []string
		changed                 bool
	}{
		"desired added":                   {current: []string{"x"}, desired: []string{"capi"}, want: []string{"capi", "x"}, changed: true},
		"owned but no longer desired out": {current: []string{"capi", "k8s", "x"}, desired: []string{"k8s"}, owned: []string{"capi", "k8s"}, want: []string{"k8s", "x"}, changed: true},
		"others' tags kept":               {current: []string{"k8s"}, owned: []string{"capi"}, want: []string{"k8s"}},
		"unchanged":                       {current: []string{"capi", "x"}, desired: []string{"capi"}, owned: []string{"capi"}, want: []string{"capi", "x"}},
		"unchanged, unsorted current":     {current: []string{"x", "capi"}, desired: []string{"capi"}, want: []string{"capi", "x"}},
		"result sorted":                   {current: []string{"z"}, desired: []string{"k8s", "capi"}, want: []string{"capi", "k8s", "z"}, changed: true},
		"owned already gone":              {current: []string{"x"}, owned: []string{"capi"}, want: []string{"x"}},
		"all removed":                     {current: []string{"capi"}, owned: []string{"capi"}, want: []string{}, changed: true},
		"nothing at all":                  {want: []string{}},
	} {
		got, changed := mergeTags(tc.current, tc.desired, tc.owned)
		if !reflect.DeepEqual(got, tc.want) || changed != tc.changed {
			t.Errorf("%s: got %v changed=%v; want %v changed=%v", name, got, changed, tc.want, tc.changed)
		}
	}
}

func TestSyncMetadataReservesKey(t *testing.T) {
	r := &NetBoxNetworkReconciler{Keys: netbox.DefaultKeys}
	nw := &v1alpha1.NetBoxNetwork{Spec: v1alpha1.NetBoxNetworkSpec{CustomFields: map[string]apiextensionsv1.JSON{
		"capi_ref": {Raw: []byte(`"x"`)}, "a": {Raw: []byte(`"x"`)},
	}}}
	if err := r.syncMetadata(ctx, nw, 1); errReason(err) != v1alpha1.ReasonInvalidSpec || err.Error() != "custom field capi_ref is managed by the provider" {
		t.Errorf("err %v, want InvalidSpec", err)
	}
}
