---
title: NetBox IPAM provider for Cluster API
authors:
  - "@MicleMaslow"
reviewers: []
creation-date: 2026-10-07
last-updated: 2026-10-11
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
  - [Identity and idempotency](#identity-and-idempotency)
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
- **Owner**: the NetBox Owner object (NetBox ≥ 4.5). An optional attribute of the objects of a network, set from `NetBoxNetwork.spec.owner`; not part of the identity of an object.
- **Tenant**: the NetBox Tenant that classifies the objects and fences the rights of the token. Optional, set from `spec.tenant` like the owner; not part of the identity either.
- **Instance name**: the name of one management zone, an optional prefix of every key. Empty: keys without a prefix.
- **L2 network**: the subnet of the nodes' network, `gateway/addressPrefixLength`. Addresses get its mask; its network and broadcast address and the gateway are never handed out.
- **Parent prefix**: an existing NetBox prefix that cluster subnets are carved from: the L2 network or a logical pool for CAPI inside it.
- **VIP**: the address of the cluster's API server, reserved in the cluster prefix and served by a load balancer of the user's template (kube-vip, keepalived, ...).
- **Key**: `[<instance>/]Kind/namespace/name` of the Kubernetes object a NetBox object belongs to, kept in a text custom field (`capi_ref` by default). An object is the provider's when its key field holds exactly its key.

## Summary

An IPAM provider that implements the Cluster API IPAM contract (`IPAddressClaim`/`IPAddress`, contract v1beta2) with NetBox as the store. A `NetBoxNetwork` holds one subnet of a cluster as a NetBox prefix, carved out of a parent prefix or adopted. A `NetBoxIPPool` holds address ranges of such subnets as NetBox IP ranges. Claims get NetBox IP addresses allocated from those ranges. A network can also reserve the control plane VIP of its cluster and write it to `Cluster.spec.controlPlaneEndpoint`. Every NetBox object the provider writes carries a key, so state is found again after a restart, `clusterctl move` or pivot, and objects with another key are never changed; owner, tenant, description and comments are attributes from the spec. The provider runs as an ordinary NetBox user: what it may write is the zone its token's permissions allow (a tenant, an owner, a parent prefix), and the key can live in a text custom field the user already has.

## Motivation

Many on-premises installations keep NetBox as the source of truth for addresses. With the in-cluster IPAM provider the addresses of machines live only in Kubernetes: NetBox has to be updated by hand or by a sync, and addresses taken by hand in NetBox can be handed out twice. Cluster subnets are usually cut from shared L2 networks (VLANs) and split by role: control plane, ingress, workers, a control plane VIP, load balancer addresses. That split should be recorded in NetBox and be the same for every cluster.

### Goals

1. Implement the IPAM contract: an `IPAddressClaim` that references a `NetBoxIPPool` gets an address allocated in NetBox, with the mask and gateway of its L2 network.
2. Carve one prefix per cluster out of a parent prefix, or adopt a prefix the NetBox admin prepared.
3. Describe pools as offsets inside the cluster subnet, so one manifest fits every cluster; let pools grow, shrink and move without changing machine templates.
4. Be idempotent and safe: never change an object that carries another key, survive `clusterctl move` and pivot, rebuild status from NetBox.
5. Need no global NetBox setup beyond a place for the key: owner and tenant are optional attributes set per network, and the key field and the instance name are configurable.
6. Stay independent of the infrastructure provider; any provider that creates `IPAddressClaim`s works.
7. Set NetBox tags and custom fields on the cluster prefix declaratively.
8. Reserve the control plane VIP of a cluster in NetBox and give it to the Cluster before CAPI renders the kubeconfig and cloud-init.

### Non-Goals

- IPv6, VRFs other than the global one, several ranges of one pool in one network.
- Several VIPs per cluster, a VIP outside the cluster prefix, running the load balancer (kube-vip or an external one): the provider only reserves the address and sets the endpoint.
- Creating VM, interface or other inventory objects in NetBox.
- Migrating existing allocations from other IPAM providers.
- Managing NetBox itself: custom field definitions, owners, tenants, users, permissions and parent prefixes are set up by the NetBox admin.
- Admission webhooks: field validation is CEL in the CRDs, cross-object checks are conditions.

See [Future Work](#future-work).

## Proposal

### User Stories

1. As a platform operator, I create a cluster and it gets a /26 out of a shared VLAN prefix, recorded in NetBox with the cluster name, without anyone editing NetBox.
2. As a network engineer, I see in NetBox which prefix, range and address belongs to which cluster and claim, and an address I take by hand is never handed to a machine.
3. As a cluster operator, I add a pool of load balancer addresses to a running cluster by shrinking the workers pool, in one `kubectl apply`.
4. As a platform operator, I move clusters from a bootstrap cluster to a permanent management cluster with `clusterctl move` and nothing in NetBox leaks or is duplicated.
5. As a platform operator, the API address of a new cluster comes from its prefix in NetBox without anyone choosing it, and the load balancer, the kubeconfig and the join configs all carry it.

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
    port: 6443                   # default; immutable while controlPlaneEndpoint is set
    dnsName: demo-api.example    # dns_name of the VIP in NetBox
    description: "..."           # templates of the VIP's text, with defaults
    comments: "..."
  owner: {name: zone-a-owner}    # optional, or {id: 7}; synced to the objects
  tenant: {slug: platform}       # optional, or {id: 42}
  description: "..."             # template of the prefix's text, with a default
  comments: "..."
  deletionPolicy: Retain         # Retain (default) or Delete
  tags: [k8s]                    # slugs of existing NetBox tags
  customFields: {labels: {cluster: demo}}
status:
  prefix: 10.10.1.64/26
  prefixID: 42
  ownerID: 7                     # a cache of what spec.owner resolves to, like tenantID
  tenantID: 42
  addressPrefixLength: 22
  controlPlaneEndpoint: {host: 10.10.1.127, port: 6443}
  conditions: [Ready, MetadataSynced, ControlPlaneEndpointReady, Paused]
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: demo-workers, namespace: demo, labels: {cluster.x-k8s.io/cluster-name: demo}}
spec:
  ranges:                        # 1..16, one per network, used in order
  - {networkName: demo, start: 16, end: -11}
  ipAddress:                     # optional Go templates of NetBox address fields
    dnsName: "{{ .Owner }}"      # default
    description: "{{ .Namespace }}/{{ .Owner }} // {{ .Cluster }} {{ .Pool }}"   # default
    comments: "..."              # default: a note that the provider manages the object
  ipRange:                       # optional Go templates of NetBox range fields
    description: "{{ .Namespace }}/{{ .Pool }} // pool of cluster {{ .Cluster }}"   # default
    comments: "..."
  addressReuseGracePeriodSeconds: 300   # keep the address of a deleted claim this long, default 0, at most 86400
  omitGateway: true              # addresses without a gateway, e.g. for a second NIC; default false; immutable
status:
  ranges:
  - {networkName: demo, id: 7, startAddress: 10.10.1.80, endAddress: 10.10.1.117, ready: true}
  gateway: 10.10.0.1
  addressPrefixLength: 22
  ipAddresses: {total: 38, used: 3, free: 35}
  conditions: [Ready, RangesReady, Paused]
```

`owner` and `tenant` name the NetBox object by name or slug, or by id, exactly one form each (CEL). They are set on the network only, without a default. The prefix and the VIP get the owner and tenant of the network, the ranges and addresses those of the network they lie in. The spec is the source of truth: the values are written on create and synced on existing objects (the network syncs its prefix and VIP, the pool its ranges, a claim its address when it is checked again), and an unset field is neither written nor cleared. A prefix under `spec.prefix` keeps its own after creation. The network resolves the name or slug to an id (or checks the id; a 403 on the check takes the id as given) and keeps it in `status.ownerID` and `status.tenantID`, a cache for its pools and claims that is refreshed on a new generation, when nothing is cached, or when the reason of `Ready` is `WaitingForCluster`, `InvalidSpec`, `NetBoxError`, `Forbidden`, `OwnerNotFound` or `TenantNotFound`. A name, slug or id that NetBox does not have is `OwnerNotFound` or `TenantNotFound`: the network is not `Ready`, its pools take its ranges out of service for new claims, allocated addresses stay. A token that cannot set the value (a constraint) reports `Forbidden` on `MetadataSynced` of the network or `RangesReady` of the pool, and stops no allocation.

The CRDs reject an L2 network (`gateway/addressPrefixLength`) that does not contain `parentPrefix`, an `addressPrefixLength` longer than the parent mask, a `prefix` outside `parentPrefix`, a `prefixLength` not longer than the parent mask, IPv6, a changed `gateway` or `addressPrefixLength`, a changed, added or removed `controlPlaneEndpoint.port` (while the endpoint is set: the Cluster keeps the endpoint it got) and `omitGateway` (an `IPAddress` never changes, so a change would mix both kinds in a pool), switching between `prefix` and `prefixLength`, `isPool` without `prefixLength`, an adopted `prefix` with `deletionPolicy: Delete`, two ranges in one network and a range whose `start` is after its `end` (for offsets of the same sign).

Claims reference a pool with `poolRef: {apiGroup: ipam.cluster.x-k8s.io, kind: NetBoxIPPool, name: <pool>}`. The `IPAddress` gets the address, the mask of the L2 network and the gateway; with `omitGateway` it gets no gateway, as for a second NIC whose default route is on the first one (it applies to addresses allocated after the change, as the `IPAddress` spec is immutable). The claim lifecycle (finalizers, owner references, pause) comes from `ipamutil` of the in-cluster IPAM provider; the claim's `Ready` condition uses the reasons of the contract (`PoolNotReady`, `PoolExhausted`, `AllocationFailed`) plus `AddressConflict`, `Allocating`, `Releasing` and `ReleaseFailed`.

An address leaves NetBox when its claim is deleted, but not earlier than the machine stops using it. The release waits while the claim has finalizers of others than ipamutil, `foregroundDeletion` and `orphan`: an infrastructure provider keeps its finalizer on the claim until the machine is gone (CAPV: `ip-claim-protection`), and the claim is `Releasing` with their names, checked again every minute or when a finalizer goes. Then `addressReuseGracePeriodSeconds` of the pool keeps the address for that long after the deletion of the claim (`Releasing` with the time), so that caches of the old machine do not meet the new one; it does not apply while the pool is being deleted or is gone. A failed release is `ReleaseFailed` with the error. A claim deleted for `clusterctl move` releases nothing.

`spec.ipAddress` sets `dns_name`, `description` and `comments` of every address of the pool; `spec.ipRange` sets `description` and `comments` of its ranges, and `NetBoxNetwork.spec.description` and `.comments` (and the same under `controlPlaneEndpoint`) those of the prefix and the VIP. All are `text/template` (`missingkey=error`); an empty field takes the default, `{{ "" }}` leaves it empty in NetBox. The description is for people, the default comments say that the provider manages the object and should not be edited by hand.

| Object | Variables | Default description |
|---|---|---|
| prefix, VIP | `.Cluster`, `.Namespace`, `.Name` | `{{ .Namespace }}/{{ .Name }} // cluster {{ .Cluster }}`, `{{ .Namespace }}/{{ .Name }} // {{ .Cluster }} control plane endpoint` |
| IP range | `.Cluster`, `.Namespace`, `.Pool`, `.Network` | `{{ .Namespace }}/{{ .Pool }} // pool of cluster {{ .Cluster }}` |
| IP address | `.Owner` (the first owner reference of the claim, else the claim itself), `.Claim`, `.Cluster`, `.Namespace`, `.Pool` | `{{ .Namespace }}/{{ .Owner }} // {{ .Cluster }} {{ .Pool }}`, and `dnsName` `{{ .Owner }}` |

The result is trimmed; `dnsName` is lowercased, as NetBox stores it, and checked against the NetBox DNS name pattern; `description` is cut to 200 characters, comments are not; a NUL character is an error. The fields are written when an object is created and compared at every reconcile: a difference is patched, so a change of the templates reaches existing objects and manual edits are overwritten. A prefix under `spec.prefix` is not synced after creation. For an address the fields are also written when it is restored or handed back to the claim that holds it, and compared at every reconcile of an allocated claim. The owner chain is not followed to the Machine; with CAPV the owner of the claim is the VSphereVM, whose name matches the Machine and the node. A template that does not render on sample values or gives an invalid DNS name makes the pool or the network `InvalidSpec`: no new addresses or ranges, allocated claims keep `Ready`. A claim whose own values do not render fails with `AllocationFailed`, or, once allocated, keeps its `Ready` and backs off; its address is still restored or handed back, with the fields empty. Templates may not use `range`, `define`, `block` or `template`, and a value is limited to 1024 bytes.

### NetBox model

| Kubernetes | NetBox | Key |
|---|---|---|
| `NetBoxNetwork` | `ipam.prefix` | `[<instance>/]NetBoxNetwork/<ns>/<name>` |
| range of a `NetBoxIPPool` | `ipam.iprange` | `[<instance>/]NetBoxIPPool/<ns>/<pool>/<network>` |
| `IPAddressClaim` | `ipam.ipaddress` | `[<instance>/]IPAddressClaim/<ns>/<name>` |

Every object the provider writes carries the key, the description and comments, the owner and tenant of its network when it has them, and a changelog message starting with `capi:`. The key is in the text custom field `capi_ref` by default; `--netbox-ref-field` names another text field, an existing one included (any `filter_logic` but `disabled`). `--instance-name` is the optional name of the management zone and the first segment of every key; empty, the keys have none, so objects made without a name are found. The provider does not migrate keys between fields or instance names: a changed name is a new zone. Prefixes are carved with `available-prefixes` of the parent and addresses allocated with `available-ips` of a range; NetBox serialises both with an advisory lock. The provider never writes parent prefixes, VRFs, tenants, owners, or tag and custom field definitions.

An object is looked up with `cf_capi_ref=<key>`, nothing else: no owner and no parent prefix take part; the ranges of a pool with `cf_capi_ref__isw=[<instance>/]NetBoxIPPool/<ns>/<pool>/`. NetBox ignores unknown `cf_` filters and matches text fields with `icontains` unless `filter_logic` is `exact`, so the provider checks the key field at start and re-checks every result on the client for the exact key. Lists follow pagination.

On start the provider checks the NetBox version (≥ 4.6) and the key field (type text; object types prefix, IP range, IP address; in NetBox 4.7 status `active`; `filter_logic`: `exact` is silent, `loose` is a warning because every result is re-checked, `disabled` stops the start). A token that may not view custom field definitions gets a warning and a check by behavior (a filter on prefixes). That NetBox answers with an `ETag` (conditional updates need it; a proxy may strip it) is checked at the first single read of an object, and an answer without it is an error on the condition of that object. The check runs in the manager without leader election, and the controllers start after it passes; until then `/readyz` is not ready and `/healthz` answers. Errors that a retry may clear (NetBox down, 5xx, a custom field in status `provisioning`) are retried in the process with a wait of 1 s growing to 1 min, so a restart during a NetBox outage does not crash-loop the pod. A setup the provider cannot work with (version, key field, settings) makes it exit with code 1 and the reason.

Every request carries `Accept-Language: en` (the overlap of ranges is recognised by the English error text of NetBox); `changelog_message` and `description` are cut to the 200 characters NetBox allows; a 404 is "not found" only when NetBox answers it with JSON, so one from a proxy is an error; lists are paged by `id` (`ordering=id&id__gt=<last>`), which creation and deletion between the pages cannot skip or repeat. The token is `NETBOX_TOKEN` or the file `NETBOX_TOKEN_FILE`, read at every request, so a rotated secret needs no restart.

### Identity and idempotency

- The key does not depend on the object UID, so a re-created object (move, restore of the management cluster) finds its prefix, ranges and addresses again.
- The key alone is the identity: an object is the provider's if its key field holds exactly the key, wherever it is in NetBox. The owner and tenant are attributes and take no part. The key has to be unique in NetBox: management planes that share a NetBox name themselves with `--instance-name` or must not repeat the namespace and name of their objects. The name is the user's to keep unique, equal on both sides of a move, and a changed one is a new zone.
- Updates that merge with state others can change (metadata, range bounds, `mark_populated`, adoption) are conditional on the ETag (`If-Match`); plain attributes are not. NetBox 4.6 is the first version with ETags.
- Out-of-band changes in NetBox:
  - a prefix gone while the status holds it: `PrefixNotFound`, no new carve;
  - an address gone while its `IPAddress` exists: created again with the same address, or `AddressConflict` if NetBox gave the address to another object;
  - an address deleted by hand and allocated to another claim before its owner noticed: given back to the claim that holds it, and the next free address is taken;
  - a prefix, or the VIP, left `deprecated` by a deleted network of the same name that does not fit the spec of the new one (another prefix length, a VIP outside the new prefix): its key becomes `<ref>/retired`, and a new prefix is carved or a new VIP reserved. The clean-up script finds retained and retired objects by the namespace in their key.
- Errors map to reasons on conditions. States that only a NetBox change can clear (`Forbidden`, `Exhausted`, `RangeInUse`, ...) are checked again every minute; waits on Kubernetes objects are woken by watches. A failing object backs off at most a minute in all three controllers.
- Sections that check and then write (prefix and VIP, a range, an address) run one at a time in the process. Each runs first without the lock on a client that fails on any write; only a section that has something to write runs again under the lock, so steady-state reconciles never wait for it. Releasing an address always takes the lock. The wait is the metric `netbox_ipam_write_lock_wait_seconds`.

### Pools and ranges

- A range is `{networkName, start, end}`, offsets inside the network's prefix; a negative offset counts from the last address. All networks of a pool must share the L2 network (gateway and mask) of the first range's network (`MixedNetworks`); logical parents of one VLAN can share a pool.
- A range must not cover the network, broadcast or gateway address of the L2 network, or the VIP (`InvalidRange`).
- Every range is reconciled on its own and reported in `status.ranges[]`; claims are served by the ready ones, and the pool is `Ready` while at least one is.
- Ranges can be changed and removed. A change that would leave allocated addresses outside the range waits (`RangeInUse`); a change that waits, overlaps another range (`RangeOverlap`) or is invalid keeps the old bounds serving while they are still valid.
- Before a range is shrunk, deleted or deprecated, the provider checks the allocated addresses, sets `mark_populated` on the range so NetBox hands out no address from it, checks again, then shrinks or deprecates it with the ETag of that update, or deletes it. A deprecated range stays closed.
- A range in the way of another NetBox range waits (`RangeOverlap`). Deprecated ranges of the provider in the way and without addresses are deleted; a deprecated range of the same pool and network is taken back and resized.

### Control-plane endpoint

KubeadmControlPlane creates no kubeconfig, machines or bootstrap data until `Cluster.spec.controlPlaneEndpoint` is set, and CAPI puts the endpoint into the kubeadm config of every node. The network reserves the VIP right after its prefix, once its Cluster exists, and writes it to the Cluster, so everything CAPI renders carries it. The field is for infrastructure providers without a load balancer of their own (CAPV, Metal3 and similar); CAPO, CAPA and others that create one set the endpoint themselves.

The provider knows nothing about the load balancer. The address is chosen after the template is applied, and CAPI puts no field of the Cluster into files of a KubeadmConfig, so the load balancer config of the user's template carries the placeholders `__CONTROL_PLANE_ENDPOINT__` and `__CONTROL_PLANE_PORT__`. One `preKubeadmCommands` command replaces them on the node with the host and port of the kubeadm config that CAPI renders there (`/run/kubeadm/kubeadm.yaml`, `/run/kubeadm/kubeadm-join-config.yaml`, `/etc/kubeadm.yml` with Ignition) and fails without an address. The file it edits is a parameter of the command: a kube-vip manifest, `keepalived.conf` or another. The command is [`docs/examples/control-plane-vip.yaml`](../examples/control-plane-vip.yaml); `make test` runs it on the configs CAPI v1.14 renders for init and join, with cloud-init and Ignition. The kube-vip manifest itself is the user's, from the CAPV cluster template.

- **Choice.** The host of the Cluster's endpoint if it is an address inside the prefix (migration of a running cluster). Otherwise the highest address of the prefix that is not the network, broadcast or gateway of the L2 network, holds no NetBox address and is in no IP range. An unusable endpoint address is `EndpointConflict`.
- **NetBox.** An IP address with the L2 mask, role `vip`, status `active`, the owner and tenant of the network when it has them, the key `[<instance>/]NetBoxNetwork/<ns>/<name>/vip`, `dns_name` from the spec, and a description and comments. It is found by the key, never moves, and is created again at its old address if it is lost; one moved by hand is `EndpointConflict`.
- **Cluster.** A merge patch of `spec.controlPlaneEndpoint` only, and only of the empty parts: the host with the VIP, the port with `spec.controlPlaneEndpoint.port`, else the port of the Cluster, else 6443 (CAPI copies a port alone from an infrastructure cluster); `status.controlPlaneEndpoint` has the same port. A host other than the VIP, or a port other than a given `spec.controlPlaneEndpoint.port`, is `EndpointConflict` and the Cluster is not touched. A conflict on the patch (409) is retried at once without a change of the condition. The Cluster is watched, so the order of applying the manifests does not matter: a new VIP waits for the Cluster, whose endpoint may be the VIP (`WaitingForCluster`). A network whose Cluster does not exist and that has no prefix yet touches nothing in NetBox (`Ready=False`, `WaitingForCluster`), one that holds a prefix works on. A network is paused with its Cluster or by its own annotation (condition `Paused`, set with `paused.EnsurePausedCondition`).
- **Conditions.** `ControlPlaneEndpointReady` is True when the VIP is reserved and the Cluster has it, else False with `WaitingForCluster`, `EndpointConflict` or the reason of the VIP error. It never changes `Ready`: a VIP error stops no pool.
- **Pools.** A range that is not in NetBox yet waits for the VIP (`NetworkNotReady`); one over the VIP is `InvalidRange`.
- **Deletion.** The VIP follows `deletionPolicy`. Under `Delete` the network waits (`WaitingForCluster`) while its Cluster has the VIP as endpoint host, since a deleted VIP could go to another cluster that the first one is still served on; a retained VIP is never handed out and needs no wait. Removing `controlPlaneEndpoint` from the spec keeps the VIP in NetBox and in the status, so pools keep off it; the address is freed by deleting the network or the VIP address.
- **Lost status.** If the prefix is not found by its key and the status is empty, but an active VIP of the network is in NetBox, the network is `PrefixNotFound`, as with a known status: the prefix is not carved again.
- **Duplicates.** NetBox does not check the uniqueness of addresses with role `vip`, so after creating the VIP the provider looks for other global-VRF objects with its address. If it finds an older one (a lower NetBox id), it deletes its own VIP and reports `EndpointConflict`; the next reconcile picks another address. Newer objects only make it log: of two racing creators the older stays, and a long-lived VIP is not deleted for a newer object. The check also runs while the status has no VIP, as after `clusterctl move`.
- **No feature gate.** The field in the spec of a network switches the behaviour on for that network. The provider needs `patch` on `clusters` and touches no infrastructure kind.

### deletionPolicy

| | Retain (default) | Delete | adopted (`spec.prefix`) |
|---|---|---|---|
| prefix | `deprecated`, reclaimed when the network is created again | deleted | released: `capi_ref` cleared, the rest stays |
| ranges in the network, when their pool releases them | `deprecated` and closed | deleted | `deprecated` and closed |
| VIP | `deprecated`, taken back when the network is created again | deleted | `deprecated` |
| addresses | deleted with the claim | deleted with the claim | deleted with the claim |

A network being deleted waits for the pools with a range in it (`WaitingForPools`); a pool waits for its `IPAddress` objects (`WaitingForAddresses`) and for NetBox addresses left in its ranges (`RangeInUse`). Without the NetBox permission a network or pool keeps its finalizer and reports `Forbidden`; a claim keeps its finalizer and is `ReleaseFailed` with the error.

### Adoption

With `spec.prefix` the network takes a given CIDR inside the parent. An existing prefix is taken when it carries no key or the key of the network; a missing one is created, with the attributes of the spec once. Adoption writes only the key: description, comments, owner, tenant and the rest stay as the admin set them, and they are not synced afterwards. A prefix with the key of another object is `Duplicate`; a deprecated prefix another network left is adopted, and made active again, only from the same namespace. What a token may take is bounded by its permissions in NetBox.

### clusterctl move and pivot

- NetBox objects are not touched by a move: a deletion with the `clusterctl.cluster.x-k8s.io/delete-for-move` annotation releases nothing. The target finds everything by key and rebuilds the status, including removed ranges that still hold addresses.
- Invariant: one identity per management plane, kept through move and pivot: source and target use the same key field and the same instance name. The target resolves the ids of owner and tenant again from the spec; they are not part of the identity.
- `clusterctl move` pauses Clusters only. A network pauses with its Cluster (`spec.clusterName`); a pool pauses with the Cluster named by its `cluster.x-k8s.io/cluster-name` label or the Cluster of any of its networks, or by its own paused annotation, which a pool shared by several clusters needs for the move; its claims are skipped with it. A network in the target without a prefix in its status waits for its Cluster and touches nothing in NetBox.
- The key is `[<instance>/]Kind/namespace/name`: a move that changes the namespace makes the target carve new objects.

### Metadata

`spec.customFields` and `spec.tags` are applied to the prefix in one conditional update. A JSON-object custom field is merged key by key; keys the provider wrote earlier (`status.customFieldKeys`) and no longer given are removed; other values replace the field. Tags the provider set (`status.tags`) and no longer given are removed, tags of others stay. The key field of the provider cannot be set (`InvalidSpec`). Failures show in `MetadataSynced` and never block `Ready`. A value is compared with what NetBox returns in the form it is written in, otherwise it would be written at every reconcile: the `value` of a select and multiselect (NetBox 4.7 returns `{value,label}`), the `id` of an object and multi-object field, a multi-object list without regard to its order. Other types are returned as written (checked on 4.6.7 and 4.7.2) except a datetime written with a space, which NetBox rewrites with `T`: write ISO 8601.

### Security Model

NetBox token. The provider authenticates with a v2 token of an ordinary NetBox user, limited to its zone by the constraints of the permissions; it needs no superuser. The zone is a tenant, an owner, a parent prefix, or a combination:

| Object types | Actions | Constraints |
|---|---|---|
| `ipam.prefix`, `ipam.iprange`, `ipam.ipaddress` | view | none |
| `tenancy.tenant`, `users.owner`, `extras.customfield`, `extras.tag` | view | none; each only when needed: a tenant by slug, an owner by name, the check of the key field at start, `spec.tags` |
| `ipam.prefix`, `ipam.iprange` | add, change | the zone, for example `{"tenant__slug": "<tenant>"}` or `{"owner__name": "<owner>"}` |
| `ipam.ipaddress` | add, change, delete | the zone |
| `ipam.prefix`, `ipam.iprange` | delete | the zone; only for `deletionPolicy: Delete` and deprecated ranges in the way; not granted in production |

- View is unconstrained: objects are found by key wherever they are, adoption and the restore of addresses look at objects around them, and `available-prefixes` reads the parent prefix with the view permission.
- Writes are constrained by the zone, and the constraint must match what the provider writes: with a tenant constraint the provider has to write that tenant (`spec.tenant`, else it writes none), else NetBox answers 403 and the object reports `Forbidden`. A production token gets no delete on prefixes and IP ranges and the networks keep `deletionPolicy: Retain`; a deletion under `Delete` keeps its finalizer and reports `Forbidden`, and deprecated ranges in the way of a new range are not deleted automatically.
- The write permissions can be narrowed to an address block. NetBox validates a constraint against every object type of its permission and the field names differ per model, so each model needs its own permission. Example constraints (the lookups were checked against the NetBox 4.6 models):

  | Model | Constraint |
  |---|---|
  | `ipam.prefix` | `{"tenant__slug": "platform", "prefix__net_contained": "10.10.0.0/16"}` |
  | `ipam.iprange` | `{"tenant__slug": "platform", "start_address__net_host_contained": "10.10.0.0/16", "end_address__net_host_contained": "10.10.0.0/16"}` |
  | `ipam.ipaddress` | `{"tenant__slug": "platform", "address__net_host_contained": "10.10.0.0/16"}` |

Kubernetes. The token reaches the provider as environment from the Secret `capi-ipam-netbox-credentials`, or as a file the user mounts (`NETBOX_TOKEN_FILE`); the provider has no RBAC on Secrets. It reads Clusters and patches their `spec.controlPlaneEndpoint`, writes its own kinds, `IPAddressClaim` status and `IPAddress` objects, and creates Events. Metrics (controller-runtime, `netbox_ipam_requests_total`, `netbox_ipam_request_duration_seconds`, `netbox_ipam_write_lock_wait_seconds`) are served on the diagnostics port behind authentication and authorization, as in Cluster API.

Tenancy. Anyone who may create a `NetBoxNetwork` in a namespace may carve from any parent prefix the token can write to and adopt deprecated prefixes of the same namespace. NetBox constraints bound what the token can reach; an admin-scoped parent object is future work.

Transport. The provider trusts the system roots plus an optional CA file. `http://` and skipping certificate verification are logged as warnings and meant for development.

### Risks and Mitigations

- **Retained prefixes fill the parent.** A deprecated prefix still occupies its space for `available-prefixes`; so do retired ones (`<ref>/retired`). Mitigation: documented; cleanup of retained objects by the NetBox admin with `hack/netbox-cleanup-retained.sh`, which deletes each object conditionally on its ETag; a report or GC is future work.
- **NetBox checks overlap and uniqueness without a lock.** Two parallel range creates, or a plain create next to an `available-*` request, can both pass. Mitigation: the provider runs its check-then-write sections (range create, resize and release with the fence, prefix adopt and carve, address allocation and restore) one at a time in its process, and leader election makes that process the only writer of its zone. Management planes of other instances that share a parent are not covered, and neither is a NetBox behind PgBouncer in transaction pooling, which does not hold the advisory lock of `available-*`.
- **Pool to Cluster link is a label, or a network.** A pool pauses with the Cluster of its label and of any of its networks; a pool shared by several clusters needs the paused annotation for a move. Mitigation: documented.
- **An address freed while the machine uses it.** ipamutil calls the release as soon as the claim is deleted, and a deleted namespace or a claim deleted by hand would free the address of a live VM. Mitigation: the release waits for finalizers of others (CAPV's `ip-claim-protection`), and `addressReuseGracePeriodSeconds` delays the reuse; neither applies to a claim deleted for `clusterctl move`.
- **NetBox outage.** The provider starts without NetBox, not ready, and retries the check with a back-off of at most a minute. While running, allocated claims stay `Ready`; new allocations and changes wait and are retried. Calls to NetBox are visible in the metrics, changes as Events.
- **Shared NetBox between management planes.** Protected only by the uniqueness of the key: every plane has its own instance name, kept unique by the user and equal through a move. Two planes with the same name take each other's objects; the provider cannot see it. Mitigation: documented; the permissions of the token bound what a plane can reach.
- **A changed instance name or key field is a new zone.** The objects with the old key are not found: a network with a status reports `PrefixNotFound` and its claims `AddressConflict`, one without a status carves a new prefix. Mitigation: documented; the keys are rewritten in NetBox before the setting changes.
- **Owner and tenant follow the spec.** A changed `spec.owner` or `spec.tenant` is synced to the existing objects at once, and a value edited in NetBox by hand is overwritten. Mitigation: documented; an unset field is never written nor cleared, and a prefix under `spec.prefix` is never synced.

## Alternatives

- **In-cluster IPAM plus a sync to NetBox.** Two sources of truth; no carving of subnets; addresses taken in NetBox are not seen by the allocator.
- **A generic NetBox operator with its own claim kinds.** It does not implement the Cluster API IPAM contract and would need a bridge controller between claims of both kinds.
- **Marking objects with the Owner, tags or descriptions instead of a key.** An Owner in the identity ties the provider to an Owner object per management plane. NetBox permissions can be constrained by owner or tenant to bound the token, but identity is simpler as one rule, as in netbox-operator: the object whose custom field holds our key. Tags give no uniqueness; the built-in `description` is fragile and clashes with the templates of descriptions.
- **The generated Go client for NetBox.** go-netbox v4.3 does not cover Owners and ETags; a thin client on `net/http` covers the few endpoints used.
- **Writing the VIP into `InfraCluster.spec.controlPlaneEndpoint`.** Needs patch rights on every infrastructure kind and makes the provider a second controller of foreign objects.
- **A VIP from a pool range or a claim.** The VIP would be an address of a pool, handed out with the machines; reserving it in the cluster prefix before any pool keeps it out of the ranges.
- **Admission webhooks.** CEL covers field validation without certificates and an extra endpoint.

## Upgrade Strategy

The API is `v1alpha1`: fields can change between minor releases, without conversion. `clusterctl upgrade` replaces the CRDs and the Deployment; the `NETBOX_*` variables of `clusterctl init` are given again. There is no migration of keys between fields or instance names. The clusterctl metadata maps release series 0.1 to contract v1beta2.

## Additional Details

### Test Plan

- Unit tests with `httptest` for the NetBox client (pagination included), preflight, metadata merge and address math.
- CRD validation tests on envtest.
- Controller tests on envtest against a real NetBox (docker compose) in CI on 4.6.7 and 4.7.2, including deletion without the delete permission, objects deleted in NetBox by hand, blocked shrinks, missing tags, custom fields that NetBox returns in another form, objects with another key, owner and tenant sync and `clusterctl move`. A second mode of the suite runs on 4.6.7: every key starts with the instance `ci` (`NETBOX_TEST_MODE=instance`).
- A shell test of the control-plane VIP command on the kubeadm configs of CAPI v1.14 (cloud-init and Ignition, init and join), run by `make test` with bash, sed, awk and grep only.
- Future: end-to-end tests with the Cluster API test framework, an infrastructure provider simulator, `clusterctl move` between two management clusters.

### Version Skew Strategy

The provider exits at start against NetBox older than 4.6 (tested with 4.6.7 and 4.7.2). It follows the Cluster API contract v1beta2 (tested with Cluster API v1.14.3).

## Future Work

1. **Control-plane endpoint with ClusterClass.** The VIP is written to the Cluster the same way for a topology cluster, but only plain clusters are tested.
2. **Address enrichment from the Machine.** Fill `dns_name` and `description` from the Machine (claim → infrastructure machine → Machine by owner references) instead of the claim's first owner, and create no VM and interface objects.
3. **Migration with `spec.importFrom`.** Record the addresses already allocated by another IPAM pool in NetBox under the keys of their claims, so running clusters switch without re-addressing.
4. **Admin-scoped parent.** A cluster-scoped `NetBoxParentNetwork` with prefix, gateway, VRF, defaults and `allowedNamespaces`, referenced by `NetBoxNetwork.spec.parentRef`; the gateway is set once and `MixedNetworks` becomes a check of the same reference.
5. Further: IPv6 and VRFs, a report or GC of retained objects.

## Implementation History

- 2026-10-06: design.
- 2026-10-11: v0.1.0.
