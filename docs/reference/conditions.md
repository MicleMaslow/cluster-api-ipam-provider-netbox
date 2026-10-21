# Conditions

Goal: look up a condition or a reason from `kubectl get` or `status.conditions` and know what it means and what to do. The commands to read them are in [Troubleshooting](../operations/troubleshooting.md).

## Condition types

| Object | Type | Meaning |
|---|---|---|
| `NetBoxNetwork` | `Ready` | `True` (`Ready`): the prefix is in NetBox as the spec asks and `spec.owner` and `spec.tenant` are resolved. Otherwise `False` with the reason of the failure; while the network is deleted, `Deleting` or the reason it is stuck |
| | `MetadataSynced` | `True` (`Synced`): `spec.customFields` and `spec.tags` are on the prefix, and owner, tenant, description and comments on the prefix and the VIP. `Unknown` (`WaitingForPrefix`) while `Ready` is not true. Never blocks `Ready` |
| | `ControlPlaneEndpointReady` | only with `spec.controlPlaneEndpoint`. `True` (`Ready`): the VIP is reserved and the Cluster has it. Never changes `Ready`. `Unknown` (`WaitingForPrefix`) while `Ready` is not true |
| | `LayoutReady` | only with `spec.pools`, `spec.addresses` or a layout in the status. `True` (`Ready`): the pools and address groups are placed, and their ranges and reserved addresses are in NetBox as the layout says, owner, tenant, description and comments included; with `allocator: InCluster`, their `InClusterIPPool`s too; and the Secrets of `publish` are written. `Unknown` (`WaitingForPrefix`) while `Ready` is not true. Never changes `Ready`; a range whose attributes only cannot be synced keeps serving |
| | `Paused` | `True` (`Paused`) while the Cluster has `spec.paused` or the object has the `cluster.x-k8s.io/paused` annotation: the object is skipped. `False` (`NotPaused`) otherwise |
| `NetBoxIPPool` | `Ready` | `True` (`Ready`) while at least one range serves (and the `ipAddress` templates are valid). The per-range state is in `status.ranges[]` |
| | `RangesReady` | `True` (`Ready`) when every range is a pool placed in its network, in NetBox, and served by this pool; else `False` with the reasons of the ranges that are not |
| | `Paused` | as for the network; also paused by the Cluster of its `cluster.x-k8s.io/cluster-name` label or of any network of its ranges |
| `IPAddressClaim` | `Ready` | `True` (`Ready`): the `IPAddress` exists. `Unknown` (`Allocating`): the address is allocated, the `IPAddress` is being created. Otherwise `False` with a reason from the table |

## How a reason is rechecked

| Recheck | Meaning |
|---|---|
| on change | the object waits for something Kubernetes can see (a Cluster, a pool, a network, an address); a watch wakes it |
| every minute | the state can only change in NetBox; the object is checked again every minute |
| backoff | an unexpected error; retried with a delay that grows from 5 ms to 1 minute |
| spec change | the spec is wrong; the object is not retried until it changes |

## Reasons

`Ready` and `Synced` are the reasons of a true condition. The others:

