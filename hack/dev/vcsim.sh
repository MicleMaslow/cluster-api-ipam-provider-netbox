#!/usr/bin/env bash

# Copyright 2026 The cluster-api-ipam-provider-netbox Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# CAPV against vcsim (a simulated vCenter) on the dev kind cluster; machines take their addresses from the pools of
# a NetBoxNetwork.
#   vcsim.sh up              install CAPV and the vcsim controller of CAPV's test infrastructure
#   vcsim.sh cluster NAME    create the workload cluster NAME in the namespace NAME; OWNER (owner name) and
#                            TENANT (tenant slug) set those of its network, METALLB the count of its address group
#                            metallb (published to the Secret NAME-endpoints); machine addresses from the NetBoxIPPools
#                            NAME-cp and NAME-workers, with ALLOCATOR=InCluster from the InClusterIPPools of those names
#                            (SYNC=1 syncs the addresses of NAME-workers to NetBox, dns_name <machine>.nodes.test);
#                            LEGACY=1: no network, the cluster as a chart left it (see legacy)
#   vcsim.sh adopt NAME      the network of a LEGACY cluster NAME, which takes it over
#   vcsim.sh delete NAME     delete the cluster NAME, its NetBox objects (retained ones with the admin token of
#                            hack/dev/.env, of the instance NETBOX_INSTANCE if set) and its namespace;
#                            DELETE_TIMEOUT seconds per wait (default 300)
set -euo pipefail
cd "$(dirname "$0")/../.."
export KUBECONFIG=$PWD/hack/dev/kubeconfig

CAPV_VERSION=${CAPV_VERSION:-v1.17.0}
CAPV_SRC=${CAPV_SRC:-${XDG_CACHE_HOME:-$HOME/.cache}/capi-ipam-netbox/capv-$CAPV_VERSION}
CLUSTERCTL=${CLUSTERCTL:-hack/tools/bin/clusterctl-v1.14.3}
KUSTOMIZE=${KUSTOMIZE:-hack/tools/bin/kustomize-v5.7.0}
KIND_CLUSTER=${KIND_CLUSTER:-capi-netbox-dev}
VCSIM_IMAGE=capv-vcsim-controller:$CAPV_VERSION
NETWORK_PATCH=${NETWORK_PATCH:-hack/dev/vcsim-network.local.yaml}
DELETE_TIMEOUT=${DELETE_TIMEOUT:-300}
[ -f "$NETWORK_PATCH" ] || NETWORK_PATCH=hack/dev/vcsim-network.example.yaml

up() {
  [ -d "$CAPV_SRC" ] || git clone -q --depth 1 --branch "$CAPV_VERSION" https://github.com/kubernetes-sigs/cluster-api-provider-vsphere.git "$CAPV_SRC"
  # vcsim controller images are not published.
  docker image inspect "$VCSIM_IMAGE" >/dev/null 2>&1 ||
    docker build --build-arg builder_image="golang:$(sed -n 's/^go \([0-9]*\.[0-9]*\).*/\1/p' "$CAPV_SRC/go.mod")" \
      --build-arg ARCH="$(go env GOARCH)" -f "$CAPV_SRC/test/infrastructure/vcsim/Dockerfile" -t "$VCSIM_IMAGE" "$CAPV_SRC"
  kind load docker-image "$VCSIM_IMAGE" --name "$KIND_CLUSTER"

  # Clusters get the vCenter credentials through identityRef; the manager's own are not used.
  kubectl -n capv-system get deployment capv-controller-manager >/dev/null 2>&1 ||
    VSPHERE_USERNAME=unused VSPHERE_PASSWORD=unused \
      "$CLUSTERCTL" init --config hack/dev/clusterctl.yaml --infrastructure "vsphere:$CAPV_VERSION" --wait-providers

  "$KUSTOMIZE" build "$CAPV_SRC/test/infrastructure/vcsim/config/default" 2>/dev/null |
    sed -e "s|image: gcr.io/k8s-staging-capi-vsphere/cluster-api-vcsim-controller:.*|image: $VCSIM_IMAGE|" \
      -e 's|imagePullPolicy: Always|imagePullPolicy: IfNotPresent|' |
    "$CLUSTERCTL" generate yaml --config hack/dev/clusterctl.yaml | kubectl apply -f -
  kubectl -n vcsim-system rollout status deployment/vcsim-controller-manager --timeout=180s
}

