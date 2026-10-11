# API

Goal: look up a field of `NetBoxNetwork` or `NetBoxIPPool`. Both kinds are namespaced, in `ipam.cluster.x-k8s.io/v1alpha1`, in the category `cluster-api`. `kubectl explain netboxnetwork.spec` and `kubectl explain netboxippool.spec` print the same descriptions. The CRDs validate with CEL; there are no admission webhooks. How the fields are used: [Networks](../guides/networks.md), [Pools](../guides/pools.md).

`IPAddressClaim` and `IPAddress` are Cluster API's (`ipam.cluster.x-k8s.io/v1beta2`); a claim points at a pool with `poolRef: {apiGroup: ipam.cluster.x-k8s.io, kind: NetBoxIPPool, name: <pool>}`.

## NetBoxNetwork

### spec

| Field | Type | Required | Meaning |
|---|---|---|---|
| `clusterName` | string, 1-63 | yes | the Cluster of the network. Immutable |
| `parentPrefix` | CIDR, IPv4 | yes | the NetBox prefix the cluster prefix is carved from, or adopted in; exactly one in the global VRF. Immutable |
| `gateway` | IPv4 | yes | the gateway of the L2 network. Immutable |
| `prefixLength` | int, 1-32 | one of `prefixLength`, `prefix` | length of the prefix carved from `parentPrefix`; longer than its mask. Immutable |
| `prefix` | CIDR | one of `prefixLength`, `prefix` | an existing prefix to adopt, or to create, inside `parentPrefix`. Immutable; no switch between `prefix` and `prefixLength` |
| `addressPrefixLength` | int, 1-32 | no | the mask of the nodes' subnet; the L2 network is `gateway/addressPrefixLength` and contains `parentPrefix`. Default: the mask of `parentPrefix`. Not longer than that mask. Immutable, cannot be added or removed |
| `isPool` | bool | no | `is_pool` on the carved prefix; needs `prefixLength`. Default false |
| `controlPlaneEndpoint` | [ControlPlaneEndpoint](#controlplaneendpoint) | no | reserve the VIP of the cluster |
| `deletionPolicy` | `Retain`, `Delete` | no | what happens to the prefix, the pool ranges in it and the VIP on deletion. Default `Retain`; `Delete` is rejected with `prefix` |
| `customFields` | map, at most 32 | no | custom fields of the prefix; the key field of the provider is reserved |
| `tags` | list of slugs, at most 32 | no | tags of the prefix; `[-a-zA-Z0-9_]+`, at most 100 characters each |
| `owner` | [OwnerRef](#ownerref--tenantref) | no | the NetBox Owner of the objects of the network and its pools, written and kept in sync. Unset: never written nor cleared. A prefix under `spec.prefix` keeps its own after creation |
| `tenant` | [TenantRef](#ownerref--tenantref) | no | the NetBox Tenant; the same rules |
| `description` | template, 1-1024 | no | description of the prefix, cut to 200 characters; variables `.Cluster`, `.Namespace`, `.Name`; default `{{ .Namespace }}/{{ .Name }} // cluster {{ .Cluster }}`. Not synced to a prefix under `spec.prefix` |
| `comments` | template, 1-1024 | no | comments of the prefix; the variables of `description`; default `// managed by cluster-api-ipam-provider-netbox, please don't edit it in NetBox unless you know what you're doing` |

The CRD also rejects an IPv6 `parentPrefix` or `gateway`, an L2 network that does not contain `parentPrefix`, a `prefix` outside `parentPrefix`, and `isPool` without `prefixLength`.

### ControlPlaneEndpoint

| Field | Type | Meaning |
|---|---|---|
| `port` | int, 1-65535 | the port of the API server behind the VIP. Default: the port of the Cluster's endpoint, else 6443. Immutable while `controlPlaneEndpoint` is set |
| `dnsName` | string, 1-255 | `dns_name` of the VIP; NetBox stores it in lower case |
| `description` | template, 1-1024 | description of the VIP, cut to 200 characters; the variables of the network's `description`; default `{{ .Namespace }}/{{ .Name }} // {{ .Cluster }} control plane endpoint` |
| `comments` | template, 1-1024 | comments of the VIP; default as for the network |

### OwnerRef / TenantRef

| Kind | Fields | Rule |
|---|---|---|
| `OwnerRef` | `name` (1-100) or `id` (at least 1) | exactly one of the two. A name that NetBox does not have, or an id it does not have, is `OwnerNotFound`; a 403 on checking an id (no view on owners) accepts it as given, and NetBox then refuses a missing one on create |
| `TenantRef` | `slug` (`[-a-zA-Z0-9_]+`, 1-100) or `id` (at least 1) | exactly one of the two; `TenantNotFound` and a 403 on checking an id likewise |

### status

| Field | Meaning |
|---|---|
| `conditions` | `Ready`, `MetadataSynced`, `ControlPlaneEndpointReady`, `Paused` ([Conditions](conditions.md)) |
| `prefix`, `prefixID` | the CIDR and the NetBox id of the prefix |
| `addressPrefixLength` | the mask handed to addresses |
| `controlPlaneEndpoint` | `host` (the VIP) and `port` of the API server behind it. It stays after `spec.controlPlaneEndpoint` is removed, so that pool ranges keep off the VIP |
| `ownerID`, `tenantID` | a cache: the NetBox ids `spec.owner` and `spec.tenant` resolve to, used for the objects of the network and its pools. Looked up again on a new generation, when nothing is cached, or when the reason of `Ready` is `WaitingForCluster`, `InvalidSpec`, `NetBoxError`, `Forbidden`, `OwnerNotFound` or `TenantNotFound`; unset without `spec.owner` or `spec.tenant` |
| `customFieldKeys` | per JSON custom field, the keys the provider wrote |
| `tags` | the tags the provider set on the prefix |

`kubectl get netboxnetworks` shows `Cluster`, `Prefix`, `VIP` (`status.controlPlaneEndpoint.host`), `Ready`, `Reason` and `Age`.

## NetBoxIPPool

### spec

| Field | Type | Required | Meaning |
|---|---|---|---|
| `ranges` | list of [ranges](#range), 1-16 | yes | used in order; one range per network |
| `ipAddress` | object | no | `dnsName`, `description` and `comments` Go templates for the pool's NetBox addresses; at least one; each at most 1024 bytes. Defaults `{{ .Owner }}`, `{{ .Namespace }}/{{ .Owner }} // {{ .Cluster }} {{ .Pool }}` and the standard comments; variables `.Owner`, `.Claim`, `.Cluster`, `.Namespace`, `.Pool` |
| `ipRange` | object | no | `description` and `comments` Go templates for the pool's NetBox IP ranges; at least one. Default description `{{ .Namespace }}/{{ .Pool }} // pool of cluster {{ .Cluster }}`, the standard comments; variables `.Cluster`, `.Namespace`, `.Pool`, `.Network` |
| `addressReuseGracePeriodSeconds` | int, 0-86400 | no | keep the address of a deleted claim in NetBox this long. Default 0 |
| `omitGateway` | bool | no | hand out addresses without a gateway. Default false. Immutable, cannot be added or removed |

The defaults, the rules of the templates and the standard comments are in [Metadata](../guides/metadata.md#description-and-comments).

### Range

| Field | Type | Meaning |
|---|---|---|
| `networkName` | string, 1-63 | a `NetBoxNetwork` in the pool's namespace |
| `start`, `end` | int | offsets from the first address of the network's prefix; a negative offset counts back from the last (`-1`). `start` must not be after `end` when both have the same sign |

### status

| Field | Meaning |
|---|---|
| `conditions` | `Ready`, `RangesReady`, `Paused` ([Conditions](conditions.md)) |
| `ranges[]` | per range, in spec order, then removed ranges that still hold addresses: `networkName`, `id` (NetBox), `startAddress`, `endAddress`, `ready`, `reason`, `message` |
| `gateway`, `addressPrefixLength` | what every address of the pool gets; the gateway is empty with `omitGateway` |
| `ipAddresses` | `total`, `used`, `free` over the ready ranges |

`kubectl get netboxippools` shows `Total`, `Used`, `Free`, `Ready`, `Reason` and `Age`.

## Immutable fields

| Kind | Field | Rule |
|---|---|---|
| `NetBoxNetwork` | `clusterName`, `parentPrefix`, `gateway`, `prefixLength`, `prefix` | cannot be changed; `prefix` and `prefixLength` cannot be switched |
| | `addressPrefixLength` | cannot be changed, added or removed |
| | `controlPlaneEndpoint.port` | cannot be changed, added or removed while `controlPlaneEndpoint` is set; the whole field can be removed |
| `NetBoxIPPool` | `omitGateway` | cannot be changed, added or removed |

Everything else changes and is synced to NetBox (except that a prefix under `spec.prefix` keeps its owner, tenant, description and comments): `isPool`, `customFields`, `tags`, `owner`, `tenant`, `description`, `comments`, `controlPlaneEndpoint.dnsName`, `.description` and `.comments`, the `ranges`, `ipAddress` and `ipRange` of a pool. `deletionPolicy` and `addressReuseGracePeriodSeconds` change too and act when they are needed. To change an immutable field, create the object again.

## Labels, annotations and finalizers

| Name | On | Meaning |
|---|---|---|
| `cluster.x-k8s.io/cluster-name` (label) | pool, claim | the Cluster; pausing it pauses the pool and its claims |
| `cluster.x-k8s.io/paused` (annotation) | network, pool, claim | skip the object |
| `cluster.x-k8s.io/watch-filter` (label) | network, pool, claim | reconciled only when its value equals `--watch-filter` |
| `clusterctl.cluster.x-k8s.io/delete-for-move` (annotation) | network, pool, claim | set by `clusterctl move`: the deletion releases nothing in NetBox |
| `clusterctl.cluster.x-k8s.io/move-hierarchy` (label on the CRDs) | CRDs | `clusterctl move` moves the objects of both kinds with their Cluster |
| `netbox.ipam.cluster.x-k8s.io/network`, `netbox.ipam.cluster.x-k8s.io/pool` (finalizers) | network, pool | release the NetBox objects on deletion |

## Next

- [Conditions](conditions.md) and [NetBox objects](netbox-objects.md).
- [Configuration](../operations/configuration.md): the flags of the provider.
