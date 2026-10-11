# Deletion and retained objects

Goal: know what stays in NetBox when a network, a pool or a claim goes, and remove what you do not want to keep.

## What happens on deletion

`deletionPolicy` of the network decides the fate of its prefix, the pool ranges inside it and the VIP:

| NetBox object | `Retain` (default) | `Delete` |
|---|---|---|
| prefix, carved by the provider | marked `deprecated`; reclaimed when a network of the same name is created again | deleted |
| prefix, adopted with `spec.prefix` | only released: the key is removed, everything else stays | not allowed for an adopted prefix (the CRD rejects it) |
| IP ranges of the pools in the network | marked `deprecated` when the pool releases them | deleted |
| VIP | marked `deprecated`, taken back when the network is created again | deleted |
| IP address of a claim | always deleted with the claim | always deleted with the claim |

- **Order.** Pool ranges follow the policy when their pool releases them, because the range was removed or the pool deleted. The network itself is deleted only after those pools (`WaitingForPools`). A pool being deleted waits while addresses are allocated from it (`WaitingForAddresses`). A claim waits for the finalizers of the infrastructure provider and for `addressReuseGracePeriodSeconds`.
- **An adopted prefix** is released, not removed: its key is cleared, everything else, tags, custom fields, owner, tenant and description included, stays; its pool ranges are deprecated. That holds for a prefix the provider created because none existed: it stays active with its description and needs manual cleanup in NetBox. The prefix is found in NetBox by the provider's key, not by the status.
- **The VIP and `Delete`.** The network waits (`WaitingForCluster`) while the Cluster still has the VIP as its endpoint host, so that the address does not go to another cluster that is still served on it.
- **Move.** An object deleted for `clusterctl move` (annotation `clusterctl.cluster.x-k8s.io/delete-for-move`) releases nothing ([clusterctl move and pivot](move-and-pivot.md)).
- **Identity.** The objects are found by their key alone: deleting depends on no owner or tenant lookup, and a network that lost its status (restore) is deleted like any other. What carries another key stays in NetBox, for example after the instance name changed ([The instance name](../getting-started/netbox-access.md#the-instance-name)); a network that had a prefix logs `The NetBox prefix of the network is not found under its key` ([Troubleshooting](../operations/troubleshooting.md#log-lines-that-explain-a-leftover)). Find such objects by their old key and remove them in NetBox.

## Permissions

Delete on `ipam.prefix` and `ipam.iprange` is needed only with `deletionPolicy: Delete`, and on ranges to delete deprecated ranges of the provider that are in the way of a new or resized range (without it the pool reports `Forbidden` until the deprecated range is deleted in NetBox). Addresses of deleted claims are always deleted.

For production, grant no delete on `ipam.prefix` and `ipam.iprange` and keep `deletionPolicy: Retain`. A network with `deletionPolicy: Delete`, or a pool with a range in such a network, then keeps its finalizer on deletion and reports `Forbidden` until an admin removes its NetBox objects or the policy is set to `Retain`. A claim stuck in deletion, for example without delete on `ipam.ipaddress`, is `ReleaseFailed`. See [NetBox access](../getting-started/netbox-access.md#permissions).

## Retained objects

`deletionPolicy: Retain` leaves the prefix the provider created, the pool IP ranges and the VIP of a deleted cluster in NetBox as `deprecated`. A prefix taken with `spec.prefix` is only released and stays active; its ranges and the VIP become deprecated. Deprecated objects still take their space in the parent; creating the same network again takes them back.

If a network of the same name is created again with a spec the retained prefix does not fit (another `prefixLength` or `spec.prefix`, or a prefix outside the `parentPrefix`), the retained prefix is not taken back: its key becomes `<key>/retired`, a new prefix is carved or `spec.prefix` adopted, and the network records `PrefixRetired`. A retained VIP outside the new prefix is retired the same way (`VIPRetired`, its key becomes `<key>/vip/retired`) and a new VIP reserved. Retired objects stay `deprecated` and keep their space like other retained ones. Objects in NetBox whose key ends in `/retired` are these.

To remove retained objects, run the cleanup script. For clusters where space matters more than protection, use `deletionPolicy: Delete`.

## Cleanup script

`hack/netbox-cleanup-retained.sh` needs bash, curl 7.76+ and jq. It needs its own token with view on `ipam.prefix`, `ipam.iprange`, `ipam.ipaddress` and delete on the three IPAM types; the provider's production token has no delete on prefixes and ranges.

```bash
export NETBOX_URL=https://netbox.example.com NETBOX_TOKEN=<token>
hack/netbox-cleanup-retained.sh <namespace>          # lists what would be deleted, with their keys
hack/netbox-cleanup-retained.sh <namespace> --yes    # deletes it
```

| Option | Meaning |
|---|---|
| `<namespace>` | the namespace of the deleted networks and pools; required |
| `--instance <name>` | the instance name of the provider (`NETBOX_INSTANCE`); keep only keys that start with it. Without it only keys without an instance match |
| `--ref-field <field>` | the text custom field of the key, as `NETBOX_REF_FIELD`; default `capi_ref` |
| `--yes` | delete; without it the script only lists |

- The script does not read the provider's settings. Pass the same `--instance` and `--ref-field` as `NETBOX_INSTANCE` and `NETBOX_REF_FIELD`; with the defaults against a provider that uses others it finds nothing ("no retained objects"; a wrong `--ref-field` also gets a warning on stderr). For example:

  ```bash
  hack/netbox-cleanup-retained.sh <namespace> --instance zone-a --ref-field my_ref
  ```

- The listing starts with the effective scope: the namespace, the field, the instance and the status. Owner and tenant are not filtered.
- Only `deprecated` objects whose key is `[<instance>/]<Kind>/<namespace>/...` are touched: the VIP (role `vip`), ranges and prefix, retired ones included; active objects, other namespaces and other instances stay.
- Run it when the namespace has no networks left: a live network's adopted prefix that an admin keeps `deprecated` carries a key of the namespace too.
- Each object is read for its `ETag` and deleted with `If-Match`, which NetBox checks, so one changed meanwhile stays; one no longer `deprecated` or no longer under the listed key when read is skipped; only HTTP 204 counts as deleted, and any other answer stops the script with its body on a line of its own.

## Check

```bash
kubectl get netboxnetworks,netboxippools -A
hack/netbox-cleanup-retained.sh <namespace>    # plus --instance <name> if the provider has one
```

- A network or pool in deletion shows why it waits in its `Ready` condition: `Deleting`, `WaitingForPools`, `WaitingForAddresses`, `WaitingForCluster`, or `Forbidden` (the token lacks delete).
- The listing of the script is empty (`no retained objects in namespace ...`) when nothing is left.

## Next

- [clusterctl move and pivot](move-and-pivot.md).
- [Cleanup recipe](../recipes/cleanup.md) and [Troubleshooting](../operations/troubleshooting.md).
- [Networks](networks.md) and [Pools](pools.md).
