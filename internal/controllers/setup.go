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
	"context"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/pkg/ipamutil"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

// DefaultConcurrency is the default number of objects of a kind reconciled at once.
const DefaultConcurrency = 10

// Options configure the provider's controllers.
type Options struct {
	NetBox           *netbox.Client
	Owner            netbox.Owner
	WatchFilterValue string
	Recorder         events.EventRecorder

	NetworkConcurrency int
	PoolConcurrency    int
	ClaimConcurrency   int
}

// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// rateLimiter backs off failed reconciles up to a minute, so a NetBox outage or a full pool is retried at least that often.
func rateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Millisecond, time.Minute),
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
}

// Setup registers the provider's controllers with mgr.
func Setup(ctx context.Context, mgr ctrl.Manager, o Options) error {
	c := ignoreGone{mgr.GetClient()}
	if err := mgr.GetFieldIndexer().IndexField(ctx, &ipamv1.IPAddressClaim{}, poolRefField, func(obj client.Object) []string {
		claim, ok := asType[*ipamv1.IPAddressClaim](ctx, obj)
		if !ok {
			return nil
		}
		return poolRefName(claim.Spec.PoolRef)
	}); err != nil {
		return err
	}
	if err := (&NetBoxNetworkReconciler{
		Client: c, NetBox: o.NetBox, Owner: o.Owner, WatchFilterValue: o.WatchFilterValue, Recorder: o.Recorder,
	}).SetupWithManager(ctx, mgr, controller.Options{MaxConcurrentReconciles: o.NetworkConcurrency, RateLimiter: rateLimiter()}); err != nil {
		return err
	}
	if err := (&NetBoxIPPoolReconciler{
		Client: c, NetBox: o.NetBox, Owner: o.Owner, WatchFilterValue: o.WatchFilterValue, Recorder: o.Recorder,
	}).SetupWithManager(ctx, mgr, controller.Options{MaxConcurrentReconciles: o.PoolConcurrency, RateLimiter: rateLimiter()}); err != nil {
		return err
	}
	return (&ipamutil.ClaimReconciler{
		Client: c, Scheme: mgr.GetScheme(), WatchFilterValue: o.WatchFilterValue,
		Adapter: &ProviderAdapter{Client: c, NetBox: o.NetBox, Owner: o.Owner, Recorder: o.Recorder, Concurrency: o.ClaimConcurrency},
	}).SetupWithManager(ctx, mgr)
}
