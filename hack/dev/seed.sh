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

# Seeds a fresh dev NetBox with what the provider expects and writes hack/dev/.env (NETBOX_ENV_FILE for another file).
set -euo pipefail
cd "$(dirname "$0")"

NB=${NETBOX_URL:-http://localhost:18000}
ENV_FILE=${NETBOX_ENV_FILE:-.env}
ADMIN_TOKEN=nbt_devadmin0001.0123456789abcdef0123456789abcdef01234567

api() {
  curl -sS --fail-with-body -X "$1" "$NB/api/$2" \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' -H 'Accept: application/json' \
    ${3:+--data "$3"}
}

# The choice set and the VLAN the select and object custom fields point to.
choices() {
  if [ "$(api GET 'extras/custom-field-choice-sets/?name=site-tiers' | jq .count)" = 0 ]; then
    api POST extras/custom-field-choice-sets/ '{"name":"site-tiers","extra_choices":[["standard","Standard"],["premium","Premium"]]}' >/dev/null
  fi
  if [ "$(api GET 'ipam/vlans/?vid=100' | jq .count)" = 0 ]; then
    api POST ipam/vlans/ '{"vid":100,"name":"dev-vlan"}' >/dev/null
  fi
}

# Prefix custom fields of each type for the metadata examples, plus those of custom-fields.local.json
# (a list of POST /api/extras/custom-fields/ bodies); missing ones are added on a seeded NetBox too.
custom_fields() {
  choices
  local fields='[
    {"name":"team","label":"Team","type":"text","object_types":["ipam.prefix"],"ui_visible":"if-set"},
    {"name":"priority","label":"Priority","type":"integer","object_types":["ipam.prefix"],"ui_visible":"if-set"},
    {"name":"monitored","label":"Monitored","type":"boolean","object_types":["ipam.prefix"],"ui_visible":"if-set"},
    {"name":"site_tier","label":"Site tier","type":"select","choice_set":{"name":"site-tiers"},"object_types":["ipam.prefix"],"ui_visible":"if-set"},
    {"name":"vlan","label":"VLAN","type":"object","related_object_type":"ipam.vlan","object_types":["ipam.prefix"],"ui_visible":"if-set"}
  ]'
  [ -f custom-fields.local.json ] && fields=$(jq -s add <(echo "$fields") custom-fields.local.json)
  while read -r f; do
    if [ "$(api GET "extras/custom-fields/?name=$(jq -r .name <<<"$f")" | jq .count)" = 0 ]; then
      api POST extras/custom-fields/ "$f" >/dev/null
    fi
  done < <(jq -c '.[]' <<<"$fields")
}

until curl -sf -o /dev/null "$NB/login/"; do sleep 5; done

if [ "$(api GET 'users/owner-groups/?name=capi' | jq .count)" != 0 ]; then
  [ -f "$ENV_FILE" ] || { echo "NetBox is seeded but $ENV_FILE is gone: run make netbox-down netbox-up" >&2; exit 1; }
  custom_fields
  echo "NetBox already seeded"
  exit 0
fi

api POST extras/custom-fields/ '[
  {"name":"capi_ref","label":"CAPI ref","type":"text","filter_logic":"exact","object_types":["ipam.prefix","ipam.iprange","ipam.ipaddress"]},
  {"name":"labels","label":"Labels","type":"json","object_types":["ipam.prefix"],"ui_visible":"if-set"}
]' >/dev/null

custom_fields
api POST extras/tags/ '[{"name":"capi","slug":"capi"},{"name":"k8s","slug":"k8s"}]' >/dev/null

# Containers above the parents, as in a real address plan.
for p in 10.0.0.0/8:org 10.10.0.0/16:region 10.20.0.0/16:region; do
  api POST ipam/prefixes/ "{\"prefix\":\"${p%%:*}\",\"status\":\"container\",\"description\":\"${p#*:}\"}" >/dev/null
done
# Parent L2 networks inside the containers; the first /26 of each holds the gateway and is reserved.
for net in 10.10.0 10.20.0; do
  api POST ipam/prefixes/ "{\"prefix\":\"$net.0/22\",\"status\":\"active\",\"description\":\"dev parent VLAN\"}" >/dev/null
  api POST ipam/prefixes/ "{\"prefix\":\"$net.0/26\",\"status\":\"reserved\",\"description\":\"reserved\"}" >/dev/null
  api POST ipam/ip-addresses/ "{\"address\":\"$net.1/22\",\"status\":\"active\",\"description\":\"gateway\"}" >/dev/null
done

# The operator sees everything. It writes objects without an owner, as the provider creates them unless a network
# sets spec.owner, and those of owners of the group zones (the tests make theirs there); owners of the group capi
# stay read-only for it (the Forbidden tests). Dev may also delete prefixes, for deletionPolicy: Delete.
user=$(api POST users/users/ '{"username":"capi-operator","password":"Capi-operator-dev-1"}' | jq .id)
api POST users/permissions/ "{\"name\":\"capi-view\",\"object_types\":[\"ipam.prefix\",\"ipam.iprange\",\"ipam.ipaddress\",\"users.owner\",\"tenancy.tenant\",\"extras.customfield\",\"extras.tag\"],\"actions\":[\"view\"],\"users\":[$user]}" >/dev/null
api POST users/permissions/ "{\"name\":\"capi-write\",\"object_types\":[\"ipam.prefix\",\"ipam.iprange\",\"ipam.ipaddress\"],\"actions\":[\"add\",\"change\",\"delete\"],\"constraints\":{\"owner__isnull\":true},\"users\":[$user]}" >/dev/null
zones=$(api POST users/owner-groups/ '{"name":"zones"}' | jq .id)
api POST users/permissions/ "{\"name\":\"capi-write-zones\",\"object_types\":[\"ipam.prefix\",\"ipam.iprange\",\"ipam.ipaddress\"],\"actions\":[\"add\",\"change\",\"delete\"],\"constraints\":{\"owner__group__name\":\"zones\"},\"users\":[$user]}" >/dev/null
# Owner and tenant of the cluster vc of make dev-all.
api POST users/owners/ "{\"name\":\"dev-team\",\"group\":$zones}" >/dev/null
api POST tenancy/tenants/ '{"name":"dev-tenant","slug":"dev-tenant"}' >/dev/null
token=$(api POST users/tokens/ "{\"user\":$user,\"version\":2}")
cat > "$ENV_FILE" <<EOF
NETBOX_URL=$NB
NETBOX_TOKEN=nbt_$(jq -r .key <<<"$token").$(jq -r .token <<<"$token")
NETBOX_ADMIN_TOKEN=$ADMIN_TOKEN
NETBOX_INSTANCE=dev
EOF

# The group capi is the "already seeded" marker, so it is created last.
api POST users/owner-groups/ '{"name":"capi"}' >/dev/null
echo "NetBox seeded, wrote $ENV_FILE"
