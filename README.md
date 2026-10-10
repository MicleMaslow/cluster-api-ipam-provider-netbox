# cluster-api-ipam-provider-netbox

Cluster API IPAM provider backed by NetBox. It carves a subnet for every cluster out of a parent prefix, keeps the pool ranges as NetBox IP ranges and hands out the addresses of `IPAddressClaim`s from NetBox.

| Kubernetes | NetBox |
|---|---|
| `NetBoxNetwork` | prefix, carved from a parent prefix or adopted |
| `NetBoxIPPool` | one IP range per range of the pool |
| `IPAddressClaim` pointing at a `NetBoxIPPool` | IP address allocated from a range |

NetBox stays the source of truth: everything the provider writes carries its Owner and a key, so the state is found again after a restart, `clusterctl move` or pivot. The design is described in [docs/proposals/20261007-netbox-ipam-provider.md](docs/proposals/20261007-netbox-ipam-provider.md).

## Compatibility

| Provider | Cluster API | NetBox |
|---|---|---|
| v0.1.x | v1.14 (contract v1beta2) | ≥ 4.6 (tested with 4.6.7, 4.7.2) |

The provider works with any infrastructure provider that consumes `IPAddressClaim`s, for example CAPV `addressesFromPools`. Only IPv4 and the global VRF are supported.

## NetBox setup

