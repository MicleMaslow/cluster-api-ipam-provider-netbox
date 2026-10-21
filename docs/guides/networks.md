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
  pools:
  - {name: cp, size: 6}
  - {name: workers, size: rest}
```

- `clusterName`, `parentPrefix` (an existing NetBox prefix the cluster prefix is carved from or adopted in) and `gateway` of the L2 network. `parentPrefix` may be the prefix of the whole VLAN or a logical pool for CAPI inside it; the gateway may lie outside a logical parent.
- `addressPrefixLength` is the mask of the nodes' subnet. The L2 network is `gateway/addressPrefixLength` with the host bits cleared; without the field it is `parentPrefix`. Addresses handed out get this mask and the gateway (`status.addressPrefixLength`). The network and broadcast address of the L2 network and the gateway are reserved: a pool or the VIP never covers them. Pools take ranges of networks in one L2 network only (`MixedNetworks` compares gateway and mask), so networks of different logical parents of one VLAN can share a pool.
- `prefixLength` carves a free prefix of that length out of `parentPrefix`; `prefix` instead takes a given CIDR inside it. Exactly one of the two, immutable. An existing prefix is adopted if it carries no key, or the key of this network; a prefix with another key is `Duplicate` (`held by ...`). A deprecated prefix a deleted network of the same namespace left is adopted, and made active again. A missing one is created. Adoption writes only the key; status, description, comments, owner, tenant and the rest stay as the NetBox admin set them. The prefix has to lie in your zone, since the token must be allowed to change it.
- `isPool: true` sets `is_pool` on the prefix the provider carves, so NetBox counts all its addresses as usable hosts, and keeps it in sync; the default is false. It needs `prefixLength`: an adopted prefix is never changed.
- `pools` lay out the prefix: [Layout](#layout).
- `addresses` reserve named groups of addresses at the top of the prefix: [Reserved addresses](#reserved-addresses).
- `publish` on a pool or a group writes its addresses to a Secret: [Published addresses](#published-addresses).
- `deletionPolicy` says what happens to the prefix, the ranges of its pools, the reserved addresses and the VIP when the network goes: `Retain` (default) or `Delete`. See [Deletion and retained objects](deletion-and-retained.md).
- `controlPlaneEndpoint` reserves the VIP of the cluster: [Control-plane VIP](control-plane-vip.md).
- `customFields` and `tags` are set on the prefix: [Metadata](metadata.md).
- `owner` and `tenant`: see below.
- `description` and `comments` are templates of the text of the prefix, with defaults: [Metadata](metadata.md#description-and-comments).

The CRD rejects:

- an L2 network that does not contain `parentPrefix`, an `addressPrefixLength` longer than the `parentPrefix` mask, a `prefix` outside `parentPrefix`, a `prefixLength` not longer than the `parentPrefix` mask;
- an IPv6 `parentPrefix` or `gateway`;
- `isPool` without `prefixLength`, an adopted `prefix` with `deletionPolicy: Delete`, none or both of `prefix` and `prefixLength`;
- a pool name that is not a DNS-1123 label, a `size` that is not 1-65535 or `rest`, more than one pool of size `rest`;
- an address group name that is not a DNS-1123 label or is the name of a pool, a `count` that is not 1-64, more than 8 groups;
- `publish` with `enabled: true` and no `secretName` (`enabled needs secretName`), a `secretName` that is not a DNS-1123 subdomain;
- a change of `clusterName`, `parentPrefix`, `gateway`, `prefixLength`, `prefix`, a change of `addressPrefixLength`, adding or removing it, and a switch between `prefix` and `prefixLength`;
- a change of `controlPlaneEndpoint.port`, adding or removing it, while `controlPlaneEndpoint` is set (the whole field can be removed).

All immutable fields of both kinds are in the [API reference](../reference/api.md#immutable-fields).

## Layout

`pools` lay out the prefix: an ordered list of pools, each of a number of addresses or `rest`. A `NetBoxIPPool` hands out the addresses of a pool ([Pools](pools.md)).

```yaml
spec:
  pools:
  - {name: cp, size: 6}
  - {name: ingress, size: 8}
  - {name: workers, size: rest}