cluster() {
  local name=$1 fields="" attrs="" pools kind=NetBoxIPPool sync=""
  # NODE_DOMAIN makes the NetBox dns_name of a node its FQDN.
  [ -z "${NODE_DOMAIN:-}" ] || fields=", ipAddress: {dnsName: \"{{ .Owner }}.$NODE_DOMAIN\"}"
  [ -z "${OWNER:-}" ] || attrs+=", owner: {name: \"$OWNER\"}"
  [ -z "${TENANT:-}" ] || attrs+=", tenant: {slug: \"$TENANT\"}"
  [ -z "${SYNC:-}" ] || sync=", sync: {enabled: true, ipAddress: {dnsName: \"{{ .Owner }}.nodes.test\"}}"
  # The pools of an InCluster network are named as their InClusterIPPools.
  case ${ALLOCATOR:-NetBox} in
    NetBox)
      [ -z "$sync" ] || { echo "SYNC needs ALLOCATOR=InCluster" >&2; exit 1; }
      pools="{name: cp, size: 6}, {name: workers, size: rest}" ;;
    InCluster)
      pools="{name: $name-cp, size: 6}, {name: $name-workers, size: rest$sync}" kind=InClusterIPPool
      attrs+=", allocator: InCluster" ;;
    *) echo "ALLOCATOR must be NetBox or InCluster, got $ALLOCATOR" >&2; exit 1 ;;
  esac
  kubectl create namespace "$name" --dry-run=client -o yaml | kubectl apply -f -
  if [ -n "${LEGACY:-}" ]; then
    # Made once: an adopted pool is the network's.
    kind=InClusterIPPool
    kubectl -n "$name" get inclusterippool "$name-cp" >/dev/null 2>&1 || legacy "$name"
  else
    kubectl apply -f - <<EOF
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxNetwork
metadata: {name: $name, namespace: $name}
spec: {clusterName: $name, parentPrefix: ${PARENT_PREFIX:-10.10.0.0/22}, gateway: ${GATEWAY:-10.10.0.1}, prefixLength: ${PREFIX_LENGTH:-26},
  pools: [$pools], addresses: $(metallb "$name")$attrs}
EOF
    [ "$kind" = InClusterIPPool ] || kubectl apply -f - <<EOF
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: $name-cp, namespace: $name, labels: {cluster.x-k8s.io/cluster-name: $name}}
spec: {ranges: [{networkName: $name, pool: cp}]$fields}
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: $name-workers, namespace: $name, labels: {cluster.x-k8s.io/cluster-name: $name}}
spec: {ranges: [{networkName: $name, pool: workers}]$fields}
EOF

    # Tags and custom fields of the cluster prefix.
    kubectl -n "$name" patch netboxnetwork "$name" --type merge --patch "$(sed "s/\${CLUSTER_NAME}/$name/g" "$NETWORK_PATCH")"
  fi

  # One vcsim instance serves every cluster; each cluster gets its own fake API server endpoint.
  # The endpoint of a deleted cluster keeps serving its certificates, so a new cluster gets a new one.
  kubectl -n "$name" get cluster "$name" >/dev/null 2>&1 ||
    kubectl -n vcsim-system delete controlplaneendpoint,envvar "$name" --ignore-not-found --wait
  kubectl apply -f - <<EOF
apiVersion: vcsim.infrastructure.cluster.x-k8s.io/v1alpha1
kind: VCenterSimulator
metadata: {name: vcsim, namespace: vcsim-system}
---
apiVersion: vcsim.infrastructure.cluster.x-k8s.io/v1alpha1
kind: ControlPlaneEndpoint
metadata: {name: $name, namespace: vcsim-system}
---
apiVersion: vcsim.infrastructure.cluster.x-k8s.io/v1alpha1
kind: EnvVar
metadata: {name: $name, namespace: vcsim-system}
spec:
  vCenterSimulator: {name: vcsim}
  controlPlaneEndpoint: {name: $name}
  cluster: {name: $name, namespace: $name}
EOF
  kubectl -n vcsim-system wait envvar/"$name" --for=jsonpath='{.status.variables.CLUSTER_NAME}'="$name" --timeout=60s
  eval "$(kubectl -n vcsim-system get envvar "$name" -o json | jq -r '.status.variables | to_entries[] | "export \(.key)=\(.value | @sh)"')"

  # The default vcsim VM template is the newest one.
  export KUBERNETES_VERSION=${K8S_VERSION:-v1.37.0} CPI_IMAGE_K8S_VERSION=${K8S_VERSION:-v1.37.0} NAMESERVER=192.0.2.53
  export CONTROL_PLANE_MACHINE_COUNT=${CP_MACHINES:-1} WORKER_MACHINE_COUNT=${WORKER_MACHINES:-2}
  export NODE_IPAM_POOL_API_GROUP=ipam.cluster.x-k8s.io NODE_IPAM_POOL_KIND=$kind NODE_IPAM_POOL_NAME=$name-workers

  # CPI and CSI addons are left out: the workload API server is fake.
  "$CLUSTERCTL" generate yaml --config hack/dev/clusterctl.yaml --from "$CAPV_SRC/templates/cluster-template-node-ipam.yaml" |
    yq 'select(.kind != "ClusterResourceSet" and .kind != "ConfigMap" and (.kind != "Secret" or .metadata.name == strenv(CLUSTER_NAME)))
      | (select(.kind == "VSphereMachineTemplate" and .metadata.name == strenv(CLUSTER_NAME))
         | .spec.template.spec.network.devices[0].addressesFromPools[0].name) = strenv(CLUSTER_NAME) + "-cp"' |
    kubectl apply -f -
}

