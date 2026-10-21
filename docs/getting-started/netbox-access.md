# NetBox access

Goal: know what to request in NetBox so that the provider runs as an ordinary NetBox user, limited to your zone, and send the request to your NetBox admin in one message.

The provider needs no superuser and no Owner of its own. It needs:

| What | Why | Where |
|---|---|---|
| A v2 API token | the identity of the provider in NetBox | [Token](#token) |
| Object permissions on IP prefixes, IP ranges and IP addresses, limited to your zone | everything it creates, changes or deletes stays in the zone | [Permissions](#permissions) |
| One text custom field | the key: how it finds its objects again | [The key](#the-key) |
| Optionally an Owner and a Tenant | mark and fence what it creates | [Owner and tenant](#owner-and-tenant) |
| A parent prefix | the subnets of the clusters are carved from it | [Parent prefix](#parent-prefix) |

NetBox 4.6 or newer is required (tested with 4.6.7 and 4.7.2). Only IPv4 and the global VRF are supported.

The parts you cannot do yourself are collected in [What to send your NetBox admin](#what-to-send-your-netbox-admin).

## Token

The provider sends the token as `Authorization: Bearer nbt_<key>.<token>`. Only v2 tokens are supported, with write enabled. The user of the token has the permissions below. The secret part of a v2 token is shown once, when the token is created.

## Permissions

| Object types | Actions | Constraint | Needed for |
|---|---|---|---|
| `ipam.prefix`, `ipam.iprange`, `ipam.ipaddress` | view | none | always |
| `ipam.prefix`, `ipam.iprange` | add, change | your zone | always |
| `ipam.ipaddress` | add, change, delete | your zone | always: the address of a deleted claim is deleted |
| `ipam.prefix`, `ipam.iprange` | delete | your zone | `deletionPolicy: Delete`; on ranges also a change of `spec.pools` that removes or moves a range, ranges a deleted network of the same name retained, and a deprecated range of the provider in the way of a new or resized range |
| `tenancy.tenant` | view | none | a tenant given by slug (an id is accepted without it) |
| `users.owner` | view | none | an owner given by name (an id is accepted without it) |
| `extras.customfield` | view | none | optional: the provider checks the key field at start; without it a warning is logged |
| `extras.tag` | view | none | `spec.tags` |

- **View is not narrowed.** The provider reads the parent prefix, the addresses and ranges around what it creates (to pick a VIP, or to find that an address is taken), and finds its objects by their key wherever they are.
- **Writes are fenced by a constraint.** The token cannot write outside the zone. The zone is whatever you can express as a NetBox constraint: a tenant, an owner, a parent prefix, or a combination:

  | Zone fenced by | `constraints` |
  |---|---|
  | tenant | `{"tenant__slug": "platform"}` |
  | owner | `{"owner__name": "zone-a-owner"}` |
  | parent prefix, on `ipam.prefix` | `{"prefix__net_contained": "10.10.0.0/16"}` |
  | parent prefix, on `ipam.iprange` | `{"start_address__net_host_contained": "10.10.0.0/16", "end_address__net_host_contained": "10.10.0.0/16"}` |
  | parent prefix, on `ipam.ipaddress` | `{"address__net_host_contained": "10.10.0.0/16"}` |

  Keys of one constraint are combined with AND, so a tenant and a parent prefix go into one object. `prefix__net_contained` exists only on prefixes and excludes the block itself; ranges and addresses use `net_host_contained` on their address fields. A constraint applies to every object type of its permission and the field names differ per model, so a parent-prefix constraint needs one permission per model. Complete bodies are in [What to send your NetBox admin](#what-to-send-your-netbox-admin).
- **The constraint must match what the provider writes.** With `tenant__slug: platform` the provider has to write that tenant: `spec.tenant: {slug: platform}` on every network. A network without `spec.tenant` creates objects without a tenant, they fail the constraint, NetBox answers 403 and the object reports `Forbidden`. The same holds for an owner constraint and `spec.owner`. An existing object outside the constraint, such as a prefix taken with `spec.prefix`, cannot be changed either. The tenant is the border of your rights; NetBox decides whether the token may write into it.
- **An object outside the constraint answers 404.** NetBox looks for the object of a change or delete only among those the constraint of that action allows, so it answers 404, not 403. The provider tells this from an object that is gone with one more GET and reports `Forbidden` (`NetBox hides <object> from change` or `delete`); a deletion keeps its finalizer.
- **Delete on prefixes and ranges is optional.** For production, grant none and keep the default `deletionPolicy: Retain`. A network with `deletionPolicy: Delete` then keeps its finalizer on deletion and reports `Forbidden` until an admin removes its NetBox objects or the policy is set to `Retain` (see [Deletion and retained objects](../guides/deletion-and-retained.md)). Without delete on ranges, a change of `spec.pools` that removes a range, retained ranges of a recreated network and a deprecated range in the way also stay, and the network reports `Forbidden` on `LayoutReady` until they are deleted in NetBox.

## The key

Every NetBox object the provider writes carries a key (`[<instance>/]Kind/namespace/name`, see [Overview](overview.md#the-key)) in a text custom field. An object is the provider's when its key field holds exactly its key. Neither the owner nor the tenant makes an object its own.

### The custom field

An admin creates one text custom field once, `capi_ref`, enabled for `ipam.prefix`, `ipam.iprange` and `ipam.ipaddress`, with `filter_logic: exact`. NetBox filters objects by it on the server (`cf_capi_ref=<key>`), so a lookup finds an object wherever it is. The request body, with the field hidden from edit forms and shown only when set, is in [What to send your NetBox admin](#what-to-send-your-netbox-admin). On NetBox 4.7 a new custom field is provisioned in the background; the provider waits while its status is `provisioning`.

- **Another name.** `NETBOX_REF_FIELD=my_ref` (see [Install](install.md)). A name is one NetBox accepts: letters, digits and `_` in any case, without `__`.
- **An existing field.** Any text custom field enabled for the three object types can hold the key, for example one another tool created: [Reusing a custom field](../recipes/reuse-custom-field.md).
- **`filter_logic`.** `exact` is the best: NetBox returns only the object asked for. The NetBox default is `loose`, which matches a part of the value ignoring case, so a lookup reads more objects; the provider checks every result for the exact key, works with it and logs a warning at start. `disabled` makes NetBox ignore the filter: the provider refuses to start with it.

### The instance name

`NETBOX_INSTANCE` (the flag `--instance-name`) names the management zone, a DNS label of at most 63 characters, for example `zone-a`. Every key then starts with `<instance>/`. Empty, the default, gives a key without a prefix.

- **Why.** Two management planes that share a NetBox and repeat the namespace and name of a network, pool or claim would hold the same key. With distinct instance names each finds only its own objects.
- **The zone is yours to keep unique.** The provider cannot see another management plane: two with the same name (or both empty) take each other's objects.
- **The same on both sides of a move.** `clusterctl move` and pivot find the objects of the moved clusters by key, so source and target use the same name ([clusterctl move and pivot](../guides/move-and-pivot.md)).
- **A new name is a new zone.** Objects that carry the old key are not found again: a network with a status reports `PrefixNotFound` and its claims `AddressConflict`, a network without one carves a new prefix. Choose the name before the first network. To rename a live installation, rewrite the key in the key field of every object in NetBox first (prefix, VIP, ranges, addresses), with the provider scaled to zero, then change the setting.
- The same holds for a changed `NETBOX_REF_FIELD`: the provider does not move keys between fields ([Upgrade](../operations/upgrade.md#the-key-does-not-move)).

## Owner and tenant

Both are optional attributes of what the provider writes. They are set on a network and are not part of the identity.

| | On a network |
|---|---|
| Owner by name | `owner: {name: zone-a-owner}` |
| Owner by id | `owner: {id: 7}` |
| Tenant by slug | `tenant: {slug: platform}` |
| Tenant by id | `tenant: {id: 42}` |

- The CRD accepts exactly one of `name` and `id` in `owner`, one of `slug` and `id` in `tenant`.
- Name and slug are readable and the same in every NetBox (dev, stage, prod); an id survives a rename. Looking up a name or slug needs view on `users.owner` or `tenancy.tenant`, and so does the check of an id. A token that may not view them (403) gets the id taken as given, with a line in the log, and a wrong id shows later as the error NetBox answers on create. A name or slug the token may not view gives `Forbidden` on the network.
- A constrained view is not the same as none: an owner or tenant outside its constraint is not found, and the network reports `OwnerNotFound` or `TenantNotFound`. Grant view on them without a constraint, or not at all.
- Ask your NetBox admin for the ids, or read them from the object in NetBox.
- Unset, the provider never writes the field and never clears it on an existing object.
- If a write permission of the token is constrained by owner or tenant, set the same owner or tenant on the networks ([Permissions](#permissions)).

How a network resolves, writes and syncs them: [Networks](../guides/networks.md#owner-and-tenant).

## Parent prefix

Clusters carve their subnets from a parent prefix: `spec.parentPrefix` of a network. NetBox must have exactly one such prefix in the global VRF, else the network is `ParentNotFound`. It may be the prefix of the whole VLAN or a logical pool for CAPI inside it.

A subnet is carved from what `available-prefixes` of the parent reports free, and that looks at child prefixes only, not at IP addresses or ranges. Cover DHCP scopes, static addresses and the gateway of the L2 network with child prefixes of the parent, or a cluster subnet may be carved over them.

## What to send your NetBox admin

Copy this message, fill in the placeholders and send it. The bodies are for `POST` on the NetBox REST API; the same can be done in the web interface.

> I run the Cluster API IPAM provider for NetBox. It creates prefixes, IP ranges and IP addresses for Kubernetes clusters in my zone, as an ordinary user. Please set up the following.
>
> **1. The place for its key** (once per NetBox; skip it if the field exists). One text custom field, `POST /api/extras/custom-fields/`:
>
> ```json
> {"name": "capi_ref", "label": "CAPI ref", "type": "text", "filter_logic": "exact",
>  "object_types": ["ipam.prefix", "ipam.iprange", "ipam.ipaddress"],
>  "group_name": "Cluster API", "ui_visible": "if-set", "ui_editable": "hidden"}
> ```
>
> If you would rather not add a field, tell me which existing text custom field may hold the key: it has to be enabled for `ipam.prefix`, `ipam.iprange` and `ipam.ipaddress` (`PATCH /api/extras/custom-fields/<id>/` with the complete `object_types` list), with any `filter_logic` but `disabled`.
>
> **2. A user and a v2 token** with write enabled for it (`POST /api/users/tokens/` with `"version": 2`). The user's id is `<user id>` below.
>
> **3. Permissions** for that user, `POST /api/users/permissions/`. Zone: tenant `platform` (instead, use `{"owner__name": "zone-a-owner"}`, or both keys). My networks then set the same tenant or owner (`spec.tenant`, `spec.owner`).
>
> ```json
> [
>   {"name": "capi-view", "object_types": ["ipam.prefix", "ipam.iprange", "ipam.ipaddress"],
>    "actions": ["view"], "users": [<user id>]},
>   {"name": "capi-write", "object_types": ["ipam.prefix", "ipam.iprange"],
>    "actions": ["add", "change"], "constraints": {"tenant__slug": "platform"}, "users": [<user id>]},
>   {"name": "capi-write-addresses", "object_types": ["ipam.ipaddress"],
>    "actions": ["add", "change", "delete"], "constraints": {"tenant__slug": "platform"}, "users": [<user id>]}
> ]
> ```
>
> To fence the zone by a parent prefix, add its key to each constraint, one entry per model, because the field names differ:
>
> ```json
> [
>   {"name": "capi-prefix-write", "object_types": ["ipam.prefix"], "actions": ["add", "change"],
>    "constraints": {"tenant__slug": "platform", "prefix__net_contained": "10.10.0.0/16"}, "users": [<user id>]},
>   {"name": "capi-iprange-write", "object_types": ["ipam.iprange"], "actions": ["add", "change"],
>    "constraints": {"tenant__slug": "platform", "start_address__net_host_contained": "10.10.0.0/16",
>                    "end_address__net_host_contained": "10.10.0.0/16"}, "users": [<user id>]},
>   {"name": "capi-ipaddress-write", "object_types": ["ipam.ipaddress"], "actions": ["add", "change", "delete"],
>    "constraints": {"tenant__slug": "platform", "address__net_host_contained": "10.10.0.0/16"}, "users": [<user id>]}
> ]
> ```
>
> Optional, add the object types to the user's view permission: `tenancy.tenant` (I give the tenant by slug), `users.owner` (I give the owner by name), `extras.customfield` (the provider then checks the key field at start), `extras.tag` (I set tags). Or give me the ids of the tenant and owner instead.
>
> Optional, only if I may use `deletionPolicy: Delete`: delete on `ipam.prefix` and `ipam.iprange` with the same constraint.
>
> **4. Tenant and owner.** The tenant `platform` and the owner `zone-a-owner` exist (skip what I do not use). Send me their ids if I may not view them.
>
> **5. The parent prefix.** `10.10.0.0/22` exists once in the global VRF, and DHCP scopes, static addresses and the gateway of that network are covered by child prefixes of it.
>
> **6. Infrastructure** (once per NetBox). NetBox is 4.6 or newer. A proxy in front of NetBox passes the `ETag` header. PgBouncer between NetBox and PostgreSQL, if any, uses session pooling: NetBox serialises `available-prefixes` and `available-ips` with a PostgreSQL advisory lock, which a transaction pool does not hold ([netbox#23009](https://github.com/netbox-community/netbox/issues/23009)).
>
> Please send me the NetBox URL and the ids from 4, if any. Hand over the token through our secrets channel, not in this message.

A working reference for all of it is [`hack/dev/seed.sh`](../../hack/dev/seed.sh), which sets up the dev stand through the API.

## Check

With the token and the URL from your admin:

```bash
export NETBOX_URL=https://netbox.example.com NETBOX_TOKEN=nbt_<key>.<token>
curl -sS -H "Authorization: Bearer $NETBOX_TOKEN" "$NETBOX_URL/api/status/" | jq '."netbox-version"'
curl -sS -H "Authorization: Bearer $NETBOX_TOKEN" "$NETBOX_URL/api/ipam/prefixes/?prefix=10.10.0.0/22&vrf_id=null" | jq .count
```

The version is 4.6 or newer, and the count is 1: the token sees exactly one parent prefix.

The write permissions are checked when you use them: a missing permission shows as `Forbidden` on the network, pool or claim. When the provider starts, its log says `NetBox preflight passed` with the instance name and the key field it uses (see [Install](install.md#check)).

## Next

- [Install the provider](install.md).
- [Networks](../guides/networks.md) for owner and tenant per network.
- [NetBox objects the provider writes](../reference/netbox-objects.md).
