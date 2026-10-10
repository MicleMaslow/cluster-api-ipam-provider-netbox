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

// Package netbox is a thin client for the NetBox endpoints the provider uses.
package netbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/version"
)

// NetBox API paths and the custom fields that mark the objects the provider manages.
const (
	Prefixes    = "ipam/prefixes/"
	IPRanges    = "ipam/ip-ranges/"
	IPAddresses = "ipam/ip-addresses/"
	Owners      = "users/owners/"

	FieldRef     = "capi_ref"
	FieldCluster = "capi_cluster"
)

var (
	// ErrNotFound means NetBox answered 404 with JSON: the object does not exist.
	ErrNotFound = errors.New("netbox: not found")
	// ErrConflict is NetBox's answer when available-prefixes or available-ips has no room left.
	ErrConflict = errors.New("netbox: conflict")
	// ErrModified means the object changed after it was read (If-Match failed).
	ErrModified = errors.New("netbox: modified since read")
	// ErrForbidden means the token lacks the permission for the request.
	ErrForbidden = errors.New("netbox: forbidden")
	// ErrOverlap means an IP range would overlap another one.
	ErrOverlap = errors.New("netbox: overlaps another IP range")
	// ErrWriteNeeded means a read-only client was asked to write.
	ErrWriteNeeded = errors.New("netbox: write needed")
	// ErrMisconfigured means NetBox is set up in a way the provider cannot work with; retrying does not help.
	ErrMisconfigured = errors.New("netbox: misconfigured")
)

// Object holds the fields the provider reads from prefixes, IP ranges, IP addresses and owners.
type Object struct {
	ID            int64                      `json:"id"`
	Prefix        string                     `json:"prefix"`
	StartAddress  string                     `json:"start_address"`
	EndAddress    string                     `json:"end_address"`
	Address       string                     `json:"address"`
	DNSName       string                     `json:"dns_name"`
	Description   string                     `json:"description"`
	Status        Choice                     `json:"status"`
	Role          Choice                     `json:"role"`
	MarkPopulated bool                       `json:"mark_populated"`
	IsPool        bool                       `json:"is_pool"`
	Owner         *Ref                       `json:"owner"`
	CustomFields  map[string]json.RawMessage `json:"custom_fields"`
	Tags          []Tag                      `json:"tags"`
	LastUpdated   string                     `json:"last_updated"`
}

// Choice is how NetBox renders choice fields.
type Choice struct {
	Value string `json:"value"`
}

// Tag is a tag as NetBox nests it in other objects.
type Tag struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
}

// Ref is a nested object.
type Ref struct {
	ID int64 `json:"id"`
}

const (
	maxBody    = 10 << 20
	maxErrBody = 512
	// maxText is the length NetBox allows for changelog_message and description.
	maxText = 200
)

// Client talks to one NetBox with a v2 API token (nbt_<key>.<token>).
type Client struct {
	URL   string
	Token string
	// TokenFile, if set, is read for the token on every request, so a rotated secret needs no restart.
	TokenFile string
	// UserAgent defaults to cluster-api-ipam-provider-netbox/<version>.
	UserAgent string
	HTTP      *http.Client

	readOnly bool
}

// ReadOnly returns a copy of c that fails every request but GET with ErrWriteNeeded.
func (c *Client) ReadOnly() *Client {
	cp := *c
	cp.readOnly = true
	return &cp
}

// CheckToken fails if the token cannot be read: a token file that is missing or empty.
func (c *Client) CheckToken() error {
	_, err := c.token()
	return err
}

func (c *Client) token() (string, error) {
	if c.TokenFile == "" {
		return c.Token, nil
	}
	b, err := os.ReadFile(filepath.Clean(c.TokenFile))
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", c.TokenFile)
	}
	return token, nil
}

var defaultHTTP = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// NewHTTPClient returns an HTTP client that trusts the system roots plus the PEM bundle in caFile (if set).
// insecureSkipVerify turns certificate checks off and is for development only.
func NewHTTPClient(caFile string, insecureSkipVerify bool) (*http.Client, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if caFile != "" {
		data, err := os.ReadFile(filepath.Clean(caFile))
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		if !roots.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("CA file %s has no PEM certificates", caFile)
		}
	}
	hc := *defaultHTTP
	hc.Transport = &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			RootCAs:            roots,
			InsecureSkipVerify: insecureSkipVerify, //nolint:gosec // opt-in, dev only
		},
	}
	return &hc, nil
}

// AvailablePrefixes is the path that carves a child prefix out of parentID.
func AvailablePrefixes(parentID int64) string {
	return Prefixes + strconv.FormatInt(parentID, 10) + "/available-prefixes/"
}

// AvailableIPs is the path that allocates the next free address of an IP range.
func AvailableIPs(rangeID int64) string {
	return IPRanges + strconv.FormatInt(rangeID, 10) + "/available-ips/"
}

