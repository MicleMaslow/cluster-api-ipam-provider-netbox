# Control-plane VIP

Goal: let the network reserve the virtual IP of the control plane in NetBox and write it to the Cluster, so that the address of the API server is planned like every other address.

The provider knows nothing about the load balancer. A `NetBoxNetwork` with `controlPlaneEndpoint` reserves the VIP in NetBox and writes it to `Cluster.spec.controlPlaneEndpoint`, and Cluster API puts it into the kubeadm config on the node and into the kubeconfig. Running the VIP is the job of your template: kube-vip, keepalived or anything else.

`controlPlaneEndpoint` is for infrastructure providers without a load balancer of their own (CAPV, Metal3 and similar). CAPO, CAPA and others that create a load balancer set the endpoint themselves: leave the field out there.

## Steps

1. **Ask for the VIP** on one network of the cluster, in the prefix of that network:

   ```yaml
   spec:
     controlPlaneEndpoint: {dnsName: demo-api.example}   # port defaults to 6443
   ```

   `port` is immutable while `controlPlaneEndpoint` is set: it cannot be changed, added or removed, because the Cluster keeps the endpoint it got and a changed port could only give `EndpointConflict`. Set `port` when you add `controlPlaneEndpoint` (if the Cluster does not use 6443); it cannot be changed later. The whole `controlPlaneEndpoint` can be removed. `dnsName`, `description` and `comments` change and are synced.

   Set it on one network of a cluster only: a second one reserves a VIP of its own that the Cluster never gets and reports `EndpointConflict`.

2. **Leave the endpoint out of the infrastructure cluster.** KubeadmControlPlane creates no machine and no kubeconfig until `Cluster.spec.controlPlaneEndpoint` is set. The network writes it first, so CAPI renders the kubeconfig and the bootstrap data with the VIP, and CAPV leaves an endpoint that the Cluster already has as it is. Omit `VSphereCluster.spec.controlPlaneEndpoint` entirely rather than leaving it empty.

3. **Put the VIP into the configuration of your load balancer.** The VIP is chosen when the network is reconciled, after the template has been applied, and CAPI puts no field of the Cluster into the files of a KubeadmConfig. So the configuration of the load balancer in your template carries two placeholders, `__CONTROL_PLANE_ENDPOINT__` and `__CONTROL_PLANE_PORT__`, and one command in `preKubeadmCommands` replaces them on the node. It takes the host and port from the kubeadm config that CAPI renders there: `controlPlaneEndpoint` in `/run/kubeadm/kubeadm.yaml` (first control plane machine), `apiServerEndpoint` in `/run/kubeadm/kubeadm-join-config.yaml` (the other control plane machines), or `/etc/kubeadm.yml` (Ignition, both). Without an address it fails, so no machine starts with a load balancer that has none.

   The command is in [`docs/examples/control-plane-vip.yaml`](../examples/control-plane-vip.yaml), and `make test` runs it on configs shaped as CAPI v1.14 renders them. The path of the file it edits is the first line of the command (`f=...`): the kube-vip manifest in the example, or `/etc/keepalived/keepalived.conf` and the like. Add the command to the `preKubeadmCommands` of the template; do not replace the list. How to build the kube-vip manifest of a CAPV template: [CAPV with kube-vip](../recipes/capv-kube-vip.md); keepalived: [keepalived](../recipes/keepalived.md).

4. **Create the cluster.** Pools of the same network keep off the VIP; see below.

## What the provider does

