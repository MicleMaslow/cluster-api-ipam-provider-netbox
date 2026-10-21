# First cluster

Goal: a CAPV cluster whose machines take their addresses from NetBox. Other infrastructure providers that consume `IPAddressClaim`s work the same way; only the machine template differs.

You need the provider [installed](install.md), a parent prefix in NetBox (here `10.10.0.0/22`, gateway `10.10.0.1`) and a CAPV cluster template of your own. This page shows the NetBox part.

## Steps

1. **Plan the subnet.** One /26 per cluster, split into pools: control plane (6 addresses) and workers (the rest). Give only the sizes: the network places the pools from the start of its prefix on free addresses, past the network address and gateway of the L2 network and anything NetBox already holds ([Layout](../guides/networks.md#layout)).

2. **Create the network and the pools** in the namespace of the Cluster `demo`:

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
   ---
   apiVersion: ipam.cluster.x-k8s.io/v1alpha1
   kind: NetBoxIPPool
   metadata: {name: demo-cp, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
   spec: {ranges: [{networkName: demo, pool: cp}]}
   ---
   apiVersion: ipam.cluster.x-k8s.io/v1alpha1
   kind: NetBoxIPPool
   metadata: {name: demo-workers, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
   spec: {ranges: [{networkName: demo, pool: workers}]}
   ```

   Label every pool with `cluster.x-k8s.io/cluster-name` so that pausing the Cluster pauses the pool. The order in which you apply the manifests, and the Cluster, does not matter: a network whose Cluster does not exist yet waits (`WaitingForCluster`) and touches nothing in NetBox. The whole scheme, with an ingress pool and the VIP, is [`docs/examples/network-scheme.yaml`](../examples/network-scheme.yaml); it is explained in [Pools](../guides/pools.md#example-network-scheme).

   If your zone is fenced by an owner or a tenant, or you want them on the NetBox objects, add `spec.owner` and `spec.tenant` to the network ([Networks](../guides/networks.md#owner-and-tenant)).

3. **Point the machine templates at the pools.** A `VSphereMachineTemplate` takes the machine address from a pool with `addressesFromPools`; CAPV creates the claims. The gateway comes with the address, so `gateway4` is not needed; `nameservers` is not part of the claim and stays in the template. Fragment, the rest of the template left out ([`docs/examples/capv-machine-template-patch.yaml`](../examples/capv-machine-template-patch.yaml)):

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
             nameservers:
             - 10.10.0.53
   ```

   Use `demo-cp` in the template of the control plane machines.

4. **Create the cluster** as usual. For the control-plane VIP see [Control-plane VIP](../guides/control-plane-vip.md).

## Check

```bash
kubectl get netboxnetworks,netboxippools -n demo
kubectl get ipaddressclaims.ipam.cluster.x-k8s.io,ipaddresses.ipam.cluster.x-k8s.io -n demo
kubectl -n demo get netboxippool demo-workers -o jsonpath='{.status.ranges}'
```

Use the full resource names: in Kubernetes 1.33 and later the plain name `ipaddresses` resolves to `networking.k8s.io`.

- The network is `Ready=True` and shows its prefix, `LayoutReady=True` with the ranges of each pool in `status.layout`; each `NetBoxIPPool` is `Ready=True` with its total, used and free addresses.
- Each machine has an `IPAddressClaim` with `status.addressRef`, and an `IPAddress` with the address, the mask of the L2 network and the gateway.
- In NetBox, the prefix, the IP ranges of the pools and one IP address per machine exist, each with the key in the key field, a description and comments, the owner and tenant if you set them, and a changelog message that starts with `capi:`. Find the prefix by its id:

  ```bash
  id=$(kubectl -n demo get netboxnetwork demo -o jsonpath='{.status.prefixID}')
  curl -sf -H "Authorization: Bearer $NETBOX_TOKEN" "$NETBOX_URL/api/ipam/prefixes/$id/" | jq -c '{prefix, status: .status.value, description, owner: .owner.name, tenant: .tenant.slug, custom_fields}'
  ```

If a condition is not `True`, read its reason: [Conditions](../reference/conditions.md) and [Troubleshooting](../operations/troubleshooting.md).

## Next

- [Networks](../guides/networks.md) and [Pools](../guides/pools.md): all fields, resizing, more pools.
- [Control-plane VIP](../guides/control-plane-vip.md): let the network reserve the VIP.
- [Metadata](../guides/metadata.md): tags and custom fields on the prefix.
- [Deletion and retained objects](../guides/deletion-and-retained.md): what stays in NetBox when the cluster goes.
- Recipes: [CAPV with kube-vip](../recipes/capv-kube-vip.md), [second NIC](../recipes/second-nic.md), [ClusterClass](../recipes/clusterclass.md).
