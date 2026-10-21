# Pools

Goal: let machines take addresses from the pools a [network](networks.md#layout) lays out. Every field is in the [API reference](../reference/api.md); `kubectl explain netboxippool.spec` describes them too.

## Steps

1. Lay out the network: `spec.pools` of the `NetBoxNetwork` ([Layout](networks.md#layout)).
2. Create a `NetBoxIPPool` on a pool of it:

   ```yaml
   apiVersion: ipam.cluster.x-k8s.io/v1alpha1
   kind: NetBoxIPPool
   metadata: {name: demo-workers, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
   spec: {ranges: [{networkName: demo, pool: workers}]}
   ```

3. Point the consumer at the pool. With CAPV that is `addressesFromPools` in the machine template ([First cluster](../getting-started/first-cluster.md)); any `IPAddressClaim` works:

   ```yaml
   apiVersion: ipam.cluster.x-k8s.io/v1beta2
   kind: IPAddressClaim
   metadata: {name: sample-claim, namespace: demo}
   spec:
     clusterName: demo
     poolRef: {apiGroup: ipam.cluster.x-k8s.io, kind: NetBoxIPPool, name: demo-workers}
   ```

## Ranges

`ranges` is a list of `{networkName, pool}`: a pool of the `spec.pools` of a `NetBoxNetwork` in the namespace. Ranges are used in order, each network pool once, and all networks of a `NetBoxIPPool` share the L2 network (gateway and mask) of the first range's network; until that network exists no range serves. The network owns the NetBox IP ranges of its pools and writes them; the `NetBoxIPPool` writes nothing to NetBox but the addresses of its claims, which a claim takes from the ranges and which leave NetBox with the claim. Label the pool with `cluster.x-k8s.io/cluster-name` so that pausing the Cluster pauses the pool.

`status.ranges[]` has one entry per NetBox range of the network pools, in spec order (`networkName`, `id`, `startAddress`, `endAddress`, `ready`, `reason`, `message`). Claims get addresses only from ready ranges; the pool is `Ready` while at least one range is, and `RangesReady` says why the others are not:

- `NetworkNotReady`: the network is missing or not `Ready` (including `OwnerNotFound` and `TenantNotFound`), its pool is not in its `spec.pools` and `status.layout` (`pool X is not placed in network Y`), or the network has not created all its NetBox ranges yet.
- `MixedNetworks`: the network is in another L2 network than the first range's.
- `Duplicate`: another `NetBoxIPPool` of the namespace takes the same network pool and was created first (of two created in the same second, the one with the lower name). Only that one serves the pool.

The ranges and the addresses taken from them carry the owner and tenant of their network, if it has them, so a pool over networks of different owners and tenants works ([Networks](networks.md#owner-and-tenant)). A change of a pool of the network (its size, its place) shows in `status.ranges` as soon as the network applies it; a change that would move an allocated address is refused by the network ([Layout](networks.md#layout)).

`status` also holds the `gateway` and `addressPrefixLength` handed to every address and `ipAddresses` (`total`, `used`, `free`), counted over the ready ranges from the pool's `IPAddress` objects. A pool being deleted waits while addresses are allocated from it (`WaitingForAddresses`).

A pool is paused when it has the `cluster.x-k8s.io/paused` annotation, when the Cluster named by its `cluster.x-k8s.io/cluster-name` label has `spec.paused: true`, or when the Cluster of any network of its ranges has: it is skipped and its condition `Paused` is True. `clusterctl move` pauses Clusters only, so label every pool and annotate a pool that several clusters share ([clusterctl move and pivot](move-and-pivot.md)).

NetBox side: other tools must not take addresses with the prefix-level `available-ips` of a cluster prefix. Since NetBox 4.3 it also returns addresses inside IP ranges that are not marked populated, as the ranges of the pools are.

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
  pools:
  - {name: cp, size: 6}
  - {name: ingress, size: 8}
  - {name: workers, size: rest}
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: demo-cp, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
spec: {ranges: [{networkName: demo, pool: cp}]}
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: demo-ingress, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
spec: {ranges: [{networkName: demo, pool: ingress}]}
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: demo-workers, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
spec: {ranges: [{networkName: demo, pool: workers}]}
```

Later the cluster needs eight more addresses at the end. Append a pool to the network: `workers` is `rest`, so it gives up its last eight addresses, as long as no worker holds one of them; otherwise the network refuses the change with `RangeInUse` and the address, and nothing changes until the machine is gone.

```yaml
  pools:
  - {name: cp, size: 6}
  - {name: ingress, size: 8}
  - {name: workers, size: rest}
  - {name: extra, size: 8}
```

[`docs/examples/network-scheme.yaml`](../examples/network-scheme.yaml) holds the scheme.

## Check

```bash
kubectl get netboxippools -A
kubectl -n demo get netboxippool demo-workers -o jsonpath='{.status.ranges}'
kubectl get ipaddressclaims.ipam.cluster.x-k8s.io,ipaddresses.ipam.cluster.x-k8s.io -A
```

- The pool is `Ready=True`; each range in `status.ranges` is `ready: true` with the `id` of its NetBox range, and `RangesReady` is `True`.
- A claim has `status.addressRef`, and the `IPAddress` has the address, `prefix` and `gateway`.
- A reason on a range or a claim is explained in [Conditions](../reference/conditions.md) and [Troubleshooting](../operations/troubleshooting.md).

## Next

- [Control-plane VIP](control-plane-vip.md).
- [Deletion and retained objects](deletion-and-retained.md): what happens to ranges and addresses when a network, a pool or a claim goes.
- Recipes: [second NIC](../recipes/second-nic.md), [a shared VLAN](../recipes/shared-vlan.md).
