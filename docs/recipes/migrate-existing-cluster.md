# Migrating an existing cluster

Goal: bring a running cluster whose addresses come from in-cluster IPAM under a `NetBoxNetwork` with `allocator: InCluster`, without a new Machine. A chart made its `InClusterIPPool`s; its VIP and MetalLB addresses sit at the top of its prefix in NetBox as IP addresses without a key; the worker pool ends below them.

The network takes over what it finds instead of placing anew:

| Object | Taken over when | Otherwise |
|---|---|---|
| prefix | `spec.prefix`; the NetBox prefix carries no key, or this network's | `Duplicate` (`held by ...`) |
| `InClusterIPPool` | it has the name of a pool of `spec.pools` and no controller (owner reference with `controller: true`), or this network | another controller: `Duplicate` |
| VIP | the status has no VIP; the Cluster's endpoint is inside the prefix and one NetBox IP address without a key holds it, in no IP range | `EndpointConflict` |
| reserved addresses | NetBox IP addresses without a key above the pools and under the VIP, each alone at its address and in no IP range, never the one at the Cluster's endpoint: a group of `spec.addresses` placed for the first time takes them from the top down, up to `count` | free addresses of the tail |

The Cluster's endpoint must be an IP address inside the prefix: that is how the VIP is found, and no group takes it. With a DNS name or an address outside the prefix this recipe is not safe, so do not migrate such a cluster with it:

- with `controlPlaneEndpoint`, the VIP is `EndpointConflict` and the network reserves a new one at the highest free address of the prefix, which the Cluster never uses. Under the VIP and the MetalLB addresses that is usually an address of the top pool of the cluster that in-cluster can still hand out: the adopted pool loses it, and groups adopt only what lies under the new VIP;
- without `controlPlaneEndpoint`, nothing tells the network which NetBox address is the VIP: a group placed for the first time can adopt the object of the Cluster's VIP as a reserved address.

