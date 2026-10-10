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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

var (
	ctx   = context.Background()
	k8s   client.Client
	admin *netbox.Client
	owner netbox.Owner
)

// TestMain starts envtest and the controllers against the dev NetBox. Without NETBOX_URL only unit tests run.
func TestMain(m *testing.M) {
	if os.Getenv("NETBOX_URL") == "" {
		fmt.Fprintln(os.Stderr, "NETBOX_URL not set: controller tests are skipped, run make test-integration")
		os.Exit(m.Run())
	}
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if os.Getenv("TEST_LOG") != "" {
		ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	} else {
		ctrl.SetLogger(zap.New(zap.WriteTo(io.Discard)))
	}
	crds := os.Getenv("CAPI_CRDS")
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join(crds, "cluster.x-k8s.io_clusters.yaml"),
			filepath.Join(crds, "ipam.cluster.x-k8s.io_ipaddressclaims.yaml"),
			filepath.Join(crds, "ipam.cluster.x-k8s.io_ipaddresses.yaml"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	defer func() { _ = env.Stop() }()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.AddToScheme, clusterv1.AddToScheme, ipamv1.AddToScheme} {
		if err := add(scheme); err != nil {
			panic(err)
		}
	}
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		panic(err)
	}

	nb := &netbox.Client{URL: os.Getenv("NETBOX_URL"), Token: os.Getenv("NETBOX_TOKEN")}
	admin = &netbox.Client{URL: os.Getenv("NETBOX_URL"), Token: os.Getenv("NETBOX_ADMIN_TOKEN")}
	if owner, err = nb.Preflight(ctx, os.Getenv("NETBOX_OWNER")); err != nil {
		panic(err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		panic(err)
	}
	mgrCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := Setup(mgrCtx, mgr, Options{
		NetBox: nb, Owner: owner, Recorder: mgr.GetEventRecorder("netbox-ipam"),
		NetworkConcurrency: DefaultConcurrency, PoolConcurrency: DefaultConcurrency, ClaimConcurrency: DefaultConcurrency,
	}); err != nil {
		panic(err)
	}
	go func() {
		if err := mgr.Start(mgrCtx); err != nil {
			panic(err)
		}
	}()
	return m.Run()
}
