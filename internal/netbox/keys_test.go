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
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestKeyFormat(t *testing.T) {
	o := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "name"}}
	a := Keys{Instance: "a", Ref: "capi_ref"}
	for got, want := range map[string]string{
		DefaultKeys.Key("NetBoxNetwork", o):         "NetBoxNetwork/ns/name",
		a.Key("NetBoxNetwork", o):                   "a/NetBoxNetwork/ns/name",
		DefaultKeys.Key("NetBoxNetwork", o, "vip"):  "NetBoxNetwork/ns/name/vip",
		a.Key("NetBoxNetwork", o, "vip", "retired"): "a/NetBoxNetwork/ns/name/vip/retired",
		a.Key("NetBoxIPPool", o, "net1"):            "a/NetBoxIPPool/ns/name/net1",
		DefaultKeys.Key("NetBoxIPPool", o, ""):      "NetBoxIPPool/ns/name/",
		DefaultKeys.KindPrefix("IPAddressClaim"):    "IPAddressClaim/",
		a.KindPrefix("IPAddressClaim"):              "a/IPAddressClaim/",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestKeysRefOf(t *testing.T) {
	for cf, want := range map[string]string{
		`{"capi_ref":"x/a"}`: "x/a",
		`{"capi_ref":null}`:  "",
		`{}`:                 "",
		`{"capi_ref":["a"]}`: "",
	} {
		var obj Object
		if err := json.Unmarshal([]byte(`{"custom_fields":`+cf+`}`), &obj); err != nil {
			t.Fatal(err)
		}
		if got := DefaultKeys.RefOf(obj); got != want {
			t.Errorf("%s: RefOf = %q, want %q", cf, got, want)
		}
	}
}

func TestKeysValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		k    Keys
		want string // empty: valid
	}{
		"default":        {k: DefaultKeys},
		"instance":       {k: Keys{Instance: "zone-1", Ref: "capi_ref"}},
		"instance 63":    {k: Keys{Instance: strings.Repeat("a", 63), Ref: "capi_ref"}},
		"no ref":         {k: Keys{}, want: "ref field"},
		"ref with dot":   {k: Keys{Ref: "a.b"}, want: "ref field"},
		"camel case":     {k: Keys{Ref: "netboxOperatorRestorationHash"}},
		"ref double _":   {k: Keys{Ref: "a__b"}, want: "ref field"},
		"instance case":  {k: Keys{Instance: "A_b", Ref: "capi_ref"}, want: `instance name "A_b"`},
		"instance slash": {k: Keys{Instance: "a/b", Ref: "capi_ref"}, want: "instance name"},
		"instance 64":    {k: Keys{Instance: strings.Repeat("a", 64), Ref: "capi_ref"}, want: "instance name"},
	} {
		err := tc.k.Validate()
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: Validate = %v, want %q", name, err, tc.want)
		}
	}
}

func TestFindByRef(t *testing.T) {
	ids := func(objs []Object) []int64 {
		out := []int64{}
		for _, o := range objs {
			out = append(out, o.ID)
		}
		return out
	}
	c, rec := newServer(t, 200, `{"results":[
		{"id":1,"owner":{"id":9},"custom_fields":{"capi_ref":"X"}},
		{"id":2,"custom_fields":{"capi_ref":"X2"}},
		{"id":3,"custom_fields":{"capi_ref":"a/X"}},
		{"id":4,"custom_fields":{"capi_ref":"x"}},
		{"id":5,"custom_fields":{"capi_ref":null}},
		{"id":6,"custom_fields":{"capi_ref":"X/vip"}}]}`)

	got, err := c.FindByRef(ctx, DefaultKeys, IPAddresses, "X")
	q, _ := url.ParseQuery(rec.query)
	if err != nil || !slices.Equal(ids(got), []int64{1}) || rec.path != "/api/"+IPAddresses || q.Get("cf_capi_ref") != "X" ||
		q.Has("owner_id") || q.Has("parent") || q.Has("within_include") {
		t.Errorf("FindByRef X: got %v, err %v, request %s?%s", ids(got), err, rec.path, rec.query)
	}
	if got, err = c.FindByRef(ctx, DefaultKeys, IPAddresses, "a/X"); err != nil || !slices.Equal(ids(got), []int64{3}) {
		t.Errorf("FindByRef a/X: got %v, err %v", ids(got), err)
	}

	got, err = c.FindByRefPrefix(ctx, DefaultKeys, IPRanges, "X/")
	q, _ = url.ParseQuery(rec.query)
	if err != nil || !slices.Equal(ids(got), []int64{6}) || q.Get("cf_capi_ref__isw") != "X/" || q.Has("cf_capi_ref") || q.Has("owner_id") {
		t.Errorf("FindByRefPrefix: got %v, err %v, query %s", ids(got), err, rec.query)
	}
}

