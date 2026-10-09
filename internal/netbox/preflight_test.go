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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// preflightServer serves a NetBox that passes the preflight; tweak edits the custom field named by its first argument.
func preflightServer(t *testing.T, nbVersion, owners string, tweak func(name string, f map[string]any)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status/":
			_, _ = io.WriteString(w, `{"netbox-version":"`+nbVersion+`"}`)
		case "/api/extras/custom-fields/":
			name := r.URL.Query().Get("name")
			f := map[string]any{
				"name":         name,
				"type":         map[string]any{"value": "text"},
				"filter_logic": map[string]any{"value": "exact"},
				"object_types": []string{"ipam.prefix", "ipam.iprange", "ipam.ipaddress"},
			}
			if tweak != nil {
				tweak(name, f)
			}
			results := []any{f}
			if len(f) == 0 {
				results = nil
			}
			b, _ := json.Marshal(map[string]any{"results": results})
			_, _ = w.Write(b)
		case "/api/users/owners/":
			_, _ = io.WriteString(w, owners)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, Token: "nbt_k.t"}
}

func TestPreflight(t *testing.T) {
	const owner = `{"results":[{"id":4}]}`
	for _, tc := range []struct {
		name    string
		version string
		owners  string
		tweak   func(string, map[string]any)
		want    string // empty: success
	}{
		{name: "ok", version: "4.6.7", owners: owner},
		{name: "newer with suffix", version: "4.7.0-beta1", owners: owner},
		{name: "old NetBox", version: "4.5.9", owners: owner, want: "NetBox ≥ 4.6 required"},
		{name: "unparsable version", version: "latest", owners: owner, want: "latest"},
		{name: "loose filter logic", version: "4.6.7", owners: owner, want: "capi_ref: filter_logic is loose",
			tweak: func(n string, f map[string]any) {
				if n == FieldRef {
					f["filter_logic"] = map[string]any{"value": "loose"}
				}
			}},
		{name: "wrong type", version: "4.6.7", owners: owner, want: "capi_cluster: type is integer",
			tweak: func(n string, f map[string]any) {
				if n == FieldCluster {
					f["type"] = map[string]any{"value": "integer"}
				}
			}},
		{name: "no iprange", version: "4.6.7", owners: owner, want: "capi_cluster: not enabled for ipam.iprange",
			tweak: func(n string, f map[string]any) {
				if n == FieldCluster {
					f["object_types"] = []string{"ipam.prefix", "ipam.ipaddress"}
				}
			}},
		{name: "field missing", version: "4.6.7", owners: owner, want: "capi_ref: not found",
			tweak: func(n string, f map[string]any) {
				if n == FieldRef {
					clear(f)
				}
			}},
		{name: "owner missing", version: "4.6.7", owners: `{"results":[]}`, want: `owner "capi-test"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := preflightServer(t, tc.version, tc.owners, tc.tweak)
			o, err := c.Preflight(ctx, "capi-test")
			if tc.want == "" {
				if err != nil || o.ID != 4 {
					t.Errorf("owner %+v, err %v", o, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want error with %q", err, tc.want)
			}
		})
	}
}

func TestPreflightForbiddenHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status/" {
			_, _ = io.WriteString(w, `{"netbox-version":"4.6.7"}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	_, err := (&Client{URL: srv.URL, Token: "t"}).Preflight(ctx, "o")
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "extras.customfield") {
		t.Errorf("got %v", err)
	}
}
