# Metadata

Goal: set the text, the tags and the custom fields that people and tools read in NetBox next to the objects of a cluster. The description and comments belong to every object the provider writes; tags and custom fields to the prefix only. The owner and tenant are in [Networks](networks.md#owner-and-tenant).

## Steps

1. In NetBox, make sure the tags and custom fields exist and are enabled for `ipam.prefix`: the provider sets them, it does not create them. Custom fields of an admin's choice are fine; the key field of the provider is not for this ([below](#the-key-field-is-reserved)).

2. Set them on the network:

   ```yaml
   spec:
     description: "{{ .Namespace }}/{{ .Name }} // network of {{ .Cluster }}, team platform"
     tags: [capi, k8s]
     customFields:
       team: platform        # text
       priority: 2           # integer
       monitored: true       # boolean
       site_tier: standard   # select
       vlan: 1               # object, the id of the VLAN
       labels:               # json, merged key by key
         cluster: demo
         env: dev
   ```

   The provider writes tags and custom fields in one update, with `If-Match`, so a concurrent edit in NetBox is not lost. `description` is optional: without it the prefix gets the default of the table below.

## Description and comments

Every NetBox object the provider writes gets a description, for people, and comments, which say that the object is managed by the provider. Both are Go templates with defaults, written when the object is created and kept in sync with the spec: a value edited in NetBox by hand is overwritten at the next reconcile.

| NetBox object | Fields in the spec | Variables | Default description | Default comments |
|---|---|---|---|---|
| prefix | `NetBoxNetwork.spec.description`, `.comments` | `.Cluster`, `.Namespace`, `.Name` | `{{ .Namespace }}/{{ .Name }} // cluster {{ .Cluster }}` | the comments below |
| VIP | `NetBoxNetwork.spec.controlPlaneEndpoint.description`, `.comments` | `.Cluster`, `.Namespace`, `.Name` | `{{ .Namespace }}/{{ .Name }} // {{ .Cluster }} control plane endpoint` | the comments below |
| IP range | `NetBoxIPPool.spec.ipRange.description`, `.comments` | `.Cluster`, `.Namespace`, `.Pool`, `.Network` | `{{ .Namespace }}/{{ .Pool }} // pool of cluster {{ .Cluster }}` | the comments below |
| IP address | `NetBoxIPPool.spec.ipAddress.description`, `.comments` | `.Owner`, `.Claim`, `.Cluster`, `.Namespace`, `.Pool` | `{{ .Namespace }}/{{ .Owner }} // {{ .Cluster }} {{ .Pool }}` | the comments below |

The default comments of every object are `// managed by cluster-api-ipam-provider-netbox, please don't edit it in NetBox unless you know what you're doing`.

- `.Cluster` is the Cluster of the network (`spec.clusterName`), of the claim for an address; `.Name` the network; `.Network` the network of the range; `.Pool` the pool. `.Owner` of an address is the first owner reference of the claim, or the claim itself without one (with CAPV the VSphereVM): a Kubernetes owner, not the NetBox Owner. The `dnsName` of the VIP and of the addresses is set next to them: [Control-plane VIP](control-plane-vip.md), [Pools](pools.md#address-options).
- A field left out takes its default. `{{ "" }}` leaves the field empty in NetBox.
- Results are trimmed. The description is cut to 200 characters, the limit of NetBox; comments are not cut. A template may use `if` and `with`, but not `range`, `define`, `block` or `template`; a rendered value is limited to 1024 bytes and may not contain a NUL character.
- A template that does not render is `InvalidSpec` (a value that fails for one claim only fails that claim: [Pools](pools.md#address-options)). On a network that is `Ready=False InvalidSpec` and its pools take its ranges out of service: new claims wait, allocated ones stay `Ready`. On a pool it is `Ready=False InvalidSpec`, with the same effect on new claims. An `ipRange` template that renders the sample values but fails on those of one range is `RangesReady=False InvalidSpec`: that range keeps serving and keeps its description and comments (a new one is created without them). Fix the spec and it goes on.
- A prefix under `spec.prefix` is not synced: an existing one keeps its description and comments, a missing one gets them once, when it is created.

## Custom fields

- A JSON-object value is merged key by key: keys given here are written, keys the provider wrote earlier and that are no longer given are removed (`status.customFieldKeys`), other keys stay. Any other value replaces the field.
- Values are compared with what NetBox returns in the form they are written in: the string of a select or multiselect value, the id of an object or multi-object field, a multi-object list without regard to its order. Write a datetime as ISO 8601 (`2026-01-02T03:04:05`) and repeat no id in a multi-object list: NetBox rewrites other forms, such a value never matches and is written at every reconcile.
- A failure shows in the `MetadataSynced` condition and never blocks `Ready`.

## Tags

`tags` are slugs of existing NetBox tags set on the prefix. A missing tag fails `MetadataSynced`. Tags added by others stay; tags the provider set (`status.tags`) and that are no longer given are removed. The token needs view on `extras.tag` ([NetBox access](../getting-started/netbox-access.md#permissions)).

## The key field is reserved

The text custom field that holds the key (`capi_ref` by default) is managed by the provider. `customFields` cannot set it: that fails `MetadataSynced` with the reason `InvalidSpec`.

## Check

```bash
kubectl -n demo get netboxnetwork demo -o jsonpath='{.status.conditions}'
id=$(kubectl -n demo get netboxnetwork demo -o jsonpath='{.status.prefixID}')
curl -sf -H "Authorization: Bearer $NETBOX_TOKEN" "$NETBOX_URL/api/ipam/prefixes/$id/" | jq -c '{description, comments, tags: [.tags[].slug], custom_fields}'
```

`MetadataSynced` is `True`, and the prefix has the description, the comments, the tags and the values. Reasons: `InvalidSpec` (a reserved key), `NetBoxError`, `Forbidden`; the condition is `Unknown` with `WaitingForPrefix` while the prefix is not `Ready`.

## Next

- [Networks](networks.md) and [Pools](pools.md).
- [Conditions](../reference/conditions.md).
- [Reuse a custom field](../recipes/reuse-custom-field.md): keep the key in a field you already have.
