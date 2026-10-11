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

# Runs the preKubeadmCommands command of docs/examples/control-plane-vip.yaml on kubeadm configs shaped as
# Cluster API v1.14.3 renders them (bootstrap/kubeadm/pkg/types/upstreamv1beta4: ClusterConfiguration,
# InitConfiguration, JoinConfiguration), and checks that the placeholders of the load balancer file are replaced.
#   cloud-init: bootstrap/kubeadm/pkg/cloudinit, /run/kubeadm/kubeadm.yaml (controlplane_init.go: cluster and init
#               configuration, controlPlaneEndpoint) and /run/kubeadm/kubeadm-join-config.yaml (controlplane_join.go:
#               join configuration, discovery.bootstrapToken.apiServerEndpoint)
#   Ignition:   bootstrap/kubeadm/pkg/ignition/clc, the same documents in /etc/kubeadm.yml
# Needs bash, sed, awk and grep only.
set -o errexit
set -o nounset
set -o pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
example=$root/docs/examples/control-plane-vip.yaml
[ -f "$example" ] || { echo "$example not found" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# The command is the folded scalar of the single "- >-" entry: its lines joined with a space.
cmd=$(awk '/^ *- >-$/ {on = 1; next} on && /^ +[^ ]/ {sub(/^ +/, ""); s = s (s == "" ? "" : " ") $0} END {print s}' "$example")
case $cmd in
  "f=/etc/kubernetes/manifests/kube-vip.yaml; "*) ;;
  *) echo "the command must start with f=/etc/kubernetes/manifests/kube-vip.yaml;" >&2; exit 1 ;;
esac
cmd=$(printf '%s' "$cmd" | sed -e "s#/run/kubeadm/#$tmp/run/kubeadm/#g" -e "s#/etc/kubeadm.yml#$tmp/etc/kubeadm.yml#g" -e "s#^f=[^;]*;#f=$tmp/lb.conf;#")
case $cmd in
  *"$tmp/lb.conf"*"$tmp/run/kubeadm/kubeadm.yaml"*"$tmp/run/kubeadm/kubeadm-join-config.yaml"*"$tmp/etc/kubeadm.yml"*) ;;
  *) echo "the command does not read /run/kubeadm/kubeadm.yaml, /run/kubeadm/kubeadm-join-config.yaml and /etc/kubeadm.yml" >&2; exit 1 ;;
esac

# The command runs on Linux nodes with GNU sed; with BSD sed, -i takes a suffix.
if ! sed --version >/dev/null 2>&1; then
  real=$(command -v sed)
  mkdir "$tmp/bin"
  # shellcheck disable=SC2016 # the shim expands them
  printf '#!/bin/sh\nif [ "$1" = -i ]; then shift; exec %s -i "" "$@"; fi\nexec %s "$@"\n' "$real" "$real" >"$tmp/bin/sed"
  chmod +x "$tmp/bin/sed"
  export PATH=$tmp/bin:$PATH
fi

# init_cfg ENDPOINT: ClusterConfiguration and InitConfiguration, the content of kubeadm.yaml and, with Ignition, /etc/kubeadm.yml.
# An empty ENDPOINT leaves the field out.
init_cfg() {
  cat <<EOF
---
apiServer: {}
apiVersion: kubeadm.k8s.io/v1beta4
clusterName: demo
${1:+controlPlaneEndpoint: $1}
controllerManager: {}
dns: {}
etcd: {}
kind: ClusterConfiguration
kubernetesVersion: v1.35.0
networking:
  dnsDomain: cluster.local
  podSubnet: 192.168.0.0/16
  serviceSubnet: 10.96.0.0/12
proxy: {}
scheduler: {}
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
localAPIEndpoint: {}
nodeRegistration:
  kubeletExtraArgs:
  - name: node-ip
    value: 10.10.0.2
EOF
}

