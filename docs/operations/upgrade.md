# Upgrade

Goal: move to a new release of the provider, and know what does not move by itself.

## Steps

1. Read the release notes of the release. The API is `v1alpha1`: a release can change fields without a conversion, and the notes say what to change in your objects.
2. Plan and apply the upgrade with clusterctl; it applies the new CRDs and replaces the Deployment:

   ```bash
   clusterctl upgrade plan
   clusterctl upgrade apply --ipam capi-ipam-netbox-system/netbox:<version>
   ```

   `clusterctl upgrade plan` prints the exact commands for your installation. clusterctl does not remember the variables of `clusterctl init`: set every `NETBOX_*` variable you used there (`NETBOX_INSTANCE`, the key field) for the upgrade too. They are optional, so one left out is no error, but the provider then runs with the default: without `NETBOX_INSTANCE` it looks for keys without a prefix and does not find the objects of a named zone ([The key does not move](#the-key-does-not-move)).

   With Cluster API Operator, change `spec.version` of the `IPAMProvider`; the variables stay in its `configSecret`.

3. Apply the patches of your Deployment again, because the Deployment is replaced: the private CA and the token file ([Install](../getting-started/install.md#tls)), and every flag you added ([Configuration](configuration.md)).
4. Check:

   ```bash
   kubectl -n capi-ipam-netbox-system rollout status deployment/capi-ipam-netbox-controller-manager
   kubectl -n capi-ipam-netbox-system logs deployment/capi-ipam-netbox-controller-manager | grep 'NetBox preflight passed'
   kubectl get netboxnetworks,netboxippools -A
   ```

   The log line names the `instance` and the key field in use. The networks and pools become `Ready` again.

## The key does not move

`NETBOX_INSTANCE` and `NETBOX_REF_FIELD` decide what a key looks like and where it is kept ([NetBox access](../getting-started/netbox-access.md#the-key)). The provider does not move keys from one setting to another. After a change it does not find the objects that carry the old key:

- a network that has a status reports `PrefixNotFound` and its claims `AddressConflict`, instead of creating twins that NetBox would reject;
- a network without a status (new, or after a move) carves a new prefix, and the old one keeps its key.

So pick the instance name and the field before the first network. To change them later, write the new key into the new place on every object in NetBox first (prefix, VIP, ranges, addresses), with the provider scaled to zero, then change the setting. Moving between management planes with `clusterctl move` needs the same settings on both ([clusterctl move and pivot](../guides/move-and-pivot.md)).

## Next

- [Troubleshooting](troubleshooting.md) and [Configuration](configuration.md).
