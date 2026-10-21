# Documentation

Cluster API IPAM provider backed by NetBox. The pages follow the path of a NetBox user: get access, install, build the first cluster, then look up what you need.

## Start here

| Page | You learn |
|---|---|
| [Overview](getting-started/overview.md) | the objects, the key, owner, tenant and text |
| [NetBox access](getting-started/netbox-access.md) | what to request: token, permissions on your zone, the key field and the instance name. Ends with a message to forward to your NetBox admin: [What to send your NetBox admin](getting-started/netbox-access.md#what-to-send-your-netbox-admin) |
| [Install](getting-started/install.md) | clusterctl or Cluster API Operator, variables, TLS, token file, what the provider checks at start |
| [First cluster](getting-started/first-cluster.md) | a CAPV cluster that takes its addresses from NetBox |

## Guides

| Page | You learn |
|---|---|
| [Networks](guides/networks.md) | the subnet of a cluster, owner and tenant per network |
| [Pools](guides/pools.md) | network pools, claims, address options, the network scheme example |
| [In-cluster allocator](guides/in-cluster.md) | the pools of a network as `InClusterIPPool`s of in-cluster IPAM, NetBox keeping the layout |
| [Control-plane VIP](guides/control-plane-vip.md) | reserve the VIP and write it to the Cluster |
| [Metadata](guides/metadata.md) | description and comments of every object, tags and custom fields on the prefix |
| [Deletion and retained objects](guides/deletion-and-retained.md) | what stays in NetBox, the cleanup script |
| [clusterctl move and pivot](guides/move-and-pivot.md) | move a cluster without touching NetBox |

## Recipes

[CAPV with kube-vip](recipes/capv-kube-vip.md), [keepalived](recipes/keepalived.md), [a second NIC](recipes/second-nic.md), [ClusterClass](recipes/clusterclass.md), [migrating an existing cluster](recipes/migrate-existing-cluster.md), [a shared VLAN](recipes/shared-vlan.md), [reusing a custom field](recipes/reuse-custom-field.md), [cleanup](recipes/cleanup.md).

## Operations

[Configuration](operations/configuration.md), [Observability](operations/observability.md), [Troubleshooting](operations/troubleshooting.md), [Upgrade](operations/upgrade.md).

## Reference

[API](reference/api.md), [Conditions](reference/conditions.md), [NetBox objects](reference/netbox-objects.md).

## Development

[Dev stand](development/dev-stand.md), [Testing](development/testing.md), [Release](development/release.md); [CONTRIBUTING](../CONTRIBUTING.md).

## Design and examples

- [Proposal](proposals/20261007-netbox-ipam-provider.md): the design of the provider.
- [`examples/`](examples): the network scheme, the CAPV machine template patch and the control-plane VIP command.
