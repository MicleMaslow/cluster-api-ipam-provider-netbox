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
| v0.1.x | v1.14 (contract v1beta2) | ≥ 4.6 (tested with 4.6.7) |

The provider works with any infrastructure provider that consumes `IPAddressClaim`s, for example CAPV `addressesFromPools`. Only IPv4 and the global VRF are supported.

## NetBox setup

Set NetBox up before installing the provider. On start the provider checks the NetBox version, the two custom fields and the Owner and exits with the reason if one of them is wrong; write permissions and parent prefixes are not checked there and show up later as conditions. [`hack/dev/seed.sh`](hack/dev/seed.sh) sets all of this up through the API and can serve as a reference.

- NetBox 4.6 or newer (the provider uses Owners, v2 API tokens and ETag/If-Match on updates).
- Custom fields `capi_ref` and `capi_cluster`: type text, `filter_logic: exact`, object types `ipam.prefix`, `ipam.iprange`, `ipam.ipaddress`. The provider stores its idempotency key and the cluster name in them.
- An Owner for the provider: the identity of its management plane. It marks everything the provider manages; objects of other owners are never touched. Every management plane needs its own Owner and keeps it through `clusterctl move` and pivot. The provider's key (`capi_ref`) is `Kind/namespace/name`, so management planes that share a NetBox must not repeat the namespace and name of these objects: a repeat blocks the other plane with `OwnedByOtherOwner`. For pool ranges the key is `NetBoxIPPool/namespace/pool/network`.
- The parent prefixes, in the global VRF, that clusters carve their subnets from.
- A user with a v2 token (`nbt_<key>.<token>`, write enabled) and the permissions below.

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

## Provider configuration

| | |
|---|---|
| `NETBOX_URL`, `NETBOX_TOKEN` (env) | NetBox base URL and token |
| `--netbox-owner` (required) | name of the NetBox Owner |
| `--netbox-ca-file` | PEM file with extra CA certificates for NetBox, added to the system roots (mount it into the pod) |
| `--netbox-insecure-skip-verify` | skip NetBox certificate verification, development only |
| `--watch-filter` | reconcile only objects labelled `cluster.x-k8s.io/watch-filter` with this value |
| `--namespace` | watch only this namespace |
| `--netboxnetwork-concurrency`, `--netboxippool-concurrency`, `--ipaddressclaim-concurrency` | objects of each kind processed in parallel (default 10) |

The standard Cluster API manager flags (`--diagnostics-address`, `--leader-elect`, `--sync-period`, `--kube-api-qps`, logging) are there too; see `--help`.

On start the provider checks the NetBox version, the two custom fields (type, `filter_logic`, object types) and the Owner, and exits with the reason if one of them is wrong.

## API

Both kinds are namespaced, in `ipam.cluster.x-k8s.io/v1alpha1`. `kubectl explain netboxnetwork.spec` and `kubectl explain netboxippool.spec` describe every field. The CRDs validate the fields with CEL; there are no admission webhooks.

### NetBoxNetwork

A subnet of one cluster inside a parent L2 network (IPv4, global VRF). See `config/samples/sample.yaml`.

