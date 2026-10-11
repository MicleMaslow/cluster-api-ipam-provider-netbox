# Overview

Goal: know the objects you work with and what each one becomes in NetBox, before you install.

## Objects

| Kubernetes | NetBox | What it is |
|---|---|---|
| `NetBoxNetwork` | prefix | the subnet of one cluster inside an L2 network, carved from a parent prefix or adopted |
| `NetBoxNetwork` with `controlPlaneEndpoint` | IP address with role `vip` | the control-plane VIP of the cluster |
| `NetBoxIPPool` | one IP range per range of the pool | spans of a network, given as offsets, that machines take addresses from |
| `IPAddressClaim` pointing at a `NetBoxIPPool` | IP address | allocated from a range of the pool, deleted with the claim |

Both kinds are namespaced, in `ipam.cluster.x-k8s.io/v1alpha1`. They live in the namespace of the Cluster they serve.

With an infrastructure provider that consumes `IPAddressClaim`s, for example CAPV with `addressesFromPools`:

1. A `NetBoxNetwork` names the Cluster and a parent prefix. The provider carves a prefix for the cluster out of it.
2. `NetBoxIPPool`s split that prefix into ranges: control plane, ingress, workers. Each range becomes an IP range in NetBox.
3. The machine template points at a pool. The infrastructure provider creates an `IPAddressClaim` per machine.
4. The provider allocates an address from the range in NetBox and answers with an `IPAddress`: the address, the mask of the L2 network and its gateway.
5. Optionally the network also reserves the VIP of the control plane and writes it to the Cluster.

## The key

NetBox stays the source of truth. An object in NetBox is the provider's when a custom field holds its key. The key is the only identity: the provider finds its objects by it after a restart, `clusterctl move` or pivot, from NetBox itself. An object with another key is never touched; only `spec.prefix` takes an existing prefix that has none.

| NetBox object | Key |
|---|---|
| prefix | `[<instance>/]NetBoxNetwork/<namespace>/<network>` |
| VIP | `[<instance>/]NetBoxNetwork/<namespace>/<network>/vip` |
| IP range | `[<instance>/]NetBoxIPPool/<namespace>/<pool>/<network>` |
| IP address | `[<instance>/]IPAddressClaim/<namespace>/<claim>` |

- The key lives in a text custom field, `capi_ref` by default, set up by the NetBox admin once ([NetBox access](netbox-access.md#the-key)); an existing text field can be reused ([Reusing a custom field](../recipes/reuse-custom-field.md)).
- The optional `<instance>/` is the name of the management zone (`NETBOX_INSTANCE`, for example `zone-a`). Empty, the default, means a key without a prefix. Set it when several management planes share one NetBox; it has to match on both sides of a move, and a changed name is a new zone ([The instance name](netbox-access.md#the-instance-name)).
- The key is made of the namespace and the name, not of the UID, which does not survive a move: keep both when you move or restore a cluster ([clusterctl move and pivot](../guides/move-and-pivot.md)).

Every change the provider makes in NetBox has a changelog message that starts with `capi:`.

## Owner, tenant, description and comments

These are attributes of the objects, set from the spec and kept in sync. They are not part of the identity.

- The **owner** and the **tenant** are optional, set per network (`spec.owner`, `spec.tenant`). Unset, the provider neither writes nor clears them. The tenant is also the border of the rights of the token when its permission is constrained by tenant.
- The **description** and **comments** are templates with defaults ([Metadata](../guides/metadata.md#description-and-comments)). The comments say that the object is managed by the provider.

See [Networks](../guides/networks.md#owner-and-tenant) and [NetBox access](netbox-access.md#owner-and-tenant).

## Limits

- IPv4 and the global VRF only.
- The provider does not run the load balancer of the control plane: it reserves the VIP and writes it to the Cluster, and your template runs kube-vip, keepalived or anything else ([Control-plane VIP](../guides/control-plane-vip.md)).
- There are no admission webhooks: the CRDs validate with CEL.

The design and what is planned (for example importing the addresses of the in-cluster IPAM provider, `spec.importFrom`) are in [the proposal](../proposals/20261007-netbox-ipam-provider.md).

## Check

You are ready to install when you can name:

- the NetBox URL and a token with the permissions of your zone;
- the parent prefix your clusters carve their subnets from;
- the text custom field of the key (the default or an existing one) and whether you set an instance name;
- the owner and tenant of your networks, if you use them.

## Next

- [NetBox access](netbox-access.md): what to request.
- [Install](install.md).
- [First cluster](first-cluster.md).
- [API reference](../reference/api.md) and [NetBox objects](../reference/netbox-objects.md).
