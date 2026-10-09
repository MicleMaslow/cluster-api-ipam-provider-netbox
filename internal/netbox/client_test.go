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
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/version"
)

var ctx = context.Background()

type recorded struct{ method, path, query, auth, ifMatch, body, ua string }

func newServer(t *testing.T, status int, body string) (*Client, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*rec = recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("If-Match"), string(b), r.Header.Get("User-Agent")}
		w.Header().Set("ETag", `W/"v1"`)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, Token: "nbt_k.t"}, rec
}

func TestListSendsTokenAndQuery(t *testing.T) {
	c, rec := newServer(t, 200, `{"results":[{"id":7,"prefix":"10.0.0.0/28","status":{"value":"active","label":"Active"},"owner":{"id":3},"custom_fields":{"capi_ref":"a"}}]}`)
	got, err := c.List(ctx, Prefixes, url.Values{"prefix": {"10.0.0.0/22"}})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != "GET" || rec.path != "/api/ipam/prefixes/" || rec.auth != "Bearer nbt_k.t" {
		t.Errorf("request %+v", rec)
	}
	q, _ := url.ParseQuery(rec.query)
	if q.Get("limit") != "1000" || q.Get("prefix") != "10.0.0.0/22" {
		t.Errorf("query %s", rec.query)
	}
	if len(got) != 1 || got[0].ID != 7 || got[0].Status.Value != "active" || !(Owner{ID: 3}).Owns(got[0]) || string(got[0].CustomFields["capi_ref"]) != `"a"` {
		t.Errorf("decoded %+v", got)
	}
}

func TestGetReturnsETagAndPatchSendsIfMatch(t *testing.T) {
	c, rec := newServer(t, 200, `{"id":1}`)
	_, etag, err := c.Get(ctx, Prefixes, 1)
	if err != nil || etag != `W/"v1"` || rec.path != "/api/ipam/prefixes/1/" {
		t.Fatalf("get: etag %q, err %v, request %+v", etag, err, rec)
	}
	if _, err := c.Patch(ctx, Prefixes, 1, etag, map[string]any{"status": "active"}); err != nil {
		t.Fatal(err)
	}
	if rec.method != "PATCH" || rec.ifMatch != `W/"v1"` || rec.body != `{"status":"active"}` {
		t.Errorf("patch %+v", rec)
	}
}

func TestErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{{404, ErrNotFound}, {409, ErrConflict}, {412, ErrModified}} {
		c, _ := newServer(t, tc.status, `{"detail":"x"}`)
		if _, err := c.Create(ctx, AvailableIPs(5), map[string]any{}); !errors.Is(err, tc.want) {
			t.Errorf("HTTP %d: got %v, want %v", tc.status, err, tc.want)
		}
	}
	c, _ := newServer(t, 500, `boom`)
	if _, err := c.Create(ctx, Prefixes, nil); err == nil || !strings.Contains(err.Error(), "HTTP 500: boom") {
		t.Errorf("HTTP 500: got %v", err)
	}
	c, _ = newServer(t, 400, `{"__all__":["Defined addresses overlap with range 10.10.0.78-94/22 in VRF None"]}`)
	if _, err := c.Create(ctx, IPRanges, nil); !errors.Is(err, ErrOverlap) || !strings.Contains(err.Error(), "10.10.0.78-94/22") {
		t.Errorf("HTTP 400 overlap: got %v", err)
	}
	c, _ = newServer(t, 400, `{"start_address":["bad"]}`)
	if _, err := c.Create(ctx, IPRanges, nil); errors.Is(err, ErrOverlap) {
		t.Errorf("HTTP 400: got %v, want no ErrOverlap", err)
	}
}

func TestDeleteIgnoresMissing(t *testing.T) {
	c, rec := newServer(t, 404, ``)
	if err := c.Delete(ctx, IPAddresses, 9, "msg"); err != nil {
		t.Fatal(err)
	}
	if rec.method != "DELETE" || rec.path != "/api/ipam/ip-addresses/9/" {
		t.Errorf("request %+v", rec)
	}
}

func TestDeleteSendsChangelogMessage(t *testing.T) {
	c, rec := newServer(t, 204, ``)
	if err := c.Delete(ctx, Prefixes, 3, "capi: released by x"); err != nil {
		t.Fatal(err)
	}
	if rec.body != `{"changelog_message":"capi: released by x"}` {
		t.Errorf("body %q", rec.body)
	}
}