- **The address.** The VIP is the host of the Cluster's endpoint if that is an address inside the prefix (the migration of a running cluster), else the highest address of the prefix that is not the network, broadcast or gateway of the L2 network, holds no NetBox address and lies in no IP range. It is created in NetBox with role `vip`, with the owner and tenant of the network if it has them, a description and comments (`controlPlaneEndpoint.description` and `.comments`, [defaults](metadata.md#description-and-comments)), and kept in sync with them. It shows in `status.controlPlaneEndpoint`, whose port is the effective one. It is found again by its key and never moves; one deleted in NetBox is created again at the same address, or the network reports `EndpointConflict` if the address was taken. A VIP moved to another address in NetBox is `EndpointConflict` too, and the status keeps the old address. NetBox lets addresses with role `vip` share an address: while the status has no VIP (a new one, or after `clusterctl move`), the provider deletes its VIP only for an older object holding the same address (the lower NetBox id wins a race of two creators); a newer one is only logged.
- **The endpoint of the Cluster.** Only the empty parts of the Cluster's endpoint are filled, by a merge patch of that field only: the host with the VIP, the port with `controlPlaneEndpoint.port`, else the port the Cluster has (a port alone is what CAPI copies from an infrastructure cluster that sets only the port), else 6443. The same port is in `status.controlPlaneEndpoint`. A host other than the VIP, or a port other than a given `controlPlaneEndpoint.port`, is never overwritten: the network reports `EndpointConflict`.
- **The condition.** `ControlPlaneEndpointReady` is True when the VIP is reserved and the Cluster has it. Otherwise it is False with `WaitingForCluster` (the Cluster does not exist yet; a new VIP is picked only once it does, as its endpoint may be the VIP; the provider watches Clusters, so the order of applying the manifests does not matter), `EndpointConflict` (the Cluster has another endpoint), or the reason of the VIP error (`Exhausted`, `EndpointConflict`, ...). It never changes `Ready`, so adding a VIP to a live network stops no pool.
- **Pools.** A pool range that is not in NetBox yet waits (`NetworkNotReady`) until the network has its VIP; one that covers the VIP is `InvalidRange`.
- **Deletion.** The VIP follows `deletionPolicy`: deleted, or `deprecated` and taken back when the network is created again. With `Delete` the network waits (`WaitingForCluster`) while the Cluster still has the VIP as its endpoint host, so that the address does not go to another cluster that is still served on it; a retained VIP is never handed out and needs no wait.
- **Removing the field** from the spec releases neither the VIP nor the Cluster endpoint: `status.controlPlaneEndpoint` keeps the host, so pool ranges still keep off the VIP, and only the `ControlPlaneEndpointReady` condition goes. To free the address, delete the network (the VIP follows `deletionPolicy`) or the VIP address in NetBox; as long as the network exists, its status keeps the host and its pools stay off it.
- **A lost prefix.** A network without status (recreated, as after a restore) whose prefix is gone from NetBox while its active VIP is still there is `PrefixNotFound`, like one that knows its prefix: the prefix is not carved again, restore it.

## A running cluster

To bring a running cluster under the network, add `controlPlaneEndpoint` to it, with the `port` the Cluster uses. The VIP is then the address the Cluster has, if it is inside the prefix of the network. It must be free in NetBox: not the network, broadcast or gateway of the L2 network, no IP address object, in no IP range; otherwise the network reports `EndpointConflict` on `ControlPlaneEndpointReady` and nothing changes. An endpoint outside the prefix, or a DNS name, is `EndpointConflict` as well: the Cluster is never changed, and the provider reserves the highest free address meanwhile. See also [migrating an existing cluster](../recipes/migrate-existing-cluster.md).

A cluster that took its VIP from a pool of its own (a `NetBoxIPPool` with one range on the VIP) must lose that pool first, and its range must be gone from NetBox: under `deletionPolicy: Retain` the range stays `deprecated` and still holds the address, so the network reports `EndpointConflict`. Delete that range in NetBox, then add `controlPlaneEndpoint`.

## Notes

- **ClusterClass.** The network writes the endpoint to a topology Cluster the same way, but only plain clusters are tested: [ClusterClass](../recipes/clusterclass.md).
- **ARP.** kube-vip in ARP mode needs all control plane machines in the L2 network of the VIP; otherwise use BGP ([CAPV#1120](https://github.com/kubernetes-sigs/cluster-api-provider-vsphere/issues/1120)).
- **kube-vip version.** Pin a version you have tested. The CAPV template uses v0.6.4; v1.2.2 deadlocked the cold start of a control plane ([kube-vip#1654](https://github.com/kube-vip/kube-vip/issues/1654)).
- **A second NIC.** A machine with two NICs gets two default routes if both addresses come with a gateway, and kube-vip picks its interface by the default route. Set `omitGateway: true` on the pool of the second NIC and `vip_interface` in the manifest ([second NIC](../recipes/second-nic.md)).

## Check

```bash
kubectl -n demo get netboxnetwork demo -o jsonpath='{.status.controlPlaneEndpoint} {.status.conditions}'
kubectl -n demo get cluster demo -o jsonpath='{.spec.controlPlaneEndpoint}'
```

- `ControlPlaneEndpointReady` is `True`, `status.controlPlaneEndpoint` shows the VIP and the port, and the Cluster has the same host and port.
- In NetBox the VIP is an IP address with role `vip`, the key `[<instance>/]NetBoxNetwork/<namespace>/<network>/vip` and the `dnsName` of the spec.
- `kubectl get netboxnetworks -A` shows the VIP in its own column.

## Next

- [Pools](pools.md) and [Networks](networks.md).
- Recipes: [CAPV with kube-vip](../recipes/capv-kube-vip.md), [keepalived](../recipes/keepalived.md), [ClusterClass](../recipes/clusterclass.md), [second NIC](../recipes/second-nic.md).
- [Conditions](../reference/conditions.md): `Exhausted`, `EndpointConflict`, `WaitingForCluster`.