| Reason | Where | Meaning | What to do | Recheck |
|---|---|---|---|---|
| `ParentNotFound` | network `Ready` | NetBox has not exactly one `parentPrefix` in the global VRF | create the parent prefix, or remove the duplicate or the VRF from it | every minute |
| `Exhausted` | network `Ready`; `ControlPlaneEndpointReady` | no free prefix of `prefixLength` is left in the parent (`Ready`), or no free address for the VIP in the prefix | free space in NetBox, or use a larger parent; for the VIP, free an address of the prefix | every minute |
| `PrefixMismatch` | network `Ready`; `ControlPlaneEndpointReady` | the NetBox prefix is outside the parent or not the length or CIDR of the spec; the NetBox address of the VIP is outside the prefix | put the object back in NetBox, or recreate the network with the right spec (its fields are immutable) | every minute |
| `PrefixNotFound` | network `Ready` | the prefix in the status is gone from NetBox or no longer carries the key (the instance name or the key field changed), or the status is empty and the active VIP of the network is there; it is not carved again | restore the prefix with its key in NetBox ([Troubleshooting](../operations/troubleshooting.md#a-prefix-is-gone)) | every minute |
| `Duplicate` | network `Ready`, `ControlPlaneEndpointReady`, `LayoutReady`; pool, range | more than one NetBox object carries the key, or the CIDR to adopt exists more than once or carries the key of another object (`held by ...`), or an `InClusterIPPool` of the name of a pool is controlled by something else (`exists and is not managed by network ...`, `LayoutReady`; one without a controller is adopted), every minute; another `NetBoxIPPool` created first takes the same network pool (pool, range), on change | remove the duplicates in NetBox, or clear the other key of the prefix; one `NetBoxIPPool` per network pool | every minute; on change |
| `OwnerNotFound`, `TenantNotFound` | network `Ready` | `spec.owner` or `spec.tenant` names something NetBox does not have: a name, a slug or an id, for example after a rename, or outside the constrained view of the token | fix the name, slug or id; see [Troubleshooting](../operations/troubleshooting.md#ownernotfound-or-tenantnotfound) | every minute |
| `WaitingForCluster` | network `Ready`, `ControlPlaneEndpointReady` | the Cluster of the network does not exist yet: a network without a prefix touches nothing in NetBox (`Ready`), a new VIP is reserved when the Cluster appears (`ControlPlaneEndpointReady`). While deleting with `deletionPolicy: Delete`, the Cluster still has the VIP as its endpoint | create the Cluster; on deletion, wait for the Cluster to go | on change |
| `EndpointConflict` | network `ControlPlaneEndpointReady` | the Cluster has another host than the VIP or another port than `controlPlaneEndpoint.port` (it is not changed); the address meant for the VIP (the Cluster's endpoint or the VIP's old address) is taken, reserved or inside an IP range (one NetBox address without a key alone at the Cluster's endpoint is adopted instead, unless the status has a VIP); the VIP was moved in NetBox; an older NetBox address holds the VIP's address too (the provider deletes its VIP, `VIPDeleted`, and picks another address next time); a newer one is only logged and the VIP stays | see [Control-plane VIP](../guides/control-plane-vip.md); free the address or fix the Cluster's endpoint | every minute |
| `WaitingForPrefix` | network `MetadataSynced`, `ControlPlaneEndpointReady`, `LayoutReady` (`Unknown`) | metadata, the VIP and the layout wait for the prefix to be `Ready` | fix the reason of `Ready` | on change |
| `InvalidSpec` | network `Ready`, `MetadataSynced`, `LayoutReady`; pool `Ready` | `customFields` sets a key field of the provider (`MetadataSynced`); a template does not render, contains a NUL character or gives an invalid DNS name: `description` or `comments` of a network or its `controlPlaneEndpoint`, `controlPlaneEndpoint.dnsName` or `dnsName` or `description` of an address group, for any index (`Ready`), `ipAddress` of a pool; `spec.pools` and `spec.addresses` do not fit in the free addresses of the prefix (the message has the numbers), a pool would take more than 32 ranges, or, after the status was lost, the pools do not match the ranges found in NetBox (`sizes must be those found: ...`); with `allocator: InCluster` and no ranges, the pools do not match the `InClusterIPPool`s of their names (`spec.pools do not match the InClusterIPPools of the network: sizes must be those found: ...`) or one of those spans addresses outside the prefix, or two of them overlap; or the pools and groups publish in more than 16 Secrets (`LayoutReady`; nothing changes) | fix the spec | spec change |
| `NetworkNotReady` | pool, range | the network of the range is missing or not `Ready` (`OwnerNotFound`, `TenantNotFound`, `InvalidSpec` and the like); its pool is not in its `spec.pools` and `status.layout` (`pool X is not placed in network Y`); the network has not created all the ranges of the pool yet | fix the network or the name of the pool; nothing to do while it starts | on change |
| `MixedNetworks` | range | the network is in another L2 network (gateway and mask) than the network of the first range | one pool per L2 network ([Pools](../guides/pools.md)) | every minute |
| `RangeInUse` | network `LayoutReady` | a change of `spec.pools` or `spec.addresses` (a larger `count`), or a VIP added while `rest` reaches the top, would move allocated addresses to another pool or out of every pool, or reserve one for a group, or a range would give up an address allocated meanwhile, or the webhook of in-cluster refuses a change of an `InClusterIPPool` (its message: `pool addresses do not contain allocated addresses`); nothing is changed, the addresses are in the message | wait until the machines go, or keep the pools as they are | every minute |
| `RangeOverlap` | network `LayoutReady` | a range of a pool overlaps a NetBox range the layout did not see, such as one that crosses the boundary of the L2 network | shrink or remove the other range; it is retried | every minute |
| `Deleting` | network, pool `Ready` | deletion in progress | wait | on change |
| `WaitingForPools` | network `Ready` while deleting | a `NetBoxIPPool` still takes a pool of the network; an `InClusterIPPool` of the network is not gone: the webhook of in-cluster refuses its deletion while addresses are allocated from it (`InClusterIPPool X: ... Pool has IPAddresses allocated. Cannot delete Pool until all IPAddresses have been removed.`, every minute), or a finalizer holds it (`InClusterIPPool X still exists`, on change). The other pools are deleted meanwhile | delete or edit the pools; let the machines go | on change; every minute while the webhook refuses |
| `AllocatorUnavailable` | network `LayoutReady`; network `Ready` while deleted | `allocator: InCluster`, and the `InClusterIPPool` CRD was not served when the provider started; the layout writes nothing. A network deleted once the CRD is served waits and releases nothing in NetBox | install cluster-api-ipam-provider-in-cluster and restart the provider ([In-cluster allocator](../guides/in-cluster.md)) | after a restart |
| `InvalidRange` | range | the network of the range has `allocator: InCluster` (`network X hands out addresses through InClusterIPPools`) | point the machines at its `InClusterIPPool`, or use a network with the `NetBox` allocator | on change |
| `WaitingForAddresses` | pool `Ready` while deleting | `IPAddress` objects of the pool still exist | delete the claims | on change |
| `Forbidden` | network | the token lacks a NetBox permission (HTTP 403), or a constraint of the permission does not match what the provider writes (for example the tenant, or the owner of a network without `spec.owner`), or excludes an object it changes or deletes: NetBox answers 404 then, and the message says `NetBox hides <object> from change` or `delete`. Also a name or slug of `spec.owner` or `spec.tenant` the token may not view | grant it ([NetBox access](../getting-started/netbox-access.md#permissions)); the message names the request | every minute |
| `NetBoxError` | network | another NetBox error; the message has the details | read the message ([Troubleshooting](../operations/troubleshooting.md#netboxerror)) | backoff |
| `InternalError` | network, pool | a Kubernetes API call failed | check the API server and the RBAC of the provider | backoff |
| `PoolNotReady` | claim `Ready` | the pool is missing or has no ready range; the claim waits | fix the pool; a claim that has its address is not touched | on change; backoff while the pool is missing |
| `PoolExhausted` | claim `Ready` | no free address left in the ready ranges | add a range or free addresses | backoff |
| `AllocationFailed` | claim `Ready` | the allocation failed: a NetBox error or a 403, or a template of the pool that does not render for this claim | read the message | backoff |
| `Allocating` | claim `Ready` (`Unknown`) | the address is allocated, the `IPAddress` is being created | wait | on change |
| `AddressConflict` | claim `Ready`; network `LayoutReady` | the allocated address is held by another object in NetBox, so it cannot be created again (claim); an address in use in NetBox inside a pool of a network with `allocator: InCluster` is allocated from its `InClusterIPPool`: it stays in the pool, the rest is applied, the addresses are in the message (network) | free the address in NetBox, or delete the claim or let the machine go | backoff; every minute |
| `SecretConflict` | network `LayoutReady` | a Secret that `publish` names exists and the network does not control it (no owner reference, or another controller); it is not changed, the other Secrets are written | delete the Secret, or name another in `publish` | every minute |
| `Releasing` | claim `Ready` | the deleted claim waits for finalizers of others (they are in the message, for example CAPV's `ip-claim-protection` until the VM is gone) or for `addressReuseGracePeriodSeconds`; its address stays in NetBox | wait; fix the owner of the finalizer | every minute |
| `ReleaseFailed` | claim `Ready` | deleting the address in NetBox failed; the message has the error | for `Forbidden`, grant delete on `ipam.ipaddress`; it is retried | backoff |

A network that is not `Ready`, for example with `OwnerNotFound`, `TenantNotFound` or `InvalidSpec`, takes the ranges of its pools out of service: new claims get no address from them (`PoolNotReady` if the pool has no other ready range); addresses already allocated stay `Ready`; its layout waits. A failed sync of owner, tenant, description or comments onto existing objects does not: it shows in `MetadataSynced` and `LayoutReady` of the network.

## Next

- [Troubleshooting](../operations/troubleshooting.md): commands and recovery steps.
- [Observability](../operations/observability.md): events, metrics and logs.
- [API reference](api.md) and [NetBox objects](netbox-objects.md).
