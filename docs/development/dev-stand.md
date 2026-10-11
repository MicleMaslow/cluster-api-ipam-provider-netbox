# Dev stand

Goal: a local NetBox, a kind management cluster with Cluster API, the provider, and a CAPV cluster on a simulated vCenter, to develop the provider and to see a change work. Contributions: see [CONTRIBUTING](../../CONTRIBUTING.md).

You need docker, kind, kubectl, go and jq, and `yq` for the CAPV cluster. Tools the Makefile needs (controller-gen, kustomize, clusterctl and the like) are installed into `hack/tools/bin`. The passwords, keys and tokens in `hack/dev` are for the local dev stand only.

## Steps

```bash
make dev-all
```

`make dev-all` runs the steps below one after the other, and can be repeated on a stand that is up. Or run them yourself:

| Target | What it does |
|---|---|
| `make dev-up` | `netbox-up` and `kind-up`: NetBox 4.6.7 on 127.0.0.1:18000 in the compose project `capi-netbox-dev`, seeded; the kind cluster `capi-netbox-dev` with Cluster API core, kubeconfig in `hack/dev/kubeconfig` |
| `make deploy-dev` | builds the image, loads it into kind, connects the NetBox container to the kind docker network, and installs the provider from `out/ipam-components.yaml` through clusterctl, with the variables of `hack/dev/.env` and NetBox reached as `http://capi-netbox-dev-netbox-1:8080` |
| `make vcsim-up` | CAPV and the vcsim controller of CAPV's test infrastructure |
| `make vcsim-cluster NAME=vc` | the network `vc`, the pools `vc-cp` and `vc-workers`, and a cluster from CAPV's node-ipam template; `dev-all` creates `vc` with `OWNER=dev-team TENANT=dev-tenant` and `vc2` without, both with `NODE_DOMAIN=nodes.test` |
| `make vcsim-delete NAME=vc` | deletes that cluster with its NetBox objects, see below. `NAME` is required: unlike `vcsim-cluster`, it has no default |
| `make dev-down` | `kind-down` and `netbox-down`: deletes the kind cluster, the NetBox containers and data, and `hack/dev/.env`. Removes the whole stand |

`netbox-up`, `netbox-down`, `kind-up` and `kind-down` run on their own as well; `make help` lists every target.

NetBox is at http://localhost:18000, bound to 127.0.0.1, user `admin`, password `admin`. The seed writes `hack/dev/.env` (`NETBOX_URL`, the operator token `NETBOX_TOKEN`, the superuser token `NETBOX_ADMIN_TOKEN`, the instance `NETBOX_INSTANCE=dev`); the Makefile reads it. kubectl against the kind cluster: `export KUBECONFIG=$PWD/hack/dev/kubeconfig`.

## What the seed creates

`hack/dev/seed.sh` sets NetBox up through its API, and is a reference for what [NetBox access](../getting-started/netbox-access.md) asks of an admin. On a NetBox it has seeded it only adds the custom fields missing from `custom-fields.local.json`. If NetBox is seeded but `hack/dev/.env` is gone, it stops: run `make netbox-down netbox-up`.

| What | Created |
|---|---|
| Tokens | v2 tokens: the superuser `nbt_devadmin0001...` (`NETBOX_ADMIN_TOKEN`, for the tests and the cleanup script) and the one of the user `capi-operator` (`NETBOX_TOKEN`, what the provider uses) |
| Owner groups | `zones`, whose owners the operator may write (the tests make an owner and a tenant of their own per test), and `capi`, read-only for the operator (created last: it marks a NetBox as seeded) |
| Prefixes | containers `10.0.0.0/8`, `10.10.0.0/16`, `10.20.0.0/16`; parents `10.10.0.0/22` and `10.20.0.0/22`, each with a reserved child `/26` and the gateway address `.1/22` |
| Custom fields | `capi_ref` (text, `filter_logic: exact`) on prefixes, IP ranges and IP addresses; for the metadata examples on prefixes `labels` (JSON), `team`, `priority`, `monitored`, `site_tier` (choice set `site-tiers`) and `vlan` (the VLAN `dev-vlan`); more from `hack/dev/custom-fields.local.json` |
| Tags | `capi`, `k8s` |
| Owner and tenant | the owner `dev-team` of the group `zones` and the tenant `dev-tenant`, for the cluster `vc` |

The operator is an ordinary user, not a superuser. Its permissions:

| Permission | Actions | Object types | Constraint |
|---|---|---|---|
| `capi-view` | view | prefixes, IP ranges, IP addresses, owners, tenants, custom fields, tags | none |
| `capi-write` | add, change, delete | prefixes, IP ranges, IP addresses | no owner |
| `capi-write-zones` | add, change, delete | the same | owners of the group `zones` |

