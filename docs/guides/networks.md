# Networks

Goal: create a `NetBoxNetwork`, the subnet of one cluster inside an L2 network, and know what it does in NetBox. Every field is in the [API reference](../reference/api.md); `kubectl explain netboxnetwork.spec` describes them too. A sample is [`config/samples/sample.yaml`](../../config/samples/sample.yaml).

Prepare the parent first: NetBox needs exactly one `parentPrefix` in the global VRF, and its child prefixes must cover what a cluster subnet must not overlap ([NetBox access](../getting-started/netbox-access.md#parent-prefix)).

## Steps

Create the network in the namespace of the Cluster:

```yaml
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxNetwork
metadata: {name: demo, namespace: demo}
spec:
  clusterName: demo
  parentPrefix: 10.10.0.0/22
  gateway: 10.10.0.1
  prefixLength: 26
```

- `clusterName`, `parentPrefix` (an existing NetBox prefix the cluster prefix is carved from or adopted in) and `gateway` of the L2 network. `parentPrefix` may be the prefix of the whole VLAN or a logical pool for CAPI inside it; the gateway may lie outside a logical parent.
- `addressPrefixLength` is the mask of the nodes' subnet. The L2 network is `gateway/addressPrefixLength` with the host bits cleared; without the field it is `parentPrefix`. Addresses handed out get this mask and the gateway (`status.addressPrefixLength`). The network and broadcast address of the L2 network and the gateway are reserved: a pool range or the VIP never covers them. Pools take ranges of networks in one L2 network only (`MixedNetworks` compares gateway and mask), so networks of different logical parents of one VLAN can share a pool.
- `prefixLength` carves a free prefix of that length out of `parentPrefix`; `prefix` instead takes a given CIDR inside it. Exactly one of the two, immutable. An existing prefix is adopted if it carries no key, or the key of this network; a prefix with another key is `Duplicate` (`held by ...`). A deprecated prefix a deleted network of the same namespace left is adopted, and made active again. A missing one is created. Adoption writes only the key; status, description, comments, owner, tenant and the rest stay as the NetBox admin set them. The prefix has to lie in your zone, since the token must be allowed to change it.
- `isPool: true` sets `is_pool` on the prefix the provider carves, so NetBox counts all its addresses as usable hosts, and keeps it in sync; the default is false. It needs `prefixLength`: an adopted prefix is never changed.
- `deletionPolicy` says what happens to the prefix, the pool ranges in it and the VIP when the network goes: `Retain` (default) or `Delete`. See [Deletion and retained objects](deletion-and-retained.md).
- `controlPlaneEndpoint` reserves the VIP of the cluster: [Control-plane VIP](control-plane-vip.md).
- `customFields` and `tags` are set on the prefix: [Metadata](metadata.md).
- `owner` and `tenant`: see below.
- `description` and `comments` are templates of the text of the prefix, with defaults: [Metadata](metadata.md#description-and-comments).

The CRD rejects:

- an L2 network that does not contain `parentPrefix`, an `addressPrefixLength` longer than the `parentPrefix` mask, a `prefix` outside `parentPrefix`, a `prefixLength` not longer than the `parentPrefix` mask;
- an IPv6 `parentPrefix` or `gateway`;
- `isPool` without `prefixLength`, an adopted `prefix` with `deletionPolicy: Delete`, none or both of `prefix` and `prefixLength`;
- a change of `clusterName`, `parentPrefix`, `gateway`, `prefixLength`, `prefix`, a change of `addressPrefixLength`, adding or removing it, and a switch between `prefix` and `prefixLength`;
- a change of `controlPlaneEndpoint.port`, adding or removing it, while `controlPlaneEndpoint` is set (the whole field can be removed).

All immutable fields of both kinds are in the [API reference](../reference/api.md#immutable-fields).

## Owner and tenant

Both are optional attributes of the NetBox objects of the network. They are set on the network and are not part of the identity: an object is the provider's by its key alone.

```yaml
spec:
  owner:  {name: zone-a-owner}   # or {id: 7}
  tenant: {slug: platform}       # or {id: 42}
```

- Each field has exactly one form (CEL): `name` or `id` for the owner, `slug` or `id` for the tenant. Name and slug are readable and the same in every NetBox installation (dev, stage, prod); an id survives a rename ([NetBox access](../getting-started/netbox-access.md#owner-and-tenant) tells which one needs which view permission).
- **Where they go.** The prefix and the VIP get the owner and tenant of the network; the ranges of a pool and the addresses of its claims get those of the network they lie in, so a pool with ranges in networks of different owners works.
- **The spec is the source of truth.** The values are written when an object is created and synced on the existing ones: the network does it for its prefix and VIP, the pool for its ranges, a claim for its address when it is checked again (a changed owner or tenant of the network wakes its claims). A change in the spec reaches existing objects, a value edited in NetBox by hand is overwritten, and an Event `AttributesSynced` records the change. A prefix under `spec.prefix` is the exception: it keeps its owner and tenant after creation, so what the admin set stays.
- **Unset is untouched.** Without `spec.owner` the provider writes no owner and does not clear one on an existing object, and the same for the tenant. Removing the field from the spec leaves the NetBox objects as they are.
- **The status is a cache.** The network looks the name or slug up (or checks the id) and records the NetBox ids in `status.ownerID` and `status.tenantID` for its pools and claims. It asks NetBox again on a new generation (the spec changed), when no id is cached, or when the reason of `Ready` is `WaitingForCluster`, `InvalidSpec`, `NetBoxError`, `Forbidden`, `OwnerNotFound` or `TenantNotFound`; any other state keeps the cache. A rename in NetBox shows on the next resolve, after a spec change or a `NetBoxError` or `Forbidden` on `Ready`: give the `id` to survive renames. The ids carry no meaning of their own: clear them or change the spec, and the next reconcile resolves again. A 403 on checking an id accepts it as given (the token has no view on owners or tenants).
- **Not found.** A name, slug or id that NetBox does not have is `Ready=False` with `OwnerNotFound` or `TenantNotFound`, checked again every minute. The pools of the network take its ranges out of service like those of any network that is not `Ready`: a new claim gets no address from them (`PoolNotReady` if the pool has no other ready range), the addresses already allocated stay `Ready`. A pool edit or deletion still releases its ranges.
- **A sync that fails** does not stop the network. When the token cannot set the value (the constraint of its permission does not allow it, `Forbidden`) the condition `MetadataSynced` of the network reports it, `RangesReady` of a pool for its ranges, and a claim keeps its `Ready` and retries with backoff; what is already allocated or served goes on.
- **A constrained token.** If a write permission is constrained by owner or tenant, set the same on the network before it creates anything: a network without `spec.owner` creates objects without an owner, and NetBox refuses them ([NetBox access](../getting-started/netbox-access.md#permissions)).
- After `clusterctl move` the status is empty and the ids are found again from the spec ([clusterctl move and pivot](move-and-pivot.md)).
- Deleting a network does not depend on owner or tenant: the objects are found by key ([Deletion and retained objects](deletion-and-retained.md)).

## The Cluster, pause and deletion

A network whose Cluster (`clusterName`) does not exist and that has no prefix yet touches nothing in NetBox, with or without `controlPlaneEndpoint`: it is `Ready=False` with `WaitingForCluster` until the Cluster appears. After `clusterctl move` or a restore the network has no status, and until its Cluster exists (and is not paused) it must not act on NetBox; for a VIP the Cluster may also come with an endpoint of its own that decides the address. A network that already holds its prefix works without the Cluster. The provider watches Clusters, so the order of applying the manifests does not matter.

A network is paused when its Cluster has `spec.paused: true` or the network has the `cluster.x-k8s.io/paused` annotation: it is skipped, nothing is written to NetBox or to the Cluster, and its condition `Paused` is True. `--watch-filter` is checked for networks as it is for pools.

A network being deleted waits while a `NetBoxIPPool` still has a range in it (`WaitingForPools`).

Do not give a `NetBoxNetwork` an `ownerReference` with `blockOwnerDeletion` to its Cluster: with foreground deletion of the Cluster, the network's wait for the Cluster (`deletionPolicy: Delete`) and the Cluster's wait for the network would block each other. No owner reference is needed: the CRDs carry the `clusterctl move` hierarchy label.

## Check

```bash
kubectl get netboxnetworks -A
kubectl -n demo get netboxnetwork demo -o jsonpath='{.status.prefix} {.status.ownerID} {.status.tenantID} {.status.conditions}'
kubectl -n demo events --for netboxnetwork/demo
```

- `Ready=True`, `status.prefix` is the CIDR in NetBox, `status.ownerID` and `status.tenantID` are set when `spec.owner` or `spec.tenant` is.
- `MetadataSynced` is `True` when `customFields`, `tags`, owner, tenant, description and comments are on the prefix and the VIP.
- The reasons you meet most often while creating a network: `WaitingForCluster`, `ParentNotFound` (NetBox has not exactly one `parentPrefix` in the global VRF), `Exhausted` (no free prefix of `prefixLength` left in the parent), `OwnerNotFound`, `TenantNotFound`, `Forbidden` (the token lacks a permission). States that only a change in NetBox can clear are checked again every minute. All reasons: [Conditions](../reference/conditions.md) and [Troubleshooting](../operations/troubleshooting.md).

## Next

- [Pools](pools.md): ranges of the network that machines take addresses from.
- [Control-plane VIP](control-plane-vip.md) and [Metadata](metadata.md).
- [Deletion and retained objects](deletion-and-retained.md) and [clusterctl move and pivot](move-and-pivot.md).
- Recipes: [a shared VLAN](../recipes/shared-vlan.md), [migrating an existing cluster](../recipes/migrate-existing-cluster.md).
