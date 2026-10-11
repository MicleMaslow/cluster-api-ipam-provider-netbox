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
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/version"
)

var ctx = context.Background()

type recorded struct{ method, path, query, auth, ifMatch, body, ua, lang string }

func newServer(t *testing.T, status int, body string) (*Client, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*rec = recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("If-Match"), string(b), r.Header.Get("User-Agent"), r.Header.Get("Accept-Language")}
		w.Header().Set("ETag", `W/"v1"`)
		w.Header().Set("Content-Type", "application/json")
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
	if len(got) != 1 || got[0].ID != 7 || got[0].Status.Value != "active" || got[0].Owner == nil || got[0].Owner.ID != 3 || string(got[0].CustomFields["capi_ref"]) != `"a"` {
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

func TestGetRequiresETag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"id":1}`) }))
	t.Cleanup(srv.Close)
	_, _, err := (&Client{URL: srv.URL, Token: "t"}).Get(ctx, Prefixes, 1)
	if !errors.Is(err, ErrMisconfigured) || !strings.Contains(err.Error(), "proxy strips") || !strings.Contains(err.Error(), "ipam/prefixes/1/") {
		t.Errorf("got %v, want ErrMisconfigured about the ETag", err)
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

// hiddenServer answers a JSON 404 to every write and get to every GET: NetBox restricts the objects of each action
// to the token's permission constraint, so a write 404s for an object outside it (get 200) as for one gone (get 404).
func hiddenServer(t *testing.T, get int) (*Client, *[]string) {
	t.Helper()
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.WriteHeader(get)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
		_, _ = io.WriteString(w, `{"detail":"Not found."}`)
	}))
	t.Cleanup(srv.Close)
	return &Client{URL: srv.URL, Token: "t"}, &reqs
}

func TestDeleteIgnoresMissing(t *testing.T) {
	c, reqs := hiddenServer(t, http.StatusNotFound)
	if err := c.Delete(ctx, IPAddresses, 9, "", "msg"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"DELETE /api/ipam/ip-addresses/9/", "GET /api/ipam/ip-addresses/9/"}; !slices.Equal(*reqs, want) {
		t.Errorf("requests %q, want %q", *reqs, want)
	}
	if _, err := c.Patch(ctx, IPAddresses, 9, "", map[string]any{"status": "deprecated"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("patch: got %v, want ErrNotFound", err)
	}
}

func TestWriteOutsideConstraint(t *testing.T) {
	c, reqs := hiddenServer(t, http.StatusOK)
	_, err := c.Patch(ctx, Prefixes, 3, "", map[string]any{"status": "deprecated"})
	if !errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "NetBox hides ipam/prefixes/3/ from change: outside the token's permission constraint") {
		t.Errorf("patch: got %v, want ErrForbidden", err)
	}
	err = c.Delete(ctx, Prefixes, 3, "", "msg")
	if !errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "from delete") {
		t.Errorf("delete: got %v, want ErrForbidden", err)
	}
	want := []string{"PATCH /api/ipam/prefixes/3/", "GET /api/ipam/prefixes/3/", "DELETE /api/ipam/prefixes/3/", "GET /api/ipam/prefixes/3/"}
	if !slices.Equal(*reqs, want) {
		t.Errorf("requests %q, want %q", *reqs, want)
	}
}

func TestDeleteSendsChangelogMessage(t *testing.T) {
	c, rec := newServer(t, 204, ``)
	if err := c.Delete(ctx, Prefixes, 3, "", "capi: released by x"); err != nil {
		t.Fatal(err)
	}
	if rec.body != `{"changelog_message":"capi: released by x"}` {
		t.Errorf("body %q", rec.body)
	}
}

func TestListKeyset(t *testing.T) {
	const pageSize = 2
	ids := []int64{1, 2, 3, 4, 5}
	var queries []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		queries = append(queries, q)
		if q.Get("ordering") != "id" || q.Get("limit") != "1000" {
			http.Error(w, "want ordering=id and limit=1000", http.StatusBadRequest)
			return
		}
		after, _ := strconv.ParseInt(q.Get("id__gt"), 10, 64)
		var page []string
		more := false
		for _, id := range ids {
			switch {
			case id <= after:
			case len(page) == pageSize:
				more = true
			default:
				page = append(page, fmt.Sprintf(`{"id":%d}`, id))
			}
		}
		next := "null"
		if more {
			// next carries the server's own idea of its host, which the client must not use.
			next = `"http://netbox.invalid/api/ipam/prefixes/?limit=1000&offset=2"`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"next":%s,"results":[%s]}`, next, strings.Join(page, ","))
		if len(queries) == 1 {
			ids = []int64{2, 3, 4, 5, 6} // 1 deleted, 6 created while the client holds page 1
		}
	}))
	t.Cleanup(srv.Close)
	c := &Client{URL: srv.URL, Token: "nbt_k.t"}
	got, err := c.List(ctx, Prefixes, url.Values{"owner_id": {"3"}})
	if err != nil {
		t.Fatal(err)
	}
	var gotIDs []int64
	for _, o := range got {
		gotIDs = append(gotIDs, o.ID)
	}
	if want := []int64{1, 2, 3, 4, 5, 6}; !slices.Equal(gotIDs, want) {
		t.Errorf("ids %v, want %v", gotIDs, want)
	}
	var after []string
	for _, q := range queries {
		after = append(after, q.Get("id__gt"))
		if q.Get("owner_id") != "3" {
			t.Errorf("query %v lost owner_id", q)
		}
	}
	if want := []string{"", "2", "4"}; !slices.Equal(after, want) {
		t.Errorf("id__gt per request %q, want %q", after, want)
	}
}

