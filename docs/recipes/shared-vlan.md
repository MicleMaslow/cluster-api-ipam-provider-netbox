# A shared VLAN

Goal: several clusters, or several networks of one cluster, in one VLAN, each carving its subnet from its own slice of the VLAN's addresses, while every machine gets the mask and the gateway of the whole VLAN.

The VLAN is one L2 network: `10.10.0.0/22`, gateway `10.10.0.1`. In NetBox it is split into logical parents, prefixes that are only a place for CAPI to carve from, for example `10.10.1.0/24` for one team and `10.10.2.0/24` for another. The gateway lies outside both.

## Steps

1. **Create the logical parents in NetBox,** once in the global VRF each, and cover DHCP scopes and static addresses inside them with child prefixes ([NetBox access](../getting-started/netbox-access.md#parent-prefix)). A token that may only write into one slice is fenced by it: `prefix__net_contained` and the like in the constraints of its permissions.

2. **Create a network per cluster** with its logical parent as `parentPrefix`, the gateway and the mask of the VLAN as `addressPrefixLength`:

   ```yaml
   apiVersion: ipam.cluster.x-k8s.io/v1alpha1
   kind: NetBoxNetwork
   metadata: {name: team-a, namespace: team-a}
   spec:
     clusterName: team-a
     parentPrefix: 10.10.1.0/24
     gateway: 10.10.0.1
     addressPrefixLength: 22
     prefixLength: 26
     pools:
     - {name: cp, size: 6}
     - {name: workers, size: rest}
   ```

   The L2 network is `gateway/addressPrefixLength` with the host bits cleared, `10.10.0.0/22`; it has to contain `parentPrefix`, and `addressPrefixLength` cannot be longer than the mask of `parentPrefix`. Addresses get the mask `/22` and the gateway `10.10.0.1`; the network and broadcast address of the VLAN and the gateway are never handed out. `addressPrefixLength` is immutable and cannot be added later.

3. **Give each cluster its pools** as usual ([Pools](../guides/pools.md)). The layout counts free addresses of the cluster's `/26`; the network and broadcast address of the VLAN fall outside every `/26` but the first and the last.

4. **To grow a cluster,** add a second network from another logical parent of the same VLAN and its pool to the existing `NetBoxIPPool`. The pool stays in one L2 network (`MixedNetworks` compares gateway and mask), so both networks have the same `gateway` and `addressPrefixLength`. They must be in the namespace of the pool; the ranges are used in order:

   ```yaml
   apiVersion: ipam.cluster.x-k8s.io/v1alpha1
   kind: NetBoxNetwork
   metadata: {name: team-a-2, namespace: team-a}
   spec:
     clusterName: team-a
     parentPrefix: 10.10.2.0/24
     gateway: 10.10.0.1
     addressPrefixLength: 22
     prefixLength: 26
     pools: [{name: workers, size: rest}]
   ---
   apiVersion: ipam.cluster.x-k8s.io/v1alpha1
   kind: NetBoxIPPool
   metadata: {name: team-a-workers, namespace: team-a, labels: {cluster.x-k8s.io/cluster-name: team-a}}
   spec:
     ranges:
     - {networkName: team-a, pool: workers}
     - {networkName: team-a-2, pool: workers}
   ```

   Put `controlPlaneEndpoint` on one network of a cluster only.

## Check

```bash
kubectl get netboxnetworks -A
kubectl -n team-a get netboxippool team-a-workers -o jsonpath='{.status.gateway} /{.status.addressPrefixLength} {.status.ranges}'
```

Each network is `Ready` with a prefix inside its own logical parent, the pool shows the gateway `10.10.0.1` and the mask `/22`, and every range is ready. A range that is `MixedNetworks` is in a network with another gateway or mask.

## Next

- [Networks](../guides/networks.md) and [Pools](../guides/pools.md).
- [NetBox access](../getting-started/netbox-access.md#permissions): zones by parent prefix.
- [A second NIC](second-nic.md): a pool in another L2 network.
