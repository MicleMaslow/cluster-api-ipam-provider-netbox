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

// Package main is the entrypoint of the NetBox IPAM provider manager.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	cliflag "k8s.io/component-base/cli/flag"
	logsv1 "k8s.io/component-base/logs/api/v1"
	_ "k8s.io/component-base/logs/json/register"
	"k8s.io/klog/v2"
	inclusterv1 "sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/cluster-api/util/flags"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/controllers"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/version"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")

	logOptions     = logsv1.NewLoggingConfiguration()
	managerOptions = flags.ManagerOptions{}

	controllerOpts              controllers.Options
	instanceName                string
	refField                    string
	netboxCAFile                string
	netboxInsecureSkipVerify    bool
	watchNamespace              string
	syncPeriod                  time.Duration
	restConfigQPS               float32
	restConfigBurst             int
	healthAddr                  string
	leaderElect                 bool
	leaderElectionLeaseDuration time.Duration
	leaderElectionRenewDeadline time.Duration
	leaderElectionRetryPeriod   time.Duration
	showVersion                 bool
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
	utilruntime.Must(ipamv1.AddToScheme(scheme))
	utilruntime.Must(inclusterv1.AddToScheme(scheme))
}

// initFlags registers the manager flags.
func initFlags(fs *pflag.FlagSet) {
	fs.StringVar(&instanceName, "instance-name", "",
		"Name of the management zone, a DNS label: every key the provider writes to NetBox starts with it. Empty: keys without a prefix.")
	fs.StringVar(&refField, "netbox-ref-field", netbox.DefaultKeys.Ref,
		"Text custom field where the provider keeps its key on NetBox objects.")
	fs.StringVar(&netboxCAFile, "netbox-ca-file", "", "PEM file with extra CA certificates to trust for NetBox, added to the system roots.")
	fs.BoolVar(&netboxInsecureSkipVerify, "netbox-insecure-skip-verify", false, "Skip verification of the NetBox TLS certificate. For development only.")
	fs.StringVar(&controllerOpts.WatchFilterValue, "watch-filter", "",
		fmt.Sprintf("Label value that the controller watches to reconcile cluster-api objects. Label key is always %s. If unspecified, the controller watches for all cluster-api objects.", clusterv1.WatchLabel))
	fs.StringVar(&watchNamespace, "namespace", "",
		"Namespace that the controller watches to reconcile cluster-api objects. If unspecified, the controller watches for cluster-api objects across all namespaces.")
	fs.DurationVar(&syncPeriod, "sync-period", 10*time.Minute,
		"The minimum interval at which watched resources are reconciled (e.g. 15m).")
	fs.Float32Var(&restConfigQPS, "kube-api-qps", 20,
		"Maximum queries per second from the controller client to the Kubernetes API server.")
	fs.IntVar(&restConfigBurst, "kube-api-burst", 30,
		"Maximum number of queries that should be allowed in one burst from the controller client to the Kubernetes API server.")

	fs.IntVar(&controllerOpts.NetworkConcurrency, "netboxnetwork-concurrency", controllers.DefaultConcurrency,
		"Number of NetBoxNetworks to process simultaneously.")
	fs.IntVar(&controllerOpts.PoolConcurrency, "netboxippool-concurrency", controllers.DefaultConcurrency,
		"Number of NetBoxIPPools to process simultaneously.")
	fs.IntVar(&controllerOpts.ClaimConcurrency, "ipaddressclaim-concurrency", controllers.DefaultConcurrency,
		"Number of IPAddressClaims to process simultaneously.")

	fs.BoolVar(&leaderElect, "leader-elect", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	fs.DurationVar(&leaderElectionLeaseDuration, "leader-elect-lease-duration", 15*time.Second,
		"Interval at which non-leader candidates will wait to force acquire leadership (duration string).")
	fs.DurationVar(&leaderElectionRenewDeadline, "leader-elect-renew-deadline", 10*time.Second,
		"Duration that the leading controller manager will retry refreshing leadership before giving up (duration string).")
	fs.DurationVar(&leaderElectionRetryPeriod, "leader-elect-retry-period", 2*time.Second,
		"Duration the LeaderElector clients should wait between tries of actions (duration string).")

	fs.StringVar(&healthAddr, "health-addr", ":9440", "The address the health endpoint binds to, 0 disables it.")
	fs.BoolVar(&showVersion, "version", false, "Print the version and exit.")

	logsv1.AddFlags(logOptions, fs)
	// The cluster-api-operator expects these flags to be present and crashes without them.
	flags.AddManagerOptions(fs, &managerOptions)
}

// netboxSettings is the NetBox part of the flags, checked. Surrounding blanks are cut:
// clusterctl cannot render an empty default, so the manifests render a blank one.
func netboxSettings() (netbox.Keys, error) {
	k := netbox.Keys{
		Instance: strings.TrimSpace(instanceName),
		Ref:      cmp.Or(strings.TrimSpace(refField), netbox.DefaultKeys.Ref),
	}
	return k, k.Validate()
}

// Add RBAC for the authorized diagnostics endpoint.
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func main() {
	initFlags(pflag.CommandLine)
	pflag.CommandLine.SetNormalizeFunc(cliflag.WordSepNormalizeFunc)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	utilruntime.Must(pflag.CommandLine.Set("v", "2"))
	pflag.Parse()

	if showVersion {
		fmt.Fprintln(os.Stdout, version.Get())
		return
	}
	if err := logsv1.ValidateAndApply(logOptions, nil); err != nil {
		fmt.Fprintf(os.Stderr, "unable to set up logging: %v\n", err)
		os.Exit(1)
	}
	// klog.Background will automatically use the right logger.
	ctrl.SetLogger(klog.Background())

	nbURL, token, tokenFile := os.Getenv("NETBOX_URL"), os.Getenv("NETBOX_TOKEN"), os.Getenv("NETBOX_TOKEN_FILE")
	if nbURL == "" {
		setupLog.Error(nil, "NETBOX_URL is required")
		os.Exit(1)
	}
	keys, err := netboxSettings()
	if err != nil {
		setupLog.Error(err, "NetBox settings")
		os.Exit(1)
	}
	if (token == "") == (tokenFile == "") {
		setupLog.Error(nil, "exactly one of NETBOX_TOKEN and NETBOX_TOKEN_FILE is required")
		os.Exit(1)
	}
	_, metricsOpts, err := flags.GetManagerOptions(managerOptions)
	if err != nil {
		setupLog.Error(err, "unable to get manager options")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	if strings.HasPrefix(nbURL, "http://") {
		setupLog.Info("WARNING: NETBOX_URL is plain http, the token is sent unencrypted")
	}
	if netboxInsecureSkipVerify {
		setupLog.Info("WARNING: NetBox TLS certificate verification is disabled, for development only")
	}
	httpClient, err := netbox.NewHTTPClient(netboxCAFile, netboxInsecureSkipVerify)
	if err != nil {
		setupLog.Error(err, "create NetBox HTTP client")
		os.Exit(1)
	}
	nb := &netbox.Client{URL: nbURL, Token: token, TokenFile: tokenFile, HTTP: httpClient}
	if err := nb.CheckToken(); err != nil {
		setupLog.Error(err, "NetBox token")
		os.Exit(1)
	}

	restConfig := ctrl.GetConfigOrDie()
	restConfig.QPS = restConfigQPS
	restConfig.Burst = restConfigBurst

	var cacheOpts cache.Options
	cacheOpts.SyncPeriod = &syncPeriod
	if watchNamespace != "" {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{watchNamespace: {}}
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                *metricsOpts,
		Cache:                  cacheOpts,
		HealthProbeBindAddress: healthAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "ipam-netbox.cluster.x-k8s.io",
		LeaseDuration:          &leaderElectionLeaseDuration,
		RenewDeadline:          &leaderElectionRenewDeadline,
		RetryPeriod:            &leaderElectionRetryPeriod,
	})
	if err != nil {
		setupLog.Error(err, "create manager")
		os.Exit(1)
	}

	// The manager serves its probes while NetBox is down: not ready, but alive.
	var ready atomic.Bool
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "add healthz")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", func(*http.Request) error {
		if !ready.Load() {
			return errors.New("waiting for NetBox preflight")
		}
		return nil
	}); err != nil {
		setupLog.Error(err, "add readyz")
		os.Exit(1)
	}

	controllerOpts.NetBox = nb
	controllerOpts.Recorder = mgr.GetEventRecorder("netbox-ipam")
	if err := mgr.Add(preflight{run: func(ctx context.Context) error {
		err := nb.WaitPreflight(ctx, keys, func(err error, after time.Duration) {
			setupLog.Error(err, "NetBox preflight failed, retrying", "after", after)
		})
		if ctx.Err() != nil {
			return nil //nolint:nilerr // stopped while waiting for NetBox
		}
		if err != nil {
			return fmt.Errorf("NetBox preflight (url %s): %w", nbURL, err)
		}
		controllerOpts.Keys = keys
		if err := controllers.Setup(ctx, mgr, controllerOpts); err != nil {
			return fmt.Errorf("set up controllers: %w", err)
		}
		ready.Store(true)
		setupLog.Info("NetBox preflight passed", "instance", keys.Instance, "refField", keys.Ref)
		<-ctx.Done()
		return nil
	}}); err != nil {
		setupLog.Error(err, "add preflight")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "version", version.Get().String())
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "run manager")
		os.Exit(1)
	}
}

// preflight waits for NetBox and starts the controllers once it passes; it needs no leader election.
type preflight struct {
	run func(ctx context.Context) error
}

func (p preflight) Start(ctx context.Context) error { return p.run(ctx) }
func (preflight) NeedLeaderElection() bool          { return false }
