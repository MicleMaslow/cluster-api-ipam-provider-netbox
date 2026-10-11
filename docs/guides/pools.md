# Pools

Goal: split a [network](networks.md) into pools of addresses and let machines take addresses from them. Every field is in the [API reference](../reference/api.md); `kubectl explain netboxippool.spec` describes them too.

## Steps

1. Create a `NetBoxIPPool` with ranges in the networks of one L2 network:

   ```yaml
   apiVersion: ipam.cluster.x-k8s.io/v1alpha1
   kind: NetBoxIPPool
   metadata: {name: demo-workers, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
   spec: {ranges: [{networkName: demo, start: 16, end: -3}]}
   ```

2. Point the consumer at the pool. With CAPV that is `addressesFromPools` in the machine template ([First cluster](../getting-started/first-cluster.md)); any `IPAddressClaim` works:

   ```yaml
   apiVersion: ipam.cluster.x-k8s.io/v1beta2
   kind: IPAddressClaim
   metadata: {name: sample-claim, namespace: demo}
   spec:
     clusterName: demo
     poolRef: {apiGroup: ipam.cluster.x-k8s.io, kind: NetBoxIPPool, name: demo-workers}
   ```

## Ranges

`ranges` is a list of `{networkName, start, end}`: a span of a `NetBoxNetwork` given as offsets from its first address; a negative offset counts back from the last one (`-1`). Ranges are used in order, one per network, and all networks of a pool share the L2 network (gateway and mask) of the first range's network; until that network exists no range serves. Each range becomes a NetBox IP range; a claim gets an address from it, and the address is deleted from NetBox with the claim. Label the pool with `cluster.x-k8s.io/cluster-name` so that pausing the Cluster pauses the pool.

