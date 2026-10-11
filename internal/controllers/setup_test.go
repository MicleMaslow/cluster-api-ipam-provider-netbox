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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestRateLimiterCap(t *testing.T) {
	rl := rateLimiter()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "n"}}
	if d := rl.When(req); d > time.Second {
		t.Errorf("first retry in %v, want a quick one", d)
	}
	var d time.Duration
	for range 30 {
		d = rl.When(req)
	}
	if d != time.Minute {
		t.Errorf("after 30 failures the retry is in %v, want the 1m cap", d)
	}
}
