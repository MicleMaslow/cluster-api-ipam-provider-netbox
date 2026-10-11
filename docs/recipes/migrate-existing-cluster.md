# Migrating an existing cluster

Goal: bring a running cluster, whose subnet and control-plane VIP already exist, under a `NetBoxNetwork` without changing the Cluster or its machines.

The provider only avoids what it can see: addresses and ranges that are objects in NetBox, and what the Cluster says about its endpoint. Prepare NetBox first.

## Steps

1. **Put the running machines' addresses into NetBox,** or keep the pool ranges off them. A pool hands out the next free address of its NetBox range; an address in use that NetBox does not know is free for it. Pick ranges (offsets from the first address of the prefix) that do not cover the addresses of running machines.

2. **Adopt the cluster's prefix** with `spec.prefix` instead of carving a new one. The prefix must exist in NetBox inside `parentPrefix` (the provider creates it when it does not) and carry no key, or the key of this network; a prefix with the key of another object is `Duplicate`. The provider writes only its key, and the rest, owner, tenant, description and comments included, stays as your NetBox admin set it. The token must be allowed to change the prefix (it is in your zone). An adopted prefix is released on deletion, not removed.

   ```yaml
   apiVersion: ipam.cluster.x-k8s.io/v1alpha1
   kind: NetBoxNetwork
   metadata: {name: demo, namespace: demo}
   spec:
     clusterName: demo
     parentPrefix: 10.10.0.0/22
     gateway: 10.10.0.1
     prefix: 10.10.0.0/26
   ```

3. **Create the pools** for the machines that come next ([Pools](../guides/pools.md)), and point the machine templates at them with `addressesFromPools`. Machines take their addresses from NetBox as they are replaced.

4. **Add the VIP,** if the cluster has one: `controlPlaneEndpoint` with the `port` the Cluster uses.

   ```yaml
   spec:
     controlPlaneEndpoint: {port: 6443, dnsName: demo-api.example}
   ```

   The VIP is the address the Cluster has as its endpoint, if that is inside the prefix of the network. It must be free in NetBox: not the network, broadcast or gateway address of the L2 network, no IP address object, in no IP range. An IP address object you made by hand for it is in the way: remove it, the provider creates its own with the role `vip`. Otherwise the network reports `EndpointConflict` on `ControlPlaneEndpointReady` and nothing changes. An endpoint outside the prefix, or a DNS name, is `EndpointConflict` as well: the Cluster is never changed, and the provider reserves the highest free address meanwhile.

5. **A cluster that took its VIP from a pool of its own** (a `NetBoxIPPool` with one range on the VIP) must lose that pool first, and its range must be gone from NetBox. Under `deletionPolicy: Retain` the range stays `deprecated` and still holds the address, so the network reports `EndpointConflict`. Delete the pool, then delete the range in NetBox, then add `controlPlaneEndpoint`.

## Check

```bash
kubectl -n demo get netboxnetwork demo -o jsonpath='{.status.prefix} {.status.controlPlaneEndpoint} {.status.conditions}'
kubectl -n demo get cluster demo -o jsonpath='{.spec.controlPlaneEndpoint}'
```

`Ready` and `ControlPlaneEndpointReady` are `True`, the Cluster has the same endpoint as before, and in NetBox the prefix carries the key and the VIP is an IP address with role `vip`, with the description and comments of the network.

## Next

- [Control-plane VIP](../guides/control-plane-vip.md#a-running-cluster).
- [Networks](../guides/networks.md) and [Pools](../guides/pools.md).
- [Troubleshooting](../operations/troubleshooting.md).