- `clusterName`, `parentPrefix` (an existing NetBox prefix) and `gateway` of the parent network. Addresses handed out get the mask of `parentPrefix` and this gateway.
- The CRD rejects: a `gateway` or `prefix` outside `parentPrefix`, a `prefixLength` not longer than the `parentPrefix` mask, an IPv6 `parentPrefix` or `gateway`, a changed `gateway`, and an adopted `prefix` with `deletionPolicy: Delete`.
- `prefixLength` carves a free prefix of that length out of `parentPrefix`; `prefix` instead takes a given CIDR inside it: an existing prefix that carries the provider's Owner is adopted, a missing one is created. Exactly one of the two, immutable. Adoption writes only `capi_ref` and `capi_cluster`; status, description and the rest stay as the NetBox admin set them. A deprecated prefix another NetBoxNetwork left is adopted, and made active again, only from the same namespace.
- `deletionPolicy`: `Retain` (default) marks the prefix `deprecated` when the object is deleted and reclaims it when it is created again; `Delete` removes it from NetBox. Pool ranges in the network follow the same policy when their pool releases them, because the range was removed or the pool deleted: they are deprecated or removed. The network itself is deleted only after those pools (`WaitingForPools`). With `prefix` the NetBox prefix is only released on deletion: `capi_ref` and `capi_cluster` are cleared, everything else, tags and custom fields included, stays; its pool ranges are deprecated. That holds for a prefix the provider created because none existed: it stays active with its description and needs manual cleanup in NetBox. The prefix is found in NetBox by the provider's key, not by the status.
- `customFields` are set on the NetBox prefix. A JSON-object value is merged key by key: keys given here are written, keys the provider wrote earlier and that are no longer given are removed, other keys stay. Any other value replaces the field. `capi_ref` and `capi_cluster` are reserved. A failure shows in the `MetadataSynced` condition and never blocks `Ready`.
- `tags` are slugs of existing NetBox tags set on the prefix; a missing tag fails `MetadataSynced`. Tags added by others stay, tags the provider set (`status.tags`) and that are no longer given are removed. Tags and custom fields are written in one update.

A network being deleted waits while a NetBoxIPPool still has a range in it (`WaitingForPools`). A retained prefix still takes its space in the parent: deprecated prefixes of deleted clusters need cleanup in NetBox, or parents fill up.

### NetBoxIPPool

`ranges` is a list of `{networkName, start, end}`: a span of a NetBoxNetwork given as offsets from its first address; a negative offset counts back from the last one (`-1`). Ranges are used in order, one per network, and all networks of a pool share the `parentPrefix` and `gateway` of the first range's network; until that network exists no range serves. Each range becomes a NetBox IP range; a claim gets an address from it, and the address is deleted from NetBox with the claim. Label the pool with `cluster.x-k8s.io/cluster-name` so that pausing the Cluster pauses the pool.

Every range is reconciled on its own and reported in `status.ranges[]` (`ready`, `reason`, `message`). Claims get addresses only from ready ranges; the pool is `Ready` while at least one range is, and `RangesReady` lists the ranges that are not as the spec asks:

- A range that covers the network, broadcast or gateway address of the parent, or lies outside its network, is `InvalidRange`; one in a network of another L2 is `MixedNetworks`.
- A range can be changed. The NetBox range is resized in place; a change that would leave allocated addresses outside it waits (`RangeInUse`, the addresses are in the message). A change that waits, overlaps another range (`RangeOverlap`) or is invalid leaves an active range serving with its old bounds, as long as they are still valid.
- A range can be removed. Its NetBox range is deleted or deprecated as the network's `deletionPolicy` says; while addresses are still allocated in it, it stays in `status.ranges`, not ready (`RangeInUse`). Removed ranges are found in NetBox by the pool's key, not by the status.
- Once no allocated address is in the way of a shrink, deletion or deprecation, NetBox stops handing out the range's addresses (`mark_populated`) and the allocated addresses are checked again before the change. A removed range that still holds addresses is not closed: claims stop using it once it is not ready, but NetBox would still hand out its free addresses to a direct request. A deprecated range stays closed; NetBox also refuses addresses created by hand inside it.
- A range that overlaps another NetBox range waits (`RangeOverlap`) and is retried when the other pool shrinks or goes. Deprecated ranges of the provider in the way, with no addresses in them, are deleted. A deprecated range of the same pool and network is taken back and resized.

`status` also holds the `gateway` and `addressPrefixLength` handed to every address and `ipAddresses` (`total`, `used`, `free`), counted over the ready ranges from the pool's `IPAddress` objects. A pool being deleted waits while addresses are allocated from it (`WaitingForAddresses`).