# env_var NAME: a value of hack/dev/.env, not exported.
env_var() (
  # shellcheck source=/dev/null
  . hack/dev/.env
  printf '%s' "${!1:-}"
)

# metallb NAME: spec.addresses of the network NAME.
metallb() {
  echo "[{name: metallb, count: ${METALLB:-2}, dnsName: \"{{ .Cluster }}-metallb-{{ .Index }}.example.test\",
    publish: {enabled: true, secretName: $1-endpoints}}]"
}

# nb METHOD PATH [curl args]: the NetBox API with the admin token of hack/dev/.env.
nb() {
  local method=$1 path=$2
  shift 2
  printf 'Authorization: Bearer %s\n' "$(env_var NETBOX_ADMIN_TOKEN)" |
    curl -sS --fail-with-body -X "$method" -H @- -H 'Accept: application/json' -H 'Content-Type: application/json' \
      "$@" "$(env_var NETBOX_URL)/api/$path"
}

# ip_int A.B.C.D and int_ip N: IPv4 address arithmetic.
ip_int() {
  local a b c d
  IFS=. read -r a b c d <<<"$1"
  echo $(((a << 24) + (b << 16) + (c << 8) + d))
}
int_ip() { echo "$(($1 >> 24 & 255)).$(($1 >> 16 & 255)).$(($1 >> 8 & 255)).$(($1 & 255))"; }

