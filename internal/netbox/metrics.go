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
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "netbox_ipam_requests_total",
		Help: "Requests to the NetBox API by method, path and HTTP status (error: no response).",
	}, []string{"method", "path", "code"})
	durations = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "netbox_ipam_request_duration_seconds",
		Help:    "Duration of requests to the NetBox API by method and path.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})
)

func init() {
	crmetrics.Registry.MustRegister(requests, durations)
}

func observe(method, path string, resp *http.Response, err error, start time.Time) {
	code := "error"
	if err == nil {
		code = strconv.Itoa(resp.StatusCode)
	}
	path = apiPath(path)
	requests.WithLabelValues(method, path, code).Inc()
	durations.WithLabelValues(method, path).Observe(time.Since(start).Seconds())
}

// apiPath replaces the numeric segments of path with {id}, to keep the label cardinality bounded.
func apiPath(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		if s != "" && strings.Trim(s, "0123456789") == "" {
			segments[i] = "{id}"
		}
	}
	return strings.Join(segments, "/")
}
