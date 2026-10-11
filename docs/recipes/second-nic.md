# A second NIC

Goal: machines with two NICs, both with addresses from NetBox, and one default route. The second NIC gets its address without a gateway.

A machine with two NICs gets two default routes if both addresses come with a gateway, and kube-vip picks its interface by the default route. Each NIC is in its own L2 network, so each has its own network and pool; the pool of the second NIC has `omitGateway: true`.

You need the network and pools of [First cluster](../getting-started/first-cluster.md) for the first NIC, and a parent prefix for the second L2 network (here `10.20.0.0/22`, gateway `10.20.0.1`).

## Steps

1. **Create the network and the pool of the second NIC** next to the first ones. A pool takes ranges of one L2 network only (`MixedNetworks`), so the second NIC cannot share the pool of the first:

   ```yaml
   apiVersion: ipam.cluster.x-k8s.io/v1alpha1
   kind: NetBoxNetwork
   metadata: {name: demo-data, namespace: demo}
   spec:
     clusterName: demo
     parentPrefix: 10.20.0.0/22
     gateway: 10.20.0.1
     prefixLength: 26
   ---
   apiVersion: ipam.cluster.x-k8s.io/v1alpha1
   kind: NetBoxIPPool
   metadata: {name: demo-data, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
   spec:
     omitGateway: true
     ranges: [{networkName: demo-data, start: 2, end: -3}]
   ```

   `omitGateway` hands out the addresses without a gateway; `status.gateway` of the pool is empty. Set it when you create the pool: it is immutable and cannot be added or removed later, because the `IPAddress` spec is immutable and a change would mix both kinds in the pool. Put `controlPlaneEndpoint` on the network of the first NIC, the one the VIP belongs to.

2. **Point the second device of the machine template at the pool.** Fragment, the rest left out:

   ```yaml
   apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
   kind: VSphereMachineTemplate
   spec:
     template:
       spec:
         network:
           devices:
           - networkName: demo-portgroup
             dhcp4: false
             addressesFromPools:
             - {apiGroup: ipam.cluster.x-k8s.io, kind: NetBoxIPPool, name: demo-workers}
             nameservers: [10.10.0.53]
           - networkName: demo-data-portgroup
             dhcp4: false
             addressesFromPools:
             - {apiGroup: ipam.cluster.x-k8s.io, kind: NetBoxIPPool, name: demo-data}
   ```

3. **Set `vip_interface`** in the kube-vip manifest of the control plane to the interface of the first NIC, if you run kube-vip ([CAPV with kube-vip](capv-kube-vip.md)).

## Check

```bash
kubectl -n demo get netboxippool demo-data -o jsonpath='{.status.gateway}|{.status.ranges}'
kubectl -n demo get ipaddresses.ipam.cluster.x-k8s.io -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.address}{" gw="}{.spec.gateway}{"\n"}{end}'
```

The pool of the second NIC has an empty gateway and ready ranges; its addresses show `gw=` empty, those of the first NIC the gateway. On a machine, `ip route` has one default route.

## Next

- [Pools](../guides/pools.md#address-options): the options of a pool.
- [CAPV with kube-vip](capv-kube-vip.md) and [a shared VLAN](shared-vlan.md).
