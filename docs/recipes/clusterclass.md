# ClusterClass

Goal: use the network with a ClusterClass cluster. The network writes the control-plane endpoint to a topology Cluster the same way as to a plain one. Only plain clusters are tested; the examples for CAPV are in govmomi mode.

## Steps

1. **Create the network and the pools** in the namespace of the topology Cluster, as for a plain cluster ([First cluster](../getting-started/first-cluster.md)). `spec.clusterName` is the name of the Cluster object. Add `controlPlaneEndpoint` to the network for a VIP ([Control-plane VIP](../guides/control-plane-vip.md)).

2. **Patch no endpoint into the infrastructure cluster template of the ClusterClass.** The network writes `Cluster.spec.controlPlaneEndpoint` itself, and a host other than the VIP in the infrastructure cluster is `EndpointConflict`.

3. **Put the placeholders into the load balancer config of the control plane template** (`__CONTROL_PLANE_ENDPOINT__` and `__CONTROL_PLANE_PORT__`) and add the command of [`docs/examples/control-plane-vip.yaml`](../examples/control-plane-vip.yaml) to its `preKubeadmCommands`, as in [CAPV with kube-vip](capv-kube-vip.md).

## Check

```bash
kubectl -n demo get netboxnetwork demo -o jsonpath='{.status.controlPlaneEndpoint} {.status.conditions}'
kubectl -n demo get cluster demo -o jsonpath='{.spec.controlPlaneEndpoint}'
```

`ControlPlaneEndpointReady` is `True` and the topology Cluster has the VIP and the port in `spec.controlPlaneEndpoint`. `EndpointConflict` means something else sets another endpoint ([Conditions](../reference/conditions.md#reasons)).

## Next

- [Control-plane VIP](../guides/control-plane-vip.md) and [CAPV with kube-vip](capv-kube-vip.md).
- [Pools](../guides/pools.md).
