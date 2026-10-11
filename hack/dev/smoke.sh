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

# Applies the sample cluster and shows what NetBox handed out. Needs a running provider (make run or make deploy-dev).
set -euo pipefail
cd "$(dirname "$0")/../.."
export KUBECONFIG=$PWD/hack/dev/kubeconfig
set -a; source hack/dev/.env; set +a

kubectl apply -f config/samples/sample.yaml
kubectl -n sample wait ipaddressclaim/sample-claim --for=jsonpath='{.status.addressRef.name}'=sample-claim --timeout=90s
kubectl -n sample get ipaddresses.ipam.cluster.x-k8s.io sample-claim -o jsonpath='{.spec.address}/{.spec.prefix} gw {.spec.gateway}{"\n"}'
kubectl -n sample get netboxnetwork,netboxippool

id=$(kubectl -n sample get netboxnetwork sample-default -o jsonpath='{.status.prefixID}')
curl -sf -H "Authorization: Bearer $NETBOX_TOKEN" "$NETBOX_URL/api/ipam/prefixes/$id/" \
  | jq -c '{prefix, status: .status.value, description, tags: [.tags[].slug], custom_fields}'
