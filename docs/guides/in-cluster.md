# In-cluster allocator

Goal: let the network lay out its prefix in NetBox while the addresses of the machines come from [cluster-api-ipam-provider-in-cluster](https://github.com/kubernetes-sigs/cluster-api-ipam-provider-in-cluster): the network creates an `InClusterIPPool` per pool of its layout and keeps it in sync. Machines keep working when NetBox is down; NetBox still holds the prefix, the ranges of the pools, the VIP and the reserved addresses.

## Steps

1. **Install in-cluster IPAM before the provider.** The provider checks for the `InClusterIPPool` CRD once, at start. Installed later, it needs a restart of the provider: `kubectl -n capi-ipam-netbox-system rollout restart deployment/capi-ipam-netbox-controller-manager`. Until then a network with `allocator: InCluster` holds its prefix and VIP but places nothing: `LayoutReady=False` with `AllocatorUnavailable`, no ranges, no reserved addresses, no `InClusterIPPool`s; networks with the `NetBox` allocator work as before ([Install](../getting-started/install.md#in-cluster-ipam)).
2. **Create the network** with `allocator: InCluster`:

   ```yaml
   apiVersion: ipam.cluster.x-k8s.io/v1alpha1
   kind: NetBoxNetwork
   metadata: {name: c1, namespace: c1}
   spec:
     clusterName: c1
     parentPrefix: 10.10.0.0/22
     gateway: 10.10.0.1
     addressPrefixLength: 22
     prefixLength: 26
     allocator: InCluster
     controlPlaneEndpoint: {port: 6443, dnsName: "{{ .Cluster }}-controlplane.example.test"}
     pools:
     - {name: c1-controlplane, size: 6}
     - {name: c1-ingress, size: 8}
     - {name: c1-worker, size: rest}
     addresses:
     - {name: metallb, count: 2, dnsName: "{{ .Cluster }}-metallb-{{ .Index }}.example.test"}
   ```

   `allocator` is `NetBox` (the default) or `InCluster`, for the whole network. It is immutable and cannot be added or removed: another allocator is another network.
3. **Point the machines at the pools.** The name of a pool is the name of its `InClusterIPPool` in the namespace of the network, as it is: you choose the names, for example `<cluster>-<pool>`; the provider makes none up. With CAPV:

   ```yaml
   addressesFromPools:
   - {apiGroup: ipam.cluster.x-k8s.io, kind: InClusterIPPool, name: c1-worker}
   ```

## What the network writes

The layout is the same as with the `NetBox` allocator ([Layout](networks.md#layout)): placed in `status.layout`, each pool one or more NetBox ranges under the network's key.

- **`InClusterIPPool`.** One per pool, named as the pool, controlled by the network (an owner reference). Its spec is written whole and rewritten on any difference:

  | Field | Value |
  |---|---|
  | `addresses` | `["<first>-<last>"]`: from the first address of the pool's ranges to the last |
  | `prefix`, `gateway` | the mask and gateway of the L2 network (`addressPrefixLength`, `gateway`) |
  | `excludedAddresses` | the addresses between the ranges, and those in use in the span: NetBox addresses and ranges of anything but this network, the VIP, the reserved addresses. Ascending, `a` or `a-b`. In an adopted pool, one at the first or last address of its span is cut off instead: `addresses` starts after it or ends before it |
  | `addressReuseGracePeriodSeconds` | `60` |
  | `allocateReservedIPAddresses` | unset |

  A pool the network no longer has is deleted.
- **Ranges in NetBox** are `mark_populated`: NetBox hands out nothing inside them, the `InClusterIPPool` does.
- **An address taken later.** The layout is fixed, so an address that appears in NetBox inside a placed pool does not move the ranges: it goes to `excludedAddresses`. If it is allocated already, it stays in the pool, and the network reports `LayoutReady=False` with `AddressConflict` and the address; the rest is applied. Free it in NetBox or let the machine go. NetBox refuses to create an address inside a `mark_populated` range, but not to move one there.
- **Published addresses.** With `publish` on a pool, the Secret holds the pool's addresses without those the pool excludes (and without the VIP), so an address in use in NetBox is not offered to the consumer; one that is allocated stays in the pool and in the value ([Published addresses](networks.md#published-addresses)).
- **A pool of the same name without a controller**, as a chart renders it, is adopted: the network becomes its controller and writes its spec whole (the Event `PoolAdopted`). A pool of the name that another controller holds is `Duplicate`: it is not changed, and the steps after it wait.
- **Positions of existing pools.** A network without a layout in its status and without ranges in NetBox (a running cluster taken over) places the pools of `spec.pools` that have an `InClusterIPPool` of their name, controlled by nothing or by the network, where those are: each on the free addresses of its `addresses` (every entry: `a-b`, a CIDR or an address), less its `excludedAddresses` and the addresses in use, which stay excluded with the addresses between the entries; one at the first or last address cuts `addresses` instead. They must be the first pools of the list, in the order of their addresses, with the sizes found: otherwise `LayoutReady=False` with `InvalidSpec` (`spec.pools do not match the InClusterIPPools of the network: sizes must be those found: ...`), and nothing changes. An entry outside the prefix is `InvalidSpec` too, and so are two pools whose addresses overlap or interleave (`InClusterIPPools <a> and <b> overlap`): they would hand out one address. The other pools are placed after them, and groups placed for the first time adopt the NetBox addresses without a key above them and under the VIP ([Migrating an existing cluster](../recipes/migrate-existing-cluster.md)).

## Addresses of the machines in NetBox

With `sync` on a pool, every address its `InClusterIPPool` hands out (an `IPAddress` of the namespace whose `poolRef` is that pool) gets a NetBox IP address, so that NetBox shows which machine has which address:

```yaml
pools:
- name: c1-worker
  size: rest
  sync:
    enabled: true
    ipAddress: {dnsName: "{{ .Owner }}.nodes.example.test"}
```

- **What is written.** The address with the mask of the L2 network, `status: active`, the key `[<instance>/]NetBoxNetwork/<namespace>/<network>/sync/<pool>/<IPAddress>`, `dns_name`, description and comments from `ipAddress`, owner and tenant of the network. The templates and their defaults are those of a `NetBoxIPPool` ([Metadata](metadata.md#description-and-comments)): `.Owner` is the first owner of the claim of the `IPAddress` (the machine, e.g. the `VSphereVM`), `.Claim` the claim, `.Pool` the pool. A template that does not render is `LayoutReady=False` with `InvalidSpec`.
- **Kept in sync.** `dns_name`, description, comments, owner and tenant follow the spec; an address moved by hand is put back.
- **Deleted** with its `IPAddress`, when `sync` is turned off, and with the network, whatever `deletionPolicy` says. An address a new `IPAddress` takes at once (in-cluster reuses it) keeps one NetBox address: the old one is deleted right before the new one is created.
- **An address of someone else.** A NetBox address at the address of an `IPAddress` that does not carry a sync key of the network (no key, a claim's, another network's) is left alone: `LayoutReady=False` with `AddressConflict` and the `IPAddress` in the message. Delete it in NetBox, or give it the key, to let the sync take the address.
- **Inside the ranges.** NetBox refuses to create an address inside a `mark_populated` range (it changes, moves and deletes one there). The sync opens the range for its creates and closes it again, under the provider's lock of NetBox writes; a range left open by a failure is closed by the next reconcile. The changelog of the range shows `capi: opened to sync addresses by <ref>` and `capi: closed to allocation by <ref>`.
- **Not in the way.** The synced addresses are the network's own: they are not excluded from the `InClusterIPPool`s and do not count as taken in the layout.
- `sync` needs `allocator: InCluster` (`sync needs allocator InCluster`). A network wakes on every `IPAddress` of its `InClusterIPPool`s.

## Order of a change

A change of `pools` or `addresses` is applied in order, and a step that fails stops the next ones:

1. reserved addresses the layout no longer holds are deleted;
2. the `InClusterIPPool`s are updated: those that keep or shrink their span first, then those that go, then new and growing ones, so that no address is in two pools at once;
3. the ranges in NetBox shrink and grow;
4. new reserved addresses are created;
5. the Secrets of `publish` are written, so that whoever reads them never sees an address the layout has not reached.

A change that would move an allocated address (an `IPAddress` of the namespace) to another pool is refused first, as with the `NetBox` allocator (`RangeInUse`). The webhook of in-cluster is the second check: it refuses an `InClusterIPPool` that would give up an allocated address, and the network reports its message as `RangeInUse`; NetBox is not changed and no address is reserved.

## Deletion

A deleted network deletes its `InClusterIPPool`s first, every one it can. While addresses are allocated from a pool, the webhook of in-cluster refuses to delete it: the network waits with `Ready=False` and `WaitingForPools`, the message is the refusal (`InClusterIPPool <name>: admission webhook ... denied the request: Pool has IPAddresses allocated. Cannot delete Pool until all IPAddresses have been removed.`), and it tries again every minute. A pool being deleted that a finalizer holds (in-cluster's `ipam.cluster.x-k8s.io/ProtectPool`) is `InClusterIPPool <name> still exists`, until it is gone. Once they are all gone, the synced addresses are deleted, then the ranges, the prefix, the VIP and the reserved addresses go as `deletionPolicy` says ([Deletion and retained objects](deletion-and-retained.md)). A network deleted by `clusterctl move` deletes neither its pools nor anything in NetBox. One deleted while in-cluster is served but was not when the provider started waits for the restart of the provider with `Ready=False` and `AllocatorUnavailable`, and releases nothing: its pools may still hand out addresses.

A `NetBoxIPPool` with a range in such a network is `InvalidRange` (`network <name> hands out addresses through InClusterIPPools`).

## The chart that renders the cluster

- It puts its values into the `NetBoxNetwork` as they are: sizes and templates, no address arithmetic.
- It does not render the `InClusterIPPool`s: the network owns them, and a tool that applies or prunes them fights it. A pool rendered before is adopted by the network; remove it from the chart then ([Migrating an existing cluster](../recipes/migrate-existing-cluster.md)).
- It renders the `PushSecret` for each `secretName` of `publish` and does not compute the addresses: they are in the Secret the network writes ([Published addresses](networks.md#published-addresses)).
- Machine templates name the pools by their names; the VIP comes from the Cluster, not from values ([Control-plane VIP](control-plane-vip.md)); the infrastructure cluster sets no endpoint.

## Check

```bash
kubectl -n c1 get netboxnetwork c1 -o jsonpath='{.status.conditions}'
kubectl -n c1 get inclusterippools
kubectl -n c1 events --for netboxnetwork/c1
```

- `LayoutReady` is `True`; each pool of `spec.pools` has an `InClusterIPPool` of its name with the addresses of `status.layout`.
- The Events `InClusterIPPoolCreated`, `PoolAdopted`, `InClusterIPPoolUpdated` and `InClusterIPPoolDeleted` record what the network changed; `AddressSynced`, `SyncedAddressReclaimed` and `SyncedAddressDeleted` what the sync did in NetBox.
- With `sync`: `curl -H "Authorization: Bearer $TOKEN" "$NETBOX_URL/api/ipam/ip-addresses/?cf_capi_ref__isw=NetBoxNetwork/c1/c1/sync/"` lists one address per `IPAddress` of the synced pools.

## Next

- [Networks](networks.md) and [Control-plane VIP](control-plane-vip.md).
- [Conditions](../reference/conditions.md): `AllocatorUnavailable`, `AddressConflict`, `RangeInUse`, `WaitingForPools`.