Set NetBox up before installing the provider. On start the provider checks the NetBox version, the two custom fields, the `ETag` header and the Owner (see [Startup](#startup)); write permissions and parent prefixes are not checked there and show up later as conditions. [`hack/dev/seed.sh`](hack/dev/seed.sh) sets all of this up through the API and can serve as a reference.

- NetBox 4.6 or newer (the provider uses Owners, v2 API tokens and ETag/If-Match on updates). A proxy in front of NetBox must pass the `ETag` header.
- Custom fields `capi_ref` and `capi_cluster`: type text, `filter_logic: exact`, object types `ipam.prefix`, `ipam.iprange`, `ipam.ipaddress`, and, in NetBox 4.7, status `active`. The provider stores its idempotency key and the cluster name in them.
- An Owner for the provider: the identity of its management plane. It marks everything the provider manages; objects of other owners are never touched. Every management plane needs its own Owner and keeps it through `clusterctl move` and pivot. The provider's key (`capi_ref`) is `Kind/namespace/name`, so management planes that share a NetBox must not repeat the namespace and name of these objects: a repeat blocks the other plane with `OwnedByOtherOwner`. For pool ranges the key is `NetBoxIPPool/namespace/pool/network`.
- The parent prefixes, in the global VRF, that clusters carve their subnets from. A subnet is carved from what `available-prefixes` of the parent reports free, and that looks at child prefixes only, not at IP addresses or ranges: cover DHCP scopes, static addresses and the gateway of the L2 network with child prefixes of the parent, or a cluster subnet may be carved over them.
- A user with a v2 token (`nbt_<key>.<token>`, write enabled) and the permissions below.
- Other tools must not take addresses with the prefix-level `available-ips` of a cluster prefix: since NetBox 4.3 it also returns addresses inside IP ranges that are not marked populated, as the pool ranges are.
- PgBouncer between NetBox and PostgreSQL must use session pooling: NetBox serialises `available-prefixes` and `available-ips` with a PostgreSQL advisory lock, which a transaction pool does not hold ([netbox#23009](https://github.com/netbox-community/netbox/issues/23009)).

### Permissions

| Object types | Actions | Constraints |
|---|---|---|
| `ipam.prefix`, `ipam.iprange`, `ipam.ipaddress`, `users.owner`, `extras.customfield`, `extras.tag` | view | none |
| `ipam.ipaddress` | add, change, delete | `{"owner__name": "<owner>"}` |
| `ipam.prefix`, `ipam.iprange` | add, change | `{"owner__name": "<owner>"}` |
| `ipam.prefix`, `ipam.iprange` | delete | `{"owner__name": "<owner>"}`; see below |

- View is unconstrained: the owner guard, adoption and the restore of addresses look at objects of other owners, and carving reads the parent prefix.
- Add, change and delete are constrained by the owner: the token cannot write objects of other owners.
- Delete on prefixes and ranges is needed only with `deletionPolicy: Delete`, and on ranges to delete deprecated ranges of the provider that are in the way of a new or resized range (without it the pool reports `Forbidden` until the deprecated range is deleted in NetBox). Addresses of deleted claims are always deleted.
- For production, grant no delete on `ipam.prefix` and `ipam.iprange` and keep the default `deletionPolicy: Retain`. A network with `deletionPolicy: Delete`, or a pool with a range in such a network, then keeps its finalizer on deletion and reports `Forbidden` until an admin removes its NetBox objects or the policy is set to `Retain`.

The write permissions can be narrowed further, for example to an address block. A constraint applies to every object type of its permission, and the field names differ per model, so such a permission needs one entry per model. Example bodies for `POST /api/users/permissions/` (add `"users": [<id>]`):

```json
[
  {"name": "capi-prefix-write", "object_types": ["ipam.prefix"], "actions": ["add", "change"],
   "constraints": {"owner__name": "capi-mgmt-a", "prefix__net_contained": "10.10.0.0/16"}},
  {"name": "capi-iprange-write", "object_types": ["ipam.iprange"], "actions": ["add", "change"],
   "constraints": {"owner__name": "capi-mgmt-a", "start_address__net_host_contained": "10.10.0.0/16",
                   "end_address__net_host_contained": "10.10.0.0/16"}},
  {"name": "capi-ipaddress-write", "object_types": ["ipam.ipaddress"], "actions": ["add", "change", "delete"],
   "constraints": {"owner__name": "capi-mgmt-a", "address__net_host_contained": "10.10.0.0/16"}}
]
```

`prefix__net_contained` exists only on `ipam.prefix` and excludes the block itself; ranges and addresses use `net_host_contained` on their address fields. Keep the view permission unconstrained.

### TLS

The provider trusts the system roots. A private CA is added with `--netbox-ca-file`, a PEM file mounted into the pod. `--netbox-insecure-skip-verify` turns verification off and is for development only. A `NETBOX_URL` with `http://` logs a warning: the token is sent unencrypted.

## Installation

Register the provider in `~/.config/cluster-api/clusterctl.yaml`, give the NetBox variables and install it:

```yaml
providers:
- name: netbox
  url: https://github.com/MicleMaslow/cluster-api-ipam-provider-netbox/releases/latest/ipam-components.yaml
  type: IPAMProvider
NETBOX_URL: https://netbox.example.com
NETBOX_TOKEN: nbt_<key>.<token>
NETBOX_OWNER: capi-mgmt-a
```

```bash
clusterctl init --ipam netbox
```

The clusterctl variables of the release manifests are `NETBOX_URL`, `NETBOX_TOKEN` (stored in the Secret `capi-ipam-netbox-credentials`) and `NETBOX_OWNER`. The image is fixed in the manifests; to pull it from a mirror, use the clusterctl `images:` setting with the key `ipam-netbox`. The provider runs as `capi-ipam-netbox-controller-manager` in `capi-ipam-netbox-system`.

A local build is installed from a local clusterctl repository. `make release` writes it to `out/repo`, with the image `<REGISTRY>/cluster-api-ipam-provider-netbox:<RELEASE_TAG>`; push that image, or load it into a kind cluster with `kind load docker-image`:

```bash
make release RELEASE_TAG=v0.1.0 REGISTRY=registry.example.com/capi
make docker-build docker-push RELEASE_TAG=v0.1.0 REGISTRY=registry.example.com/capi   # docker-push-all: linux/amd64 and linux/arm64
NETBOX_URL=... NETBOX_TOKEN=... NETBOX_OWNER=... clusterctl init --ipam netbox --config hack/dev/clusterctl.yaml
```

`hack/dev/clusterctl.yaml` is used instead of the global clusterctl config and installs the latest version in `out/repo`; run clusterctl from the repository root.

`clusterctl delete --ipam netbox`, as for other providers, keeps the provider's ClusterRoles and ClusterRoleBindings: `kubectl delete clusterrole,clusterrolebinding -l cluster.x-k8s.io/provider=ipam-netbox`.

To trust a private CA, put it into a ConfigMap and patch the Deployment:

```bash
kubectl -n capi-ipam-netbox-system create configmap netbox-ca --from-file=ca.crt=<ca.pem>
kubectl -n capi-ipam-netbox-system patch deployment capi-ipam-netbox-controller-manager --type=json -p '[
  {"op": "add", "path": "/spec/template/spec/volumes", "value": [{"name": "netbox-ca", "configMap": {"name": "netbox-ca"}}]},
  {"op": "add", "path": "/spec/template/spec/containers/0/volumeMounts", "value": [{"name": "netbox-ca", "mountPath": "/etc/netbox-ca", "readOnly": true}]},
  {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--netbox-ca-file=/etc/netbox-ca/ca.crt"}
]'
```

`clusterctl upgrade` replaces the Deployment, so the patch has to be applied again after it.

To read the token from a file instead (`NETBOX_TOKEN_FILE`, see [Provider configuration](#provider-configuration)), mount a Secret with the token as a directory, not with `subPath`, which Kubernetes does not update, and drop `NETBOX_TOKEN` from the credentials Secret:

```bash
kubectl -n capi-ipam-netbox-system create secret generic netbox-token --from-literal=token=nbt_<key>.<token>
kubectl -n capi-ipam-netbox-system patch secret capi-ipam-netbox-credentials --type=json -p '[{"op": "remove", "path": "/data/NETBOX_TOKEN"}]'
kubectl -n capi-ipam-netbox-system patch deployment capi-ipam-netbox-controller-manager --type=json -p '[
  {"op": "add", "path": "/spec/template/spec/volumes", "value": [{"name": "netbox-token", "secret": {"secretName": "netbox-token"}}]},
  {"op": "add", "path": "/spec/template/spec/containers/0/volumeMounts", "value": [{"name": "netbox-token", "mountPath": "/etc/netbox-token", "readOnly": true}]},
  {"op": "add", "path": "/spec/template/spec/containers/0/env", "value": [{"name": "NETBOX_TOKEN_FILE", "value": "/etc/netbox-token/token"}]}
]'
```

A rotated token then takes effect without a restart. After `clusterctl upgrade` apply the last two commands again.

Both patches add `volumes` and `volumeMounts`, and a JSON-patch `add` of a whole list replaces the one that is there: the patch applied second must append to the lists (`/-`, the element as `value`), or the first one's volume is lost and, with `--netbox-ca-file` still set, the pod exits with code 1. The token patch after the CA patch:

```bash
kubectl -n capi-ipam-netbox-system patch deployment capi-ipam-netbox-controller-manager --type=json -p '[
  {"op": "add", "path": "/spec/template/spec/volumes/-", "value": {"name": "netbox-token", "secret": {"secretName": "netbox-token"}}},
  {"op": "add", "path": "/spec/template/spec/containers/0/volumeMounts/-", "value": {"name": "netbox-token", "mountPath": "/etc/netbox-token", "readOnly": true}},
  {"op": "add", "path": "/spec/template/spec/containers/0/env", "value": [{"name": "NETBOX_TOKEN_FILE", "value": "/etc/netbox-token/token"}]}
]'
```

The CA patch after the token patch is the same with the CA volume and mount.

## Provider configuration

| | |
|---|---|
| `NETBOX_URL` (env) | NetBox base URL |
| `NETBOX_TOKEN` or `NETBOX_TOKEN_FILE` (env, exactly one) | NetBox token, or the path of a file with it. The file is read at every request, whitespace around the token is cut and an empty file is an error: a rotated token needs no restart |
| `--netbox-owner` (required) | name of the NetBox Owner |
| `--netbox-ca-file` | PEM file with extra CA certificates for NetBox, added to the system roots (mount it into the pod) |
| `--netbox-insecure-skip-verify` | skip NetBox certificate verification, development only |
| `--watch-filter` | reconcile only objects labelled `cluster.x-k8s.io/watch-filter` with this value |
| `--namespace` | watch only this namespace |
| `--health-addr` | address of the probes `/healthz` and `/readyz` (default `:9440`, `0` disables) |
| `--netboxnetwork-concurrency`, `--netboxippool-concurrency`, `--ipaddressclaim-concurrency` | objects of each kind processed in parallel (default 10) |

The standard Cluster API manager flags (`--diagnostics-address`, `--leader-elect`, `--sync-period`, `--kube-api-qps`, logging) are there too; see `--help`.

### Startup

The manager serves its probes at once: `/healthz` answers, `/readyz` answers `waiting for NetBox preflight` until the provider has checked NetBox and started its controllers. The check covers the NetBox version, the custom fields `capi_ref` and `capi_cluster` (type, `filter_logic`, object types, status), the `ETag` header on a single object and the Owner.

- If NetBox cannot be reached or answers with an error, the check is retried inside the process, with waits growing from 1 s to 1 min and every failure logged. The pod stays alive and not ready, so a restart during a NetBox outage does not end in a crash loop.
- If NetBox is set up in a way the provider cannot work with, the provider logs the reason and exits with code 1: NetBox older than 4.6, a missing custom field or one of the wrong type, `filter_logic` or object types, a custom field with status `deleting`, no `ETag` in the answer (a proxy strips it?), no Owner of that name. A custom field with status `provisioning` (NetBox 4.7) is waited for.
- A token file that is missing or empty (`NETBOX_TOKEN_FILE`) also stops the start.

### Metrics

The metrics are served by the diagnostics endpoint (`--diagnostics-address`, `:8443` in the release manifests) over HTTPS, with authentication and authorization as in Cluster API: the caller needs `get` on the non-resource URL `/metrics`, which the ClusterRole `capi-ipam-netbox-metrics-reader` grants. Besides the controller-runtime metrics there are:

| Metric | Labels | Meaning |
|---|---|---|
| `netbox_ipam_requests_total` | `method`, `path`, `code` | requests to the NetBox API; `path` is the API path with numeric segments replaced by `{id}`, `code` the HTTP status or `error` when there was no response |
| `netbox_ipam_request_duration_seconds` | `method`, `path` | duration of those requests |
| `netbox_ipam_write_lock_wait_seconds` | | time a section that writes to NetBox waited for the write lock; reconciles that only read NetBox never wait |

### Events

Changes of the kinds below are recorded as Normal Events on the object that caused them (`kubectl describe`, `kubectl get events`). Other changes, such as the allocation of a new address, metadata and tags, or taking a deprecated prefix back, have none. There are no Warning events: a failure is on the conditions.

| Object | Reason | When |
|---|---|---|
| `NetBoxNetwork` | `PrefixAcquired` | a prefix was carved, or created or adopted for `spec.prefix` |
| | `PrefixReleased` | the prefix was deleted, deprecated or let go on deletion of the network (`deletionPolicy`) |
| | `PrefixRetired` | a retained prefix that did not fit the spec got the key `<ref>/retired` (see [Retained objects](#retained-objects)) |
| | `VIPReserved` | the VIP was created in NetBox |
| | `VIPReleased` | the VIP was deleted or deprecated on deletion of the network |
| | `VIPRetired` | a retained VIP outside the new prefix got the key `<ref>/vip/retired` |
| | `VIPDeleted` | the provider deleted its new VIP because an older NetBox address held its address too (see [Troubleshooting](#troubleshooting)) |
| `NetBoxIPPool` | `RangeCreated` | a range was created in NetBox |
| | `RangeUpdated` | a range was resized, or a deprecated range of the pool was taken back |
| | `RangeReleased` | a range was deleted or deprecated by `deletionPolicy`, or a deprecated range in the way was deleted |
| `IPAddressClaim` | `AddressRestored` | an address deleted in NetBox by hand was created again |
| | `AddressHandedBack` | an address deleted in NetBox by hand, which NetBox offered to a new claim, went back to the claim that holds it; the new claim takes the next one |
| | `AddressReleased` | the address was deleted from NetBox with the claim |

## API

Both kinds are namespaced, in `ipam.cluster.x-k8s.io/v1alpha1`. `kubectl explain netboxnetwork.spec` and `kubectl explain netboxippool.spec` describe every field. The CRDs validate the fields with CEL; there are no admission webhooks.

### NetBoxNetwork

A subnet of one cluster inside an L2 network (IPv4, global VRF). See `config/samples/sample.yaml`.

- `clusterName`, `parentPrefix` (an existing NetBox prefix the cluster prefix is carved from or adopted in) and `gateway` of the L2 network. `parentPrefix` may be the prefix of the whole VLAN or a logical pool for CAPI inside it; the gateway may lie outside a logical parent.
- `addressPrefixLength` is the mask of the nodes' subnet. The L2 network is `gateway/addressPrefixLength` with the host bits cleared; without the field it is `parentPrefix`. Addresses handed out get this mask and the gateway (`status.addressPrefixLength`). The network and broadcast address of the L2 network and the gateway are reserved: a pool range or the VIP never covers them. Pools take ranges of networks in one L2 network only (`MixedNetworks` compares gateway and mask), so networks of different logical parents of one VLAN can share a pool.
- The CRD rejects: an L2 network that does not contain `parentPrefix`, an `addressPrefixLength` longer than the `parentPrefix` mask, a `prefix` outside `parentPrefix`, a `prefixLength` not longer than the `parentPrefix` mask, an IPv6 `parentPrefix` or `gateway`, a changed `gateway` or `addressPrefixLength`, `isPool` without `prefixLength`, and an adopted `prefix` with `deletionPolicy: Delete`.
- `prefixLength` carves a free prefix of that length out of `parentPrefix`; `prefix` instead takes a given CIDR inside it: an existing prefix that carries the provider's Owner is adopted, a missing one is created. Exactly one of the two, immutable. Adoption writes only `capi_ref` and `capi_cluster`; status, description and the rest stay as the NetBox admin set them. A deprecated prefix another NetBoxNetwork left is adopted, and made active again, only from the same namespace.
- `isPool: true` sets `is_pool` on the prefix the provider carves, so NetBox counts all its addresses as usable hosts, and keeps it in sync; the default is false. It needs `prefixLength`: an adopted prefix is never changed.
- `deletionPolicy`: `Retain` (default) marks the prefix `deprecated` when the object is deleted and reclaims it when it is created again; `Delete` removes it from NetBox. Pool ranges in the network follow the same policy when their pool releases them, because the range was removed or the pool deleted: they are deprecated or removed. The network itself is deleted only after those pools (`WaitingForPools`). With `prefix` the NetBox prefix is only released on deletion: `capi_ref` and `capi_cluster` are cleared, everything else, tags and custom fields included, stays; its pool ranges are deprecated. That holds for a prefix the provider created because none existed: it stays active with its description and needs manual cleanup in NetBox. The prefix is found in NetBox by the provider's key, not by the status.
- `controlPlaneEndpoint` (`port`, default 6443; `dnsName`, kept in lower case as NetBox does) reserves the cluster's VIP right after the prefix and writes it to `Cluster.spec.controlPlaneEndpoint`; see [Control-plane VIP](#control-plane-vip). Without the field no VIP is reserved. Set it on one network of a cluster only: a second one reserves a VIP of its own that the Cluster never gets and reports `EndpointConflict`.
  - The VIP is the host of the Cluster's endpoint if that is an address inside the prefix (the migration of a running cluster), else the highest address of the prefix that is not the network, broadcast or gateway of the L2 network, holds no NetBox address and lies in no IP range. It is created in NetBox with role `vip` and shows in `status.controlPlaneEndpoint`, whose port is the effective one. It is found again by its key and never moves; one deleted in NetBox is created again at the same address, or the network reports `EndpointConflict` if the address was taken. A VIP moved to another address in NetBox is `EndpointConflict` too, and the status keeps the old address. NetBox lets addresses with role `vip` share an address: while the status has no VIP (a new one, or after `clusterctl move`), the provider deletes its VIP only for an older object holding the same address (the lower NetBox id wins a race of two creators); a newer one is only logged.
  - Only the empty parts of the Cluster's endpoint are filled, by a merge patch of that field only: the host with the VIP, the port with `controlPlaneEndpoint.port`, else the port the Cluster has (a port alone is what CAPI copies from an infrastructure cluster that sets only the port), else 6443. The same port is in `status.controlPlaneEndpoint`. A host other than the VIP, or a port other than a given `controlPlaneEndpoint.port`, is never overwritten: the network reports `EndpointConflict`.
  - `ControlPlaneEndpointReady` is True when the VIP is reserved and the Cluster has it. Otherwise it is False with `WaitingForCluster` (the Cluster does not exist yet; a new VIP is picked only once it does, as its endpoint may be the VIP; the provider watches Clusters, so the order of applying the manifests does not matter), `EndpointConflict` (the Cluster has another endpoint), or the reason of the VIP error (`Exhausted`, `EndpointConflict`, ...). It never changes `Ready`, so adding a VIP to a live network stops no pool.
  - A pool range that is not in NetBox yet waits (`NetworkNotReady`) until the network has its VIP; one that covers the VIP is `InvalidRange`.
  - The VIP follows `deletionPolicy`: deleted, or `deprecated` and taken back when the network is created again. With `Delete` the network waits (`WaitingForCluster`) while the Cluster still has the VIP as its endpoint host, so that the address does not go to another cluster that is still served on it; a retained VIP is never handed out and needs no wait. Removing the field from the spec releases neither the VIP nor the Cluster endpoint: `status.controlPlaneEndpoint` keeps the host, so pool ranges still keep off the VIP, and only the `ControlPlaneEndpointReady` condition goes. To free the address, delete the network (the VIP follows `deletionPolicy`) or the VIP address in NetBox; as long as the network exists, its status keeps the host and its pools stay off it.
  - A network without status (recreated, as after a restore) whose prefix is gone from NetBox while its active VIP is still there is `PrefixNotFound`, like one that knows its prefix: the prefix is not carved again, restore it.
- `customFields` are set on the NetBox prefix. A JSON-object value is merged key by key: keys given here are written, keys the provider wrote earlier and that are no longer given are removed, other keys stay. Any other value replaces the field. Values are compared with what NetBox returns in the form they are written in: the string of a select or multiselect value, the id of an object or multi-object field, a multi-object list without regard to its order. Write a datetime as ISO 8601 (`2026-01-02T03:04:05`) and repeat no id in a multi-object list: NetBox rewrites other forms, such a value never matches and is written at every reconcile. `capi_ref` and `capi_cluster` are reserved. A failure shows in the `MetadataSynced` condition and never blocks `Ready`.
- `tags` are slugs of existing NetBox tags set on the prefix; a missing tag fails `MetadataSynced`. Tags added by others stay, tags the provider set (`status.tags`) and that are no longer given are removed. Tags and custom fields are written in one update.

A network being deleted waits while a NetBoxIPPool still has a range in it (`WaitingForPools`).

Do not give a NetBoxNetwork an `ownerReference` with `blockOwnerDeletion` to its Cluster: with foreground deletion of the Cluster, the network's wait for the Cluster (`deletionPolicy: Delete`, above) and the Cluster's wait for the network would block each other. No owner reference is needed: the CRDs carry the `clusterctl move` hierarchy label.

A network whose Cluster (`clusterName`) does not exist and that has no prefix yet touches nothing in NetBox, with or without `controlPlaneEndpoint`: it is `Ready=False` with `WaitingForCluster` until the Cluster appears: after `clusterctl move` or a restore the network has no status, and until its Cluster exists (and is not paused) it must not act on NetBox; for a VIP the Cluster may also come with an endpoint of its own that decides the address. A network that already holds its prefix works without the Cluster. The provider watches Clusters, so the order of applying the manifests does not matter.

A network is paused when its Cluster has `spec.paused: true` or the network has the `cluster.x-k8s.io/paused` annotation: it is skipped, nothing is written to NetBox or to the Cluster, and its condition `Paused` is True. `--watch-filter` is checked for networks as it is for pools.

### Retained objects

`deletionPolicy: Retain` leaves the prefix the provider created, the pool IP ranges and the VIP of a deleted cluster in NetBox as `deprecated`. A prefix taken with `spec.prefix` is only released and stays active; its ranges and the VIP become deprecated. Deprecated objects still take their space in the parent; creating the same network again takes them back. If a network of the same name is created again with a spec the retained prefix does not fit (another `prefixLength`, or a prefix outside the `parentPrefix`), the retained prefix is not taken back: its `capi_ref` becomes `<ref>/retired` (`capi_cluster` stays), a new prefix is carved and the network records `PrefixRetired`. A retained VIP outside the new prefix is retired the same way (`VIPRetired`) and a new VIP reserved. Retired objects stay `deprecated` and keep their space like other retained ones. To remove them, run `hack/netbox-cleanup-retained.sh` (bash, curl 7.76+ and jq). It needs its own admin token with view on `ipam.prefix`, `ipam.iprange`, `ipam.ipaddress` and `users.owner` and delete on the three IPAM types; the provider's production token has no delete on prefixes and ranges (see [Permissions](#permissions)).

```bash
export NETBOX_URL=https://netbox.example.com NETBOX_TOKEN=<token> NETBOX_OWNER=<owner>
hack/netbox-cleanup-retained.sh <cluster> --namespace <ns>          # lists what would be deleted, with their capi_ref
hack/netbox-cleanup-retained.sh <cluster> --namespace <ns> --yes    # deletes it
```

Only `deprecated` objects of that cluster and owner are touched: the VIP (role `vip`), ranges and prefix, retired ones included; active objects and other clusters stay. Clusters of the same name in different namespaces share `capi_cluster`: give `--namespace`, which matches the namespace in `capi_ref` (also in `<ref>/retired`), or check the refs in the listing before `--yes`. Each object is read for its `ETag` and deleted with `If-Match`, which NetBox checks, so one changed meanwhile stays; only HTTP 204 counts as deleted, and any other answer stops the script with its body on a line of its own. For clusters where space matters more than protection, use `deletionPolicy: Delete`.

### NetBoxIPPool

`ranges` is a list of `{networkName, start, end}`: a span of a NetBoxNetwork given as offsets from its first address; a negative offset counts back from the last one (`-1`). Ranges are used in order, one per network, and all networks of a pool share the L2 network (gateway and mask) of the first range's network; until that network exists no range serves. Each range becomes a NetBox IP range; a claim gets an address from it, and the address is deleted from NetBox with the claim. Label the pool with `cluster.x-k8s.io/cluster-name` so that pausing the Cluster pauses the pool.

Every range is reconciled on its own and reported in `status.ranges[]` (`ready`, `reason`, `message`). Claims get addresses only from ready ranges; the pool is `Ready` while at least one range is, and `RangesReady` lists the ranges that are not as the spec asks:

- A range that covers the network, broadcast or gateway address of the L2 network or the VIP, or lies outside its network, is `InvalidRange`; one in a network of another L2 is `MixedNetworks`.
- A range can be changed. The NetBox range is resized in place; a change that would leave allocated addresses outside it waits (`RangeInUse`, the addresses are in the message). A change that waits, overlaps another range (`RangeOverlap`) or is invalid leaves an active range serving with its old bounds, as long as they are still valid.
- A range can be removed. Its NetBox range is deleted or deprecated as the network's `deletionPolicy` says; while addresses are still allocated in it, it stays in `status.ranges`, not ready (`RangeInUse`). Removed ranges are found in NetBox by the pool's key, not by the status.
- Once no allocated address is in the way of a shrink, deletion or deprecation, NetBox stops handing out the range's addresses (`mark_populated`) and the allocated addresses are checked again before the change. A removed range that still holds addresses is not closed: claims stop using it once it is not ready, but NetBox would still hand out its free addresses to a direct request. A deprecated range stays closed; NetBox also refuses addresses created by hand inside it.
- A range that overlaps another NetBox range waits (`RangeOverlap`) and is retried when the other pool shrinks or goes. Deprecated ranges of the provider in the way, with no addresses in them, are deleted. A deprecated range of the same pool and network is taken back and resized.

`status` also holds the `gateway` and `addressPrefixLength` handed to every address and `ipAddresses` (`total`, `used`, `free`), counted over the ready ranges from the pool's `IPAddress` objects. A pool being deleted waits while addresses are allocated from it (`WaitingForAddresses`).

A pool is paused when it has the `cluster.x-k8s.io/paused` annotation, when the Cluster named by its `cluster.x-k8s.io/cluster-name` label has `spec.paused: true`, or when the Cluster of any network it has a range in has: it is skipped and its condition `Paused` is True. `clusterctl move` pauses Clusters only, so label every pool and annotate a pool that several clusters share (see [clusterctl move and pivot](#clusterctl-move-and-pivot)).

`addressReuseGracePeriodSeconds` (default 0, at most 86400, one day) keeps the NetBox address of a deleted claim for that many seconds after the claim's deletion, so that a new machine does not get the address while caches of the old one (ARP entries, for example) still point at it, as in a rolling update. The claim shows `Ready=False Releasing` with the time until which the address is kept. The period does not apply while the pool itself is being deleted or is gone: a cluster being torn down is not held up.

`omitGateway: true` hands out the addresses without a gateway. Use it for a second NIC of a machine whose default route is on the first one: with two gateways the machine gets two default routes. `status.gateway` is empty then. It applies to addresses allocated after the change, as the `IPAddress` spec is immutable.

`ipAddress` sets `dns_name` and `description` of the pool's NetBox addresses with Go templates: `dnsName` (default `{{ .Owner }}`) and `description` (default `{{ .Cluster }} {{ .Pool }} {{ .Owner }}`); `{{ "" }}` leaves a field empty. Variables: `.Owner`, the first owner reference of the claim, or the claim itself without one (with CAPV the VSphereVM, named like the Machine and the node); `.Claim`; `.Cluster`, the `spec.clusterName` or cluster-name label of the claim; `.Namespace`; `.Pool`. Results are trimmed; `dnsName` is lowercased, as NetBox stores it, and must be a valid DNS name; `description` is cut to 200 characters. Templates may use `if` and `with`, but not `range`, `define`, `block` or `template`, and a value is limited to 1024 bytes.

```yaml
spec:
  ipAddress:
    dnsName: "{{ .Owner }}.nodes.example"
```

The fields are written when an address is created in NetBox and compared with the templates at every reconcile of the claim, so a change of `ipAddress` reaches allocated addresses and a value edited in NetBox by hand is overwritten. A template that does not render or gives an invalid DNS name makes the pool `InvalidSpec`: no new addresses are issued, allocated claims stay `Ready`. Values can also fail for one claim only: `{{ .Cluster }}.nodes.example` gives `.nodes.example` for a claim without a cluster. Only a new claim then fails, with `AllocationFailed`; an allocated claim stays `Ready` and retries, and the error is in the controller log. An address restored or handed back to such a claim is still written, with both fields empty.

### Claims

Claims point at the pool with `poolRef: {apiGroup: ipam.cluster.x-k8s.io, kind: NetBoxIPPool, name: <pool>}`. The `IPAddress` gets the address, the mask of the L2 network as `prefix` and the gateway (unless the pool has `omitGateway`). An address deleted in NetBox by hand is created again with the same address; if NetBox has given that address to another object, the claim reports `AddressConflict`. A claim is skipped while it has the `cluster.x-k8s.io/paused` annotation, its Cluster (`spec.clusterName` or the `cluster.x-k8s.io/cluster-name` label) is paused, or its pool has the annotation.

The address of a deleted claim leaves NetBox only when no other finalizer guards the claim and the grace period of the pool is over. An infrastructure provider keeps its finalizer on a claim while the machine uses the address: CAPV removes `vspherevm.infrastructure.cluster.x-k8s.io/ip-claim-protection` once the VM is gone. Until then the claim is `Ready=False Releasing` with the finalizers it waits for, and is checked again every minute or as soon as a finalizer goes. A claim deleted for `clusterctl move` (annotation `clusterctl.cluster.x-k8s.io/delete-for-move`) releases nothing. If the release fails, the claim is `Ready=False ReleaseFailed` with the error.

## Example: network scheme

One /26 per cluster split into control plane (6), ingress (8) and workers (the rest); the control plane VIP is the address the network reserves itself.

```yaml
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxNetwork
metadata: {name: demo, namespace: demo}
spec:
  clusterName: demo
  parentPrefix: 10.10.0.0/22
  gateway: 10.10.0.1
  prefixLength: 26
  controlPlaneEndpoint: {dnsName: demo-api.example}   # the VIP: the highest free address of the /26
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: demo-cp, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
spec: {ranges: [{networkName: demo, start: 2, end: 7}]}
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: demo-ingress, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
spec: {ranges: [{networkName: demo, start: 8, end: 15}]}
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: demo-workers, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
spec: {ranges: [{networkName: demo, start: 16, end: -3}]}
```

Later the cluster needs addresses for MetalLB. Shrink the workers and add the new pool in one `kubectl apply`:

```yaml
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: demo-workers, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
spec: {ranges: [{networkName: demo, start: 16, end: -11}]}
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: demo-metallb, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
spec: {ranges: [{networkName: demo, start: -10, end: -3}]}
```

`demo-metallb` is `RangeOverlap` until the workers range is shrunk, then `Ready`. If a worker still holds an address in the last eight, the workers range shows `RangeInUse` with that address and `demo-metallb` waits until the machine is gone. MetalLB itself is configured with the addresses of `demo-metallb`'s `status.ranges`; the pool only reserves them in NetBox.

[`docs/examples/network-scheme.yaml`](docs/examples/network-scheme.yaml) holds the resulting scheme with all four pools.

## Example: CAPV

A `VSphereMachineTemplate` takes the machine address from a pool with `addressesFromPools`; CAPV creates the claims. The gateway comes with the address, so `gateway4` is not needed; `nameservers` is not part of the claim and stays in the template. Fragment, the rest of the template left out ([`docs/examples/capv-machine-template-patch.yaml`](docs/examples/capv-machine-template-patch.yaml)):

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: VSphereMachineTemplate
spec:
  template:
    spec:
      network:
        devices:
        - networkName: demo-portgroup
          dhcp4: false
          addressesFromPools:
          - {apiGroup: ipam.cluster.x-k8s.io, kind: NetBoxIPPool, name: demo-workers}
          nameservers:
          - 10.10.0.53
```

## Control-plane VIP

The provider knows nothing about the load balancer. A `NetBoxNetwork` with `controlPlaneEndpoint` reserves the VIP in NetBox and writes it to `Cluster.spec.controlPlaneEndpoint`, and Cluster API puts it into the kubeadm config on the node and into the kubeconfig. Running the VIP is the job of your template: kube-vip, keepalived or anything else.

`controlPlaneEndpoint` is for infrastructure providers without a load balancer of their own (CAPV, Metal3 and similar). CAPO, CAPA and others that create a load balancer set the endpoint themselves: leave the field out there.

KubeadmControlPlane creates no machine and no kubeconfig until `Cluster.spec.controlPlaneEndpoint` is set. The network writes it first, so CAPI renders the kubeconfig and the bootstrap data with the VIP, and CAPV leaves an endpoint that the Cluster already has as it is. Omit `VSphereCluster.spec.controlPlaneEndpoint` entirely rather than leaving it empty.

The VIP is chosen when the network is reconciled, after the template has been applied, and CAPI puts no field of the Cluster into the files of a KubeadmConfig. So the configuration of the load balancer in your template carries two placeholders, `__CONTROL_PLANE_ENDPOINT__` and `__CONTROL_PLANE_PORT__`, and one command in `preKubeadmCommands` replaces them on the node. It takes the host and port from the kubeadm config that CAPI renders there: `controlPlaneEndpoint` in `/run/kubeadm/kubeadm.yaml` (first control plane machine), `apiServerEndpoint` in `/run/kubeadm/kubeadm-join-config.yaml` (the other control plane machines), or `/etc/kubeadm.yml` (Ignition, both). Without an address it fails, so no machine starts with a load balancer that has none. The command is in [`docs/examples/control-plane-vip.yaml`](docs/examples/control-plane-vip.yaml), and `make test` runs it on configs shaped as CAPI v1.14 renders them. The path of the file it edits is the first line of the command (`f=...`): the kube-vip manifest in the example, or `/etc/keepalived/keepalived.conf` and the like. Add the command to the `preKubeadmCommands` of the template; do not replace the list.

For kube-vip, put the placeholders into the environment of the static pod manifest of the template (quote the port: an unquoted number is not a string for the Pod):

```yaml
- {name: address, value: __CONTROL_PLANE_ENDPOINT__}
- {name: port, value: "__CONTROL_PLANE_PORT__"}
```

Take the kube-vip Pod manifest and its helper files, and nothing else, from the CAPV cluster template, [`templates/cluster-template-node-ipam.yaml`](https://github.com/kubernetes-sigs/cluster-api-provider-vsphere/blob/main/templates/cluster-template-node-ipam.yaml). In the manifest, `address` is `${CONTROL_PLANE_ENDPOINT_IP}` there: write the placeholder instead. The helper files are the `/etc/hosts` mount that kube-vip needs (`/etc/kube-vip.hosts`) and, for Kubernetes 1.29 and later, the `super-admin.conf` workaround for `kubeadm init` (`/etc/pre-kubeadm-commands/50-kube-vip-prepare.sh`); leave neither out. The same variable fills `VSphereCluster.spec.controlPlaneEndpoint.host` in the template, which stays out of your cluster (see above).

- **ClusterClass.** The network writes the endpoint to a topology Cluster the same way, but only plain clusters are tested. Patch no endpoint into the infrastructure cluster template of the ClusterClass, and put the placeholders into the load balancer config of the control plane template. The examples are CAPV in govmomi mode.
- **ARP.** kube-vip in ARP mode needs all control plane machines in the L2 network of the VIP; otherwise use BGP ([CAPV#1120](https://github.com/kubernetes-sigs/cluster-api-provider-vsphere/issues/1120)).
- **kube-vip version.** Pin a version you have tested. The CAPV template uses v0.6.4; v1.2.2 deadlocked the cold start of a control plane ([kube-vip#1654](https://github.com/kube-vip/kube-vip/issues/1654)).
- **A second NIC.** A machine with two NICs gets two default routes if both addresses come with a gateway, and kube-vip picks its interface by the default route. Set `omitGateway: true` on the pool of the second NIC and `vip_interface` in the manifest.

To bring a running cluster under the network, add `controlPlaneEndpoint` to it, with the `port` the Cluster uses. The VIP is then the address the Cluster has, if it is inside the prefix of the network. It must be free in NetBox: not the network, broadcast or gateway of the L2 network, no IP address object, in no IP range; otherwise the network reports `EndpointConflict` on `ControlPlaneEndpointReady` and nothing changes. An endpoint outside the prefix, or a DNS name, is `EndpointConflict` as well: the Cluster is never changed, and the provider reserves the highest free address meanwhile.

A cluster that took its VIP from a pool of its own (a NetBoxIPPool with one range on the VIP) must lose that pool first, and its range must be gone from NetBox: under `deletionPolicy: Retain` the range stays `deprecated` and still holds the address, so the network reports `EndpointConflict`. Delete it with `hack/netbox-cleanup-retained.sh` or in NetBox, then add `controlPlaneEndpoint`.

## clusterctl move and pivot

`clusterctl move` leaves the NetBox objects alone. The target cluster finds the prefix, VIP, ranges and addresses by their `capi_ref`, so the status is rebuilt there, including removed ranges that still hold addresses; the endpoint of the moved Cluster already is the VIP. This works only between providers with the same `--netbox-owner`: if an object with the same `capi_ref` belongs to another owner, nothing is created and the object reports `OwnedByOtherOwner` (a claim: `AllocationFailed` with that message; an allocated address: `AddressConflict`).

- `clusterctl move` pauses Clusters only. A network is paused with its Cluster, a pool with the Cluster of its `cluster.x-k8s.io/cluster-name` label or of any of its networks. Label every pool, and put the `cluster.x-k8s.io/paused` annotation on a pool that several clusters share for the time of the move: its claims are skipped as well.
- A network in the target cluster that has no prefix in its status yet waits for its Cluster (`WaitingForCluster`) and touches nothing in NetBox, so the order in which clusterctl creates the objects does not matter.
- Keep the namespace and the names: `capi_ref` is `Kind/namespace/name`. A mutator or tool that changes the namespace on the move makes the target carve new objects instead of finding the old ones.

## Troubleshooting

```bash
kubectl get netboxnetworks,netboxippools -A
kubectl get ipaddressclaims.ipam.cluster.x-k8s.io,ipaddresses.ipam.cluster.x-k8s.io -A
kubectl -n <ns> get netboxippool <pool> -o jsonpath='{.status.ranges}'
kubectl -n <ns> get netboxnetwork <network> -o jsonpath='{.status.controlPlaneEndpoint} {.status.conditions}'
kubectl -n <ns> events --for netboxnetwork/<network>
```

Use the full resource names: in Kubernetes ≥ 1.33 the plain resource name `ipaddresses` resolves to `networking.k8s.io`. Every change the provider makes in NetBox carries a changelog message that starts with `capi:`. States that only a change in NetBox can clear are checked again every minute.

| Reason | Where | Meaning |
|---|---|---|
| `ParentNotFound` | network `Ready` | NetBox has not exactly one `parentPrefix` in the global VRF |
| `Exhausted` | network `Ready`, `ControlPlaneEndpointReady` | no free prefix of `prefixLength` left in the parent (`Ready`), or no free address for the VIP in the prefix |
| `EndpointConflict` | network `ControlPlaneEndpointReady` | the Cluster has another host than the VIP, or another port than `controlPlaneEndpoint.port` (it is not changed); the address meant for the VIP (the Cluster's endpoint, or the VIP's old address) is taken, reserved or inside an IP range; an older NetBox address holds the VIP's address too (the provider deletes its VIP, `VIPDeleted`, and the next reconcile picks another address); a newer one is only logged and the VIP stays |
| `WaitingForCluster` | network `Ready`, `ControlPlaneEndpointReady` | the Cluster of the network does not exist yet: a network without a prefix touches nothing in NetBox (`Ready`), a new VIP is reserved and written when the Cluster appears (`ControlPlaneEndpointReady`). While deleting with `deletionPolicy: Delete`, the Cluster still has the VIP as its endpoint |
| `PrefixMismatch` | network `Ready`, `ControlPlaneEndpointReady` | the NetBox prefix is outside the parent or not the length or CIDR of the spec; the NetBox address of the VIP is outside the prefix |
| `PrefixNotFound` | network `Ready` | the prefix in the status is gone from NetBox, or the status is empty and the active VIP of the network is there; it is not carved again, restore it |
| `Paused` | network, pool | the condition `Paused` is True: the Cluster has `spec.paused`, or the object has the `cluster.x-k8s.io/paused` annotation; the object is skipped |
| `NotOwned` | network `Ready`, `ControlPlaneEndpointReady`, pool | the object (for `ControlPlaneEndpointReady`, the VIP) exists in NetBox without the provider's Owner; to adopt a prefix, set its owner in NetBox |
| `OwnedByOtherOwner` | network `Ready`, `ControlPlaneEndpointReady`, pool | a NetBox object (for `ControlPlaneEndpointReady`, the VIP) with the same `capi_ref` belongs to another owner (see move and pivot) |
| `Duplicate` | network `Ready`, `ControlPlaneEndpointReady`, pool | more than one NetBox object carries the key (for `ControlPlaneEndpointReady`, the VIP's), or the CIDR to adopt exists more than once or is held by another network |
| `InvalidSpec` | network `MetadataSynced`, pool `Ready` | `customFields` sets `capi_ref` or `capi_cluster`; the `ipAddress` template of a pool does not render or gives an invalid DNS name |
| `WaitingForPrefix` | network `MetadataSynced`, `ControlPlaneEndpointReady` | metadata and the VIP wait for the prefix to be `Ready` |
| `Deleting` | network, pool `Ready` | deletion in progress |
| `WaitingForPools` | network `Ready` while deleting | a NetBoxIPPool still has a range in the network |
| `NetworkNotReady` | pool, range | the network of the range is missing or not `Ready`, or a new range waits for the VIP of its network |
| `MixedNetworks`, `InvalidRange`, `RangeInUse`, `RangeOverlap` | range | see [NetBoxIPPool](#netboxippool) |
| `WaitingForAddresses` | pool `Ready` while deleting | `IPAddress` objects of the pool still exist |
| `Forbidden` | network, pool, range | the token lacks a NetBox permission (HTTP 403) |
| `NetBoxError` | network, pool, range | another NetBox error; the message has the details |
| `InternalError` | network, pool | a Kubernetes API call failed |
| `PoolNotReady`, `PoolExhausted`, `AllocationFailed` | claim `Ready` | the pool has no ready range, no free address, or the allocation failed |
| `Releasing` | claim `Ready` | the deleted claim waits for finalizers of others (they are in the message, for example CAPV's `ip-claim-protection` until the VM is gone) or for `addressReuseGracePeriodSeconds`; its address stays in NetBox |
| `ReleaseFailed` | claim `Ready` | deleting the address in NetBox failed; the message has the error and the release is retried |
| `AddressConflict` | claim `Ready` | the allocated address is held by another object in NetBox |
| `Allocating` | claim `Ready` (Unknown) | the address is allocated, the `IPAddress` is being created |

On a claim, a NetBox error or a 403 during allocation shows as `AllocationFailed` with the message. A claim stuck in deletion, for example without delete on `ipam.ipaddress`, is `ReleaseFailed` with the error: the claim, its `IPAddress` and the NetBox address stay (CAPV still finishes deleting the Machine) and deleting the pool waits with `WaitingForAddresses`. A claim whose `spec.clusterName` or `cluster.x-k8s.io/cluster-name` label names a Cluster that does not exist is skipped without a condition; the provider logs it at Info level.

Objects in NetBox whose `capi_ref` ends in `/retired` are retained objects that a recreated network did not take back because they did not fit its spec (see [Retained objects](#retained-objects)): they are `deprecated`, still take their space and are removed with `hack/netbox-cleanup-retained.sh`.

## Development

Needs docker, kind, kubectl, go, jq. Contributions: see [CONTRIBUTING.md](CONTRIBUTING.md).

### Dev stand

```bash
make dev-up
```

NetBox: http://localhost:18000, bound to 127.0.0.1 (admin / admin). The seed writes the operator token to `hack/dev/.env`.
kubectl against the kind cluster: `export KUBECONFIG=$PWD/hack/dev/kubeconfig`.
The passwords, keys and tokens in `hack/dev` are for the local dev stand only.

```bash
make dev-down
```

### Run locally

```bash
make run            # provider against hack/dev/kubeconfig and the dev NetBox
make smoke          # in another terminal: sample cluster, network, pool and claim
make smoke-clean
```

### Tests

```bash
make test               # unit + CRD validation + the control-plane VIP command (hack/test-control-plane-vip.sh)
make test-integration   # + controllers against the dev NetBox (make netbox-up first)
```

The dev NetBox is 4.6.7, and CI runs the integration tests on 4.6.7 and 4.7.2. `NETBOX_IMAGE` sets another NetBox image for `make netbox-up`, `NETBOX_PORT` the host port, `NETBOX_ENV_FILE` the file `hack/dev/seed.sh` writes, and `DEV_ENV_FILE` the file `make test-integration` and `make run` read; a second NetBox needs a compose project of its own so that the dev data stays.

### Run inside kind

```bash
make deploy-dev     # image IMG + clusterctl components, NetBox reached over the kind docker network
make smoke
```

`make release-manifests` writes `out/ipam-components.yaml` with the image `IMG` and `out/metadata.yaml`.
Variables: `NETBOX_URL`, `NETBOX_TOKEN`, `NETBOX_OWNER`.

To install a release build the way users do, replace the deploy-dev install with `clusterctl init` from the local repository:

```bash
export KUBECONFIG=$PWD/hack/dev/kubeconfig
kubectl delete -f out/ipam-components.yaml   # the deploy-dev install, after make smoke-clean
make release docker-build RELEASE_TAG=v0.1.0-rc.0
kind load docker-image ghcr.io/miclemaslow/cluster-api-ipam-provider-netbox:v0.1.0-rc.0 --name capi-netbox-dev
(set -a; source hack/dev/.env; NETBOX_URL=http://capi-netbox-dev-netbox-1:8080 \
  hack/tools/bin/clusterctl-v1.14.3 init --ipam netbox --config hack/dev/clusterctl.yaml --wait-providers)
make smoke smoke-clean
```

Back to deploy-dev: `hack/tools/bin/clusterctl-v1.14.3 delete --ipam netbox --include-namespace --config hack/dev/clusterctl.yaml`, then `make deploy-dev`.

### CAPV on vcsim

A CAPV workload cluster on a simulated vCenter, with machine addresses from NetBox:

```bash
make vcsim-up                # CAPV and the vcsim controller of CAPV's test infrastructure
make vcsim-cluster NAME=vc   # NetBoxNetwork vc, pools vc-cp and vc-workers, and a cluster from CAPV's node-ipam template
```

`vcsim-up` builds the vcsim controller image from the CAPV sources (`CAPV_VERSION`, default v1.17.0, cloned into `~/.cache/capi-ipam-netbox`) and installs CAPV with clusterctl. The machines get their addresses through `addressesFromPools`, control plane from `<NAME>-cp`, workers from `<NAME>-workers`, and CAPV writes them into the VM metadata in vcsim. The workload cluster itself is fake: the vcsim controller simulates the machine bootstrap and the API server behind the control plane endpoint, and without cloud-init the VMs report the address 192.0.2.100. `CP_MACHINES`, `WORKER_MACHINES`, `K8S_VERSION`, `PARENT_PREFIX`, `GATEWAY` and `PREFIX_LENGTH` tune the cluster; `NODE_DOMAIN` makes the `dns_name` of the node addresses `<node>.<NODE_DOMAIN>`. Tags and custom fields of the cluster prefix come from `hack/dev/vcsim-network.local.yaml`, else from [`hack/dev/vcsim-network.example.yaml`](hack/dev/vcsim-network.example.yaml), a merge patch of the NetBoxNetwork with `${CLUSTER_NAME}` substituted; the custom fields it needs beyond the seeded ones go into `hack/dev/custom-fields.local.json`, created by `make netbox-up`. Both local files are git-ignored. Deleting the Cluster releases its addresses; the network and the pools stay.

`CONTROL_PLANE_ENDPOINT=1` gives the network a `controlPlaneEndpoint` (`dnsName` `<NAME>-api[.<NODE_DOMAIN>]`) and takes the endpoint out of the VSphereCluster. The cluster then does not reach `Available`, because the fake API server of vcsim lives on its own address, but the VIP in NetBox, `Cluster.spec.controlPlaneEndpoint` and the endpoint in the bootstrap data of the control plane can be checked. For the same reason the vcsim controller does not release the VMs on deletion: remove the `vcsim.fake.infrastructure.cluster.x-k8s.io` finalizer of the VSphereVMs.

### Develop with Tilt

The provider runs in the Tilt setup of a [cluster-api](https://github.com/kubernetes-sigs/cluster-api) checkout (`make tilt-up`, see its Tilt docs): `tilt-provider.yaml` describes the build, `clusterctl-settings.json` serves `create-local-repository.py`. Add the provider to `tilt-settings.yaml` in the cluster-api checkout; Cluster API core is always enabled:

```yaml
provider_repos:
- ../cluster-api-ipam-provider-netbox
enable_providers:
- ipam-netbox
kustomize_substitutions:
  NETBOX_URL: http://capi-netbox-dev-netbox-1:8080
  NETBOX_TOKEN: nbt_<key>.<token>
  NETBOX_OWNER: capi-mgmt-dev
```

Tilt substitutes the `${NETBOX_URL}`, `${NETBOX_TOKEN}` and `${NETBOX_OWNER}` placeholders of the manifests from `kustomize_substitutions`. `NETBOX_TOKEN` is the token of the dev stand (`hack/dev/.env`); keep real tokens out of `tilt-settings.yaml`.

The dev NetBox (`make netbox-up`) listens on 127.0.0.1 only. Attach it to the docker network of the Tilt kind cluster, as `make deploy-dev` does for `kind`, so that the pod reaches it by container name:

```bash
docker network connect kind capi-netbox-dev-netbox-1
```

Tilt rebuilds the manager and restarts it on changes of `main.go`, `go.mod`, `go.sum`, `api` and `internal`. The image in `tilt-provider.yaml` is the one set by `config/default/kustomization.yaml`; change both together.

## Community

- [Contributing](CONTRIBUTING.md)
- [Security policy](SECURITY.md)
- [Code of conduct](CODE_OF_CONDUCT.md)

## License

Apache License 2.0, see [LICENSE](LICENSE).