# legacy NAME: the cluster NAME as a chart left it, before any network. In NetBox, with the admin token and without a
# key: the prefix LEGACY_PREFIX (default 10.10.3.128/26), its MetalLB addresses at offsets -2 and -3 and, with
# LEGACY_TAKEN_IN_WORKERS=1, an address in the middle of the workers, which the network then excludes from them.
# The InClusterIPPools NAME-cp at offsets 2-7 and NAME-workers at 8 to -4, without a controller or excludedAddresses.
legacy() {
  local name=$1 prefix=${LEGACY_PREFIX:-10.10.3.128/26} mask=${PARENT_PREFIX:-10.10.0.0/22} first last ips a
  mask=${mask#*/} first=$(ip_int "${prefix%/*}")
  last=$((first + (1 << (32 - ${prefix#*/})) - 1))
  ips=("$(int_ip $((last - 1))) metallb" "$(int_ip $((last - 2))) metallb")
  [ -z "${LEGACY_TAKEN_IN_WORKERS:-}" ] || ips+=("$(int_ip $(((first + 8 + last - 3) / 2))) taken")
  echo "NetBox objects of $name without a key, which delete leaves:"
  nb POST ipam/prefixes/ --data "$(jq -nc --arg p "$prefix" --arg d "$name, as a chart left it" '{prefix: $p, description: $d}')" |
    jq -r '"prefix \(.id) \(.prefix)"'
  for a in "${ips[@]}"; do
    nb POST ipam/ip-addresses/ --data "$(jq -nc --arg a "${a% *}/$mask" --arg d "$name ${a#* }" '{address: $a, description: $d}')" |
      jq -r '"ip-address \(.id) \(.address)"'
  done
  kubectl apply -f - <<EOF
apiVersion: ipam.cluster.x-k8s.io/v1alpha2
kind: InClusterIPPool
metadata: {name: $name-cp, namespace: $name}
spec: {addresses: [$(int_ip $((first + 2)))-$(int_ip $((first + 7)))], prefix: $mask, gateway: ${GATEWAY:-10.10.0.1}}
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha2
kind: InClusterIPPool
metadata: {name: $name-workers, namespace: $name}
spec: {addresses: [$(int_ip $((first + 8)))-$(int_ip $((last - 3)))], prefix: $mask, gateway: ${GATEWAY:-10.10.0.1}}
EOF
}

# adopt NAME: the network of the LEGACY cluster NAME, which takes over its prefix, pools and MetalLB addresses:
# allocator InCluster, NAME-cp of 6, NAME-workers of the span of its InClusterIPPool less the NetBox addresses in it.
adopt() {
  local name=$1 prefix=${LEGACY_PREFIX:-10.10.3.128/26} span from to taken
  span=$(kubectl -n "$name" get inclusterippool "$name-workers" -o jsonpath='{.spec.addresses[0]}')
  from=$(ip_int "${span%-*}") to=$(ip_int "${span#*-}")
  taken=$(nb GET ipam/ip-addresses/ -G --data-urlencode "parent=$prefix" --data-urlencode limit=1000 |
    jq --argjson from "$from" --argjson to "$to" '[.results[].address | split("/")[0] | split(".") | map(tonumber)
      | .[0] * 16777216 + .[1] * 65536 + .[2] * 256 + .[3] | select(. >= $from and . <= $to)] | length')
  echo "$name-workers: $span less $taken NetBox address(es)"
  kubectl apply -f - <<EOF
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxNetwork
metadata: {name: $name, namespace: $name}
spec: {clusterName: $name, parentPrefix: ${PARENT_PREFIX:-10.10.0.0/22}, gateway: ${GATEWAY:-10.10.0.1}, prefix: $prefix,
  allocator: InCluster, pools: [{name: $name-cp, size: 6}, {name: $name-workers, size: $((to - from + 1 - taken))}],
  addresses: $(metallb "$name")}
EOF
}

delete() {
  local name=$1 end=$((SECONDS + DELETE_TIMEOUT)) cur token url instance cleanup=()
  case $name in
    default | kube-* | *-system | cert-manager | local-path-storage)
      echo "refusing to delete $name: not a workload namespace" >&2
      exit 1 ;;
  esac
  token=$(env_var NETBOX_ADMIN_TOKEN)
  [ -n "$token" ] || { echo "NETBOX_ADMIN_TOKEN is not set in hack/dev/.env" >&2; exit 1; }
  url=$(env_var NETBOX_URL)
  instance=${NETBOX_INSTANCE:-$(env_var NETBOX_INSTANCE)}
  cleanup=("$name" --yes)
  [ -z "$instance" ] || cleanup+=(--instance "$instance")

  echo "deleting cluster $name"
  kubectl -n "$name" delete cluster "$name" --ignore-not-found --wait=false
  while :; do
    cur=$(kubectl -n "$name" get cluster "$name" -o name --ignore-not-found) ||
      { echo "cannot read cluster $name: kubectl -n $name get cluster $name" >&2; exit 1; }
    [ -n "$cur" ] || break
    if [ "$SECONDS" -ge "$end" ]; then
      kubectl -n "$name" get cluster,machine,vspherevm >&2 || true
      echo "cluster $name is still there after ${DELETE_TIMEOUT}s" >&2
      exit 1
    fi
    sleep 3
  done

  echo "deleting pools and network of $name"
  kubectl -n "$name" delete netboxippool,netboxnetwork --all --ignore-not-found --timeout="${DELETE_TIMEOUT}s" ||
    { echo "pools or network of $name are not deleted after ${DELETE_TIMEOUT}s: kubectl -n $name get netboxippool,netboxnetwork" >&2; exit 1; }

  NETBOX_URL=$url NETBOX_TOKEN=$token hack/netbox-cleanup-retained.sh "${cleanup[@]}" ||
    { echo "NetBox cleanup of $name failed: fix it and run delete $name again" >&2; exit 1; }

  echo "deleting vcsim endpoint and namespace of $name"
  kubectl -n vcsim-system delete controlplaneendpoint,envvar "$name" --ignore-not-found --timeout="${DELETE_TIMEOUT}s" ||
    { echo "vcsim endpoint or envvar $name are not deleted after ${DELETE_TIMEOUT}s: kubectl -n vcsim-system get controlplaneendpoint,envvar $name" >&2; exit 1; }
  kubectl delete namespace "$name" --ignore-not-found --timeout="${DELETE_TIMEOUT}s" ||
    { echo "namespace $name is not deleted after ${DELETE_TIMEOUT}s: kubectl get ns $name -o yaml" >&2; exit 1; }
}

case "${1:-}" in
  up) up ;;
  cluster) [ -n "${2:-}" ] || { echo "usage: $0 cluster NAME" >&2; exit 1; }; cluster "$2" ;;
  adopt) [ -n "${2:-}" ] || { echo "usage: $0 adopt NAME" >&2; exit 1; }; adopt "$2" ;;
  delete) [ -n "${2:-}" ] || { echo "usage: $0 delete NAME" >&2; exit 1; }; delete "$2" ;;
  *) echo "usage: $0 up | cluster NAME | adopt NAME | delete NAME" >&2; exit 1 ;;
esac
