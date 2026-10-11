# clusterctl move and pivot

Goal: move a cluster between management clusters (`clusterctl move`, pivot) without touching its NetBox objects: the target finds them again.

`clusterctl move` leaves the NetBox objects alone. The target cluster finds the prefix, VIP, ranges and addresses by their key, so the status is rebuilt there, including removed ranges that still hold addresses; the endpoint of the moved Cluster already is the VIP. The ids of the owner and tenant of a network are resolved again from its `spec.owner` and `spec.tenant`.

## Before you move

The provider of the target must find the same objects, so it needs the same key:

- **The same instance name.** `NETBOX_INSTANCE` of both providers is equal, or empty on both ([The instance name](../getting-started/netbox-access.md#the-instance-name)). A target with another name does not find the objects of the source: they are another zone for it.
- **The same key field.** `NETBOX_REF_FIELD` of both providers is equal. The provider does not migrate keys between fields ([Upgrade](../operations/upgrade.md#the-key-does-not-move)).
- **The same namespace and names.** The key is `[<instance>/]Kind/namespace/name`. A mutator or tool that changes the namespace on the move makes the target carve new objects instead of finding the old ones.
- **The same owner and tenant in the spec,** if you use them: they move with the objects of the network. Neither is part of the key, so a difference changes nothing about finding the objects: the target syncs the objects to its spec.

## Steps

1. Install the provider in the target cluster with the settings above ([Install](../getting-started/install.md)).
2. Check that every pool is labelled with `cluster.x-k8s.io/cluster-name`, and put the `cluster.x-k8s.io/paused` annotation on a pool that several clusters share, for the time of the move: its claims are skipped as well. `clusterctl move` pauses Clusters only. A network is paused with its Cluster, a pool with the Cluster of its `cluster.x-k8s.io/cluster-name` label or of any of its networks.
3. Run `clusterctl move`. The objects deleted in the source carry `clusterctl.cluster.x-k8s.io/delete-for-move`: they release nothing in NetBox.
4. Remove the pause annotation on the target.

A network in the target cluster that has no prefix in its status yet waits for its Cluster (`WaitingForCluster`) and touches nothing in NetBox, so the order in which clusterctl creates the objects does not matter.

## Check

```bash
kubectl get netboxnetworks,netboxippools -A
kubectl get ipaddressclaims.ipam.cluster.x-k8s.io,ipaddresses.ipam.cluster.x-k8s.io -A
```

On the target, the networks and pools become `Ready` with the same prefixes, ranges and addresses as before; NetBox has no new prefix, range or address. A second prefix in NetBox, or `AddressConflict` on claims, mean that the key does not match: compare the instance name, the key field and the namespace.

## Next

- [Deletion and retained objects](deletion-and-retained.md).
- [Troubleshooting](../operations/troubleshooting.md) and [Upgrade](../operations/upgrade.md).
- [Networks](networks.md#owner-and-tenant).