NetBox adds permissions up, so the operator writes what the provider creates without an owner (a network without `spec.owner`) and the objects of owners of `zones`; those of owners of `capi` are read-only for it, which the tests of `Forbidden` use. Dev may delete prefixes (`deletionPolicy: Delete`); production tokens should not ([NetBox access](../getting-started/netbox-access.md#permissions)). `hack/dev/vcsim-network.local.yaml` and `custom-fields.local.json` are git-ignored files of your own.

## A second NetBox (4.7.2)

The dev NetBox is 4.6.7; CI runs the integration tests on 4.6.7 and 4.7.2. Run another version as a compose project of its own, so that the dev data stays, and never switch the image of the dev project in place:

| Variable | Default | Meaning |
|---|---|---|
| `NETBOX_IMAGE` | `docker.io/netboxcommunity/netbox:v4.6.7-5.0.2` | the NetBox image of `hack/dev/compose.yaml` |
| `NETBOX_PORT` | `18000` | the host port, bound to 127.0.0.1 |
| `NETBOX_ENV_FILE` | `hack/dev/.env` | the file `hack/dev/seed.sh` writes; with `NETBOX_URL`, the NetBox it seeds. A relative path resolves against `hack/dev`, because the seed changes into it: give an absolute one |
| `DEV_ENV_FILE` | `hack/dev/.env` | the file `make test-integration`, `make run` and `make deploy-dev` read |

```bash
NETBOX_IMAGE=docker.io/netboxcommunity/netbox:v4.7.2-5.1.1 NETBOX_PORT=18001 \
  docker compose -p capi-netbox-47 -f hack/dev/compose.yaml up -d --wait
NETBOX_URL=http://localhost:18001 NETBOX_ENV_FILE=/tmp/netbox-47.env hack/dev/seed.sh
make test-integration DEV_ENV_FILE=/tmp/netbox-47.env
docker compose -p capi-netbox-47 -f hack/dev/compose.yaml down -v
```

Set the variables on the command, as above, and do not `export` them: in a shell where `NETBOX_IMAGE` stays set, a later `make netbox-up` or `make dev-up` would recreate the dev NetBox on that image over its 4.6.7 database.

`--wait` takes up to five minutes while NetBox starts. Run one integration run at a time. The suite has a second mode, `make test-integration NETBOX_TEST_MODE=instance`: every key starts with the instance `ci` ([Testing](testing.md#the-two-key-modes)); CI runs it on 4.6.7.

## Run the provider locally

```bash
make run            # provider against hack/dev/kubeconfig and the dev NetBox, with the token of hack/dev/.env
make smoke          # in another terminal: sample cluster, network, pool and claim
make smoke-clean
```

`make run` installs the CRDs first. Stop it before `make deploy-dev`.

## Run inside kind

```bash
make deploy-dev     # image IMG + clusterctl components, NetBox reached over the kind docker network
make smoke
```

`make release-manifests` writes `out/ipam-components.yaml` with the image `IMG` and `out/metadata.yaml`. The variables come from `DEV_ENV_FILE`: `NETBOX_URL` (replaced by the container address), `NETBOX_TOKEN`, and any of the optional ones of [Configuration](../operations/configuration.md).

**The instance.** The stand runs with the instance `dev` (`NETBOX_INSTANCE` of `hack/dev/.env`, used by `make run`, `make deploy-dev` and `make vcsim-delete`): its keys start with `dev/`. The integration tests do not read it, so their keys stay apart. To try another name, deploy with an env file of your own, a copy of `hack/dev/.env` with

```
NETBOX_INSTANCE=zone-a
```

and run `make deploy-dev DEV_ENV_FILE=<file>`. The provider then does not find the objects made under the other name ([The instance name](../getting-started/netbox-access.md#the-instance-name)): delete the stand's clusters before you switch (`NETBOX_INSTANCE=zone-a make vcsim-delete NAME=<name>` cleans a cluster of that instance), then switch back with a plain `make deploy-dev`.

**An installation the way users have it.** To install a release build with `clusterctl init` from the local repository (the same steps as [Install](../getting-started/install.md#a-build-of-your-own)), replace the deploy-dev install:

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

## CAPV on vcsim

A CAPV workload cluster on a simulated vCenter, with machine addresses from NetBox:

```bash
make vcsim-up                # CAPV and the vcsim controller of CAPV's test infrastructure
make vcsim-cluster NAME=vc   # NetBoxNetwork vc, pools vc-cp and vc-workers, and a cluster from CAPV's node-ipam template
```

`make dev-all` creates two clusters: `vc`, whose network has the owner `dev-team` and the tenant `dev-tenant`, and `vc2` without them.

`vcsim-up` builds the vcsim controller image from the CAPV sources (`CAPV_VERSION`, default v1.17.0, cloned into `~/.cache/capi-ipam-netbox`) and installs CAPV with clusterctl. The machines get their addresses through `addressesFromPools`, control plane from `<NAME>-cp`, workers from `<NAME>-workers`, and CAPV writes them into the VM metadata in vcsim. The workload cluster itself is fake: the vcsim controller simulates the machine bootstrap and the API server behind the control plane endpoint, and without cloud-init the VMs report the address 192.0.2.100.

| Variable | Default | Meaning |
|---|---|---|
| `NAME` | `vc` | the cluster, its namespace, network and pools |
| `CP_MACHINES`, `WORKER_MACHINES` | 1, 2 | machine counts |
| `K8S_VERSION` | `v1.37.0` | Kubernetes version of the machines |
| `PARENT_PREFIX`, `GATEWAY`, `PREFIX_LENGTH` | `10.10.0.0/22`, `10.10.0.1`, `26` | the network |
| `NODE_DOMAIN` | empty | makes the `dns_name` of the node addresses `<node>.<NODE_DOMAIN>` |
| `OWNER`, `TENANT` | empty | `spec.owner.name` and `spec.tenant.slug` of the network; empty: left out |
| `NETWORK_PATCH` | `hack/dev/vcsim-network.local.yaml`, else `hack/dev/vcsim-network.example.yaml` | a merge patch of the NetBoxNetwork (tags and custom fields), `${CLUSTER_NAME}` substituted |
| `CAPV_VERSION`, `CAPV_SRC` | `v1.17.0`, `~/.cache/capi-ipam-netbox/capv-<version>` | the CAPV sources for `vcsim-up` |

The control plane endpoint is the fake API server of vcsim, so the network has no `controlPlaneEndpoint`; the VIP is covered by the integration tests ([Testing](testing.md)). Deleting only the Cluster releases its addresses; the network and the pools stay.

### Delete a cluster

```bash
make vcsim-delete NAME=vc
make vcsim-delete NAME=vc2
```

`DELETE_TIMEOUT` (seconds, default 300) bounds each wait. In order, the script:

1. refuses a namespace that is not a workload one: `default`, `kube-*`, `*-system`, `cert-manager`, `local-path-storage`;
2. needs `NETBOX_ADMIN_TOKEN` in `hack/dev/.env`;
3. deletes the Cluster and waits until it is gone;
4. deletes the pools and the network of the namespace;
5. runs the [cleanup script](../recipes/cleanup.md) with the admin token for the namespace, and with `--instance` when `NETBOX_INSTANCE` is set in the environment or in `hack/dev/.env`: the retained prefix and ranges leave NetBox;
6. deletes the vcsim endpoint and the namespace.

A failed wait prints what it waited for and stops; fix it and run the command again. After the vcsim controller pod is recreated (a docker or kind restart), the VSphereVMs of existing clusters keep the finalizer `vcsim.fake.infrastructure.cluster.x-k8s.io`: remove it by hand. With another key field than `capi_ref` the cleanup finds nothing: remove the retained objects as in [Cleanup](../recipes/cleanup.md).

## Develop with Tilt

The provider runs in the Tilt setup of a [cluster-api](https://github.com/kubernetes-sigs/cluster-api) checkout (`make tilt-up`, see its Tilt docs): `tilt-provider.yaml` describes the build, `clusterctl-settings.json` serves `create-local-repository.py`. Add the provider to `tilt-settings.yaml` in the cluster-api checkout; Cluster API core is always enabled:

```yaml
provider_repos:
- ../cluster-api-ipam-provider-netbox
enable_providers:
- ipam-netbox
kustomize_substitutions:
  NETBOX_URL: http://capi-netbox-dev-netbox-1:8080
  NETBOX_TOKEN: nbt_<key>.<token>
```

Tilt substitutes the placeholders of the manifests from `kustomize_substitutions`. `NETBOX_URL` and `NETBOX_TOKEN` are required; `NETBOX_TOKEN` is the token of the dev stand (`hack/dev/.env`), and real tokens stay out of `tilt-settings.yaml`. The optional variables (`NETBOX_INSTANCE`, `NETBOX_REF_FIELD`) can be left out: Tilt renders the `${VAR:= }` forms of the manifests the way clusterctl does, blank for an unset one.

The dev NetBox (`make netbox-up`) listens on 127.0.0.1 only. Attach it to the docker network of the Tilt kind cluster, as `make deploy-dev` does for `kind`, so that the pod reaches it by container name:

```bash
docker network connect kind capi-netbox-dev-netbox-1
```

Tilt rebuilds the manager and restarts it on changes of `main.go`, `go.mod`, `go.sum`, `api` and `internal`. The image in `tilt-provider.yaml` is the one set by `config/default/kustomization.yaml`; change both together.

## Check

```bash
export KUBECONFIG=$PWD/hack/dev/kubeconfig
kubectl -n capi-ipam-netbox-system rollout status deployment/capi-ipam-netbox-controller-manager
kubectl get netboxnetworks,netboxippools -A
```

After `make dev-all` the clusters `vc` and `vc2` are `Available` with all their machines `Running`, their networks and pools are `Ready`, the machines have addresses, and NetBox at http://localhost:18000 shows the prefix, the IP ranges and the addresses of each cluster with the key `dev/...` in `capi_ref`, a description and comments; those of `vc` with the owner `dev-team` and the tenant `dev-tenant`.

## Next

- [Testing](testing.md) and [Release](release.md).
- [Install](../getting-started/install.md) for a real installation.