func TestManagedAttrs(t *testing.T) {
	const msg, keys = `"changelog_message":"msg"`, `"custom_fields":{"capi_ref":"NetBoxNetwork/ns/a"}`
	for name, tc := range map[string]struct {
		a    Attrs
		want string
	}{
		"none":        {Attrs{}, `{` + msg + `,` + keys + `,"status":"active"}`},
		"owner":       {Attrs{Owner: 3}, `{` + msg + `,` + keys + `,"owner":3,"status":"active"}`},
		"tenant":      {Attrs{Tenant: 4}, `{` + msg + `,` + keys + `,"status":"active","tenant":4}`},
		"description": {Attrs{Description: "d"}, `{` + msg + `,` + keys + `,"description":"d","status":"active"}`},
		"comments":    {Attrs{Comments: "c"}, `{` + msg + `,"comments":"c",` + keys + `,"status":"active"}`},
		"all": {Attrs{Owner: 3, Tenant: 4, Description: "d", Comments: "c"},
			`{` + msg + `,"comments":"c",` + keys + `,"description":"d","owner":3,"status":"active","tenant":4}`},
	} {
		b, err := json.Marshal(DefaultKeys.Managed(tc.a, "NetBoxNetwork/ns/a", "msg", map[string]any{"status": "active"}))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.want {
			t.Errorf("%s: Managed = %s, want %s", name, b, tc.want)
		}
	}
}

// Changes are the attributes to write: those of the spec the object lacks. An unset owner or tenant is never written
// nor cleared; description and comments are always brought to the given values, empty ones too.
func TestAttrsChanges(t *testing.T) {
	for name, tc := range map[string]struct {
		a    Attrs
		o    Object
		want string
	}{
		"unset":          {Attrs{}, Object{Owner: &Ref{ID: 3}, Tenant: &Ref{ID: 4}}, `{}`},
		"in sync":        {Attrs{Owner: 3, Tenant: 4}, Object{Owner: &Ref{ID: 3}, Tenant: &Ref{ID: 4}}, `{}`},
		"none on object": {Attrs{Owner: 3, Tenant: 4}, Object{}, `{"owner":3,"tenant":4}`},
		"other owner":    {Attrs{Owner: 3, Tenant: 4}, Object{Owner: &Ref{ID: 5}, Tenant: &Ref{ID: 4}}, `{"owner":3}`},
		"tenant only":    {Attrs{Tenant: 4}, Object{Owner: &Ref{ID: 5}, Tenant: &Ref{ID: 6}}, `{"tenant":4}`},
		"texts in sync":  {Attrs{Description: "d", Comments: "c"}, Object{Description: "d", Comments: "c"}, `{}`},
		"texts":          {Attrs{Owner: 3, Description: "d", Comments: "c"}, Object{Owner: &Ref{ID: 3}, Description: "x"}, `{"comments":"c","description":"d"}`},
		"texts cleared":  {Attrs{}, Object{Description: "d", Comments: "c"}, `{"comments":"","description":""}`},
	} {
		b, err := json.Marshal(tc.a.Changes(tc.o))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.want {
			t.Errorf("%s: Changes = %s, want %s", name, b, tc.want)
		}
	}
}
