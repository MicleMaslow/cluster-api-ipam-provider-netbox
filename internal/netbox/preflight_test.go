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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// preflightServer serves a NetBox that passes the preflight; tweak edits the custom field named by its first argument.
// The custom field answers with etag, none if it is empty, and with a body no Object decodes from.
func preflightServer(t *testing.T, nbVersion, owners, etag string, tweak func(name string, f map[string]any)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status/":
			_, _ = io.WriteString(w, `{"netbox-version":"`+nbVersion+`"}`)
		case "/api/extras/custom-fields/":
			name := r.URL.Query().Get("name")
			f := map[string]any{
				"id":           11,
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
		case "/api/extras/custom-fields/11/":
			if etag != "" {
				w.Header().Set("ETag", etag)
			}
			_, _ = io.WriteString(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, Token: "nbt_k.t"}
}

func TestPreflight(t *testing.T) {
	const owner = `{"results":[{"id":4}]}`
	const etag = `W/"o1"`
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
			c := preflightServer(t, tc.version, tc.owners, etag, tc.tweak)
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

func TestPreflightFieldStatus(t *testing.T) {
	const owner = `{"results":[{"id":4}]}`
	status := func(v string) func(string, map[string]any) {
		return func(n string, f map[string]any) {
			if n == FieldCluster && v != "" {
				f["status"] = map[string]any{"value": v, "label": v}
			}
		}
	}
	for _, tc := range []struct {
		name, status  string
		want          string // empty: success
		misconfigured bool   // otherwise the error is transient
	}{
		{name: "NetBox 4.6 has no status"},
		{name: "active", status: "active"},
		{name: "provisioning is transient", status: "provisioning", want: "capi_cluster: status is provisioning"},
		{name: "deleting", status: "deleting", want: "capi_cluster: status is deleting, want active", misconfigured: true},
		{name: "unknown", status: "archived", want: "capi_cluster: status is archived, want active", misconfigured: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := preflightServer(t, "4.7.2", owner, `W/"o1"`, status(tc.status)).Preflight(ctx, "capi-test")
			if tc.want == "" {
				if err != nil {
					t.Errorf("got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || errors.Is(err, ErrMisconfigured) != tc.misconfigured {
				t.Errorf("got %v, want error with %q, ErrMisconfigured=%v", err, tc.want, tc.misconfigured)
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

func TestPreflightNoETag(t *testing.T) {
	c := preflightServer(t, "4.6.7", `{"results":[{"id":4}]}`, "", nil)
	_, err := c.Preflight(ctx, "capi-test")
	if !errors.Is(err, ErrMisconfigured) || !strings.Contains(err.Error(), "NetBox answers without ETag") {
		t.Errorf("got %v, want ErrMisconfigured about the ETag", err)
	}
}

func TestPreflightClassifies(t *testing.T) {
	const owner = `{"results":[{"id":4}]}`
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(unavailable.Close)

	for _, tc := range []struct {
		name          string
		c             *Client
		misconfigured bool
	}{
		{"old version", preflightServer(t, "4.5.9", owner, `W/"o1"`, nil), true},
		{"unparsable version", preflightServer(t, "latest", owner, `W/"o1"`, nil), true},
		{"field is not text", preflightServer(t, "4.6.7", owner, `W/"o1"`, func(_ string, f map[string]any) {
			f["type"] = map[string]any{"value": "integer"}
		}), true},
		{"field is missing", preflightServer(t, "4.6.7", owner, `W/"o1"`, func(_ string, f map[string]any) { clear(f) }), true},
		{"no owner", preflightServer(t, "4.6.7", `{"results":[]}`, `W/"o1"`, nil), true},
		{"no ETag", preflightServer(t, "4.6.7", owner, "", nil), true},
		{"transport error", &Client{URL: closed.URL, Token: "t"}, false},
		{"HTTP 503", &Client{URL: unavailable.URL, Token: "t"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.c.Preflight(ctx, "capi-test")
			if err == nil {
				t.Fatal("want an error")
			}
			if got := errors.Is(err, ErrMisconfigured); got != tc.misconfigured {
				t.Errorf("errors.Is(ErrMisconfigured) = %v, want %v: %v", got, tc.misconfigured, err)
			}
		})
	}
}

func fastBackoff(t *testing.T, initial, limit time.Duration) {
	t.Helper()
	old := preflightBackoff
	preflightBackoff.Initial, preflightBackoff.Max = initial, limit
	t.Cleanup(func() { preflightBackoff = old })
}

func TestWaitPreflightRetries(t *testing.T) {
	fastBackoff(t, time.Millisecond, 4*time.Millisecond)
	var down atomic.Int32
	down.Store(2)
	c := preflightServer(t, "4.6.7", `{"results":[{"id":4}]}`, `W/"o1"`, nil)
	target, _ := url.Parse(c.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status/" && down.Add(-1) >= 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(flaky.Close)
	c.URL = flaky.URL

	var delays []time.Duration
	o, err := c.WaitPreflight(ctx, "capi-test", func(err error, after time.Duration) {
		if err == nil {
			t.Error("retry called without an error")
		}
		delays = append(delays, after)
	})
	if err != nil || o.ID != 4 {
		t.Fatalf("owner %+v, err %v", o, err)
	}
	if len(delays) != 2 || delays[0] != time.Millisecond || delays[1] != 2*time.Millisecond {
		t.Errorf("retry delays %v, want [1ms 2ms]", delays)
	}
}

func TestWaitPreflightStopsOnMisconfig(t *testing.T) {
	fastBackoff(t, time.Millisecond, time.Millisecond)
	c := preflightServer(t, "4.5.9", `{"results":[{"id":4}]}`, `W/"o1"`, nil)
	_, err := c.WaitPreflight(ctx, "capi-test", func(error, time.Duration) { t.Error("retry called on a misconfigured NetBox") })
	if !errors.Is(err, ErrMisconfigured) {
		t.Errorf("got %v, want ErrMisconfigured", err)
	}
}

func TestWaitPreflightCtx(t *testing.T) {
	fastBackoff(t, time.Hour, time.Hour)
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(unavailable.Close)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := &Client{URL: unavailable.URL, Token: "t"}
	_, err := c.WaitPreflight(cctx, "capi-test", func(error, time.Duration) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}
