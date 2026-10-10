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

# Deletes the deprecated NetBox objects a cluster left under deletionPolicy: Retain:
# the control-plane VIP, the pool IP ranges and the prefix. Lists them unless --yes is given.
# --namespace keeps only objects whose capi_ref is in that namespace: same-named clusters share capi_cluster.
# Needs bash, curl, jq and a token that may delete them.
set -euo pipefail

usage() {
  echo "usage: NETBOX_URL=<url> NETBOX_TOKEN=<token> NETBOX_OWNER=<owner> $0 <cluster> [--namespace <ns>] [--yes]" >&2
  exit 2
}

cluster='' yes='' ns=''
while [ $# -gt 0 ]; do
  case $1 in
    --yes) yes=1 ;;
    --namespace) [ $# -gt 1 ] && [ -n "$2" ] || usage; ns=$2; shift ;;
    -*) usage ;;
    *) [ -z "$cluster" ] || usage; cluster=$1 ;;
  esac
  shift
done
[ -n "$cluster" ] && [ -n "${NETBOX_URL:-}" ] && [ -n "${NETBOX_TOKEN:-}" ] && [ -n "${NETBOX_OWNER:-}" ] || usage

api() {
  local method=$1 path=$2
  shift 2
  printf 'Authorization: Bearer %s\n' "$NETBOX_TOKEN" \
    | curl -sS -X "$method" -H @- -H 'Accept: application/json' "$@" "${NETBOX_URL%/}/api/$path"
}

# Lists "<type> <id> <address|prefix|range> <ref>" of the cluster's deprecated objects, tab separated.
# The server filters may match loosely, so everything is checked again here.
list() {
  local off=0 page count len
  while :; do
    page=$(api GET "ipam/$1/" --fail-with-body -G \
      --data-urlencode "cf_capi_cluster=$cluster" --data-urlencode "owner_id=$owner" \
      --data-urlencode status=deprecated --data-urlencode limit=1000 --data-urlencode "offset=$off") || return 1
    count=$(jq -e .count <<<"$page") || { echo "unexpected NetBox response listing $1" >&2; return 1; }
    len=$(jq '.results | length' <<<"$page") || return 1
    jq -r --arg t "$1" --arg c "$cluster" --arg ns "$ns" --argjson o "$owner" '.results[]
      | select(.custom_fields.capi_cluster == $c and .status.value == "deprecated" and .owner.id == $o
          and ($t != "ip-addresses" or .role.value == "vip")
          and ($ns == "" or ((.custom_fields.capi_ref // "") | split("/"))[1] == $ns))
      | [$t, .id, (.address // .prefix // "\(.start_address)-\(.end_address)"), .custom_fields.capi_ref] | @tsv' <<<"$page" || return 1
    off=$((off + len))
    [ "$len" -gt 0 ] && [ "$off" -lt "$count" ] || return 0
  done
}

remove() {
  local body
  body=$(jq -nc --arg m "capi: retained objects of $cluster cleaned up" '{changelog_message: $m}')
  api DELETE "ipam/$1/$2/" --fail-with-body -H 'Content-Type: application/json' --data "$body" \
    || { echo "DELETE $1/$2 failed" >&2; return 1; }
}

res=$(api GET users/owners/ --fail-with-body -G --data-urlencode "name=$NETBOX_OWNER")
owner=$(jq -r --arg n "$NETBOX_OWNER" '[.results[] | select(.name == $n)][0].id // empty' <<<"$res")
[ -n "$owner" ] || { echo "owner $NETBOX_OWNER not found" >&2; exit 1; }

found=''
for t in ip-addresses ip-ranges prefixes; do
  out=$(list "$t")
  [ -z "$out" ] || found+=$out$'\n'
done

n=0
while IFS=$'\t' read -r type id what ref; do
  [ -n "$type" ] || continue
  n=$((n + 1))
  if [ -n "$yes" ]; then
    remove "$type" "$id"
    echo "deleted $type $id $what $ref"
  else
    echo "$type $id $what $ref"
  fi
done <<<"$found"

if [ "$n" = 0 ]; then
  echo "no retained objects of cluster $cluster${ns:+ in namespace $ns}"
elif [ -z "$yes" ]; then
  echo "$n object(s) would be deleted; run again with --yes to delete them"
else
  echo "$n object(s) deleted"
fi
