# Reusing a custom field

Goal: keep the key of the provider in a text custom field NetBox already has, when you cannot or do not want to add `capi_ref`. Typically that is a field another tool created, for example the one netbox-operator keeps its restoration hash in (`netboxOperatorRestorationHash` by default). The provider takes every name NetBox does (letters, digits and `_` in any case, without `__`), so that field works as it is.

The provider calls an object its own only when the field holds exactly its key, so a value another tool wrote there never matches; and it writes the key only to objects of its own networks, pools and claims. Choose the field before the first network: the provider does not migrate keys between fields ([Upgrade](../operations/upgrade.md#the-key-does-not-move)).

## Steps

1. **Check the field.** It must be a `text` custom field, enabled for `ipam.prefix`, `ipam.iprange` and `ipam.ipaddress`, and with the status `active`. Any `filter_logic` but `disabled` works; `exact` is best ([NetBox access](../getting-started/netbox-access.md#the-custom-field)). An existing field gets the object types with `PATCH /api/extras/custom-fields/<id>/` and the complete `object_types` list ([What to send your NetBox admin](../getting-started/netbox-access.md#what-to-send-your-netbox-admin)).

2. **Tell the provider where the key is** when you install it ([Install](../getting-started/install.md#variables)):

   ```yaml
   NETBOX_REF_FIELD: netboxOperatorRestorationHash
   ```

3. **Know the limits.**

   - With `filter_logic: loose` (NetBox's default for a new field, so likely for a field another tool created) NetBox returns every object whose value contains the key, so lookups read more objects; the provider checks each for the exact key. It logs a warning at start. `disabled` stops the start.
   - A prefix whose field already holds another tool's value is not adopted with `spec.prefix` (`Duplicate`, `held by ...`): the field has to be empty or hold the key of the network.
   - A token that may not view custom field definitions is not checked at start: the provider warns and probes the filter on prefixes instead.

## Check

```bash
id=$(kubectl -n demo get netboxnetwork demo -o jsonpath='{.status.prefixID}')
curl -sf -H "Authorization: Bearer $NETBOX_TOKEN" "$NETBOX_URL/api/ipam/prefixes/$id/" | jq -c '.custom_fields'
```

The field holds the key `NetBoxNetwork/demo/demo` (with the instance name in front, if you set one). The log line `NetBox preflight passed` names the `refField` in use.

## Next

- [NetBox access](../getting-started/netbox-access.md#the-key).
- [Cleanup](cleanup.md): the script with `--ref-field`.
- [NetBox objects](../reference/netbox-objects.md#keys).
