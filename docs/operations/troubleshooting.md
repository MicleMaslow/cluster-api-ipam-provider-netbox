# Troubleshooting

Goal: go from a symptom to the cause and the fix. Every reason a condition can show is in [Conditions](../reference/conditions.md); this page has the commands and the recoveries that need more than one line.

## Look first

```bash
kubectl get netboxnetworks,netboxippools -A
kubectl get ipaddressclaims.ipam.cluster.x-k8s.io,ipaddresses.ipam.cluster.x-k8s.io -A
kubectl -n <ns> get netboxippool <pool> -o jsonpath='{.status.ranges}'
kubectl -n <ns> get netboxnetwork <network> -o jsonpath='{.status.controlPlaneEndpoint} {.status.layout} {.status.conditions}'
kubectl -n <ns> events --for netboxnetwork/<network>
kubectl -n capi-ipam-netbox-system logs deployment/capi-ipam-netbox-controller-manager
```

- Use the full resource names: in Kubernetes 1.33 and later the plain resource name `ipaddresses` resolves to `networking.k8s.io`.
- Every change the provider makes in NetBox carries a changelog message that starts with `capi:`: NetBox's changelog tells which change was the provider's.
- States that only a change in NetBox can clear are checked again every minute; the others are woken by what they wait for ([Conditions](../reference/conditions.md#how-a-reason-is-rechecked)).

| Symptom | Go to |
|---|---|
| the pod is not ready or restarts | [The provider does not start](#the-provider-does-not-start) |
| `Ready=False` on a network, with `OwnerNotFound` or `TenantNotFound` | [OwnerNotFound or TenantNotFound](#ownernotfound-or-tenantnotfound) |
| the owner, tenant or text in NetBox is not what the spec says | [Attributes are not synced](#attributes-are-not-synced) |
| `PrefixNotFound` | [A prefix is gone](#a-prefix-is-gone) |
| `Forbidden` | [Forbidden](#forbidden) |
| `NetBoxError` | [NetBoxError](#netboxerror) |
| `LayoutReady=False` on a network | [Layout](#layout) |
| a pool has no ready range, or a claim is `PoolNotReady`, `PoolExhausted`, `AllocationFailed` | [Pools and claims](#pools-and-claims) |
| a deleted object left something in NetBox | [Log lines that explain a leftover](#log-lines-that-explain-a-leftover), [Deletion and retained objects](../guides/deletion-and-retained.md) |

## The provider does not start

While NetBox cannot be reached or answers with an error, the pod stays alive and not ready: the log says `NetBox preflight failed, retrying` with the error and the wait. Fix NetBox or the URL; the provider starts by itself.

When NetBox or the settings are such that the provider cannot work, it logs the reason and exits (code 1, and code 2 for an unknown flag):

| Message contains | Cause and fix |
|---|---|
| `NETBOX_URL is required`; `exactly one of NETBOX_TOKEN and NETBOX_TOKEN_FILE is required` | the Secret `capi-ipam-netbox-credentials` or the environment lacks it, or has both token variables |
| `token file ... is empty`; `read token file` | `NETBOX_TOKEN_FILE` points at a file that is missing or empty ([Install](../getting-started/install.md#token-from-a-file)) |
| `read CA file`; `has no PEM certificates` | `--netbox-ca-file` names a missing file or one without a PEM certificate. After a patch that replaced the volumes, the CA volume is gone ([Install](../getting-started/install.md#token-from-a-file)) |
| `unknown flag` | an own patch of the Deployment passes an argument the provider does not have ([Configuration](configuration.md)) |
| `NetBox settings: ... name must match` | a field name NetBox does not accept (a dot or `__`, for example) ([Configuration](configuration.md)) |
| `NetBox settings: instance name ...` | `NETBOX_INSTANCE` is not a DNS label of at most 63 characters ([The instance name](../getting-started/netbox-access.md#the-instance-name)) |
| `NetBox ≥ 4.6 required` | NetBox is older than 4.6 |
| `custom field <name>: not found` | the key field does not exist; create it ([NetBox access](../getting-started/netbox-access.md#the-key)) |
| `custom field <name>: type is ..., want text` | the field is not a text field |
| `custom field <name>: NetBox ignores cf_ filters with filter_logic disabled` | the key field has `filter_logic: disabled`; set it to `exact` (or `loose`) |
| `custom field <name>: not enabled for ...` | enable it for `ipam.prefix`, `ipam.iprange` and `ipam.ipaddress` |
| `custom field <name>: status is deleting` | the field is being deleted; `provisioning` (NetBox 4.7) is waited for |

### Warnings at start

Lines that start with `WARNING:` do not stop the start.

| Warning | Meaning |
|---|---|
| `NETBOX_URL is plain http, the token is sent unencrypted` | use `https://` |
| `NetBox TLS certificate verification is disabled` | `--netbox-insecure-skip-verify` is set; development only |
| `custom field <field>: filter_logic is loose, not exact: lookups read more objects` | the key field is `loose` (the NetBox default): the provider works, checks every result for the exact key and reads more objects. Set `exact` to get rid of it |
| `the token may not view custom field definitions (extras.customfield), <field> is not checked` | the token has no view on `extras.customfield`. The field is checked by behavior (a filter on prefixes, which needs view on `ipam.prefix`). Grant the view to get the checks |
| `NetBox ignores the filter cf_<field>: the custom field is missing or not filterable, lookups read more objects` | the probe found that NetBox does not filter by the field: create it as a text field with `filter_logic: exact` |

## OwnerNotFound or TenantNotFound

`spec.owner` or `spec.tenant` names an owner or tenant NetBox does not have: a name, a slug or an id, for example after a rename, or outside a constrained view of the token. The network is `Ready=False` and the message names what was not found; it is checked again every minute. The pools of the network hold back new claims meanwhile; allocated ones stay `Ready` ([Conditions](../reference/conditions.md#reasons)).

A name or slug is looked up when the generation of the network changes or no id is cached, an id is checked then too. Otherwise the cached id is kept; when the reason of `Ready` is `WaitingForCluster`, `InvalidSpec`, `NetBoxError`, `Forbidden`, `OwnerNotFound` or `TenantNotFound`, the name is looked up again at every reconcile. So a rename in NetBox shows on the next resolve, after a spec change or a `NetBoxError` or `Forbidden` on `Ready`: give the id to survive renames. Point the spec at what exists:

```bash
kubectl -n <ns> patch netboxnetwork <network> --type=merge -p '{"spec":{"owner":{"name":null,"id":7}}}'    # by id: survives a rename
kubectl -n <ns> patch netboxnetwork <network> --type=merge -p '{"spec":{"tenant":{"slug":"new-slug","id":null}}}'    # or the new slug
```

Check that the network is `Ready=True` again and has its ids (within a minute of the patch):

```bash
kubectl -n <ns> get netboxnetwork <network> -o jsonpath='{.status.ownerID} {.status.tenantID} {.status.conditions}'
```

If the token may not view owners or tenants (403), an id is accepted as given and the log says `The token may not check the id in NetBox, it is taken as given`; a name or slug gives `Forbidden`. A wrong id then shows as the error NetBox answers on create.

## Attributes are not synced

The owner, tenant, description or comments of an existing object differ from the spec, or `MetadataSynced` or `LayoutReady` of the network is `False`. A failed sync stops no allocation: the message says which object and why, for example `attributes of the prefix 10.10.0.0/26: ... forbidden`.

- `Forbidden`: a constraint of the write permission does not allow the value, for example another tenant than the permission is limited to. Change the spec or the permission ([NetBox access](../getting-started/netbox-access.md#permissions)).
- `HTTP 400`: NetBox refuses the value, for example an owner id it does not have.
- A claim's address: a failed sync stays out of the claim's status. The claim stays `Ready`, with no condition and no Event; the error shows only in the controller log and as a reconcile error of the claim (`Reconciler error`, retried with backoff).
- Not synced by design: a field the spec leaves out (an owner or tenant is never cleared), and a prefix under `spec.prefix` after its creation.
- A synced change is an Event `AttributesSynced` on the network or claim and a changelog message `capi: attributes of ...` or `capi: address fields for ...` in NetBox. A value edited in NetBox by hand is overwritten at the next reconcile.

## A prefix is gone

`PrefixNotFound`: the prefix in `status.prefix` is not in NetBox, or the status is empty and the active VIP of the network is. The provider does not carve a new prefix, because machines may use addresses of the old one:

```
NetBox prefix 10.10.0.0/26 of NetBoxNetwork/<ns>/<network> is gone, restore it
```

Create the prefix again in NetBox with the same CIDR, the key in the key field (`[<instance>/]NetBoxNetwork/<ns>/<network>`). The condition clears within a minute.

The same state appears when the prefix is still there but no longer carries this key, because the instance name or the key field of the provider changed. Put the old setting back, or rewrite the keys in NetBox ([The instance name](../getting-started/netbox-access.md#the-instance-name)).

## Forbidden

The token lacks a permission, or a constraint of the permission does not match what the provider writes. The message starts with the request, for example `POST ipam/ip-ranges/...: netbox: forbidden`.

- Compare the request with the table in [NetBox access](../getting-started/netbox-access.md#permissions): add, change and delete per object type.
- A constraint such as `tenant__slug: platform` needs the provider to write that tenant, and `owner__name` the owner: an object without it fails the constraint. A network without `spec.tenant` or `spec.owner` writes none: check them on the network.
- Delete on ranges is needed when the layout changes (the range of a pool that moved or left `spec.pools`), for deprecated ranges in the way and for `deletionPolicy: Delete`; on prefixes only for `Delete`. A network in deletion with no delete keeps its finalizer until an admin removes the objects ([Deletion and retained objects](../guides/deletion-and-retained.md#permissions)).

## NetBoxError

Any other NetBox error; the message has the request and the answer. The common ones:

| Message contains | Cause and fix |
|---|---|
| `answers without ETag (a proxy strips it?)` | a proxy in front of NetBox drops the `ETag` header; pass it |
| `HTTP 400` | NetBox rejected the request; the body says which field. For example a custom field that is required, or one that does not accept the value |
| `HTTP 301`, `302`, `307`, `308` | redirects are not followed: give the final `NETBOX_URL` (for example the `https://` one) |
| `HTTP 404` with an HTML body | `NETBOX_URL` is not the root of NetBox's API (a path prefix of the proxy?) |
| `HTTP 500`, `502`, `503` | NetBox or its proxy fails; the request is retried with backoff |
| `modified since read` | the object changed in NetBox between read and update; retried |
| `x509: certificate signed by unknown authority` | add the CA ([Install](../getting-started/install.md#tls)) |
| `dial tcp`, `no such host`, timeout | NetBox is unreachable from the pod; the requests have a 30 second timeout |

## Layout

```bash
kubectl -n <ns> get netboxnetwork <network> -o jsonpath='{.status.layout}'
```

- `InvalidSpec`: the pools do not fit in the free addresses of the prefix, and the message has the numbers (`cp 20, workers at least 1 need 21 free addresses, 16 are free from ...`). Shrink the pools or free addresses in NetBox. After `clusterctl move` or a restore, `sizes must be those found: ...` means `spec.pools` do not match the ranges in NetBox: put the sizes found back, and change them once the network has its layout again.
- `RangeInUse`: the change would move the listed allocated addresses to another pool, or reserve them for a group (an `IPAddress` of a pool the layout does not have). Keep the pools in front of them as they are and change the tail, or wait until those machines are gone. The layout and the ranges stay as they were.
- `RangeOverlap`: a range of a pool overlaps a NetBox range the layout did not see; remove or shrink that range.
- `SecretConflict`: a Secret that `publish` names exists and the network does not control it (`kubectl -n <ns> get secret <name> -o jsonpath='{.metadata.ownerReferences}'`). Delete it, or name another `secretName`; the network never changes it.

## Pools and claims

```bash
kubectl -n <ns> get netboxippool <pool> -o jsonpath='{range .status.ranges[*]}{.networkName}{" "}{.ready}{" "}{.reason}{" "}{.message}{"\n"}{end}'
kubectl -n <ns> get ipaddressclaim <claim> -o jsonpath='{.status.conditions}'
```

- A range is out of service (`NetworkNotReady`) while its network is not `Ready`, which includes `OwnerNotFound`, `TenantNotFound` and an `InvalidSpec` of the network. Fix the network. A new claim is `PoolNotReady` meanwhile if the pool has no other ready range; allocated claims stay `Ready`.
- `pool X is not placed in network Y`: `ranges[].pool` names no pool of the network's `spec.pools`, or the network has not placed it (see its `LayoutReady`).
- `Duplicate`: another `NetBoxIPPool` of the namespace, created first, serves the same network pool. One `NetBoxIPPool` per network pool.
- On a claim, a NetBox error, a 403 or a template of the pool that does not render for this claim during allocation shows as `AllocationFailed` with the message. An allocated address deleted in NetBox by hand is created again with the same address, or the claim reports `AddressConflict` if NetBox gave it to another object.
- A claim stuck in deletion, for example without delete on `ipam.ipaddress`, is `ReleaseFailed` with the error: the claim, its `IPAddress` and the NetBox address stay (CAPV still finishes deleting the Machine), and deleting the pool waits with `WaitingForAddresses`.
- A claim whose `spec.clusterName` or `cluster.x-k8s.io/cluster-name` label names a Cluster that does not exist is skipped without a condition; the provider logs it at Info level.

## Log lines that explain a leftover

A deletion finds its objects by key and does not wait for ones it cannot find. What it could not find stays in NetBox until you remove it by its key (`kubectl logs`, then search NetBox for the key; the [cleanup script](../recipes/cleanup.md) removes deprecated objects only). Info lines that matter:

| Line | Meaning |
|---|---|
| `The NetBox prefix of the network is not found under its key: nothing to release` | a network that had a prefix is deleted, and NetBox holds none with its key: it was removed, or carries another key (the instance name or the key field changed). Find it by its old key |
| `Another NetBox address holds the control plane VIP too, it is newer: keeping the VIP` | informational: NetBox lets VIP-role addresses share an address; the older one wins |
| `Deleted the control plane VIP, an older NetBox address holds it` | the same with the event `VIPDeleted`; the next reconcile picks another address |
| `Synced attributes of a NetBox object` | the owner, tenant, description or comments of an object were changed to the spec; the line names the attributes |
| `The token may not check the id in NetBox, it is taken as given` | an owner or tenant id was accepted without a check ([OwnerNotFound or TenantNotFound](#ownernotfound-or-tenantnotfound)) |
| `Waiting for the pool to get ready` | a claim waits for its pool |

Objects in NetBox whose key ends in `/retired` are retained objects that a recreated network did not take back because they did not fit its spec ([Deletion and retained objects](../guides/deletion-and-retained.md#retained-objects)). They are `deprecated`, still take their space, and the cleanup script removes them.

## Next

- [Conditions](../reference/conditions.md), [Observability](observability.md).
- [Upgrade](upgrade.md) for the moves between releases.
- [Cleanup](../recipes/cleanup.md).