The range and the addresses taken from it carry the owner and tenant of the network the range lies in, if it has them, so a pool with ranges in networks of different owners and tenants works: each range carries those of its network, and they are kept in sync ([Networks](networks.md#owner-and-tenant)). A range in a network that is not `Ready`, including `OwnerNotFound` and `TenantNotFound`, is out of service (`NetworkNotReady`). A range whose owner or tenant cannot be synced goes on serving; `RangesReady` and the `reason` of the range say why.

Every range is reconciled on its own and reported in `status.ranges[]` (`ready`, `reason`, `message`). Claims get addresses only from ready ranges; the pool is `Ready` while at least one range is, and `RangesReady` lists the ranges that are not as the spec asks:

- A range that covers the network, broadcast or gateway address of the L2 network or the VIP, or lies outside its network, is `InvalidRange`; one in a network of another L2 is `MixedNetworks`.
- A range can be changed. The NetBox range is resized in place; a change that would leave allocated addresses outside it waits (`RangeInUse`, the addresses are in the message). A change that waits, overlaps another range (`RangeOverlap`) or is invalid leaves an active range serving with its old bounds, as long as they are still valid.
- A range can be removed. Its NetBox range is deleted or deprecated as the network's `deletionPolicy` says; while addresses are still allocated in it, it stays in `status.ranges`, not ready (`RangeInUse`). Removed ranges are found in NetBox by the pool's key, not by the status.
- Once no allocated address is in the way of a shrink, deletion or deprecation, NetBox stops handing out the range's addresses (`mark_populated`) and the allocated addresses are checked again before the change. A removed range that still holds addresses is not closed: claims stop using it once it is not ready, but NetBox would still hand out its free addresses to a direct request. A deprecated range stays closed; NetBox also refuses addresses created by hand inside it.
- A range that overlaps another NetBox range waits (`RangeOverlap`) and is retried when the other pool shrinks or goes. Deprecated ranges of the provider in the way, with no addresses in them, are deleted (this needs delete on `ipam.iprange`, else the pool reports `Forbidden` until the range is deleted in NetBox). A deprecated range of the same pool and network is taken back and resized.
- A range that is not in NetBox yet waits (`NetworkNotReady`) until the network has its VIP, if the network reserves one ([Control-plane VIP](control-plane-vip.md)).

`status` also holds the `gateway` and `addressPrefixLength` handed to every address and `ipAddresses` (`total`, `used`, `free`), counted over the ready ranges from the pool's `IPAddress` objects. A pool being deleted waits while addresses are allocated from it (`WaitingForAddresses`).

A pool is paused when it has the `cluster.x-k8s.io/paused` annotation, when the Cluster named by its `cluster.x-k8s.io/cluster-name` label has `spec.paused: true`, or when the Cluster of any network it has a range in has: it is skipped and its condition `Paused` is True. `clusterctl move` pauses Clusters only, so label every pool and annotate a pool that several clusters share ([clusterctl move and pivot](move-and-pivot.md)).

NetBox side: other tools must not take addresses with the prefix-level `available-ips` of a cluster prefix. Since NetBox 4.3 it also returns addresses inside IP ranges that are not marked populated, as the pool ranges are.

## Address options

- **`addressReuseGracePeriodSeconds`** (default 0, at most 86400, one day) keeps the NetBox address of a deleted claim for that many seconds after the claim's deletion, so that a new machine does not get the address while caches of the old one (ARP entries, for example) still point at it, as in a rolling update. The claim shows `Ready=False Releasing` with the time until which the address is kept. The period does not apply while the pool itself is being deleted or is gone: a cluster being torn down is not held up.
- **`omitGateway: true`** hands out the addresses without a gateway. Use it for a second NIC of a machine whose default route is on the first one: with two gateways the machine gets two default routes. `status.gateway` is empty then. It is set when the pool is created and cannot be changed, added or removed later, because the `IPAddress` spec is immutable and a change would mix both kinds in the pool: create another pool instead. See [second NIC](../recipes/second-nic.md).
- **`ipAddress`** sets `dns_name`, `description` and `comments` of the pool's NetBox addresses with Go templates, rendered per claim. The defaults and the rules of the templates are in [Metadata](metadata.md#description-and-comments); the variables are `.Owner`, the first owner reference of the claim, or the claim itself without one (with CAPV the VSphereVM, named like the Machine and the node); `.Claim`; `.Cluster`, the `spec.clusterName` or cluster-name label of the claim; `.Namespace`; `.Pool`. `dnsName` (default `{{ .Owner }}`) is lowercased, as NetBox stores it, and must be a valid DNS name.

  ```yaml
  spec:
    ipAddress:
      dnsName: "{{ .Owner }}.nodes.example"
  ```

  The fields are written when an address is created in NetBox and compared with the templates at every reconcile of the claim, so a change of `ipAddress` reaches allocated addresses and a value edited in NetBox by hand is overwritten. A template that does not render or gives an invalid DNS name makes the pool `InvalidSpec`: no new addresses are issued, allocated claims stay `Ready`. Values can also fail for one claim only: `{{ .Cluster }}.nodes.example` gives `.nodes.example` for a claim without a cluster. Only a new claim then fails, with `AllocationFailed`; an allocated claim stays `Ready` and retries, and the error is in the controller log. An address restored or handed back to such a claim is still written, with `dns_name`, description and comments empty.
- **`ipRange`** sets `description` and `comments` of the pool's NetBox IP ranges with templates, kept in sync; the variables are `.Cluster` (of the network), `.Namespace`, `.Pool` and `.Network`. An invalid `ipRange` makes the pool `InvalidSpec` as well.

## Claims

Claims point at the pool with `poolRef: {apiGroup: ipam.cluster.x-k8s.io, kind: NetBoxIPPool, name: <pool>}`. The `IPAddress` gets the address, the mask of the L2 network as `prefix` and the gateway (unless the pool has `omitGateway`). An address deleted in NetBox by hand is created again with the same address; if NetBox has given that address to another object, the claim reports `AddressConflict`. A claim is skipped while it has the `cluster.x-k8s.io/paused` annotation, its Cluster (`spec.clusterName` or the `cluster.x-k8s.io/cluster-name` label) is paused, or its pool has the annotation.

The address of a deleted claim leaves NetBox only when no other finalizer guards the claim and the grace period of the pool is over. An infrastructure provider keeps its finalizer on a claim while the machine uses the address: CAPV removes `vspherevm.infrastructure.cluster.x-k8s.io/ip-claim-protection` once the VM is gone. Until then the claim is `Ready=False Releasing` with the finalizers it waits for, and is checked again every minute or as soon as a finalizer goes. A claim deleted for `clusterctl move` (annotation `clusterctl.cluster.x-k8s.io/delete-for-move`) releases nothing. If the release fails, the claim is `Ready=False ReleaseFailed` with the error.

On a claim, a NetBox error or a 403 during allocation shows as `AllocationFailed` with the message. A claim stuck in deletion, for example without delete on `ipam.ipaddress`, is `ReleaseFailed` with the error: the claim, its `IPAddress` and the NetBox address stay (CAPV still finishes deleting the Machine) and deleting the pool waits with `WaitingForAddresses`. A claim whose `spec.clusterName` or `cluster.x-k8s.io/cluster-name` label names a Cluster that does not exist is skipped without a condition; the provider logs it at Info level.

## Example: network scheme

One /26 per cluster split into control plane (6), ingress (8) and workers (the rest); the control plane VIP is the address the network reserves itself.

```yaml
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxNetwork
metadata: {name: demo, namespace: demo}
spec:
  clusterName: demo
  parentPrefix: 10.10.0.0/22
  gateway: 10.10.0.1
  prefixLength: 26
  controlPlaneEndpoint: {dnsName: demo-api.example}   # the VIP: the highest free address of the /26
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: demo-cp, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
spec: {ranges: [{networkName: demo, start: 2, end: 7}]}
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: demo-ingress, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
spec: {ranges: [{networkName: demo, start: 8, end: 15}]}
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: demo-workers, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
spec: {ranges: [{networkName: demo, start: 16, end: -3}]}
```

Later the cluster needs addresses for MetalLB. Shrink the workers and add the new pool in one `kubectl apply`:

```yaml
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: demo-workers, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
spec: {ranges: [{networkName: demo, start: 16, end: -11}]}
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: demo-metallb, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
spec: {ranges: [{networkName: demo, start: -10, end: -3}]}
```

`demo-metallb` is `RangeOverlap` until the workers range is shrunk, then `Ready`. If a worker still holds an address in the last eight, the workers range shows `RangeInUse` with that address and `demo-metallb` waits until the machine is gone. MetalLB itself is configured with the addresses of `demo-metallb`'s `status.ranges`; the pool only reserves them in NetBox.

[`docs/examples/network-scheme.yaml`](../examples/network-scheme.yaml) holds the resulting scheme with all four pools.

## Check

```bash
kubectl get netboxippools -A
kubectl -n demo get netboxippool demo-workers -o jsonpath='{.status.ranges}'
kubectl get ipaddressclaims.ipam.cluster.x-k8s.io,ipaddresses.ipam.cluster.x-k8s.io -A
```

- The pool is `Ready=True`; each range in `status.ranges` is `ready: true`, and `RangesReady` is `True`.
- A claim has `status.addressRef`, and the `IPAddress` has the address, `prefix` and `gateway`.
- A reason on a range or a claim is explained in [Conditions](../reference/conditions.md) and [Troubleshooting](../operations/troubleshooting.md).

## Next

- [Control-plane VIP](control-plane-vip.md).
- [Deletion and retained objects](deletion-and-retained.md): what happens to ranges and addresses when a pool or a claim goes.
- Recipes: [second NIC](../recipes/second-nic.md), [a shared VLAN](../recipes/shared-vlan.md).