func TestListFollowsNext(t *testing.T) {
	var srv *httptest.Server
	var queries []string
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Query().Get("offset") != "" {
			_, _ = io.WriteString(w, `{"next":null,"results":[{"id":1000}]}`)
			return
		}
		var b strings.Builder
		for i := range 1000 {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"id":%d}`, i)
		}
		// next carries the server's own idea of its host, which the client must not trust.
		_, _ = fmt.Fprintf(w, `{"next":"http://netbox.invalid/api/ipam/prefixes/?limit=1000&offset=1000&owner_id=3","results":[%s]}`, b.String())
	}))
	t.Cleanup(srv.Close)
	c := &Client{URL: srv.URL, Token: "nbt_k.t"}
	got, err := c.List(ctx, Prefixes, url.Values{"owner_id": {"3"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1001 || got[0].ID != 0 || got[1000].ID != 1000 {
		t.Errorf("got %d objects", len(got))
	}
	if len(queries) != 2 || !strings.Contains(queries[1], "offset=1000") || !strings.Contains(queries[1], "owner_id=3") {
		t.Errorf("queries %v", queries)
	}
}

func TestListPaginationLoop(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch {
		case requests > 50: // lets an unguarded List terminate
			_, _ = io.WriteString(w, `{"next":null,"results":[]}`)
		case r.URL.Query().Get("offset") == "":
			_, _ = io.WriteString(w, `{"next":"http://netbox.invalid/api/ipam/prefixes/?limit=1000&offset=1000","results":[{"id":1}]}`)
		default:
			_, _ = io.WriteString(w, `{"next":"http://netbox.invalid/api/ipam/prefixes/?limit=1000","results":[{"id":2}]}`)
		}
	}))
	t.Cleanup(srv.Close)
	c := &Client{URL: srv.URL, Token: "nbt_k.t"}
	_, err := c.List(ctx, Prefixes, nil)
	if err == nil || !strings.Contains(err.Error(), "pagination loop") || !strings.Contains(err.Error(), Prefixes) {
		t.Errorf("got %v, want pagination loop error naming the path", err)
	}
	if requests > 3 {
		t.Errorf("%d requests, want at most 3", requests)
	}
}

func TestForbidden(t *testing.T) {
	c, _ := newServer(t, 403, `{"detail":"You do not have permission to perform this action."}`)
	_, err := c.List(ctx, Prefixes, nil)
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "do not have permission") {
		t.Errorf("got %v", err)
	}
}

func TestUserAgent(t *testing.T) {
	c, rec := newServer(t, 200, `{"results":[]}`)
	if _, err := c.List(ctx, Prefixes, nil); err != nil {
		t.Fatal(err)
	}
	if want := "cluster-api-ipam-provider-netbox/" + version.Get().GitVersion; rec.ua != want {
		t.Errorf("User-Agent %q, want %q", rec.ua, want)
	}
	c.UserAgent = "custom/1"
	if _, err := c.List(ctx, Prefixes, nil); err != nil || rec.ua != "custom/1" {
		t.Errorf("User-Agent %q, err %v", rec.ua, err)
	}
}

func TestErrorBodyTruncated(t *testing.T) {
	c, _ := newServer(t, 500, strings.Repeat("x", 2000))
	_, err := c.Create(ctx, Prefixes, nil)
	if err == nil || len(err.Error()) > 700 || !strings.Contains(err.Error(), "...") {
		t.Errorf("error is %d bytes, want truncated", len(fmt.Sprint(err)))
	}
}

func TestResponseTooLarge(t *testing.T) {
	c, _ := newServer(t, 200, strings.Repeat(" ", 10<<20+1))
	if _, err := c.List(ctx, Prefixes, nil); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("got %v", err)
	}
}

func TestNewHTTPClient(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		ca       string
		insecure bool
		newErr   bool
		listErr  bool
	}{
		{name: "system roots only", listErr: true},
		{name: "custom CA", ca: caFile},
		{name: "insecure", insecure: true},
		{name: "missing CA file", ca: filepath.Join(t.TempDir(), "none.pem"), newErr: true},
		{name: "CA file without certificates", ca: junk, newErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hc, err := NewHTTPClient(tc.ca, tc.insecure)
			if (err != nil) != tc.newErr {
				t.Fatalf("NewHTTPClient: %v", err)
			}
			if err != nil {
				return
			}
			c := &Client{URL: srv.URL, Token: "t", HTTP: hc}
			if _, err := c.List(ctx, Prefixes, nil); (err != nil) != tc.listErr {
				t.Errorf("List: %v", err)
			}
		})
	}
}

func TestAvailablePaths(t *testing.T) {
	if got := AvailablePrefixes(4); got != "ipam/prefixes/4/available-prefixes/" {
		t.Errorf("AvailablePrefixes = %s", got)
	}
	if got := AvailableIPs(2); got != "ipam/ip-ranges/2/available-ips/" {
		t.Errorf("AvailableIPs = %s", got)
	}
}

func TestOwner(t *testing.T) {
	o := Owner{ID: 3}
	b, err := json.Marshal(o.Managed("NetBoxNetwork/ns/a", "c1", "msg", map[string]any{"status": "active"}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"changelog_message":"msg","custom_fields":{"capi_cluster":"c1","capi_ref":"NetBoxNetwork/ns/a"},"owner":3,"status":"active"}`
	if string(b) != want {
		t.Errorf("Managed = %s", b)
	}
	if q := o.ByRef("x").Encode(); q != "cf_capi_ref=x&owner_id=3" {
		t.Errorf("ByRef = %s", q)
	}
}