func TestListKeysetRequiresProgress(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		next := `"http://netbox.invalid/api/ipam/prefixes/"`
		if requests > 50 { // lets an unguarded List terminate
			next = "null"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"next":%s,"results":[{"id":1},{"id":2}]}`, next) // ignores id__gt
	}))
	t.Cleanup(srv.Close)
	c := &Client{URL: srv.URL, Token: "nbt_k.t"}
	_, err := c.List(ctx, Prefixes, nil)
	if err == nil || !strings.Contains(err.Error(), "pagination did not advance") || !strings.Contains(err.Error(), Prefixes) {
		t.Errorf("got %v, want pagination error naming the path", err)
	}
	if requests > 2 {
		t.Errorf("%d requests, want at most 2", requests)
	}
}

func TestListEmptyPageWithNext(t *testing.T) {
	c, _ := newServer(t, 200, `{"next":"http://netbox.invalid/","results":[]}`)
	if _, err := c.List(ctx, Prefixes, nil); err == nil || !strings.Contains(err.Error(), "pagination did not advance") {
		t.Errorf("got %v", err)
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

func TestLookupOwnerAndTenant(t *testing.T) {
	c, rec := newServer(t, 200, `{"results":[{"id":4}]}`)
	id, err := c.LookupOwner(ctx, "dev")
	if err != nil || id != 4 || rec.path != "/api/users/owners/" || !strings.Contains(rec.query, "name=dev") {
		t.Errorf("owner %d, err %v, request %+v", id, err, rec)
	}
	id, err = c.LookupTenant(ctx, "dev")
	if err != nil || id != 4 || rec.path != "/api/tenancy/tenants/" || !strings.Contains(rec.query, "slug=dev") {
		t.Errorf("tenant %d, err %v, request %+v", id, err, rec)
	}
	c, _ = newServer(t, 200, `{"results":[]}`)
	if _, err := c.LookupOwner(ctx, "missing"); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), `"missing"`) {
		t.Errorf("missing owner: got %v, want ErrNotFound", err)
	}
	if _, err := c.LookupTenant(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing tenant: got %v, want ErrNotFound", err)
	}
	c, _ = newServer(t, 200, `{"results":[{"id":4},{"id":5}]}`)
	if _, err := c.LookupOwner(ctx, "twice"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("two owners: got %v, want an error other than ErrNotFound", err)
	}
	c, _ = newServer(t, 403, `{}`)
	if _, err := c.LookupOwner(ctx, "o"); !errors.Is(err, ErrForbidden) {
		t.Errorf("forbidden: got %v", err)
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
	if err := c.Delete(ctx, IPAddresses, 9, "", "msg"); err == nil || !strings.Contains(err.Error(), "HTTP 301") {
		t.Errorf("redirect: got %v, want error with HTTP 301", err)
	}
	if redirected {
		t.Error("redirect: /elsewhere was requested, CheckRedirect should have prevented it")
	}
}

func TestAcceptLanguage(t *testing.T) {
	c, rec := newServer(t, 200, `{"results":[]}`)
	if _, err := c.List(ctx, Prefixes, nil); err != nil || rec.lang != "en" {
		t.Errorf("List: Accept-Language %q, err %v", rec.lang, err)
	}
	if _, err := c.Create(ctx, Prefixes, map[string]any{}); err != nil || rec.lang != "en" {
		t.Errorf("Create: Accept-Language %q, err %v", rec.lang, err)
	}
}

func TestTruncatesLongTexts(t *testing.T) {
	c, rec := newServer(t, 200, `{}`)
	long := strings.Repeat("я", 300)
	fields := map[string]any{"changelog_message": long, "description": long, "dns_name": long, "status": "active"}
	if _, err := c.Create(ctx, IPAddresses, fields); err != nil {
		t.Fatal(err)
	}
	var sent map[string]string
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatal(err)
	}
	short := strings.Repeat("я", 200)
	if sent["changelog_message"] != short || sent["description"] != short {
		t.Errorf("changelog_message %d runes, description %d runes, want 200",
			len([]rune(sent["changelog_message"])), len([]rune(sent["description"])))
	}
	if sent["dns_name"] != long || sent["status"] != "active" {
		t.Errorf("other fields changed: %d runes in dns_name, status %q", len([]rune(sent["dns_name"])), sent["status"])
	}
	if fields["changelog_message"] != long || fields["description"] != long {
		t.Error("the caller's map was modified")
	}

	if err := c.Delete(ctx, IPAddresses, 1, "", long); err != nil {
		t.Fatal(err)
	}
	if want := `{"changelog_message":"` + short + `"}`; rec.body != want {
		t.Errorf("delete body %d bytes, want the message cut to 200 runes", len(rec.body))
	}
}

func TestNotFoundNeedsJSON(t *testing.T) {
	c, _ := newServer(t, 404, `{"detail":"Not found."}`)
	if _, _, err := c.Get(ctx, Prefixes, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("JSON 404: got %v, want ErrNotFound", err)
	}
	if err := c.Delete(ctx, Prefixes, 1, "", "msg"); err != nil {
		t.Errorf("JSON 404: Delete returned %v, want nil", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "<html>no route</html>")
	}))
	t.Cleanup(srv.Close)
	c = &Client{URL: srv.URL, Token: "t"}
	err := c.Delete(ctx, Prefixes, 1, "", "msg")
	if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "HTTP 404: <html>no route</html>") {
		t.Errorf("HTML 404: Delete returned %v, want an HTTP 404 error", err)
	}
	if _, _, err := c.Get(ctx, Prefixes, 1); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("HTML 404: Get returned %v, want an HTTP 404 error", err)
	}
}

func TestDeleteIfMatch(t *testing.T) {
	c, rec := newServer(t, 204, ``)
	if err := c.Delete(ctx, IPRanges, 4, `W/"abc"`, "msg"); err != nil || rec.method != "DELETE" || rec.ifMatch != `W/"abc"` {
		t.Errorf("conditional: err %v, request %+v", err, rec)
	}
	if err := c.Delete(ctx, IPRanges, 4, "", "msg"); err != nil || rec.method != "DELETE" || rec.ifMatch != "" {
		t.Errorf("unconditional: err %v, request %+v", err, rec)
	}
}

func TestDeletePreconditionFailed(t *testing.T) {
	c, _ := newServer(t, 412, `{"detail":"x"}`)
	if err := c.Delete(ctx, IPRanges, 4, `W/"old"`, "msg"); !errors.Is(err, ErrModified) {
		t.Errorf("got %v, want ErrModified", err)
	}
}

func TestTokenFile(t *testing.T) {
	c, rec := newServer(t, 200, `{"results":[]}`)
	c.Token = "ignored"
	c.TokenFile = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(c.TokenFile, []byte("nbt_a.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.List(ctx, Prefixes, nil); err != nil || rec.auth != "Bearer nbt_a.1" {
		t.Fatalf("first: Authorization %q, err %v", rec.auth, err)
	}
	if err := os.WriteFile(c.TokenFile, []byte(" nbt_b.2 \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.List(ctx, Prefixes, nil); err != nil || rec.auth != "Bearer nbt_b.2" {
		t.Errorf("second: Authorization %q, err %v", rec.auth, err)
	}

	*rec = recorded{}
	c.TokenFile = filepath.Join(t.TempDir(), "none")
	if _, err := c.List(ctx, Prefixes, nil); !errors.Is(err, os.ErrNotExist) || rec.method != "" {
		t.Errorf("missing file: err %v, request %+v, want an error and no request", err, rec)
	}
}

func TestTokenFileEmpty(t *testing.T) {
	c, rec := newServer(t, 200, `{"results":[]}`)
	c.TokenFile = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(c.TokenFile, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := c.List(ctx, Prefixes, nil)
	if err == nil || !strings.Contains(err.Error(), "token file "+c.TokenFile+" is empty") || rec.method != "" {
		t.Errorf("err %v, request %+v, want \"token file ... is empty\" and no request", err, rec)
	}
	if err := c.CheckToken(); err == nil {
		t.Error("CheckToken passes an empty token file")
	}
}

func TestReadOnly(t *testing.T) {
	c, rec := newServer(t, 200, `{"id":1}`)
	ro := c.ReadOnly()
	if _, _, err := ro.Get(ctx, Prefixes, 1); err != nil || rec.method != "GET" {
		t.Fatalf("GET: err %v, request %+v", err, rec)
	}
	*rec = recorded{}
	for _, tc := range []struct {
		method string
		do     func() error
	}{
		{"POST", func() error { _, err := ro.Create(ctx, Prefixes, map[string]any{}); return err }},
		{"PATCH", func() error { _, err := ro.Patch(ctx, Prefixes, 1, "", map[string]any{}); return err }},
		{"DELETE", func() error { return ro.Delete(ctx, Prefixes, 1, "", "msg") }},
	} {
		if err := tc.do(); !errors.Is(err, ErrWriteNeeded) || !strings.Contains(err.Error(), tc.method) {
			t.Errorf("%s: got %v, want ErrWriteNeeded", tc.method, err)
		}
	}
	if rec.method != "" {
		t.Errorf("the server got %+v, want no request", rec)
	}
	if _, err := c.Create(ctx, Prefixes, map[string]any{}); err != nil || rec.method != "POST" {
		t.Errorf("original client: err %v, request %+v", err, rec)
	}
}

func TestMetricsPath(t *testing.T) {
	for in, want := range map[string]string{
		"ipam/ip-ranges/12/available-ips/": "ipam/ip-ranges/{id}/available-ips/",
		"ipam/prefixes/":                   "ipam/prefixes/",
		"ipam/ip-addresses/7/":             "ipam/ip-addresses/{id}/",
		"status/":                          "status/",
	} {
		if got := apiPath(in); got != want {
			t.Errorf("apiPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMetricsRecorded(t *testing.T) {
	count := func(method, path, code string) float64 {
		return testutil.ToFloat64(requests.WithLabelValues(method, path, code))
	}
	c, _ := newServer(t, 200, `{"id":1}`)
	ok, missing := count("GET", "ipam/prefixes/{id}/", "200"), count("GET", "ipam/prefixes/{id}/", "404")
	if _, _, err := c.Get(ctx, Prefixes, 5); err != nil {
		t.Fatal(err)
	}
	if got := count("GET", "ipam/prefixes/{id}/", "200"); got != ok+1 {
		t.Errorf("200 count %v, want %v", got, ok+1)
	}
	if got := count("GET", "ipam/prefixes/{id}/", "404"); got != missing {
		t.Errorf("404 count %v, want %v", got, missing)
	}

	failed := count("GET", "ipam/prefixes/{id}/", "error")
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	if _, _, err := (&Client{URL: srv.URL, Token: "t"}).Get(ctx, Prefixes, 5); err == nil {
		t.Fatal("want a transport error")
	}
	if got := count("GET", "ipam/prefixes/{id}/", "error"); got != failed+1 {
		t.Errorf("error count %v, want %v", got, failed+1)
	}
}
