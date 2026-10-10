---
title: NetBox IPAM provider for Cluster API
authors:
  - "@MicleMaslow"
reviewers: []
creation-date: 2026-10-07
last-updated: 2026-10-10
status: implemented
see-also:
  - "https://github.com/kubernetes-sigs/cluster-api/blob/main/docs/proposals/20220125-ipam-integration.md"
---

# NetBox IPAM provider for Cluster API

## Table of Contents

- [Glossary](#glossary)
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [API](#api)
  - [NetBox model](#netbox-model)
  - [Ownership and idempotency](#ownership-and-idempotency)
  - [Pools and ranges](#pools-and-ranges)
  - [Control-plane endpoint](#control-plane-endpoint)
  - [deletionPolicy](#deletionpolicy)
  - [Adoption](#adoption)
  - [clusterctl move and pivot](#clusterctl-move-and-pivot)
  - [Metadata](#metadata)
  - [Security Model](#security-model)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Alternatives](#alternatives)
- [Upgrade Strategy](#upgrade-strategy)
- [Additional Details](#additional-details)
  - [Test Plan](#test-plan)
  - [Version Skew Strategy](#version-skew-strategy)
- [Future Work](#future-work)
- [Implementation History](#implementation-history)

## Glossary

Cluster API terms: see the [Cluster API Book Glossary](https://cluster-api.sigs.k8s.io/reference/glossary.html).

- **Management plane**: the management cluster that runs the provider, including the clusters its state is moved to with `clusterctl move`.
- **Owner**: the NetBox Owner object (NetBox ≥ 4.5) that marks the objects of one management plane.
- **L2 network**: the subnet of the nodes' network, `gateway/addressPrefixLength`. Addresses get its mask; its network and broadcast address and the gateway are never handed out.
- **Parent prefix**: an existing NetBox prefix that cluster subnets are carved from: the L2 network or a logical pool for CAPI inside it.
- **VIP**: the address of the cluster's API server (kube-vip), reserved in the cluster prefix.
- **Key**: the `capi_ref` custom field, `Kind/namespace/name` of the Kubernetes object a NetBox object belongs to.

## Summary

An IPAM provider that implements the Cluster API IPAM contract (`IPAddressClaim`/`IPAddress`, contract v1beta2) with NetBox as the store. A `NetBoxNetwork` holds one subnet of a cluster as a NetBox prefix, carved out of a parent prefix or adopted. A `NetBoxIPPool` holds address ranges of such subnets as NetBox IP ranges. Claims get NetBox IP addresses allocated from those ranges. A network can also reserve the control plane VIP of its cluster and write it to `Cluster.spec.controlPlaneEndpoint`. Every NetBox object the provider writes carries its Owner and a key, so state is found again after a restart, `clusterctl move` or pivot, and objects of others are never changed.

## Motivation

Many on-premises installations keep NetBox as the source of truth for addresses. With the in-cluster IPAM provider the addresses of machines live only in Kubernetes: NetBox has to be updated by hand or by a sync, and addresses taken by hand in NetBox can be handed out twice. Cluster subnets are usually cut from shared L2 networks (VLANs) and split by role: control plane, ingress, workers, a control plane VIP, load balancer addresses. That split should be recorded in NetBox and be the same for every cluster.

### Goals

1. Implement the IPAM contract: an `IPAddressClaim` that references a `NetBoxIPPool` gets an address allocated in NetBox, with the mask and gateway of its L2 network.
2. Carve one prefix per cluster out of a parent prefix, or adopt a prefix the NetBox admin prepared.
3. Describe pools as offsets inside the cluster subnet, so one manifest fits every cluster; let pools grow, shrink and move without changing machine templates.
4. Be idempotent and safe: never change objects of another owner, survive `clusterctl move` and pivot, rebuild status from NetBox.
5. Stay independent of the infrastructure provider; any provider that creates `IPAddressClaim`s works.
6. Set NetBox tags and custom fields on the cluster prefix declaratively.
7. Reserve the control plane VIP of a cluster in NetBox and give it to the Cluster before CAPI renders the kubeconfig and cloud-init.

### Non-Goals

- IPv6, VRFs other than the global one, several ranges of one pool in one network.
- Several VIPs per cluster, a VIP outside the cluster prefix, running the load balancer (kube-vip or an external one): the provider only reserves the address and sets the endpoint.
- Creating VM, interface or other inventory objects in NetBox.
- Migrating existing allocations from other IPAM providers.
- Managing NetBox itself: custom field definitions, owners, users, permissions and parent prefixes are set up by the NetBox admin.
- Admission webhooks: field validation is CEL in the CRDs, cross-object checks are conditions.

See [Future Work](#future-work).

## Proposal

### User Stories

1. As a platform operator, I create a cluster and it gets a /26 out of a shared VLAN prefix, recorded in NetBox with the cluster name, without anyone editing NetBox.
2. As a network engineer, I see in NetBox which prefix, range and address belongs to which cluster and claim, and an address I take by hand is never handed to a machine.
3. As a cluster operator, I add a pool of load balancer addresses to a running cluster by shrinking the workers pool, in one `kubectl apply`.
4. As a platform operator, I move clusters from a bootstrap cluster to a permanent management cluster with `clusterctl move` and nothing in NetBox leaks or is duplicated.
5. As a platform operator, the API address of a new cluster comes from its prefix in NetBox without anyone choosing it, and kube-vip, the kubeconfig and the join configs all carry it.

### API

Group `ipam.cluster.x-k8s.io`, version `v1alpha1`, both kinds namespaced. The CRDs carry the labels `cluster.x-k8s.io/v1beta2: v1alpha1` and `clusterctl.cluster.x-k8s.io/move-hierarchy: ""`.

```yaml
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxNetwork
metadata: {name: demo, namespace: demo}
spec:
  clusterName: demo              # immutable
  parentPrefix: 10.10.1.0/24     # immutable, IPv4; the L2 network or a logical pool inside it
  gateway: 10.10.0.1             # immutable, inside the L2 network
  addressPrefixLength: 22        # immutable; mask of the L2 network, by default that of parentPrefix
  prefixLength: 26               # carve; or prefix: 10.10.1.64/26 to adopt; exactly one, immutable
  isPool: true                   # is_pool on the carved prefix, default false
  controlPlaneEndpoint:          # reserve the VIP and set it as the endpoint of the Cluster
    port: 6443                   # default
    dnsName: demo-api.example    # dns_name of the VIP in NetBox
  deletionPolicy: Retain         # Retain (default) or Delete
  tags: [k8s]                    # slugs of existing NetBox tags
  customFields: {labels: {cluster: demo}}
status:
  prefix: 10.10.1.64/26
  prefixID: 42
  addressPrefixLength: 22
  controlPlaneEndpoint: {host: 10.10.1.127, port: 6443}
  conditions: [Ready, MetadataSynced, ControlPlaneEndpointReady]
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: demo-workers, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
spec:
  ranges:                        # 1..16, one per network, used in order
  - {networkName: demo, start: 16, end: -11}
  ipAddress:                     # optional Go templates of NetBox address fields
    dnsName: "{{ .Owner }}"      # default
    description: "{{ .Cluster }} {{ .Pool }} {{ .Owner }}"   # default
status:
  ranges:
  - {networkName: demo, id: 7, startAddress: 10.10.1.80, endAddress: 10.10.1.117, ready: true}
  gateway: 10.10.0.1
  addressPrefixLength: 22
  ipAddresses: {total: 38, used: 3, free: 35}
  conditions: [Ready, RangesReady]
```

The CRDs reject an L2 network (`gateway/addressPrefixLength`) that does not contain `parentPrefix`, an `addressPrefixLength` longer than the parent mask, a `prefix` outside `parentPrefix`, a `prefixLength` not longer than the parent mask, IPv6, a changed `gateway` or `addressPrefixLength`, switching between `prefix` and `prefixLength`, `isPool` without `prefixLength`, an adopted `prefix` with `deletionPolicy: Delete`, two ranges in one network and a range whose `start` is after its `end` (for offsets of the same sign).

Claims reference a pool with `poolRef: {apiGroup: ipam.cluster.x-k8s.io, kind: NetBoxIPPool, name: <pool>}`. The `IPAddress` gets the address, the mask of the L2 network and the gateway. The claim lifecycle (finalizers, owner references, pause) comes from `ipamutil` of the in-cluster IPAM provider; the claim's `Ready` condition uses the reasons of the contract (`PoolNotReady`, `PoolExhausted`, `AllocationFailed`) plus `AddressConflict` and `Allocating`.

`spec.ipAddress` sets `dns_name` and `description` of every address of the pool. Both are `text/template` (`missingkey=error`) over `.Owner` (the first owner reference of the claim, else the claim itself), `.Claim`, `.Cluster`, `.Namespace` and `.Pool`; an empty field takes the default, `{{ "" }}` leaves it empty in NetBox. The result is trimmed; `dnsName` is lowercased, as NetBox stores it, and checked against the NetBox DNS name pattern; `description` is cut to 200 characters. The fields are written when an address is created, restored or handed back to the claim that holds it, and compared at every reconcile of an allocated claim: a difference is patched, so a change of the templates reaches allocated addresses and manual edits of the two fields are overwritten. The owner chain is not followed to the Machine; with CAPV the owner of the claim is the VSphereVM, whose name matches the Machine and the node. A template that does not render on sample values or gives an invalid DNS name makes the pool `InvalidSpec`: no new addresses, allocated claims keep `Ready`. A claim whose own values do not render fails with `AllocationFailed`, or, once allocated, keeps its `Ready` and backs off; its address is still restored or handed back, with both fields empty. Templates may not use `range`, `define`, `block` or `template`, and a value is limited to 1024 bytes.

### NetBox model

| Kubernetes | NetBox | Key (`capi_ref`) |
|---|---|---|
| `NetBoxNetwork` | `ipam.prefix` | `NetBoxNetwork/<ns>/<name>` |
| range of a `NetBoxIPPool` | `ipam.iprange` | `NetBoxIPPool/<ns>/<pool>/<network>` |
| `IPAddressClaim` | `ipam.ipaddress` | `IPAddressClaim/<ns>/<name>` |

Every object the provider writes carries `owner`, the custom fields `capi_ref` and `capi_cluster`, and a changelog message starting with `capi:`. Prefixes are carved with `available-prefixes` of the parent and addresses allocated with `available-ips` of a range; NetBox serialises both with an advisory lock. The provider never writes parent prefixes, VRFs, tenants, owners, or tag and custom field definitions.

An object is looked up with `cf_capi_ref=<key>&owner_id=<owner>`, the ranges of a pool with `cf_capi_ref__isw=NetBoxIPPool/<ns>/<pool>/`. NetBox ignores unknown `cf_` filters and matches text fields with `icontains` unless `filter_logic` is `exact`, so the provider checks both custom fields at start and re-checks every result on the client. Lists follow pagination.

On start the provider checks the NetBox version (≥ 4.6), both custom fields (type text, `filter_logic: exact`, object types prefix, IP range, IP address) and the Owner, and exits with the reason otherwise.

### Ownership and idempotency

- The key does not depend on the object UID, so a re-created object (move, restore of the management cluster) finds its prefix, ranges and addresses again.
- Before creating an object under a new key the provider looks for that key under any owner: under another owner it reports `OwnedByOtherOwner`, without an owner `NotOwned`, and creates nothing.
- Updates that merge with state others can change (metadata, range bounds, `mark_populated`) are conditional on the ETag (`If-Match`); NetBox 4.6 is the first version with ETags.
- Out-of-band changes in NetBox:
  - a prefix gone while the status holds it: `PrefixNotFound`, no new carve;
  - an address gone while its `IPAddress` exists: created again with the same address, or `AddressConflict` if NetBox gave the address to another object;
  - an address deleted by hand and allocated to another claim before its owner noticed: given back to the claim that holds it, and the next free address is taken.
- Errors map to reasons on conditions. States that only a NetBox change can clear (`Forbidden`, `Exhausted`, `RangeInUse`, ...) are checked again every minute; waits on Kubernetes objects are woken by watches.

### Pools and ranges

- A range is `{networkName, start, end}`, offsets inside the network's prefix; a negative offset counts from the last address. All networks of a pool must share the L2 network (gateway and mask) of the first range's network (`MixedNetworks`); logical parents of one VLAN can share a pool.
- A range must not cover the network, broadcast or gateway address of the L2 network, or the VIP (`InvalidRange`).
- Every range is reconciled on its own and reported in `status.ranges[]`; claims are served by the ready ones, and the pool is `Ready` while at least one is.
- Ranges can be changed and removed. A change that would leave allocated addresses outside the range waits (`RangeInUse`); a change that waits, overlaps another range (`RangeOverlap`) or is invalid keeps the old bounds serving while they are still valid.
- Before a range is shrunk, deleted or deprecated, the provider checks the allocated addresses, sets `mark_populated` on the range so NetBox hands out no address from it, checks again, then shrinks or deprecates it with the ETag of that update, or deletes it. A deprecated range stays closed.
- A range in the way of another NetBox range waits (`RangeOverlap`). Deprecated ranges of the provider in the way and without addresses are deleted; a deprecated range of the same pool and network is taken back and resized.

### Control-plane endpoint

KubeadmControlPlane creates no kubeconfig, machines or bootstrap data until `Cluster.spec.controlPlaneEndpoint` is set, and CAPI puts the endpoint into the kubeadm config of every node. The network reserves the VIP right after its prefix, once its Cluster exists, and writes it to the Cluster, so everything CAPI renders carries it and kube-vip reads it from the kubeadm config.

- **Choice.** The host of the Cluster's endpoint if it is an address inside the prefix (migration of a running cluster). Otherwise the highest address of the prefix that is not the network, broadcast or gateway of the L2 network, holds no NetBox address and is in no IP range. An unusable endpoint address is `EndpointConflict`.
- **NetBox.** An IP address with the L2 mask, role `vip`, status `active`, the owner, the key `NetBoxNetwork/<ns>/<name>/vip` and `dns_name` from the spec. It is found by the key, never moves, and is created again at its old address if it is lost; one moved by hand is `EndpointConflict`.
- **Cluster.** A merge patch of `spec.controlPlaneEndpoint` only, with the VIP and `status.controlPlaneEndpoint.port`, and only while the endpoint has no host or no port. Another endpoint is `EndpointConflict` and the Cluster is not touched. The Cluster is watched, so the order of applying the manifests does not matter: a new VIP waits for the Cluster, whose endpoint may be the VIP (`WaitingForCluster`); a paused Cluster is skipped.
- **Conditions.** `ControlPlaneEndpointReady` is True when the VIP is reserved and the Cluster has it, else False with `WaitingForCluster`, `EndpointConflict` or the reason of the VIP error. It never changes `Ready`: a VIP error stops no pool.
- **Pools.** A range that is not in NetBox yet waits for the VIP (`NetworkNotReady`); one over the VIP is `InvalidRange`.
- **Deletion.** The VIP follows `deletionPolicy`. Removing `controlPlaneEndpoint` from the spec keeps the VIP in NetBox and in the status, so pools keep off it.
- **No feature gate.** The field in the spec of a network switches the behaviour on for that network. The provider needs `patch` on `clusters` and touches no infrastructure kind.

### deletionPolicy

| | Retain (default) | Delete | adopted (`spec.prefix`) |
|---|---|---|---|
| prefix | `deprecated`, reclaimed when the network is created again | deleted | released: `capi_ref` and `capi_cluster` cleared, the rest stays |
| ranges in the network, when their pool releases them | `deprecated` and closed | deleted | `deprecated` and closed |
| VIP | `deprecated`, taken back when the network is created again | deleted | `deprecated` |
| addresses | deleted with the claim | deleted with the claim | deleted with the claim |

A network being deleted waits for the pools with a range in it (`WaitingForPools`); a pool waits for its `IPAddress` objects (`WaitingForAddresses`) and for NetBox addresses left in its ranges (`RangeInUse`). Without the NetBox permission a network or pool keeps its finalizer and reports `Forbidden`; a claim keeps its finalizer and the error is only logged, since ipamutil sets no condition on a failed release.

### Adoption

With `spec.prefix` the network takes a given CIDR inside the parent. An existing prefix must carry the provider's Owner, set by the NetBox admin, otherwise the network reports `NotOwned`; a missing one is created. Adoption writes only `capi_ref` and `capi_cluster`. A prefix held by another network is `Duplicate`; a deprecated prefix another network left is adopted, and made active again, only from the same namespace.

### clusterctl move and pivot

- NetBox objects are not touched by a move: a deletion with the `clusterctl.cluster.x-k8s.io/delete-for-move` annotation releases nothing. The target finds everything by key and rebuilds the status, including removed ranges that still hold addresses.
- Invariant: one Owner per management plane, kept through move and pivot; source and target run with the same `--netbox-owner`. Management planes that share a NetBox must not repeat the namespace and name of these objects.
- A network pauses with its Cluster (`spec.clusterName`); a pool pauses with the Cluster named by its `cluster.x-k8s.io/cluster-name` label.

### Metadata

`spec.customFields` and `spec.tags` are applied to the prefix in one conditional update. A JSON-object custom field is merged key by key; keys the provider wrote earlier (`status.customFieldKeys`) and no longer given are removed; other values replace the field. Tags the provider set (`status.tags`) and no longer given are removed, tags of others stay. `capi_ref` and `capi_cluster` cannot be set. Failures show in `MetadataSynced` and never block `Ready`.

### Security Model

NetBox token. The provider authenticates with a v2 token of a dedicated NetBox user:

| Object types | Actions | Constraints |
|---|---|---|
| `ipam.prefix`, `ipam.iprange`, `ipam.ipaddress`, `users.owner`, `extras.customfield`, `extras.tag` | view | none |
| `ipam.prefix`, `ipam.iprange`, `ipam.ipaddress` | add, change | `{"owner__name": "<owner>"}` |
| `ipam.ipaddress` | delete | `{"owner__name": "<owner>"}` |
| `ipam.prefix`, `ipam.iprange` | delete | `{"owner__name": "<owner>"}`; only for `deletionPolicy: Delete` and deprecated ranges in the way; not granted in production |

- View is unconstrained: the owner guard, adoption and the restore of addresses look at objects of other owners, and `available-prefixes` reads the parent prefix with the view permission.
- Writes are constrained by the owner. A production token gets no delete on prefixes and IP ranges and the networks keep `deletionPolicy: Retain`; a deletion under `Delete` keeps its finalizer and reports `Forbidden`, and deprecated ranges in the way of a new range are not deleted automatically.
- The write permissions can be narrowed to an address block. NetBox validates a constraint against every object type of its permission and the field names differ per model, so each model needs its own permission. Example constraints (the lookups were checked against the NetBox 4.6 models):

  | Model | Constraint |
  |---|---|
  | `ipam.prefix` | `{"owner__name": "capi-mgmt-a", "prefix__net_contained": "10.10.0.0/16"}` |
  | `ipam.iprange` | `{"owner__name": "capi-mgmt-a", "start_address__net_host_contained": "10.10.0.0/16", "end_address__net_host_contained": "10.10.0.0/16"}` |
  | `ipam.ipaddress` | `{"owner__name": "capi-mgmt-a", "address__net_host_contained": "10.10.0.0/16"}` |

Kubernetes. The token reaches the provider as environment from the Secret `capi-ipam-netbox-credentials`; the provider has no RBAC on Secrets. It reads Clusters and patches their `spec.controlPlaneEndpoint`, writes its own kinds, `IPAddressClaim` status and `IPAddress` objects. Metrics are served on the diagnostics port behind authentication and authorization, as in Cluster API.

Tenancy. Anyone who may create a `NetBoxNetwork` in a namespace may carve from any parent prefix the token can write to and adopt deprecated prefixes of the same namespace. NetBox constraints bound what the token can reach; an admin-scoped parent object is future work.

Transport. The provider trusts the system roots plus an optional CA file. `http://` and skipping certificate verification are logged as warnings and meant for development.

### Risks and Mitigations

- **Retained prefixes fill the parent.** A deprecated prefix still occupies its space for `available-prefixes`. Mitigation: documented; cleanup of retained objects by the NetBox admin; a report or GC is future work.
- **NetBox checks overlap and uniqueness without a lock.** Two parallel range creates, or a plain create next to an `available-*` request, can both pass. Mitigation: the provider runs its check-then-write sections (range create, resize and release with the fence, prefix adopt and carve, address allocation and restore) one at a time in its process, and leader election makes that process the only writer of its Owner. Management planes with other Owners that share a parent are not covered.
- **Pool to Cluster link is a label.** Without `cluster.x-k8s.io/cluster-name` a pool does not pause with its Cluster. Mitigation: documented; deriving the cluster from the pool's networks is possible later.
- **NetBox outage.** The provider does not start without NetBox. While running, allocated claims stay `Ready`; new allocations and changes wait and are retried.
- **Shared NetBox between management planes.** Protected by the Owner, the owner guard and the key invariant; a broken invariant blocks with `OwnedByOtherOwner` instead of writing.

## Alternatives

- **In-cluster IPAM plus a sync to NetBox.** Two sources of truth; no carving of subnets; addresses taken in NetBox are not seen by the allocator.
- **A generic NetBox operator with its own claim kinds.** It does not implement the Cluster API IPAM contract and would need a bridge controller between claims of both kinds.
- **Marking objects with tags or descriptions instead of an Owner.** NetBox permissions can be constrained by owner, so the token cannot touch foreign objects; tags give no such guarantee.
- **The generated Go client for NetBox.** go-netbox v4.3 does not cover Owners and ETags; a thin client on `net/http` covers the few endpoints used.
- **Writing the VIP into `InfraCluster.spec.controlPlaneEndpoint`.** Needs patch rights on every infrastructure kind and makes the provider a second controller of foreign objects.
- **A VIP from a pool range or a claim.** The VIP would be an address of a pool, handed out with the machines; reserving it in the cluster prefix before any pool keeps it out of the ranges.
- **Admission webhooks.** CEL covers field validation without certificates and an extra endpoint.

## Upgrade Strategy

The API is `v1alpha1`: fields can change between minor releases, without conversion. The clusterctl metadata maps release series 0.1 to contract v1beta2.

## Additional Details

### Test Plan

- Unit tests with `httptest` for the NetBox client (pagination included), preflight, metadata merge and address math.
- CRD validation tests on envtest.
- Controller tests on envtest against a real NetBox 4.6.7 (docker compose), including deletion without the delete permission, objects deleted in NetBox by hand, blocked shrinks, missing tags, objects of another owner and `clusterctl move`.
- Future: end-to-end tests with the Cluster API test framework, an infrastructure provider simulator, `clusterctl move` between two management clusters.

### Version Skew Strategy

The provider refuses to start against NetBox older than 4.6. It follows the Cluster API contract v1beta2 (tested with Cluster API v1.14.3).

## Future Work

1. **Control-plane endpoint with ClusterClass.** The VIP is written to the Cluster the same way for a topology cluster, but only plain clusters are tested.
2. **Address enrichment from the Machine.** Fill `dns_name` and `description` from the Machine (claim → infrastructure machine → Machine by owner references) instead of the claim's first owner, and create no VM and interface objects.
3. **Migration with `spec.importFrom`.** Record the addresses already allocated by another IPAM pool in NetBox under the keys of their claims, so running clusters switch without re-addressing.
4. **Admin-scoped parent.** A cluster-scoped `NetBoxParentNetwork` with prefix, gateway, VRF, defaults and `allowedNamespaces`, referenced by `NetBoxNetwork.spec.parentRef`; the gateway is set once and `MixedNetworks` becomes a check of the same reference.
5. Further: IPv6 and VRFs, a report or GC of retained objects, a `Paused` condition, a token file with rotation, start without NetBox.

## Implementation History

- 2026-10-06: design.
- 2026-10-07: v0.1 implemented: networks, pools with per-range readiness and editable ranges, claims, metadata, owner guard, move support.
- 2026-10-10: the L2 network separate from the parent prefix (`addressPrefixLength`, `isPool`); the control plane VIP (`controlPlaneEndpoint`) reserved in NetBox and written to `Cluster.spec.controlPlaneEndpoint`.
