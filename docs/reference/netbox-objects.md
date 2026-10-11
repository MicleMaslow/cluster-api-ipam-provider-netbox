# NetBox objects

Goal: know exactly what the provider reads and writes in NetBox, so that you can review it with your NetBox admin, write permissions and find an object by hand. Only IPv4 and the global VRF are used. The setup the objects need is in [NetBox access](../getting-started/netbox-access.md).

## Objects the provider writes

| Kubernetes | NetBox | Written when it is created | Changed later |
|---|---|---|---|
| `NetBoxNetwork` | prefix (`ipam/prefixes/`) | carved from the parent with `prefix_length` and `is_pool`; `status: active`, the key, description and comments from the templates, owner and tenant if the network has them. An adopted `spec.prefix` gets only the key (it is created, with the same fields as a carved one, when NetBox has none) | `is_pool`, owner, tenant, description and comments are kept in sync on a carved prefix; `spec.tags` and `spec.customFields`; `status: deprecated` on release, `active` again when taken back. A prefix under `spec.prefix` is not synced after creation |
| `NetBoxNetwork` with `controlPlaneEndpoint` | IP address (`ipam/ip-addresses/`) | `address` with the mask of the L2 network, `role: vip`, `status: active`, `dns_name` from the spec, key, description and comments from the templates, owner and tenant if the network has them | `dns_name`, owner, tenant, description and comments follow the spec; `deprecated` on release |
| `NetBoxIPPool` range | IP range (`ipam/ip-ranges/`) | `start_address` and `end_address` with the mask of the L2 network, `status: active`, key, description and comments from the templates, owner and tenant of the network the range lies in if it has them | resized in place; `mark_populated` is set before a shrink, a removal or a deprecation; owner, tenant, description and comments follow the spec; `deprecated` on release |
| `IPAddressClaim` | IP address (`ipam/ip-addresses/`) | allocated from the range (`available-ips`): `status: active`, `dns_name`, description and comments from the pool's `ipAddress` templates, key, owner and tenant of the network if it has them | `dns_name`, owner, tenant, description and comments follow the spec and the templates; deleted with the claim |

