<p align="center"><img src="docs/images/logo.svg" width="128" alt=""></p>

# cluster-api-ipam-provider-netbox

NetBox IPAM for Cluster API: cluster subnets, node addresses and control-plane VIPs, allocated in NetBox.

> **Alpha.** v0.1 is the first release; the `v1alpha1` API may still change.

- **A subnet per cluster.** `NetBoxNetwork` carves a prefix out of a parent prefix, or adopts an existing one.
- **Node addresses from NetBox.** `NetBoxIPPool` keeps its ranges as NetBox IP ranges and answers the `IPAddressClaim`s of any infrastructure provider that supports IPAM, such as CAPV.
- **Control-plane VIP.** A network can reserve the VIP and set `Cluster.spec.controlPlaneEndpoint`, for infrastructure providers without a load balancer of their own.
- **NetBox stays the source of truth.** Everything the provider writes carries a key, so it finds its objects again after a restart, `clusterctl move` or pivot.
- **An ordinary NetBox user.** No superuser and no global setup beyond one text custom field; the permissions of the token limit the provider to its zone. Owner, tenant, description and comments come from the spec.

| Kubernetes | NetBox |
|---|---|
| `NetBoxNetwork` | prefix, carved from a parent prefix or adopted; the VIP as an IP address |
| `NetBoxIPPool` | one IP range per range of the pool |
| `IPAddressClaim` pointing at a `NetBoxIPPool` | IP address allocated from a range |

The design is described in [docs/proposals/20261007-netbox-ipam-provider.md](docs/proposals/20261007-netbox-ipam-provider.md).

## Compatibility

| Provider | Cluster API | NetBox |
|---|---|---|
| v0.1.x | v1.14 (contract v1beta2) | ≥ 4.6 (tested with 4.6.7, 4.7.2) |

The provider works with any infrastructure provider that consumes `IPAddressClaim`s, for example CAPV `addressesFromPools`. Only IPv4 and the global VRF are supported.

## Quickstart

You need a management cluster with Cluster API, a NetBox 4.6 or newer with a parent prefix in the global VRF, and a v2 token with write permissions on your zone. What to request, and a message for your NetBox admin: [NetBox access](docs/getting-started/netbox-access.md). The provider keeps its key in one text custom field, `capi_ref` by default, on prefixes, IP ranges and IP addresses; the admin creates it once, or you reuse an existing text field.

1. Register the provider and give it NetBox in `~/.config/cluster-api/clusterctl.yaml`, then install it:

   ```yaml
   providers:
   - name: netbox
     url: https://github.com/MicleMaslow/cluster-api-ipam-provider-netbox/releases/latest/ipam-components.yaml
     type: IPAMProvider
   NETBOX_URL: https://netbox.example.com
   NETBOX_TOKEN: nbt_<key>.<token>
   NETBOX_INSTANCE: zone-a   # optional: the name of this management zone
   ```

   ```bash
   clusterctl init --ipam netbox
   ```

   With Cluster API Operator: [Install](docs/getting-started/install.md#with-cluster-api-operator).

2. Ask for a subnet, a pool and an address:

   ```yaml
   apiVersion: v1
   kind: Namespace
   metadata: {name: sample}
   ---
   apiVersion: cluster.x-k8s.io/v1beta2
   kind: Cluster
   metadata: {name: sample, namespace: sample}
   spec:
     paused: false
     controlPlaneRef: {apiGroup: controlplane.cluster.x-k8s.io, kind: KubeadmControlPlane, name: sample}
   ---
   apiVersion: ipam.cluster.x-k8s.io/v1alpha1
   kind: NetBoxNetwork
   metadata: {name: sample-default, namespace: sample}
   spec: {clusterName: sample, parentPrefix: 10.10.0.0/22, gateway: 10.10.0.1, prefixLength: 27}
   ---
   apiVersion: ipam.cluster.x-k8s.io/v1alpha1
   kind: NetBoxIPPool
   metadata: {name: sample-worker, namespace: sample, labels: {cluster.x-k8s.io/cluster-name: sample}}
   spec: {ranges: [{networkName: sample-default, start: 14, end: 30}]}
   ---
   apiVersion: ipam.cluster.x-k8s.io/v1beta2
   kind: IPAddressClaim
   metadata: {name: sample-claim, namespace: sample}
   spec:
     clusterName: sample
     poolRef: {apiGroup: ipam.cluster.x-k8s.io, kind: NetBoxIPPool, name: sample-worker}
   ```

3. Check that NetBox handed out the address:

   ```bash
   kubectl -n sample wait ipaddressclaim/sample-claim --for=jsonpath='{.status.addressRef.name}'=sample-claim --timeout=90s
   kubectl -n sample get ipaddresses.ipam.cluster.x-k8s.io sample-claim -o jsonpath='{.spec.address}/{.spec.prefix} gw {.spec.gateway}{"\n"}'
   kubectl -n sample get netboxnetwork,netboxippool
   ```

   The prefix and the IP range are now in NetBox, with the key in the key field, a description and comments; delete the `sample` namespace to release the address (the prefix and the range stay as `deprecated`, see [Deletion and retained objects](docs/guides/deletion-and-retained.md)).

No NetBox yet? `make dev-all` brings up a dev stand with a seeded NetBox, the provider and two CAPV clusters on a simulated vCenter: [dev stand](docs/development/dev-stand.md).

## Documentation

Start with the [documentation map](docs/README.md).

- **Getting started.** [Overview](docs/getting-started/overview.md), [NetBox access](docs/getting-started/netbox-access.md), [Install](docs/getting-started/install.md), [First cluster](docs/getting-started/first-cluster.md).
- **Guides.** [Networks](docs/guides/networks.md), [Pools](docs/guides/pools.md), [Control-plane VIP](docs/guides/control-plane-vip.md), [Metadata](docs/guides/metadata.md), [Deletion and retained objects](docs/guides/deletion-and-retained.md), [clusterctl move and pivot](docs/guides/move-and-pivot.md).
- **Recipes.** [CAPV with kube-vip](docs/recipes/capv-kube-vip.md), [keepalived](docs/recipes/keepalived.md), [a second NIC](docs/recipes/second-nic.md), [ClusterClass](docs/recipes/clusterclass.md), [migrating an existing cluster](docs/recipes/migrate-existing-cluster.md), [a shared VLAN](docs/recipes/shared-vlan.md), [reusing a custom field](docs/recipes/reuse-custom-field.md), [cleanup](docs/recipes/cleanup.md).
- **Operations.** [Configuration](docs/operations/configuration.md), [Observability](docs/operations/observability.md), [Troubleshooting](docs/operations/troubleshooting.md), [Upgrade](docs/operations/upgrade.md).
- **Reference.** [API](docs/reference/api.md), [Conditions](docs/reference/conditions.md), [NetBox objects](docs/reference/netbox-objects.md).
- **Development.** [Dev stand](docs/development/dev-stand.md), [Testing](docs/development/testing.md), [Release](docs/development/release.md).

## Community

- [Contributing](CONTRIBUTING.md)
- [Security policy](SECURITY.md)
- [Code of conduct](CODE_OF_CONDUCT.md)

## License

Apache License 2.0, see [LICENSE](LICENSE).
