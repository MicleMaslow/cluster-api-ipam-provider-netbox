# Cleanup

Goal: remove what the deleted networks and pools of a namespace left in NetBox under `deletionPolicy: Retain`: the control-plane VIP, the pool IP ranges and the prefix, `deprecated` and still taking their space.

`hack/netbox-cleanup-retained.sh` finds them by the namespace in their key and deletes them. It needs bash, curl 7.76 or newer and jq, and its own token: view on `ipam.prefix`, `ipam.iprange`, `ipam.ipaddress` and delete on the three types. The production token of the provider has no delete on prefixes and ranges. What stays after a deletion and why: [Deletion and retained objects](../guides/deletion-and-retained.md).

## Steps

1. **Export the NetBox settings** of the script:

   ```bash
   export NETBOX_URL=https://netbox.example.com NETBOX_TOKEN=<token>
   ```

2. **List what would be deleted** (dry run, the default). The script starts with the scope it uses, then prints `<type> <id> <object> <key>` for every object:

   ```bash
   hack/netbox-cleanup-retained.sh <namespace>
   ```

   The instance name and the key field must be those of the provider. With the defaults (no instance, `capi_ref`) they are right. Otherwise pass the same values as `NETBOX_INSTANCE` and `NETBOX_REF_FIELD`:

   ```bash
   hack/netbox-cleanup-retained.sh <namespace> --instance zone-a --ref-field my_ref
   ```

3. **Read the listing,** especially the keys. Then delete with `--yes`, the same command plus the flag:

   ```bash
   hack/netbox-cleanup-retained.sh <namespace> --yes
   ```

## Options

| Option | Meaning |
|---|---|
| `<namespace>` | the namespace of the deleted networks and pools; required |
| `--instance <name>` | the instance name of the provider (`NETBOX_INSTANCE`); keep only keys that start with it. Without it only keys without an instance match, so objects of another instance are never taken |
| `--ref-field <field>` | the text custom field of the key, as `NETBOX_REF_FIELD`; default `capi_ref` |
| `--yes` | delete; without it the script only lists |

## Limits

- **Only `deprecated` objects** whose key is `[<instance>/]<Kind>/<namespace>/...` are touched: the VIP (role `vip`), ranges and prefix, retired ones (`<key>/retired`) included. Active objects, other namespaces and other instances stay, so do objects a deletion could not find ([Troubleshooting](../operations/troubleshooting.md#log-lines-that-explain-a-leftover)). Owner and tenant are not filtered.
- **Run it when the namespace has no networks left:** a live network's adopted prefix that an admin keeps `deprecated` carries a key of the namespace too.
- **It does not read the provider's settings.** With the defaults against a provider that keeps its key elsewhere it finds nothing. If NetBox returned deprecated objects and none carries the key in `--ref-field`, the script warns on stderr (`none carries the key in ...`) before the usual `no retained objects` or a listing without keys; that is the sign of a wrong `--ref-field`.
- Each object is read for its `ETag` and deleted with `If-Match`, which NetBox checks, so one changed meanwhile stays. One that is no longer `deprecated` or no longer carries the listed key when it is read is skipped (`skipped ...`). Only HTTP 204 counts as deleted; any other answer stops the script with the body on a line of its own.
- The `make vcsim-delete` of the [dev stand](../development/dev-stand.md#delete-a-cluster) runs the script with the admin token of the stand, and with `--instance` when `NETBOX_INSTANCE` is set.

## Check

```bash
hack/netbox-cleanup-retained.sh <namespace>
```

The listing is empty (`no retained objects in namespace <namespace>, instance <name>`, or `, no instance`) when nothing is left; the scope line above it shows the namespace, field and instance that were used. The prefix and the VIP disappear from NetBox, and the changelog has the message `capi: retained objects of namespace <namespace> cleaned up`.

## Next

- [Deletion and retained objects](../guides/deletion-and-retained.md) and [Troubleshooting](../operations/troubleshooting.md).
- [Reusing a custom field](reuse-custom-field.md): keys in a field you already have.