An owner or tenant the network does not have is neither written nor cleared. Tags and custom fields of `spec` go to the prefix only. The templates and their defaults: [Metadata](../guides/metadata.md#description-and-comments).

## Keys

The key is `[<instance>/]Kind/namespace/name`, never the UID, which does not survive a move.

| NetBox object | Key | After a retire |
|---|---|---|
| prefix | `[<instance>/]NetBoxNetwork/<namespace>/<network>` | `<key>/retired` |
| VIP | `[<instance>/]NetBoxNetwork/<namespace>/<network>/vip` | `<key>/retired` |
| IP range | `[<instance>/]NetBoxIPPool/<namespace>/<pool>/<network>` | |
| IP address of a claim | `[<instance>/]IPAddressClaim/<namespace>/<claim>` | |

A retained prefix or VIP that does not fit a recreated network gets the `/retired` key and stays `deprecated` ([Deletion and retained objects](../guides/deletion-and-retained.md#retained-objects)).

The key lives in a text custom field, set by `--netbox-ref-field`; the optional `<instance>/` by `--instance-name` ([Configuration](../operations/configuration.md)):

| | |
|---|---|
| Setting | `capi_ref` by default; `NETBOX_INSTANCE` is empty by default |
| Type | `text`; `filter_logic: exact` is the best, `loose` works with a warning, `disabled` stops the start |
| Object types | `ipam.prefix`, `ipam.iprange`, `ipam.ipaddress` |
| Status | `active` (NetBox 4.7: `provisioning` is waited for) |

The provider does not migrate keys between fields or instance names ([Upgrade](../operations/upgrade.md#the-key-does-not-move)).

## How objects are found

The provider finds an object by its key, not by the status, so that the state survives a restart, `clusterctl move` and pivot. An object is the provider's when its key field holds exactly the key; the owner and tenant take no part in the search.

| What is looked up | Filter |
|---|---|
| prefix, VIP, address of a claim, range of a pool | `cf_<field>=<key>`, anywhere in NetBox |
| the ranges of a pool, including removed ones | `cf_<field>__isw=[<instance>/]NetBoxIPPool/<namespace>/<pool>/` |

NetBox matches text custom fields with `icontains` unless `filter_logic` is `exact`, so every result is checked again for the exact key. NetBox ignores a filter on a custom field it does not know instead of failing; with a token that may not view the field definitions the provider probes for this at start and logs a warning.

## Changelog

Every change carries a changelog message that starts with `capi:`, so that NetBox's changelog tells the provider's changes from others'.

| Message | Change |
|---|---|
| `capi: network <ref>`, `capi: range <ref>`, `capi: VIP for <ref>`, `capi: address for <ref>` | the object was created |
| `capi: adopted by <ref>`, `capi: reclaimed by <ref>`, `capi: kept by <ref>` | an existing prefix, VIP or range was taken (back) |
| `capi: resized by <ref>`, `capi: closed to allocation by <ref>`, `capi: opened again by <ref>`, `capi: replaced by <ref>` | a range was resized, its `mark_populated` toggled, or a deprecated range in the way was deleted |
| `capi: retired by <ref>` | a retained object got the `/retired` key |
| `capi: VIP of <ref> duplicates NetBox address #<id>` | the provider deleted its new VIP because an older address held the same address |
| `capi: released by <ref>` | the object was deprecated, deleted or let go |
| `capi: attributes of <ref>`, `capi: address fields for <ref>` | the owner, tenant, description or comments of an existing prefix, VIP, range or address were synced to the spec (the second also `dns_name`) |
| `capi: metadata of <ref>`, `capi: is_pool of <ref>`, `capi: dns_name of <ref>` | custom fields and tags, `is_pool` or `dns_name` of an existing object were synced (on the VIP, `dns_name of` and `reclaimed by` also carry its attributes: one entry) |
| `capi: address restored for <ref>`, `capi: restored for <ref>` | an address deleted by hand was created again, or handed back to its claim |
| `capi: retained objects of namespace <namespace> cleaned up` | the [cleanup script](../recipes/cleanup.md) deleted the object |

## What the provider calls

| Path | Used for | Permission |
|---|---|---|
| `status/` | the NetBox version | any |
| `extras/custom-fields/` | the check of the key field at start | view `extras.customfield` (optional) |
| `users/owners/`, `tenancy/tenants/` | owner by name, tenant by slug, and the check of an id | view `users.owner`, `tenancy.tenant` (an id is taken as given without it) |
| `ipam/prefixes/`, `ipam/prefixes/<id>/available-prefixes/` | prefix, parent, carving | view; add, change; delete for `deletionPolicy: Delete` |
| `ipam/ip-ranges/`, `ipam/ip-ranges/<id>/available-ips/` | ranges, allocation | view; add, change; delete for `Delete` and for a deprecated range in the way |
| `ipam/ip-addresses/` | VIP, claim addresses, free-address checks | view; add, change, delete |
| tags of the prefix (a nested `{slug}` in the update) | `spec.tags` | view `extras.tag`, which NetBox needs to resolve them |

Updates that merge with state others can change (custom fields and tags of the prefix, the bounds and `mark_populated` of a range, the adoption of a prefix) are conditional with `If-Match`; plain attributes, such as owner, tenant, description and comments, are not. The `ETag` header is required: the first single read checks it, and an answer without it is `NetBoxError`. Requests have a 30 second timeout and redirects are not followed; the proxy variables of the environment are honoured. Writes that check address space are serialised inside the provider.

## Next

- [NetBox access](../getting-started/netbox-access.md): the permissions as a request to your admin.
- [Conditions](conditions.md) and [API](api.md).
- [Recipes: cleanup](../recipes/cleanup.md) for removing what the provider left.