```

- **Free addresses.** A size counts free addresses of the prefix: not the network or broadcast address of the L2 network, not the gateway, not the VIP, not a reserved address of the network, not an address or inside a range NetBox holds for anything but this network and its claims. An address taken inside the span of a pool splits its range in NetBox around it.
- **Order.** Pools are placed from the start of the prefix in the order of the list, each on the next free addresses. The VIP and the [reserved addresses](#reserved-addresses) take the top: the VIP the highest free address, the groups the next ones down. `rest` takes the free addresses the other pools, the VIP and the groups leave, at least one; at most one pool is `rest`, not necessarily the last. Until the VIP is reserved the layout keeps its address free ([Control-plane VIP](control-plane-vip.md)).
- **Fixed.** The placement is recorded in `status.layout` with the NetBox ids of the ranges, and placed again only when the names or sizes of `pools` or the names or counts of `addresses` change, or while `controlPlaneEndpoint` is set and the VIP is not reserved yet, which places it again on every reconcile against what NetBox holds then. Otherwise a change in NetBox does not move it. The pools in front of the first changed one keep their ranges as they are; that one, those after it and `rest` are placed again. Reserved addresses stay where they are.
- **Refused changes.** A change that would move an allocated address (an `IPAddress` of the namespace, or an address NetBox holds for a claim) to another pool or out of every pool, or reserve it for a group, is refused as a whole: nothing is written, `LayoutReady=False` with `RangeInUse` lists the addresses, checked again every minute. On a live cluster that leaves the tail free to change: the `rest` pool, a new pool at the end, pools without allocated addresses, the count of a group. Pools and groups that do not fit in the prefix are `InvalidSpec` with the numbers: nothing changes.
- **In NetBox.** Each pool is one or more IP ranges the network owns, all under the key `[<instance>/]NetBoxNetwork/<namespace>/<network>/pool/<pool>`, active, with the owner and tenant of the network ([NetBox objects](../reference/netbox-objects.md)). Ranges shrink, move and go before others grow or are created; a range gives up addresses only once none of them is allocated (it is closed to allocation with `mark_populated` and checked again). The ranges of a pool removed from the list are deleted.
- **Status lost** (`clusterctl move`, a restore): the network takes its layout back from its active ranges and its reserved addresses in NetBox, found by key; nothing is placed again or created. The pools in the spec must match the ranges, otherwise `InvalidSpec` names the sizes found (`sizes must be those found: cp 3, workers 13`). Ranges a deleted network of the same name retained (`deprecated`) are deleted, and the pools placed anew; its reserved addresses inside the prefix are taken back.
- `LayoutReady` reports the layout and never changes `Ready`. Without `pools` and `addresses` the network has no ranges, no reserved addresses and no `LayoutReady`.

## Reserved addresses

`addresses` reserve named groups of addresses in NetBox, for addresses a cluster needs outside its pools, such as MetalLB:

```yaml
spec:
  controlPlaneEndpoint: {dnsName: "{{ .Cluster }}-controlplane.example.test"}
  addresses:
  - {name: metallb, count: 2, dnsName: "{{ .Cluster }}-metallb-{{ .Index }}.example.test"}
