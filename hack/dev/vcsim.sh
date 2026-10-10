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

# CAPV against vcsim (a simulated vCenter) on the dev kind cluster; machines take their addresses from NetBoxIPPools.
#   vcsim.sh up              install CAPV and the vcsim controller of CAPV's test infrastructure
#   vcsim.sh cluster NAME    create the workload cluster NAME in the namespace NAME
#                            (CONTROL_PLANE_ENDPOINT=1: its VIP comes from the NetBoxNetwork)
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
  local name=$1 fields="" endpoint="" template_fix="."
  # NODE_DOMAIN makes the NetBox dns_name of a node its FQDN.
  [ -z "${NODE_DOMAIN:-}" ] || fields=", ipAddress: {dnsName: \"{{ .Owner }}.$NODE_DOMAIN\"}"
  # CONTROL_PLANE_ENDPOINT=1: the network reserves the VIP and sets the endpoint of the Cluster, so the VSphereCluster has none.
  if [ "${CONTROL_PLANE_ENDPOINT:-}" = 1 ]; then
    endpoint=", controlPlaneEndpoint: {dnsName: $name-api${NODE_DOMAIN:+.$NODE_DOMAIN}}"
    template_fix='(select(.kind == "VSphereCluster") | .spec) |= del(.controlPlaneEndpoint)'
  fi
  kubectl create namespace "$name" --dry-run=client -o yaml | kubectl apply -f -
  kubectl apply -f - <<EOF
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxNetwork
metadata: {name: $name, namespace: $name}
spec: {clusterName: $name, parentPrefix: ${PARENT_PREFIX:-10.10.0.0/22}, gateway: ${GATEWAY:-10.10.0.1}, prefixLength: ${PREFIX_LENGTH:-26}$endpoint}
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: $name-cp, namespace: $name, labels: {cluster.x-k8s.io/cluster-name: $name}}
spec: {ranges: [{networkName: $name, start: 2, end: 7}]$fields}
---
apiVersion: ipam.cluster.x-k8s.io/v1alpha1
kind: NetBoxIPPool
metadata: {name: $name-workers, namespace: $name, labels: {cluster.x-k8s.io/cluster-name: $name}}
spec: {ranges: [{networkName: $name, start: 16, end: -3}]$fields}
EOF

  # Tags and custom fields of the cluster prefix.
  kubectl -n "$name" patch netboxnetwork "$name" --type merge --patch "$(sed "s/\${CLUSTER_NAME}/$name/g" "$NETWORK_PATCH")"

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
  export NODE_IPAM_POOL_API_GROUP=ipam.cluster.x-k8s.io NODE_IPAM_POOL_KIND=NetBoxIPPool NODE_IPAM_POOL_NAME=$name-workers

  # CPI and CSI addons are left out: the workload API server is fake.
  "$CLUSTERCTL" generate yaml --config hack/dev/clusterctl.yaml --from "$CAPV_SRC/templates/cluster-template-node-ipam.yaml" |
    yq 'select(.kind != "ClusterResourceSet" and .kind != "ConfigMap" and (.kind != "Secret" or .metadata.name == strenv(CLUSTER_NAME)))
      | (select(.kind == "VSphereMachineTemplate" and .metadata.name == strenv(CLUSTER_NAME))
         | .spec.template.spec.network.devices[0].addressesFromPools[0].name) = strenv(CLUSTER_NAME) + "-cp"
      | '"$template_fix" |
    kubectl apply -f -
}

case "${1:-}" in
  up) up ;;
  cluster) [ -n "${2:-}" ] || { echo "usage: $0 cluster NAME" >&2; exit 1; }; cluster "$2" ;;
  *) echo "usage: $0 up | cluster NAME" >&2; exit 1 ;;
esac
