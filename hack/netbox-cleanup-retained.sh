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

# Deletes the deprecated NetBox objects the networks and pools of a namespace left under deletionPolicy: Retain:
# control-plane VIPs, pool IP ranges and prefixes, retired ones (key <key>/retired) included. Lists them unless --yes.
# An object is the namespace's when its key is [<instance>/]<Kind>/<namespace>/...
# Run it when the namespace has no networks left: a live network's adopted prefix kept deprecated is in scope too.
# --instance is the provider's instance name (NETBOX_INSTANCE); without it only keys without an instance match.
# --ref-field names the text custom field of the key, as NETBOX_REF_FIELD.
# If NetBox returns deprecated objects and none carries a key in --ref-field, it warns on stderr (a wrong --ref-field).
# Each delete is conditional on the ETag of the object read just before (NetBox checks it) and counts
# only with HTTP 204. An object that is no longer deprecated or no longer carries the listed key is skipped.
# Needs bash, curl, jq and a token that may delete them.
set -euo pipefail

usage() {
  echo "usage: NETBOX_URL=<url> NETBOX_TOKEN=<token> $0 <namespace> [--instance <name>] [--ref-field <field>] [--yes]" >&2
  exit 2
}

label='^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'
yes='' ns='' instance='' ref_field=capi_ref
while [ $# -gt 0 ]; do
  case $1 in
    --yes) yes=1 ;;
    --instance | --ref-field)
      case ${2-} in '' | --*) usage ;; esac
      case $1 in
        --instance) [[ $2 =~ $label ]] || usage; instance=$2 ;;
        --ref-field) [[ $2 =~ ^[A-Za-z0-9_]+$ && $2 != *__* ]] || usage; ref_field=$2 ;;
      esac
      shift ;;
    -*) usage ;;
    *) [ -z "$ns" ] && [[ $1 =~ $label ]] || usage; ns=$1 ;;
  esac
  shift
done
[ -n "$ns" ] && [ -n "${NETBOX_URL:-}" ] && [ -n "${NETBOX_TOKEN:-}" ] || usage

api() {
  local method=$1 path=$2
  shift 2
  printf 'Authorization: Bearer %s\n' "$NETBOX_TOKEN" \
    | curl -sS -X "$method" -H @- -H 'Accept: application/json' "$@" "${NETBOX_URL%/}/api/$path"
}

# Lists "<type> <id> <address|prefix|range> <key>" of the namespace's deprecated objects, tab separated.
# The server filters may match loosely, so everything is checked again here.
# Appends "<objects returned> <of them with a key>" to $tmp/stats.
list() {
  local off=0 page count len seen=0 keyed=0 n pfx
  case $1 in ip-ranges) pfx=NetBoxIPPool ;; *) pfx=NetBoxNetwork ;; esac
  pfx=${instance:+$instance/}$pfx/$ns/
  while :; do
    page=$(api GET "ipam/$1/" --fail-with-body -G --data-urlencode "cf_${ref_field}__isw=$pfx" \
      --data-urlencode status=deprecated --data-urlencode limit=1000 --data-urlencode "offset=$off") || return 1
    count=$(jq -e .count <<<"$page") || { echo "unexpected NetBox response listing $1" >&2; return 1; }
    len=$(jq '.results | length' <<<"$page") || return 1
    n=$(jq --arg rf "$ref_field" '[.results[] | select((.custom_fields[$rf] // "") != "")] | length' <<<"$page") || return 1
    seen=$((seen + len)) keyed=$((keyed + n))
    jq -r --arg t "$1" --arg pfx "$pfx" --arg rf "$ref_field" '
      .results[] | (.custom_fields[$rf] // "" | tostring) as $ref
      | select(.status.value == "deprecated" and ($t != "ip-addresses" or .role.value == "vip") and ($ref | startswith($pfx)))
      | [$t, .id, (.address // .prefix // "\(.start_address)-\(.end_address)"), $ref] | @tsv' <<<"$page" || return 1
    off=$((off + len))
    [ "$len" -gt 0 ] && [ "$off" -lt "$count" ] || break
  done
  echo "$seen $keyed" >>"$tmp/stats"
}

# call METHOD PATH [curl args]: the status in $code, the headers in $tmp/head, the body in $tmp/body.
call() {
  code=$(api "$@" -o "$tmp/body" -D "$tmp/head" -w '%{http_code}') || return 1
}

# fail MESSAGE: MESSAGE, then the body of the last answer on a line of its own.
fail() {
  echo "$1 (HTTP $code)" >&2
  [ ! -s "$tmp/body" ] || { cat "$tmp/body"; echo; } >&2
  return 1
}

# remove TYPE ID WHAT REF: reads the object for its ETag, deletes it only if it is still deprecated under REF
# and still that version.
remove() {
  local etag body
  call GET "ipam/$1/$2/" || return 1
  [ "$code" = 200 ] || fail "GET $1/$2 failed" || return 1
  if ! jq -e --arg rf "$ref_field" --arg ref "$4" '.status.value == "deprecated" and .custom_fields[$rf] == $ref' \
    "$tmp/body" >/dev/null; then
    echo "skipped $1 $2 $3: no longer deprecated with key $4"
    return 0
  fi
  etag=$(tr -d '\r' <"$tmp/head" | awk 'tolower($1) == "etag:" {sub(/^[^:]*: */, ""); print; exit}')
  [ -n "$etag" ] || { echo "GET $1/$2 returned no ETag (a proxy strips it?)" >&2; return 1; }
  body=$(jq -nc --arg m "capi: retained objects of namespace $ns cleaned up" '{changelog_message: $m}')
  call DELETE "ipam/$1/$2/" -H 'Content-Type: application/json' -H "If-Match: $etag" --data "$body" || return 1
  [ "$code" = 204 ] || fail "DELETE $1/$2 failed" || return 1
  echo "deleted $1 $2 $3 $4"
  deleted=$((deleted + 1))
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

[ -n "$yes" ] || echo "scope: namespace $ns, key in $ref_field, instance ${instance:-none}, status deprecated"

found=''
: >"$tmp/stats"
for t in ip-addresses ip-ranges prefixes; do
  out=$(list "$t")
  [ -z "$out" ] || found+=$out$'\n'
done
read -r seen keyed < <(awk '{s += $1; k += $2} END {print s + 0, k + 0}' "$tmp/stats")
if [ "$seen" -gt 0 ] && [ "$keyed" = 0 ]; then
  echo "warning: NetBox returned $seen deprecated object(s), none carries the key in $ref_field: is --ref-field the provider's NETBOX_REF_FIELD?" >&2
fi

n=0 deleted=0
while IFS=$'\t' read -r type id what ref; do
  [ -n "$type" ] || continue
  n=$((n + 1))
  if [ -n "$yes" ]; then
    remove "$type" "$id" "$what" "$ref"
  else
    echo "$type $id $what $ref"
  fi
done <<<"$found"

if [ "$n" = 0 ]; then
  zone=${instance:+instance $instance}
  echo "no retained objects in namespace $ns, ${zone:-no instance}"
elif [ -z "$yes" ]; then
  echo "$n object(s) would be deleted; run again with --yes to delete them"
else
  echo "$deleted of $n object(s) deleted"
fi