```

- **Place.** A group takes `count` free addresses outside the pools, from the top of the prefix down, under the VIP; groups follow the order of the list. `.Index` numbers the addresses of a group from the top down from 1: in a `/26` from `10.10.0.0` with the VIP on `.63`, `metallb` 1 is `.62` and 2 is `.61`. `status.layout.addresses` lists them in that order. A group placed for the first time takes NetBox addresses without a key above the pools and under the VIP first (never the one at the Cluster's endpoint), from the top down, as a running cluster has them for MetalLB: they keep their objects and get the key and fields of the group ([Migrating an existing cluster](../recipes/migrate-existing-cluster.md)).
- **Free for nothing else.** An address placed in `status.layout.addresses` is never taken by a new VIP, even before it is created in NetBox. A template of a group that does not render, for any index, is `Ready=False` with `InvalidSpec`, like a bad `controlPlaneEndpoint.dnsName`: nothing is written.
- **In NetBox.** Each address is an active IP address under the key `[<instance>/]NetBoxNetwork/<namespace>/<network>/addresses/<group>/<index>`, with the mask of the L2 network, no role, the owner and tenant of the network. `dnsName` (without it there is no `dns_name`) and `description` are templates over `.Cluster`, `.Namespace`, `.Network`, `.Group`, `.Index`, with the rules of the [pool templates](metadata.md#description-and-comments); the default description is `{{ .Namespace }}/{{ .Network }} // {{ .Cluster }} {{ .Group }} {{ .Index }}`, comments are the default ones. All of it is kept in sync: an edit by hand is overwritten.
- **Fixed.** Placed addresses keep their place while the group stays. A group that grows takes the highest free addresses left, which usually come out of `rest`; one that shrinks gives up its last addresses (the highest indexes).
- **Order.** A change is applied in order, and a step that fails stops the next ones: reserved addresses the layout no longer holds are deleted, then the ranges of the pools shrink and grow, then new reserved addresses are created. Shrinking `rest` for a larger count is refused like any change that moves an allocated address: `RangeInUse`, the address is not reserved, NetBox does not change.
- **Smaller count.** An address the group gives up is deleted from NetBox whatever `deletionPolicy` says (the policy is for the deletion of the network), so that a pool can take it.
- **Deletion of the network.** The reserved addresses go with the VIP, as `deletionPolicy` says: deleted, or `deprecated` under `Retain`. A new network of the same name takes them back, active again, at their addresses; those outside its prefix are retired (`<key>/retired`, still `deprecated`) and new ones reserved ([Deletion and retained objects](deletion-and-retained.md#retained-objects)).

## Published addresses

A pool or a group of reserved addresses can write its addresses to a Secret, for a tool that carries them out of the cluster, such as External Secrets Operator to Vault. It is off until you opt in, per pool or group:

```yaml
spec:
  pools:
  - {name: c1-ingress, size: 8, publish: {enabled: true, secretName: cluster-endpoints-c1}}
  addresses:
  - {name: metallb, count: 2, publish: {enabled: true, secretName: cluster-endpoints-c1}}
```

- **Value.** The key is the name of the pool or group. The value is its addresses without a mask, ascending, one per line, no trailing newline:

  ```text
  10.10.0.61
  10.10.0.62
  ```

  A pool's value is the addresses of its ranges, less the VIP and less the addresses the `InCluster` allocator excludes because they are in use in NetBox ([In-cluster allocator](in-cluster.md#what-the-network-writes)); a group's value is its addresses. An address that is allocated to a machine but in use in NetBox stays in the pool, so it stays in the value.
- **One Secret, several pools and groups.** Pools and groups that name the same `secretName` are keys of the same Secret. The Secret exists only while at least one enabled pool or group names it, and holds only their keys: a pool or group that turns `enabled` off loses its key, and the last one takes the Secret with it. `enabled` unset or `false` publishes nothing.
- **The Secret belongs to one network.** It is `Opaque`, in the namespace of the network, with the network as its controller (an owner reference), so it goes with the network. Two networks must not name the same Secret: one that exists and is not controlled by the network (no owner reference, or another owner) is not touched, and `LayoutReady` is `False` with `SecretConflict`, checked again every minute; the rest of the layout is applied and the other Secrets are written. Delete the Secret or name another. `status.publishedSecrets` lists the Secrets the network wrote; one that no pool or group names any more is deleted if the network controls it.
- **When.** Only after the layout is applied: the ranges, the `InClusterIPPool`s and the reserved addresses are as the layout says. While a change is refused (`RangeInUse`) or a step fails, the Secrets keep the values last published. A Secret is written only when its data differs. The provider does not watch Secrets (it reads them directly and holds no cache of them): one edited or deleted by hand is put right at the next reconcile of the network, within `--sync-period` (10 minutes by default).
- **RBAC.** The ClusterRole of the manifests grants `get`, `create`, `update` and `delete` on `secrets` in every namespace, whether or not a network publishes: networks can be in any namespace, and a Secret to create has no name to grant on yet. The provider touches only the Secrets that `publish` names, in the namespace of the network: it reads them past its cache (no `list` or `watch`, so it holds no Secret in memory), and writes and deletes only those the network controls. The grant is still wider than that: the service account of the provider can read any Secret of the management cluster, Cluster kubeconfigs and infrastructure credentials included, so guard it as you guard those of the other Cluster API controllers. And whoever may create a `NetBoxNetwork` in a namespace can have the provider create a Secret of any free name there, holding addresses only.

External Secrets Operator pushes such a Secret to a store with a `PushSecret`, one entry per key:

```yaml
apiVersion: external-secrets.io/v1alpha1
kind: PushSecret
metadata: {name: cluster-endpoints-c1, namespace: c1}
spec:
  refreshInterval: 1h
  secretStoreRefs:
  - {name: vault, kind: ClusterSecretStore}
  selector:
    secret: {name: cluster-endpoints-c1}
  data:
  - match:
      secretKey: c1-ingress
      remoteRef: {remoteKey: clusters/c1/endpoints, property: c1-ingress}
  - match:
      secretKey: metallb
      remoteRef: {remoteKey: clusters/c1/endpoints, property: metallb}
```

## Owner and tenant

Both are optional attributes of the NetBox objects of the network. They are set on the network and are not part of the identity: an object is the provider's by its key alone.

```yaml
spec:
  owner:  {name: zone-a-owner}   # or {id: 7}
  tenant: {slug: platform}       # or {id: 42}
```

- Each field has exactly one form (CEL): `name` or `id` for the owner, `slug` or `id` for the tenant. Name and slug are readable and the same in every NetBox installation (dev, stage, prod); an id survives a rename ([NetBox access](../getting-started/netbox-access.md#owner-and-tenant) tells which one needs which view permission).
- **Where they go.** The prefix, the VIP and the ranges of the pools get the owner and tenant of the network; the address of a claim gets those of the network it lies in, so a `NetBoxIPPool` over pools of networks of different owners works.
- **The spec is the source of truth.** The values are written when an object is created and synced on the existing ones: the network does it for its prefix, VIP and ranges, a claim for its address when it is checked again (a changed owner or tenant of the network wakes its claims). A change in the spec reaches existing objects, a value edited in NetBox by hand is overwritten, and an Event `AttributesSynced` records the change. A prefix under `spec.prefix` is the exception: it keeps its owner and tenant after creation, so what the admin set stays.
- **Unset is untouched.** Without `spec.owner` the provider writes no owner and does not clear one on an existing object, and the same for the tenant. Removing the field from the spec leaves the NetBox objects as they are.
- **The status is a cache.** The network looks the name or slug up (or checks the id) and records the NetBox ids in `status.ownerID` and `status.tenantID` for its pools and claims. It asks NetBox again on a new generation (the spec changed), when no id is cached, or when the reason of `Ready` is `WaitingForCluster`, `InvalidSpec`, `NetBoxError`, `Forbidden`, `OwnerNotFound` or `TenantNotFound`; any other state keeps the cache. A rename in NetBox shows on the next resolve, after a spec change or a `NetBoxError` or `Forbidden` on `Ready`: give the `id` to survive renames. The ids carry no meaning of their own: clear them or change the spec, and the next reconcile resolves again. A 403 on checking an id accepts it as given (the token has no view on owners or tenants).
- **Not found.** A name, slug or id that NetBox does not have is `Ready=False` with `OwnerNotFound` or `TenantNotFound`, checked again every minute. The `NetBoxIPPool`s of the network take its ranges out of service like those of any network that is not `Ready`: a new claim gets no address from them (`PoolNotReady` if the pool has no other ready range), the addresses already allocated stay `Ready`. The layout waits too (`LayoutReady` is `Unknown`).
- **A sync that fails** does not stop the network. When the token cannot set the value (the constraint of its permission does not allow it, `Forbidden`) the condition `MetadataSynced` of the network reports it, `LayoutReady` for its ranges, and a claim keeps its `Ready` and retries with backoff; what is already allocated or served goes on.
- **A constrained token.** If a write permission is constrained by owner or tenant, set the same on the network before it creates anything: a network without `spec.owner` creates objects without an owner, and NetBox refuses them ([NetBox access](../getting-started/netbox-access.md#permissions)).
- After `clusterctl move` the status is empty and the ids are found again from the spec ([clusterctl move and pivot](move-and-pivot.md)).
- Deleting a network does not depend on owner or tenant: the objects are found by key ([Deletion and retained objects](deletion-and-retained.md)).

## The Cluster, pause and deletion

A network whose Cluster (`clusterName`) does not exist and that has no prefix yet touches nothing in NetBox, with or without `controlPlaneEndpoint`: it is `Ready=False` with `WaitingForCluster` until the Cluster appears. After `clusterctl move` or a restore the network has no status, and until its Cluster exists (and is not paused) it must not act on NetBox; for a VIP the Cluster may also come with an endpoint of its own that decides the address. A network that already holds its prefix works without the Cluster. The provider watches Clusters, so the order of applying the manifests does not matter.

A network is paused when its Cluster has `spec.paused: true` or the network has the `cluster.x-k8s.io/paused` annotation: it is skipped, nothing is written to NetBox or to the Cluster, and its condition `Paused` is True. `--watch-filter` is checked for networks as it is for pools.

A network being deleted waits while a `NetBoxIPPool` still takes one of its pools (`WaitingForPools`). Then the ranges of its pools are released as `deletionPolicy` says, before the prefix, the VIP and the reserved addresses.

Do not give a `NetBoxNetwork` an `ownerReference` with `blockOwnerDeletion` to its Cluster: with foreground deletion of the Cluster, the network's wait for the Cluster (`deletionPolicy: Delete`) and the Cluster's wait for the network would block each other. No owner reference is needed: the CRDs carry the `clusterctl move` hierarchy label.

## Check

```bash
kubectl get netboxnetworks -A
kubectl -n demo get netboxnetwork demo -o jsonpath='{.status.prefix} {.status.ownerID} {.status.tenantID} {.status.conditions}'
kubectl -n demo events --for netboxnetwork/demo
```

- `Ready=True`, `status.prefix` is the CIDR in NetBox, `status.ownerID` and `status.tenantID` are set when `spec.owner` or `spec.tenant` is.
- `MetadataSynced` is `True` when `customFields`, `tags`, owner, tenant, description and comments are on the prefix and the VIP.
- `LayoutReady` is `True` when the pools and groups are placed and their ranges and reserved addresses are in NetBox, and the Secrets they publish in are written; `status.layout` lists the ranges of each pool with their NetBox ids and the addresses of each group.
- The reasons you meet most often while creating a network: `WaitingForCluster`, `ParentNotFound` (NetBox has not exactly one `parentPrefix` in the global VRF), `Exhausted` (no free prefix of `prefixLength` left in the parent), `OwnerNotFound`, `TenantNotFound`, `Forbidden` (the token lacks a permission). States that only a change in NetBox can clear are checked again every minute. All reasons: [Conditions](../reference/conditions.md) and [Troubleshooting](../operations/troubleshooting.md).

## Next

- [Pools](pools.md): pools of the network that machines take addresses from.
- [Control-plane VIP](control-plane-vip.md) and [Metadata](metadata.md).
- [Deletion and retained objects](deletion-and-retained.md) and [clusterctl move and pivot](move-and-pivot.md).
- Recipes: [a shared VLAN](../recipes/shared-vlan.md), [migrating an existing cluster](../recipes/migrate-existing-cluster.md).