An adopted pool keeps its addresses: its ranges are the free ones of its `addresses`, less its `excludedAddresses`, which stay excluded; its spec is written whole ([In-cluster allocator](../guides/in-cluster.md#what-the-network-writes)). The VIP and the reserved addresses keep their NetBox objects and ids; they get the key, `dns_name`, description, comments, owner and tenant of the network; a reserved address also gets the mask of the L2 network and status `active`.

## Steps

1. **Keep the GitOps tool off the pools.** Once adopted, the network owns the `InClusterIPPool`s and rewrites their spec; a tool that applies the chart's version back or prunes them fights it. Turn off pruning and self-heal for them before the network exists, for example in Argo CD the annotation `argocd.argoproj.io/sync-options: Prune=false` on the pools and `ignoreDifferences` on their `spec` and `metadata.ownerReferences` (or self-heal off for the application); in Flux `kustomize.toolkit.fluxcd.io/prune: disabled` and `kustomize.toolkit.fluxcd.io/reconcile: disabled`.

2. **Find the sizes.** The size of a pool is the number of free addresses of its `addresses`, less its `excludedAddresses`: not an address or inside a range NetBox holds for something else, not the VIP, not the gateway, network or broadcast address of the L2 network. A pool `10.10.0.8-10.10.0.60` with nothing of NetBox inside is 53; with the VIP inside, 52. When in doubt, apply any size: the network reports the sizes it found and changes nothing (step 4).

3. **Apply the network** with the prefix of the cluster, its pools by name in the order of their addresses with the sizes found, and the groups with the count the cluster has:

   ```yaml
   apiVersion: ipam.cluster.x-k8s.io/v1alpha1
   kind: NetBoxNetwork
   metadata: {name: demo, namespace: demo}
   spec:
     clusterName: demo
     parentPrefix: 10.10.0.0/22
     gateway: 10.10.0.1
     prefix: 10.10.0.0/26
     allocator: InCluster
     controlPlaneEndpoint: {port: 6443, dnsName: "{{ .Cluster }}-controlplane.example.test"}
     pools:
     - {name: demo-cp, size: 6}        # InClusterIPPool demo-cp: 10.10.0.2-10.10.0.7
     - {name: demo-workers, size: 53}  # InClusterIPPool demo-workers: 10.10.0.8-10.10.0.60
     addresses:
     - {name: metallb, count: 2, dnsName: "{{ .Cluster }}-metallb-{{ .Index }}.example.test"}  # .62 and .61 in NetBox
   ```

   `prefix` adopts the NetBox prefix as for any network: only the key is written, the rest stays as your NetBox admin set it. Give numbers, not `rest`: a `rest` pool is placed again up to the VIP and the groups, and can grow. A pool of `spec.pools` without an `InClusterIPPool` comes after the adopted ones and is placed by the network.

4. **Check.**

   ```bash
   kubectl -n demo get netboxnetwork demo -o jsonpath='{.status.conditions}'
   kubectl -n demo get inclusterippools -o custom-columns=NAME:.metadata.name,OWNER:.metadata.ownerReferences[0].name,ADDRESSES:.spec.addresses,EXCLUDED:.spec.excludedAddresses
   kubectl -n demo get machines -o custom-columns=NAME:.metadata.name,UID:.metadata.uid,ADDRESSES:.status.addresses[*].address
   kubectl -n demo events --for netboxnetwork/demo
   ```

   - `Ready`, `ControlPlaneEndpointReady` and `LayoutReady` are `True`; `status.layout` has the spans of the pools.
   - The pools are controlled by `demo`, with the same addresses; `excludedAddresses` lists what NetBox holds inside them.
   - In NetBox the prefix, the VIP and the MetalLB addresses carry the key of the network with their old ids, and each pool is one or more ranges with `mark_populated`.
   - The Machines have the same names, UIDs and addresses, and there is no new one. The Events `PoolAdopted`, `VIPAdopted` and `ReserveAdopted` record what was taken over.

   `LayoutReady=False` with `InvalidSpec` and `spec.pools do not match the InClusterIPPools of the network: sizes must be those found: demo-cp 6, demo-workers 52` means other sizes or another order than found: fix the spec, nothing was changed. `Duplicate` is a pool of the name that another controller holds. `EndpointConflict` on `ControlPlaneEndpointReady` is a VIP address held by an object with a key, by more than one object, or inside an IP range: fix it in NetBox ([Control-plane VIP](../guides/control-plane-vip.md#a-running-cluster)).

5. **Remove the pools from the chart** once `LayoutReady` is `True`. They belong to the network: the chart no longer renders them, and the machine templates keep pointing at them by name ([The chart that renders the cluster](../guides/in-cluster.md#the-chart-that-renders-the-cluster)).

## Addresses inside a pool

A VIP or a MetalLB address that NetBox holds inside the span of a pool follows the rule for every address in use: it goes to the `excludedAddresses` of the pool, the ranges in NetBox split around it, and the size counts without it. One at the first or last address of the span shrinks the pool instead: its `addresses` start after it or end before it. The webhook of in-cluster accepts that as long as no Machine has the address; if one has it, `LayoutReady` reports `RangeInUse` with the webhook's message and the pool stays as it was. Such a MetalLB address is not taken over by a group: groups take addresses above the pools.

## Without in-cluster IPAM

A cluster whose machines take their addresses from NetBox has no `InClusterIPPool` to take positions from: the network places its pools around what NetBox holds ([Layout](../guides/networks.md#layout)). Put the running machines' addresses into NetBox first, so that no pool hands them out again. The VIP and the reserved addresses are taken over as above.

## Next

- [In-cluster allocator](../guides/in-cluster.md) and [Control-plane VIP](../guides/control-plane-vip.md#a-running-cluster).
- [Networks](../guides/networks.md) and [Pools](../guides/pools.md).
- [Troubleshooting](../operations/troubleshooting.md).