func TestLookupOwner(t *testing.T) {
	c, rec := newServer(t, 200, `{"results":[{"id":4}]}`)
	o, err := c.LookupOwner(ctx, "capi-mgmt-dev")
	if err != nil || o.ID != 4 || rec.path != "/api/users/owners/" || !strings.Contains(rec.query, "name=capi-mgmt-dev") {
		t.Errorf("owner %+v, err %v, request %+v", o, err, rec)
	}
	c, _ = newServer(t, 200, `{"results":[]}`)
	if _, err := c.LookupOwner(ctx, "missing"); err == nil {
		t.Error("missing owner: want error")
	}
}

func TestFindByRefIsExact(t *testing.T) {
	c, rec := newServer(t, 200, `{"results":[{"id":1,"custom_fields":{"capi_ref":"x/a"}},{"id":2,"custom_fields":{"capi_ref":"x/a2"}},{"id":3,"custom_fields":{"capi_ref":null}}]}`)
	got, err := c.FindByRef(ctx, IPAddresses, Owner{ID: 3}, "x/a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 1 {
		t.Errorf("FindByRef: got %d objects with ID %v, want 1 with ID 1", len(got), got)
	}
	if !strings.Contains(rec.query, "cf_capi_ref=x%2Fa") || !strings.Contains(rec.query, "owner_id=3") {
		t.Errorf("query missing expected params: %s", rec.query)
	}
}

func TestFindAnyByRefIgnoresOwner(t *testing.T) {
	c, rec := newServer(t, 200, `{"results":[{"id":1,"owner":{"id":9},"custom_fields":{"capi_ref":"x/a"}},{"id":2,"custom_fields":{"capi_ref":"x/a2"}}]}`)
	got, err := c.FindAnyByRef(ctx, Prefixes, "x/a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 1 {
		t.Errorf("FindAnyByRef: got %v, want object 1", got)
	}
	if !strings.Contains(rec.query, "cf_capi_ref=x%2Fa") || strings.Contains(rec.query, "owner_id") {
		t.Errorf("query: %s", rec.query)
	}
}

func TestFindByRefPrefixRechecksCase(t *testing.T) {
	c, rec := newServer(t, 200, `{"results":[{"id":1,"custom_fields":{"capi_ref":"x/p/a"}},{"id":2,"custom_fields":{"capi_ref":"X/P/b"}},{"id":3,"custom_fields":{"capi_ref":null}}]}`)
	got, err := c.FindByRefPrefix(ctx, IPRanges, Owner{ID: 3}, "x/p/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 1 || got[0].Ref() != "x/p/a" {
		t.Errorf("FindByRefPrefix: got %v, want object 1", got)
	}
	if !strings.Contains(rec.query, "cf_capi_ref__isw=x%2Fp%2F") || !strings.Contains(rec.query, "owner_id=3") {
		t.Errorf("query missing expected params: %s", rec.query)
	}
}

func TestRedirectError(t *testing.T) {
	redirected := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			redirected = true
		}
		if r.URL.Path == "/api/ipam/ip-addresses/9/" {
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := &Client{URL: srv.URL, Token: "nbt_k.t"}
	if err := c.Delete(ctx, IPAddresses, 9, "msg"); err == nil || !strings.Contains(err.Error(), "HTTP 301") {
		t.Errorf("redirect: got %v, want error with HTTP 301", err)
	}
	if redirected {
		t.Error("redirect: /elsewhere was requested, CheckRedirect should have prevented it")
	}
}
