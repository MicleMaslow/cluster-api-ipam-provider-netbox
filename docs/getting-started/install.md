# Install

Goal: the provider runs in the management cluster, has passed its checks of NetBox and is ready.

You need a management cluster with Cluster API ([`clusterctl init`](https://cluster-api.sigs.k8s.io/user/quick-start)), and from [NetBox access](netbox-access.md): the NetBox URL, the token, the text custom field of the key and whether you set an instance name.

## Steps

1. Register the provider in `~/.config/cluster-api/clusterctl.yaml` and give the NetBox variables:

   ```yaml
   providers:
   - name: netbox
     url: https://github.com/MicleMaslow/cluster-api-ipam-provider-netbox/releases/latest/ipam-components.yaml
     type: IPAMProvider
   NETBOX_URL: https://netbox.example.com
   NETBOX_TOKEN: nbt_<key>.<token>
   NETBOX_INSTANCE: zone-a                     # optional, see the table
   ```

2. Install it:

   ```bash
   clusterctl init --ipam netbox
   ```

The provider runs as `capi-ipam-netbox-controller-manager` in `capi-ipam-netbox-system`. The image is fixed in the manifests; to pull it from a mirror, use the clusterctl `images:` setting with the key `ipam-netbox`.

Its ClusterRole reaches every namespace, as those of the other Cluster API controllers do. Besides its own kinds, Clusters, `IPAddress`es, `IPAddressClaim`s and `InClusterIPPool`s, it allows `get`, `create`, `update` and `delete` on all Secrets, which `publish` of a network needs ([Published addresses](../guides/networks.md#published-addresses)): the provider can read any Secret of the management cluster, though it touches only those it publishes in.

clusterctl lists the releases through the GitHub API, which allows 60 requests an hour per address without a token. If it hits the rate limit, set `GITHUB_TOKEN` to a GitHub token: in the environment of clusterctl, or as a key of the `configSecret` with the operator.

### With Cluster API Operator

With [Cluster API Operator](https://cluster-api-operator.sigs.k8s.io) the provider is an `IPAMProvider`; the variables of the table below go into the Secret that `configSecret` names:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: capi-ipam-netbox-system
---
apiVersion: v1
kind: Secret
metadata:
  name: netbox-variables
  namespace: capi-ipam-netbox-system
stringData:
  NETBOX_URL: https://netbox.example.com
  NETBOX_TOKEN: nbt_<key>.<token>
  NETBOX_INSTANCE: zone-a                     # optional
---
apiVersion: operator.cluster.x-k8s.io/v1alpha2
kind: IPAMProvider
metadata:
  name: netbox
  namespace: capi-ipam-netbox-system
spec:
  version: v0.1.0
  configSecret:
    name: netbox-variables
  fetchConfig:
    url: https://github.com/MicleMaslow/cluster-api-ipam-provider-netbox/releases/latest/ipam-components.yaml
```

- The name is `netbox`: it gives the label `cluster.x-k8s.io/provider: ipam-netbox` and the object names of this page.
- `fetchConfig.url` is the whole URL of the components file, as in the clusterctl config; the operator rejects `…/releases` alone. `spec.version` picks the release.
- `kubectl -n capi-ipam-netbox-system get ipamprovider netbox` shows the installed version and `READY True`; then run [Check](#check).
- Deployment patches (private CA, token file) go into `spec.patches` with `target: {kind: Deployment, name: capi-ipam-netbox-controller-manager}` and the operations of the `kubectl patch` commands below, so the operator keeps them.
- Upgrade: change `spec.version`. Remove: delete the `IPAMProvider`; the CRDs, ClusterRoles and ClusterRoleBinding stay, as with clusterctl ([Remove](#remove)).
- Air-gapped: put `ipam-components.yaml` and `metadata.yaml` of the release into a ConfigMap with the keys `components` and `metadata` and the label `provider.cluster.x-k8s.io/version: v0.1.0`, and use `fetchConfig.selector` with its labels instead of `url`.

### In-cluster IPAM

Optional: needed only for networks with `allocator: InCluster` ([In-cluster allocator](../guides/in-cluster.md)). Install [cluster-api-ipam-provider-in-cluster](https://github.com/kubernetes-sigs/cluster-api-ipam-provider-in-cluster) before this provider: `clusterctl init --ipam in-cluster`, then `clusterctl init --ipam netbox`. The provider checks for the `InClusterIPPool` CRD once, at start; without it it logs `InClusterIPPool is not served` and such networks report `AllocatorUnavailable`, while the `NetBox` allocator works. Installed later, in-cluster needs a restart of this provider:

```bash
kubectl -n capi-ipam-netbox-system rollout restart deployment/capi-ipam-netbox-controller-manager
```

The ClusterRole of the provider allows `inclusterippools` (get, list, watch, create, update, patch, delete) either way.

### Variables

| Variable | Default | Meaning |
|---|---|---|
| `NETBOX_URL` | required | base URL of NetBox |
| `NETBOX_TOKEN` | required | v2 token, stored in the Secret `capi-ipam-netbox-credentials` |
| `NETBOX_INSTANCE` | blank | name of the management zone, a DNS label; every key starts with it. Blank: keys without a prefix ([The instance name](netbox-access.md#the-instance-name)) |
| `NETBOX_REF_FIELD` | `capi_ref` | the text custom field of the key |

- The last two are optional. Owner and tenant are not variables: they are set on a network (`spec.owner`, `spec.tenant`, see [Networks](../guides/networks.md#owner-and-tenant)).
- Unset optional variables render as a blank, which the provider reads as unset.
- Pick the instance name and the field before the first network: the provider does not migrate keys when they change ([NetBox access](netbox-access.md#the-instance-name)).
- All flags of the provider, with the standard Cluster API manager flags, are in [Configuration](../operations/configuration.md).

### TLS

The provider trusts the system roots. A private CA is added with `--netbox-ca-file`, a PEM file mounted into the pod. `--netbox-insecure-skip-verify` turns verification off and is for development only. A `NETBOX_URL` with `http://` logs a warning: the token is sent unencrypted.

To trust a private CA, put it into a ConfigMap and patch the Deployment:

```bash
kubectl -n capi-ipam-netbox-system create configmap netbox-ca --from-file=ca.crt=<ca.pem>
kubectl -n capi-ipam-netbox-system patch deployment capi-ipam-netbox-controller-manager --type=json -p '[
  {"op": "add", "path": "/spec/template/spec/volumes", "value": [{"name": "netbox-ca", "configMap": {"name": "netbox-ca"}}]},
  {"op": "add", "path": "/spec/template/spec/containers/0/volumeMounts", "value": [{"name": "netbox-ca", "mountPath": "/etc/netbox-ca", "readOnly": true}]},
  {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--netbox-ca-file=/etc/netbox-ca/ca.crt"}
]'
```

`clusterctl upgrade` replaces the Deployment, so the patch has to be applied again after it.

### Token from a file

To read the token from a file instead (`NETBOX_TOKEN_FILE`), mount a Secret with the token as a directory, not with `subPath`, which Kubernetes does not update, and drop `NETBOX_TOKEN` from the credentials Secret:

```bash
kubectl -n capi-ipam-netbox-system create secret generic netbox-token --from-literal=token=nbt_<key>.<token>
kubectl -n capi-ipam-netbox-system patch secret capi-ipam-netbox-credentials --type=json -p '[{"op": "remove", "path": "/data/NETBOX_TOKEN"}]'
kubectl -n capi-ipam-netbox-system patch deployment capi-ipam-netbox-controller-manager --type=json -p '[
  {"op": "add", "path": "/spec/template/spec/volumes", "value": [{"name": "netbox-token", "secret": {"secretName": "netbox-token"}}]},
  {"op": "add", "path": "/spec/template/spec/containers/0/volumeMounts", "value": [{"name": "netbox-token", "mountPath": "/etc/netbox-token", "readOnly": true}]},
  {"op": "add", "path": "/spec/template/spec/containers/0/env", "value": [{"name": "NETBOX_TOKEN_FILE", "value": "/etc/netbox-token/token"}]}
]'
```

The pod runs as user and group 65530, so a mounted CA or token file must be readable by them (the default Secret and ConfigMap mode 0644 is). The file is read at every request, whitespace around the token is cut and an empty file is an error. A rotated token then takes effect without a restart. After `clusterctl upgrade` apply the last two commands again. Exactly one of `NETBOX_TOKEN` and `NETBOX_TOKEN_FILE` is required.

Both patches add `volumes` and `volumeMounts`, and a JSON-patch `add` of a whole list replaces the one that is there: the patch applied second must append to the lists (`/-`, the element as `value`), or the first one's volume is lost and, with `--netbox-ca-file` still set, the pod exits with code 1. The token patch after the CA patch:

```bash
kubectl -n capi-ipam-netbox-system patch deployment capi-ipam-netbox-controller-manager --type=json -p '[
  {"op": "add", "path": "/spec/template/spec/volumes/-", "value": {"name": "netbox-token", "secret": {"secretName": "netbox-token"}}},
  {"op": "add", "path": "/spec/template/spec/containers/0/volumeMounts/-", "value": {"name": "netbox-token", "mountPath": "/etc/netbox-token", "readOnly": true}},
  {"op": "add", "path": "/spec/template/spec/containers/0/env", "value": [{"name": "NETBOX_TOKEN_FILE", "value": "/etc/netbox-token/token"}]}
]'
```

The CA patch after the token patch is the same with the CA volume and mount.

### A build of your own

A local build is installed from a local clusterctl repository. `make release` writes it to `out/repo`, with the image `<REGISTRY>/cluster-api-ipam-provider-netbox:<RELEASE_TAG>`; push that image, or load it into a kind cluster with `kind load docker-image`:

```bash
make release RELEASE_TAG=v0.1.0 REGISTRY=registry.example.com/capi
make docker-build docker-push RELEASE_TAG=v0.1.0 REGISTRY=registry.example.com/capi   # docker-push-all: linux/amd64 and linux/arm64
NETBOX_URL=... NETBOX_TOKEN=... clusterctl init --ipam netbox --config hack/dev/clusterctl.yaml
```

`hack/dev/clusterctl.yaml` is used instead of the global clusterctl config and installs the latest version in `out/repo`; run clusterctl from the repository root. The optional variables of the table are taken from the environment as well.

### Remove

`clusterctl delete --ipam netbox`, as for other providers, keeps the provider's ClusterRoles and ClusterRoleBindings: `kubectl delete clusterrole,clusterrolebinding -l cluster.x-k8s.io/provider=ipam-netbox`. The CRDs stay too (`--include-crd` removes them, and with them every NetBoxNetwork and NetBoxIPPool).

## Check

```bash
kubectl -n capi-ipam-netbox-system rollout status deployment/capi-ipam-netbox-controller-manager
kubectl -n capi-ipam-netbox-system logs deployment/capi-ipam-netbox-controller-manager | grep 'NetBox preflight passed'
```

The log line carries the `instance` (empty is none) and the `refField` in use.

### What the provider checks at start

The manager serves its probes at once: `/healthz` answers, `/readyz` answers `waiting for NetBox preflight` until the provider has checked NetBox and started its controllers. The check covers:

- the NetBox version (4.6 or newer);
- the custom field that holds the key: its type (`text`), the object types `ipam.prefix`, `ipam.iprange` and `ipam.ipaddress`, its status and `filter_logic`: `exact` is silent, `loose` logs a warning (lookups read more objects and are checked for the exact key), `disabled` stops the start.

Write permissions, owners, tenants and parent prefixes are not checked there and show up later as conditions. The `ETag` header is checked on the first read of a single prefix, range or address: an answer without it is an error ("answers without ETag (a proxy strips it?)") on the condition of that object, with the reason `NetBoxError`.

- If NetBox cannot be reached or answers with an error, the check is retried inside the process, with waits growing from 1 s to 1 min and every failure logged. The pod stays alive and not ready, so a restart during a NetBox outage does not end in a crash loop.
- If NetBox is set up in a way the provider cannot work with, the provider logs the reason and exits with code 1: NetBox older than 4.6; a key field that is missing or has the wrong type or object types, or `filter_logic: disabled`; a custom field with status `deleting`; settings that are not valid (an instance name that is not a DNS label, a field name NetBox does not accept: letters, digits and `_`, without `__`). A custom field with status `provisioning` (NetBox 4.7) is waited for.
- A token without view on `extras.customfield` is not an error: the provider logs a warning. The key field is then checked by behavior (a filter on prefixes, which needs view on `ipam.prefix`), and a filter that NetBox ignores shows only as a warning.
- A token file that is missing or empty (`NETBOX_TOKEN_FILE`) also stops the start.

Troubleshooting at start: [Troubleshooting](../operations/troubleshooting.md).

## Next

- [First cluster](first-cluster.md).
- [Configuration](../operations/configuration.md), [Observability](../operations/observability.md) and [Upgrade](../operations/upgrade.md).
