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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// fakeNetBox is a NetBox that passes the preflight; the fields tune what it answers.
type fakeNetBox struct {
	version string // 4.6.7 if empty
	// tweak edits the definition of the custom field.
	tweak         func(f map[string]any)
	forbidden     map[string]bool // API paths that answer 403
	filterIgnored bool            // prefixes ignore the cf_ filter, as NetBox does for a field it does not know
	filterInvalid bool            // prefixes answer 400 to the cf_ filter, as NetBox does for a value a non-text field rejects

	mu   sync.Mutex
	seen []string // path?query of the requests
}

func (n *fakeNetBox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	api := strings.TrimPrefix(r.URL.Path, "/api/")
	n.mu.Lock()
	n.seen = append(n.seen, api+"?"+r.URL.RawQuery)
	n.mu.Unlock()
	if n.forbidden[api] {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch api {
	case "status/":
		v := n.version
		if v == "" {
			v = "4.6.7"
		}
		_, _ = io.WriteString(w, `{"netbox-version":"`+v+`"}`)
	case "extras/custom-fields/":
		name := r.URL.Query().Get("name")
		f := map[string]any{
			"id":           11,
			"name":         name,
			"type":         map[string]any{"value": "text"},
			"filter_logic": map[string]any{"value": "exact"},
			"object_types": []string{"ipam.prefix", "ipam.iprange", "ipam.ipaddress"},
		}
		if n.tweak != nil {
			n.tweak(f)
		}
		results := []any{f}
		if len(f) == 0 {
			results = nil
		}
		b, err := json.Marshal(map[string]any{"results": results})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(b)
	case "ipam/prefixes/":
		if n.filterInvalid {
			http.Error(w, `{"cf_capi_ref":["Enter a whole number."]}`, http.StatusBadRequest)
			return
		}
		if n.filterIgnored {
			_, _ = io.WriteString(w, `{"count":9,"results":[{"id":1}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"count":0,"results":[]}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (n *fakeNetBox) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(n)
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, Token: "nbt_k.t"}
}

// saw reports whether a request with this path?query was made.
func (n *fakeNetBox) saw(pathQuery string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, s := range n.seen {
		if strings.Contains(s, pathQuery) {
			return true
		}
	}
	return false
}

// logged returns ctx with a logger that writes to the returned buffer.
func logged() (context.Context, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return log.IntoContext(ctx, zap.New(zap.WriteTo(buf))), buf
}

func TestPreflight(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		tweak   func(map[string]any)
		want    string // empty: success
	}{
		{name: "ok", version: "4.6.7"},
		{name: "newer with suffix", version: "4.7.0-beta1"},
		{name: "old NetBox", version: "4.5.9", want: "NetBox ≥ 4.6 required"},
		{name: "unparsable version", version: "latest", want: "latest"},
		{name: "wrong type", version: "4.6.7", want: "capi_ref: type is integer, want text",
			tweak: func(f map[string]any) { f["type"] = map[string]any{"value": "integer"} }},
		{name: "no iprange", version: "4.6.7", want: "capi_ref: not enabled for ipam.iprange",
			tweak: func(f map[string]any) { f["object_types"] = []string{"ipam.prefix", "ipam.ipaddress"} }},
		{name: "field missing", version: "4.6.7", want: "capi_ref: not found",
			tweak: func(f map[string]any) { clear(f) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &fakeNetBox{version: tc.version, tweak: tc.tweak}
			err := n.client(t).Preflight(ctx, DefaultKeys)
			if tc.want == "" {
				if err != nil {
					t.Errorf("err %v", err)
				}
				if n.saw("users/owners/") || n.saw("tenancy/tenants/") {
					t.Errorf("owners or tenants asked for: %v", n.seen)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want error with %q", err, tc.want)
			}
		})
	}
}

// A ref field with filter_logic loose works: lookups are checked on the client. It is only warned about.
// With filter_logic disabled NetBox ignores the cf_ filter: ErrMisconfigured. A field that is not text is ErrMisconfigured.
func TestPreflightLooseFilterLogic(t *testing.T) {
	logic := func(v string) func(map[string]any) {
		return func(f map[string]any) { f["filter_logic"] = map[string]any{"value": v} }
	}
	n := &fakeNetBox{tweak: logic("loose")}
	lctx, logs := logged()
	if err := n.client(t).Preflight(lctx, DefaultKeys); err != nil {
		t.Errorf("loose: %v", err)
	}
	if !strings.Contains(logs.String(), "WARNING") || !strings.Contains(logs.String(), "filter_logic is loose") {
		t.Errorf("loose: no warning: %s", logs)
	}
	n = &fakeNetBox{tweak: logic("disabled")}
	if err := n.client(t).Preflight(ctx, DefaultKeys); !errors.Is(err, ErrMisconfigured) || !strings.Contains(err.Error(), "NetBox ignores cf_ filters with filter_logic disabled") {
		t.Errorf("disabled: got %v, want ErrMisconfigured", err)
	}
	n = &fakeNetBox{tweak: func(f map[string]any) {
		f["type"] = map[string]any{"value": "json"}
		f["filter_logic"] = map[string]any{"value": "loose"}
	}}
	if err := n.client(t).Preflight(ctx, DefaultKeys); !errors.Is(err, ErrMisconfigured) || !strings.Contains(err.Error(), "capi_ref: type is json, want text") {
		t.Errorf("json field: got %v, want ErrMisconfigured", err)
	}
	if err := (&fakeNetBox{}).client(t).Preflight(ctx, Keys{Instance: "A_b", Ref: "capi_ref"}); !errors.Is(err, ErrMisconfigured) {
		t.Errorf("invalid instance: got %v, want ErrMisconfigured", err)
	}
}

func TestPreflightForbiddenDefinitions(t *testing.T) {
	const defs = "extras/custom-fields/"
	t.Run("warns and probes the filter", func(t *testing.T) {
		n := &fakeNetBox{forbidden: map[string]bool{defs: true}}
		lctx, logs := logged()
		if err := n.client(t).Preflight(lctx, DefaultKeys); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logs.String(), "WARNING") || !strings.Contains(logs.String(), "custom field definitions") {
			t.Errorf("no warning about the definitions: %s", logs)
		}
		if !n.saw("ipam/prefixes/?cf_capi_ref=") {
			t.Errorf("no probe of cf_capi_ref in %v", n.seen)
		}
		if strings.Contains(logs.String(), "ignores") {
			t.Errorf("the filter works but the log says otherwise: %s", logs)
		}
	})
	t.Run("ignored filter is logged, not an error", func(t *testing.T) {
		n := &fakeNetBox{forbidden: map[string]bool{defs: true}, filterIgnored: true}
		lctx, logs := logged()
		if err := n.client(t).Preflight(lctx, DefaultKeys); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logs.String(), "ignores the filter cf_capi_ref") {
			t.Errorf("no warning about the filter: %s", logs)
		}
	})
	t.Run("no prefix access is an error", func(t *testing.T) {
		n := &fakeNetBox{forbidden: map[string]bool{defs: true, "ipam/prefixes/": true}}
		err := n.client(t).Preflight(ctx, DefaultKeys)
		if !errors.Is(err, ErrForbidden) || errors.Is(err, ErrMisconfigured) || !strings.Contains(err.Error(), "the token needs view on ipam.prefix") {
			t.Errorf("got %v, want a transient ErrForbidden with a hint to view on ipam.prefix", err)
		}
	})
	t.Run("filter rejected is misconfigured", func(t *testing.T) {
		n := &fakeNetBox{forbidden: map[string]bool{defs: true}, filterInvalid: true}
		if err := n.client(t).Preflight(ctx, DefaultKeys); !errors.Is(err, ErrMisconfigured) {
			t.Errorf("got %v, want ErrMisconfigured", err)
		}
	})
}

func TestPreflightFieldStatus(t *testing.T) {
	status := func(v string) func(map[string]any) {
		return func(f map[string]any) {
			if v != "" {
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
		{name: "provisioning is transient", status: "provisioning", want: "capi_ref: status is provisioning"},
		{name: "deleting", status: "deleting", want: "capi_ref: status is deleting, want active", misconfigured: true},
		{name: "unknown", status: "archived", want: "capi_ref: status is archived, want active", misconfigured: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (&fakeNetBox{version: "4.7.2", tweak: status(tc.status)}).client(t).Preflight(ctx, DefaultKeys)
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

func TestPreflightClassifies(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(unavailable.Close)

	for _, tc := range []struct {
		name          string
		c             *Client
		keys          Keys
		misconfigured bool
	}{
		{"old version", (&fakeNetBox{version: "4.5.9"}).client(t), DefaultKeys, true},
		{"unparsable version", (&fakeNetBox{version: "latest"}).client(t), DefaultKeys, true},
		{"field is not text", (&fakeNetBox{tweak: func(f map[string]any) {
			f["type"] = map[string]any{"value": "integer"}
		}}).client(t), DefaultKeys, true},
		{"field is missing", (&fakeNetBox{tweak: func(f map[string]any) { clear(f) }}).client(t), DefaultKeys, true},
		{"transport error", &Client{URL: closed.URL, Token: "t"}, DefaultKeys, false},
		{"HTTP 503", &Client{URL: unavailable.URL, Token: "t"}, DefaultKeys, false},
		{"no view on prefixes", (&fakeNetBox{forbidden: map[string]bool{"extras/custom-fields/": true, "ipam/prefixes/": true}}).client(t), DefaultKeys, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Preflight(ctx, tc.keys)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := errors.Is(err, ErrMisconfigured); got != tc.misconfigured {
				t.Errorf("errors.Is(ErrMisconfigured) = %v, want %v: %v", got, tc.misconfigured, err)
			}
		})
	}
}

func TestPreflightMakesNoDetailRequest(t *testing.T) {
	n := &fakeNetBox{}
	if err := n.client(t).Preflight(ctx, DefaultKeys); err != nil {
		t.Fatal(err)
	}
	for _, s := range n.seen {
		if strings.Contains(s, "custom-fields/11/") {
			t.Errorf("preflight still reads a custom field by id: %v", n.seen)
		}
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
	c := (&fakeNetBox{}).client(t)
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
	err := c.WaitPreflight(ctx, DefaultKeys, func(err error, after time.Duration) {
		if err == nil {
			t.Error("retry called without an error")
		}
		delays = append(delays, after)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(delays) != 2 || delays[0] != time.Millisecond || delays[1] != 2*time.Millisecond {
		t.Errorf("retry delays %v, want [1ms 2ms]", delays)
	}
}

func TestWaitPreflightStopsOnMisconfig(t *testing.T) {
	fastBackoff(t, time.Millisecond, time.Millisecond)
	c := (&fakeNetBox{version: "4.5.9"}).client(t)
	err := c.WaitPreflight(ctx, DefaultKeys, func(error, time.Duration) { t.Error("retry called on a misconfigured NetBox") })
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
	err := c.WaitPreflight(cctx, DefaultKeys, func(error, time.Duration) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
}