// List returns all objects at path matching q. It pages by id (ordering=id, id__gt=<last id>), so objects
// created or deleted meanwhile cannot shift a page; the next link only tells whether there is more of it.
func (c *Client) List(ctx context.Context, path string, q url.Values) ([]Object, error) {
	query := url.Values{"limit": {"1000"}}
	for k, v := range q {
		query[k] = v
	}
	query.Set("ordering", "id")
	var all []Object
	prev := int64(math.MinInt64)
	for {
		var page struct {
			Next    string   `json:"next"`
			Results []Object `json:"results"`
		}
		if _, err := c.do(ctx, http.MethodGet, path, query, "", nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Results...)
		if page.Next == "" {
			return all, nil
		}
		if len(page.Results) == 0 || page.Results[len(page.Results)-1].ID <= prev {
			return nil, fmt.Errorf("GET %s: pagination did not advance", path)
		}
		prev = page.Results[len(page.Results)-1].ID
		query.Set("id__gt", strconv.FormatInt(prev, 10))
	}
}

// Get returns one object and its ETag.
func (c *Client) Get(ctx context.Context, path string, id int64) (Object, string, error) {
	var o Object
	etag, err := c.do(ctx, http.MethodGet, objectPath(path, id), nil, "", nil, &o)
	return o, etag, err
}

// Create posts fields to path and returns the created object.
func (c *Client) Create(ctx context.Context, path string, fields map[string]any) (Object, error) {
	var o Object
	_, err := c.do(ctx, http.MethodPost, path, nil, "", fields, &o)
	return o, err
}

// Patch updates fields of one object and returns its new ETag; a non-empty etag makes the update conditional.
func (c *Client) Patch(ctx context.Context, path string, id int64, etag string, fields map[string]any) (string, error) {
	return c.do(ctx, http.MethodPatch, objectPath(path, id), nil, etag, fields, nil)
}

// Delete removes one object, recording message in the changelog; one that is already gone is not an error.
// A non-empty etag makes the delete conditional.
func (c *Client) Delete(ctx context.Context, path string, id int64, etag, message string) error {
	_, err := c.do(ctx, http.MethodDelete, objectPath(path, id), nil, etag, map[string]any{"changelog_message": message}, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func objectPath(path string, id int64) string { return path + strconv.FormatInt(id, 10) + "/" }

func (c *Client) do(ctx context.Context, method, path string, q url.Values, ifMatch string, body, out any) (etag string, err error) {
	if c.readOnly && method != http.MethodGet {
		return "", fmt.Errorf("%s %s: %w", method, path, ErrWriteNeeded)
	}
	token, err := c.token()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", method, path, err)
	}
	u := strings.TrimSuffix(c.URL, "/") + "/api/" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(truncateTexts(body))
		if err != nil {
			return "", err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en")
	ua := c.UserAgent
	if ua == "" {
		ua = "cluster-api-ipam-provider-netbox/" + version.Get().GitVersion
	}
	req.Header.Set("User-Agent", ua)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	start := time.Now()
	resp, err := hc.Do(req)
	observe(method, path, resp, err, start)
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("%s %s: close body: %w", method, path, cerr)
		}
	}()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxBody {
		return "", fmt.Errorf("%s %s: response too large (over %d bytes)", method, path, maxBody)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json"):
		return "", fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	case resp.StatusCode == http.StatusForbidden:
		return "", fmt.Errorf("%s %s: %w: %s", method, path, ErrForbidden, snippet(data))
	case resp.StatusCode == http.StatusConflict:
		return "", fmt.Errorf("%s %s: %w: %s", method, path, ErrConflict, snippet(data))
	case resp.StatusCode == http.StatusPreconditionFailed:
		return "", fmt.Errorf("%s %s: %w", method, path, ErrModified)
	case resp.StatusCode == http.StatusBadRequest && bytes.Contains(data, []byte("overlap with range")):
		return "", fmt.Errorf("%s %s: %w: %s", method, path, ErrOverlap, snippet(data))
	case resp.StatusCode >= 300:
		return "", fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, snippet(data))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return "", fmt.Errorf("%s %s: decode: %w", method, path, err)
		}
	}
	return resp.Header.Get("ETag"), nil
}

// truncateTexts cuts the texts NetBox limits to maxText runes in a copy of body.
func truncateTexts(body any) any {
	fields, ok := body.(map[string]any)
	if !ok {
		return body
	}
	fields = maps.Clone(fields)
	for _, k := range []string{"changelog_message", "description"} {
		if s, ok := fields[k].(string); ok {
			if r := []rune(s); len(r) > maxText {
				fields[k] = string(r[:maxText])
			}
		}
	}
	return fields
}

func snippet(data []byte) string {
	data = bytes.TrimSpace(data)
	if len(data) > maxErrBody {
		return string(data[:maxErrBody]) + "..."
	}
	return string(data)
}
