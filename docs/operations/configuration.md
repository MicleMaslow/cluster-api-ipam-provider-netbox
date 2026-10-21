# Configuration

Goal: know every setting of the provider and where to set it: the clusterctl variables, the environment, the flags, and what the Deployment fixes.

The provider runs as `capi-ipam-netbox-controller-manager` in `capi-ipam-netbox-system`. clusterctl fills the variables when it installs the manifests ([Install](../getting-started/install.md)); everything else is a flag or an environment variable of the container.

## clusterctl variables

| Variable | Default | Becomes | Meaning |
|---|---|---|---|
| `NETBOX_URL` | required | `NETBOX_URL` in the Secret `capi-ipam-netbox-credentials` | base URL of NetBox |
| `NETBOX_TOKEN` | required | `NETBOX_TOKEN` in the same Secret | v2 token, `nbt_<key>.<token>` |
| `NETBOX_INSTANCE` | blank | `--instance-name` | name of the management zone, a DNS label; blank: keys without a prefix |
| `NETBOX_REF_FIELD` | `capi_ref` | `--netbox-ref-field` | the text custom field of the key |

- The optional variables are written `${VAR:= }` in the manifests: clusterctl treats an empty default as "required", so an unset variable renders as a single blank. The provider trims every value and reads a blank as unset.
- A blank `NETBOX_REF_FIELD` gives the default.
- Owner and tenant are not settings of the provider: they are set per network (`spec.owner`, `spec.tenant`).
- clusterctl does not remember the variables of `clusterctl init`: keep every `NETBOX_*` variable you set at init set for `clusterctl upgrade` too. An optional one left out renders blank without an error, and the provider runs with the default: a left out `NETBOX_INSTANCE` is a new zone ([Upgrade](upgrade.md#steps)).
- Local builds read the optional variables from the environment as well ([Install](../getting-started/install.md#a-build-of-your-own)).

## Environment of the provider

| Variable | Meaning |
|---|---|
| `NETBOX_URL` | required, else the provider exits with code 1. `http://` logs a warning: the token is sent unencrypted |
| `NETBOX_TOKEN`, `NETBOX_TOKEN_FILE` | exactly one is required. The file is read at every request, whitespace around the token is cut and an empty file is an error: a rotated token needs no restart |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` | the usual proxy variables are honoured for requests to NetBox |

## Flags: NetBox

| Flag | Default | Meaning |
|---|---|---|
| `--instance-name` | empty | name of the management zone, a DNS label of at most 63 characters; every key the provider writes starts with it. Empty or blank: keys without a prefix |
| `--netbox-ref-field` | `capi_ref` | the text custom field of the key. Blank: the default |
| `--netbox-ca-file` | empty | PEM file with extra CA certificates for NetBox, added to the system roots (mount it into the pod) |
| `--netbox-insecure-skip-verify` | false | skip verification of the NetBox certificate; development only |

Rules the provider checks at start; a violation is logged and ends the start with exit code 1:

- The field name is one NetBox accepts: letters, digits and `_` in any case, without `__`.
- The instance name is a DNS label (lowercase letters, digits and `-`, at most 63 characters).

## Flags: controllers

| Flag | Default | Meaning |
|---|---|---|
| `--watch-filter` | empty | reconcile only objects labelled `cluster.x-k8s.io/watch-filter` with this value; empty: all |
| `--namespace` | empty | watch only this namespace; empty: all |
| `--netboxnetwork-concurrency` | 10 | `NetBoxNetwork`s processed at once |
| `--netboxippool-concurrency` | 10 | `NetBoxIPPool`s processed at once |
| `--ipaddressclaim-concurrency` | 10 | `IPAddressClaim`s processed at once |
| `--sync-period` | `10m` | the minimum interval at which watched objects are reconciled again |
| `--kube-api-qps`, `--kube-api-burst` | 20, 30 | limits of the client to the Kubernetes API |
| `--kubeconfig` | empty | kubeconfig, only out of cluster |
| `--leader-elect` | false | leader election; the manifests set it. Only the leader runs the reconcilers, the checks of NetBox at start run in every replica |
| `--leader-elect-lease-duration`, `--leader-elect-renew-deadline`, `--leader-elect-retry-period` | `15s`, `10s`, `2s` | the timings of the leader election (`ipam-netbox.cluster.x-k8s.io`) |

## Flags: probes, diagnostics and logging

| Flag | Default | Meaning |
|---|---|---|
| `--health-addr` | `:9440` | address of `/healthz` and `/readyz`; `0` disables them |
| `--diagnostics-address` | `:8443` | address of the metrics endpoint (HTTPS, with authentication and authorization); with it also pprof and the endpoint that changes the log level. `0` disables it |
| `--insecure-diagnostics` | false | serve the metrics over HTTP, without authentication and without pprof |
| `--tls-min-version` | `VersionTLS12` | minimum TLS version of the metrics server |
| `--tls-cipher-suites`, `--tls-curve-preferences` | Go defaults | comma-separated lists for the metrics server |
| `-v` | 2 | log verbosity |
| `--logging-format` | `text` | `text` or `json` |
| `--vmodule`, `--log-flush-frequency` | `5s` for the latter | the standard Kubernetes logging flags |
| `--log-text-info-buffer-size`, `--log-text-split-stream`, `--log-json-info-buffer-size`, `--log-json-split-stream` | | alpha logging flags of Kubernetes (they need a feature gate this binary does not expose); `--help` describes them |
| `--version` | | print the version and exit |

## Optional dependency: in-cluster IPAM

`allocator: InCluster` needs the `InClusterIPPool` CRD of cluster-api-ipam-provider-in-cluster. The provider looks for it once, at start, and watches `InClusterIPPool`s only when it is served; without it it logs `InClusterIPPool is not served: networks with allocator InCluster wait for cluster-api-ipam-provider-in-cluster and a restart`, and those networks report `AllocatorUnavailable`. Restart the provider after installing in-cluster ([Install](../getting-started/install.md#in-cluster-ipam)). The RBAC of the manifests covers `inclusterippools` in any case.

## What the Deployment fixes

The manifests set `--leader-elect`, `--diagnostics-address=:8443`, `--health-addr=:9440` and the two flags `--instance-name` and `--netbox-ref-field`, and take `NETBOX_URL` and `NETBOX_TOKEN` from the Secret. One replica; requests 10m CPU and 64Mi memory, limit 256Mi memory; it runs as user and group 65530, non-root, with a read-only root filesystem, no privilege escalation, all capabilities dropped and the `RuntimeDefault` seccomp profile; it tolerates the control-plane taint. The ports are `healthz` (9440) and `metrics` (8443). The namespace enforces the `restricted` Pod Security level. The ClusterRole allows `get`, `create`, `update` and `delete` on Secrets in every namespace, for `publish` ([Published addresses](../guides/networks.md#published-addresses)).

To change another flag, patch the Deployment's args as in [Install](../getting-started/install.md#tls):

```bash
kubectl -n capi-ipam-netbox-system patch deployment capi-ipam-netbox-controller-manager --type=json -p '[
  {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--netboxippool-concurrency=20"}
]'
```

`clusterctl upgrade` replaces the Deployment: apply the patch again afterwards.

## Check

```bash
kubectl -n capi-ipam-netbox-system get deployment capi-ipam-netbox-controller-manager -o jsonpath='{.spec.template.spec.containers[0].args}'
kubectl -n capi-ipam-netbox-system logs deployment/capi-ipam-netbox-controller-manager | grep 'NetBox preflight passed'
```

The args show the values clusterctl rendered; the log line shows the instance name and the key field the provider uses ([Install](../getting-started/install.md#check)). A setting the provider cannot work with ends the start with the reason in the log ([Troubleshooting](troubleshooting.md#the-provider-does-not-start)).

## Next

- [Observability](observability.md): probes, metrics, events and logs.
- [Upgrade](upgrade.md) and [Troubleshooting](troubleshooting.md).
