# Conditions

Goal: look up a condition or a reason from `kubectl get` or `status.conditions` and know what it means and what to do. The commands to read them are in [Troubleshooting](../operations/troubleshooting.md).

## Condition types

| Object | Type | Meaning |
|---|---|---|
| `NetBoxNetwork` | `Ready` | `True` (`Ready`): the prefix is in NetBox as the spec asks and `spec.owner` and `spec.tenant` are resolved. Otherwise `False` with the reason of the failure; while the network is deleted, `Deleting` or the reason it is stuck |
| | `MetadataSynced` | `True` (`Synced`): `spec.customFields` and `spec.tags` are on the prefix, and owner, tenant, description and comments on the prefix and the VIP. `Unknown` (`WaitingForPrefix`) while `Ready` is not true. Never blocks `Ready` |
| | `ControlPlaneEndpointReady` | only with `spec.controlPlaneEndpoint`. `True` (`Ready`): the VIP is reserved and the Cluster has it. Never changes `Ready`. `Unknown` (`WaitingForPrefix`) while `Ready` is not true |
| | `Paused` | `True` (`Paused`) while the Cluster has `spec.paused` or the object has the `cluster.x-k8s.io/paused` annotation: the object is skipped. `False` (`NotPaused`) otherwise |
| `NetBoxIPPool` | `Ready` | `True` (`Ready`) while at least one range serves (and the `ipAddress` and `ipRange` templates are valid). The per-range state is in `status.ranges[]` |
| | `RangesReady` | `True` (`Ready`) when every range is in NetBox as the spec asks, owner, tenant, description and comments included; else `False` with the reasons of the ranges that are not. A range whose attributes only cannot be synced keeps serving |
| | `Paused` | as for the network; also paused by the Cluster of its `cluster.x-k8s.io/cluster-name` label or of any network it has a range in |
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
| `Duplicate` | network `Ready`, `ControlPlaneEndpointReady`; pool, range | more than one NetBox object carries the key, or the CIDR to adopt exists more than once or carries the key of another object (`held by ...`) | remove the duplicates in NetBox, or clear the other key of the prefix | every minute |
| `OwnerNotFound`, `TenantNotFound` | network `Ready` | `spec.owner` or `spec.tenant` names something NetBox does not have: a name, a slug or an id, for example after a rename, or outside the constrained view of the token | fix the name, slug or id; see [Troubleshooting](../operations/troubleshooting.md#ownernotfound-or-tenantnotfound) | every minute |
| `WaitingForCluster` | network `Ready`, `ControlPlaneEndpointReady` | the Cluster of the network does not exist yet: a network without a prefix touches nothing in NetBox (`Ready`), a new VIP is reserved when the Cluster appears (`ControlPlaneEndpointReady`). While deleting with `deletionPolicy: Delete`, the Cluster still has the VIP as its endpoint | create the Cluster; on deletion, wait for the Cluster to go | on change |
| `EndpointConflict` | network `ControlPlaneEndpointReady` | the Cluster has another host than the VIP or another port than `controlPlaneEndpoint.port` (it is not changed); the address meant for the VIP (the Cluster's endpoint or the VIP's old address) is taken, reserved or inside an IP range; the VIP was moved in NetBox; an older NetBox address holds the VIP's address too (the provider deletes its VIP, `VIPDeleted`, and picks another address next time); a newer one is only logged and the VIP stays | see [Control-plane VIP](../guides/control-plane-vip.md); free the address or fix the Cluster's endpoint | every minute |
| `WaitingForPrefix` | network `MetadataSynced`, `ControlPlaneEndpointReady` (`Unknown`) | metadata and the VIP wait for the prefix to be `Ready` | fix the reason of `Ready` | on change |
| `InvalidSpec` | network `Ready`, `MetadataSynced`; pool `Ready`, `RangesReady` | `customFields` sets a key field of the provider (`MetadataSynced`); a template does not render, contains a NUL character or gives an invalid DNS name: `description` or `comments` of a network or its `controlPlaneEndpoint` (`Ready`), `ipAddress` or `ipRange` of a pool; an `ipRange` template that fails only on the values of one range (`RangesReady`; the range keeps serving and its texts) | fix the spec | spec change |
| `NetworkNotReady` | pool, range | the network of the range is missing or not `Ready` (`OwnerNotFound`, `TenantNotFound`, `InvalidSpec` and the like); a new range waits for the VIP of its network | fix the network; nothing to do while it starts | on change |
| `MixedNetworks` | range | the network is in another L2 network (gateway and mask) than the network of the first range | one pool per L2 network ([Pools](../guides/pools.md)) | every minute |
| `InvalidRange` | range | the range covers the network, broadcast or gateway address of the L2 network, or the VIP, or lies outside its network | fix `start` and `end`; an active range keeps its old bounds meanwhile | every minute |
| `RangeInUse` | range | a change or removal would leave allocated addresses outside the range; the addresses are in the message | wait until the machines go, or keep the bounds | every minute |
| `RangeOverlap` | range | the range overlaps another NetBox range | shrink or remove the other range; it is retried | every minute |
| `Deleting` | network, pool `Ready` | deletion in progress | wait | on change |
| `WaitingForPools` | network `Ready` while deleting | a `NetBoxIPPool` still has a range in the network | delete or edit the pools | on change |
| `WaitingForAddresses` | pool `Ready` while deleting | `IPAddress` objects of the pool still exist | delete the claims | on change |
| `Forbidden` | network, pool, range | the token lacks a NetBox permission (HTTP 403), or a constraint of the permission does not match what the provider writes (for example the tenant, or the owner of a network without `spec.owner`), or excludes an object it changes or deletes: NetBox answers 404 then, and the message says `NetBox hides <object> from change` or `delete`. Also a name or slug of `spec.owner` or `spec.tenant` the token may not view | grant it ([NetBox access](../getting-started/netbox-access.md#permissions)); the message names the request | every minute |
| `NetBoxError` | network, pool, range | another NetBox error; the message has the details | read the message ([Troubleshooting](../operations/troubleshooting.md#netboxerror)) | backoff |
| `InternalError` | network, pool | a Kubernetes API call failed | check the API server and the RBAC of the provider | backoff |
| `PoolNotReady` | claim `Ready` | the pool is missing or has no ready range; the claim waits | fix the pool; a claim that has its address is not touched | on change; backoff while the pool is missing |
| `PoolExhausted` | claim `Ready` | no free address left in the ready ranges | add a range or free addresses | backoff |
| `AllocationFailed` | claim `Ready` | the allocation failed: a NetBox error or a 403, or a template of the pool that does not render for this claim | read the message | backoff |
| `Allocating` | claim `Ready` (`Unknown`) | the address is allocated, the `IPAddress` is being created | wait | on change |
| `AddressConflict` | claim `Ready` | the allocated address is held by another object in NetBox, so it cannot be created again | free the address in NetBox, or delete the claim | backoff |
| `Releasing` | claim `Ready` | the deleted claim waits for finalizers of others (they are in the message, for example CAPV's `ip-claim-protection` until the VM is gone) or for `addressReuseGracePeriodSeconds`; its address stays in NetBox | wait; fix the owner of the finalizer | every minute |
| `ReleaseFailed` | claim `Ready` | deleting the address in NetBox failed; the message has the error | for `Forbidden`, grant delete on `ipam.ipaddress`; it is retried | backoff |

A network that is not `Ready`, for example with `OwnerNotFound`, `TenantNotFound` or `InvalidSpec`, takes the ranges of its pools out of service: new claims get no address from them (`PoolNotReady` if the pool has no other ready range); addresses already allocated stay `Ready`; a pool edit or deletion still releases its ranges. A failed sync of owner, tenant, description or comments onto existing objects does not: it shows in `MetadataSynced` of the network and `RangesReady` of the pool.

## Next

- [Troubleshooting](../operations/troubleshooting.md): commands and recovery steps.
- [Observability](../operations/observability.md): events, metrics and logs.
- [API reference](api.md) and [NetBox objects](netbox-objects.md).