`ipAddress` sets `dns_name` and `description` of the pool's NetBox addresses with Go templates: `dnsName` (default `{{ .Owner }}`) and `description` (default `{{ .Cluster }} {{ .Pool }} {{ .Owner }}`); `{{ "" }}` leaves a field empty. Variables: `.Owner`, the first owner reference of the claim, or the claim itself without one (with CAPV the VSphereVM, named like the Machine and the node); `.Claim`; `.Cluster`, the `spec.clusterName` or cluster-name label of the claim; `.Namespace`; `.Pool`. Results are trimmed; `dnsName` is lowercased, as NetBox stores it, and must be a valid DNS name; `description` is cut to 200 characters. Templates may use `if` and `with`, but not `range`, `define`, `block` or `template`, and a value is limited to 1024 bytes.

```yaml
spec:
  ipAddress:
    dnsName: "{{ .Owner }}.nodes.example"
```

The fields are written when an address is created in NetBox and compared with the templates at every reconcile of the claim, so a change of `ipAddress` reaches allocated addresses and a value edited in NetBox by hand is overwritten. A template that does not render or gives an invalid DNS name makes the pool `InvalidSpec`: no new addresses are issued, allocated claims stay `Ready`. Values can also fail for one claim only: `{{ .Cluster }}.nodes.example` gives `.nodes.example` for a claim without a cluster. Only a new claim then fails, with `AllocationFailed`; an allocated claim stays `Ready` and retries, and the error is in the controller log. An address restored or handed back to such a claim is still written, with both fields empty.

### Claims

Claims point at the pool with `poolRef: {apiGroup: ipam.cluster.x-k8s.io, kind: NetBoxIPPool, name: <pool>}`. The `IPAddress` gets the address, the mask of the parent network as `prefix` and the gateway. An address deleted in NetBox by hand is created again with the same address; if NetBox has given that address to another object, the claim reports `AddressConflict`.

## Example: network scheme

One /26 per cluster split into control plane (6), ingress (8), workers (the rest) and the kube-vip address.

```yaml
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxNetwork
metadata: {name: demo, namespace: demo}
spec: {clusterName: demo, parentPrefix: 10.10.0.0/22, gateway: 10.10.0.1, prefixLength: 26}
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
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: demo-vip, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
spec: {ranges: [{networkName: demo, start: -2, end: -2}]}
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

[`docs/examples/network-scheme.yaml`](docs/examples/network-scheme.yaml) holds the resulting scheme with all five pools.

## Example: CAPV

A `VSphereMachineTemplate` takes the machine address from a pool with `addressesFromPools`; CAPV creates the claims. The gateway comes with the address, so `gateway4` is not needed. Fragment, the rest of the template left out ([`docs/examples/capv-machine-template-patch.yaml`](docs/examples/capv-machine-template-patch.yaml)):

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
```

## clusterctl move and pivot

`clusterctl move` leaves the NetBox objects alone. The target cluster finds the prefix, ranges and addresses by their `capi_ref`, so the status is rebuilt there, including removed ranges that still hold addresses. This works only between providers with the same `--netbox-owner`: if an object with the same `capi_ref` belongs to another owner, nothing is created and the object reports `OwnedByOtherOwner` (a claim: `AllocationFailed` with that message; an allocated address: `AddressConflict`).

## Troubleshooting

```bash
kubectl get netboxnetworks,netboxippools -A
kubectl get ipaddressclaims.ipam.cluster.x-k8s.io,ipaddresses.ipam.cluster.x-k8s.io -A
kubectl -n <ns> get netboxippool <pool> -o jsonpath='{.status.ranges}'
```

Use the full resource names: in Kubernetes ≥ 1.33 the plain resource name `ipaddresses` resolves to `networking.k8s.io`. Every change the provider makes in NetBox carries a changelog message that starts with `capi:`. States that only a change in NetBox can clear are checked again every minute.

