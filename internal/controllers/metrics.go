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
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// lockWait is observed each time a section takes netBoxWrites, with the time it waited for it.
var lockWait = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "netbox_ipam_write_lock_wait_seconds",
	Help:    "Time a section that writes to NetBox waited for the write lock.",
	Buckets: prometheus.ExponentialBuckets(0.001, 4, 9),
})

func init() {
	crmetrics.Registry.MustRegister(lockWait)
}