# join_cfg ENDPOINT: JoinConfiguration of a control plane machine, the content of kubeadm-join-config.yaml and,
# with Ignition, /etc/kubeadm.yml.
join_cfg() {
  cat <<EOF
apiVersion: kubeadm.k8s.io/v1beta4
caCertPath: /etc/kubernetes/pki/ca.crt
controlPlane:
  localAPIEndpoint: {}
discovery:
  bootstrapToken:
${1:+    apiServerEndpoint: $1}
    caCertHashes:
    - sha256:0000000000000000000000000000000000000000000000000000000000000000
    token: abcdef.0123456789abcdef
kind: JoinConfiguration
nodeRegistration:
  kubeletExtraArgs:
  - name: node-ip
    value: 10.10.0.3
timeouts:
  discovery: 5m0s
EOF
}

failed=0 total=0

# check NAME FILE CONFIG HOST PORT: both shells that run the command (cloud-init: sh, Ignition: bash -e) must
# replace the placeholders; an empty HOST means the command must fail and leave the file as it is.
check() {
  local name=$1 file=$2 config=$3 host=$4 port=$5 sh out rc want
  for sh in sh bash; do
    total=$((total + 1))
    rm -rf "${tmp:?}/run" "${tmp:?}/etc"
    mkdir -p "$tmp/run/kubeadm" "$tmp/etc"
    [ -z "$file" ] || printf '%s\n' "$config" >"$tmp/$file"
    printf 'address: __CONTROL_PLANE_ENDPOINT__\nport: "__CONTROL_PLANE_PORT__"\nkeep: __KEEP__\n' >"$tmp/lb.conf"
    rc=0
    out=$("$sh" -ec "$cmd" 2>&1) || rc=$?
    if [ -n "$host" ]; then
      want=$(printf 'address: %s\nport: "%s"\nkeep: __KEEP__' "$host" "$port")
      if [ "$rc" != 0 ] || [ "$(cat "$tmp/lb.conf")" != "$want" ]; then
        echo "FAIL $name ($sh): exit $rc, want $host:$port" >&2
        echo "$out" >&2
        cat "$tmp/lb.conf" >&2
        failed=$((failed + 1))
      fi
    else
      want=$(printf 'address: __CONTROL_PLANE_ENDPOINT__\nport: "__CONTROL_PLANE_PORT__"\nkeep: __KEEP__')
      if [ "$rc" = 0 ] || [ "$(cat "$tmp/lb.conf")" != "$want" ] || ! grep -q "no control plane endpoint" <<<"$out"; then
        echo "FAIL $name ($sh): want an error and the file unchanged, exit $rc" >&2
        echo "$out" >&2
        failed=$((failed + 1))
      fi
    fi
  done
}

ep=10.10.0.127:6443
check "cloud-init init"   run/kubeadm/kubeadm.yaml             "$(init_cfg "$ep")"            10.10.0.127 6443
check "cloud-init join"   run/kubeadm/kubeadm-join-config.yaml "$(join_cfg "$ep")"            10.10.0.127 6443
check "Ignition init"     etc/kubeadm.yml                      "$(init_cfg "$ep")"            10.10.0.127 6443
check "Ignition join"     etc/kubeadm.yml                      "$(join_cfg "$ep")"            10.10.0.127 6443
check "port 443, init"    run/kubeadm/kubeadm.yaml             "$(init_cfg 10.10.0.127:443)"  10.10.0.127 443
check "port 443, join"    run/kubeadm/kubeadm-join-config.yaml "$(join_cfg 10.10.0.127:443)"  10.10.0.127 443
check "quoted endpoint"   run/kubeadm/kubeadm-join-config.yaml "$(join_cfg "\"$ep\"")"        10.10.0.127 6443
check "no endpoint, init" run/kubeadm/kubeadm.yaml             "$(init_cfg "")"               "" ""
check "no endpoint, join" run/kubeadm/kubeadm-join-config.yaml "$(join_cfg "")"               "" ""
check "no config file"    ""                                   ""                             "" ""

if [ "$failed" != 0 ]; then
  echo "control-plane VIP command: $failed of $total checks failed" >&2
  exit 1
fi
echo "control-plane VIP command: $total checks passed"