| Reason | Where | Meaning |
|---|---|---|
| `ParentNotFound` | network `Ready` | NetBox has not exactly one `parentPrefix` in the global VRF |
| `Exhausted` | network `Ready` | no free prefix of `prefixLength` left in the parent |
| `PrefixMismatch` | network `Ready` | the NetBox prefix is outside the parent or not the length or CIDR of the spec |
| `PrefixNotFound` | network `Ready` | the prefix in the status is gone from NetBox; it is not carved again, restore it |
| `NotOwned` | network `Ready`, pool | the object exists in NetBox without the provider's Owner; to adopt a prefix, set its owner in NetBox |
| `OwnedByOtherOwner` | network `Ready`, pool | a NetBox object with the same `capi_ref` belongs to another owner (see move and pivot) |
| `Duplicate` | network `Ready`, pool | more than one NetBox object carries the key, or the CIDR to adopt exists more than once or is held by another network |
| `InvalidSpec` | network `MetadataSynced`, pool `Ready` | `customFields` sets `capi_ref` or `capi_cluster`; the `ipAddress` template of a pool does not render or gives an invalid DNS name |
| `WaitingForPrefix` | network `MetadataSynced` | metadata waits for the prefix to be `Ready` |
| `Deleting` | network, pool `Ready` | deletion in progress |
| `WaitingForPools` | network `Ready` while deleting | a NetBoxIPPool still has a range in the network |
| `NetworkNotReady` | pool, range | the network of the range is missing or not `Ready` |
| `MixedNetworks`, `InvalidRange`, `RangeInUse`, `RangeOverlap` | range | see [NetBoxIPPool](#netboxippool) |
| `WaitingForAddresses` | pool `Ready` while deleting | `IPAddress` objects of the pool still exist |
| `Forbidden` | network, pool, range | the token lacks a NetBox permission (HTTP 403) |
| `NetBoxError` | network, pool, range | another NetBox error; the message has the details |
| `InternalError` | network, pool | a Kubernetes API call failed |
| `PoolNotReady`, `PoolExhausted`, `AllocationFailed` | claim `Ready` | the pool has no ready range, no free address, or the allocation failed |
| `AddressConflict` | claim `Ready` | the allocated address is held by another object in NetBox |
| `Allocating` | claim `Ready` (Unknown) | the address is allocated, the `IPAddress` is being created |

On a claim, a NetBox error or a 403 during allocation shows as `AllocationFailed` with the message. A claim stuck in deletion, for example without delete on `ipam.ipaddress`, sets no condition: the claim, its `IPAddress` and the NetBox address stay (CAPV still finishes deleting the Machine), deleting the pool waits with `WaitingForAddresses`, and the reason is only in the provider logs (`release address: ... forbidden`). A claim whose `spec.clusterName` or `cluster.x-k8s.io/cluster-name` label names a Cluster that does not exist is skipped without a condition; the provider logs it at Info level.

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
make test               # unit + CRD validation
make test-integration   # + controllers against the dev NetBox (make netbox-up first)
```

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

`vcsim-up` builds the vcsim controller image from the CAPV sources (`CAPV_VERSION`, default v1.17.0, cloned into `~/.cache/capi-ipam-netbox`) and installs CAPV with clusterctl. The machines get their addresses through `addressesFromPools`, control plane from `<NAME>-cp`, workers from `<NAME>-workers`, and CAPV writes them into the VM metadata in vcsim. The workload cluster itself is fake: the vcsim controller simulates the machine bootstrap and the API server behind the control plane endpoint, and without cloud-init the VMs report the address 192.0.2.100. `CP_MACHINES`, `WORKER_MACHINES`, `K8S_VERSION`, `PARENT_PREFIX`, `GATEWAY` and `PREFIX_LENGTH` tune the cluster. Deleting the Cluster releases its addresses; the network and the pools stay.

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
