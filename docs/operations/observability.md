# Observability

Goal: see what the provider is doing: probes, metrics, events, conditions and logs.

## Probes

The manager serves its probes at once, on `--health-addr` (`:9440`): `/healthz` answers always, `/readyz` answers `waiting for NetBox preflight` until the provider has checked NetBox and started its controllers. The pod stays alive and not ready during a NetBox outage at start ([Install](../getting-started/install.md#what-the-provider-checks-at-start)).

## Metrics

The metrics are served by the diagnostics endpoint (`--diagnostics-address`, `:8443` in the release manifests) over HTTPS, with authentication and authorization as in Cluster API: the caller needs `get` on the non-resource URL `/metrics`, which the ClusterRole `capi-ipam-netbox-metrics-reader` grants. Besides the controller-runtime metrics there are:

| Metric | Labels | Meaning |
|---|---|---|
| `netbox_ipam_requests_total` | `method`, `path`, `code` | requests to the NetBox API; `path` is the API path with numeric segments replaced by `{id}`, `code` the HTTP status or `error` when there was no response |
| `netbox_ipam_request_duration_seconds` | `method`, `path` | duration of those requests |
| `netbox_ipam_write_lock_wait_seconds` | | time a section that writes to NetBox waited for the write lock; reconciles that only read NetBox never wait |

Queries to start with:

```promql
sum by (path, code) (rate(netbox_ipam_requests_total{code!~"2.."}[5m]))
histogram_quantile(0.95, sum by (le, method) (rate(netbox_ipam_request_duration_seconds_bucket[5m])))
rate(netbox_ipam_write_lock_wait_seconds_sum[5m]) / rate(netbox_ipam_write_lock_wait_seconds_count[5m])
```

A 403 on a path is a missing permission (`Forbidden` on the object), `error` a network failure, and a rising lock wait means many networks and pools write at once ([Configuration](configuration.md) has the concurrency flags).

The same endpoint serves pprof and `PUT /debug/flags/v` (the log level as the body), unless `--insecure-diagnostics` is set.

## Events

Changes of the kinds below are recorded as Normal Events on the object that caused them. Other changes, such as the allocation of a new address, tags and custom fields, or taking a deprecated prefix back, have none. There are no Warning events: a failure is on the conditions.

| Object | Reason | When |
|---|---|---|
| `NetBoxNetwork` | `PrefixAcquired` | a prefix was carved, or created or adopted for `spec.prefix` |
| | `PrefixReleased` | the prefix was deleted, deprecated or let go on deletion of the network (`deletionPolicy`) |
| | `PrefixRetired` | a retained prefix that did not fit the spec got the key `<ref>/retired` ([Deletion and retained objects](../guides/deletion-and-retained.md#retained-objects)) |
| | `VIPReserved` | the VIP was created in NetBox |
| | `VIPReleased` | the VIP was deleted or deprecated on deletion of the network |
| | `VIPRetired` | a retained VIP outside the new prefix got the key `<ref>/vip/retired` |
| | `VIPDeleted` | the provider deleted its new VIP because an older NetBox address held its address too ([Troubleshooting](troubleshooting.md)) |
| `NetBoxIPPool` | `RangeCreated` | a range was created in NetBox |
| | `RangeUpdated` | a range was resized, or a deprecated range of the pool was taken back |
| | `RangeReleased` | a range was deleted or deprecated by `deletionPolicy`, or a deprecated range in the way was deleted |
| `NetBoxNetwork`, `NetBoxIPPool`, `IPAddressClaim` | `AttributesSynced` | the owner, tenant, description or comments of an existing NetBox object (the prefix or VIP of the network, a range of the pool, the address of the claim) were changed to the spec |
| `IPAddressClaim` | `AddressRestored` | an address deleted in NetBox by hand was created again |
| | `AddressHandedBack` | an address deleted in NetBox by hand, which NetBox offered to a new claim, went back to the claim that holds it; the new claim takes the next one |
| | `AddressReleased` | the address was deleted from NetBox with the claim |

```bash
kubectl -n <ns> events --for netboxnetwork/<network>
kubectl get events -A --field-selector reason=VIPDeleted
```

## Conditions

Every object reports its state on `status.conditions`, and `kubectl get netboxnetworks,netboxippools -A` shows `Ready` and its reason in columns. What each reason means: [Conditions](../reference/conditions.md).

## Logs

The provider logs through klog: text by default, JSON with `--logging-format=json`, verbosity `-v` (2 by default). Every change in NetBox is an Info line with the object and the NetBox id (`Created the NetBox range`, `Released the NetBox prefix`, `Reserved the control plane VIP` and the like). Lines that explain a leftover or a fallback are listed in [Troubleshooting](troubleshooting.md#log-lines-that-explain-a-leftover). At start:

| Line | Meaning |
|---|---|
| `starting manager` | with the version |
| `NetBox preflight failed, retrying` | NetBox is unreachable or answered with an error; with the error and the wait |
| `NetBox preflight passed` | with the `instance` (empty is none) and the `refField` in use |
| `WARNING: ...` | the token may not view something, or the setup is weaker than it should be; see [Troubleshooting](troubleshooting.md#warnings-at-start) |

## Check

```bash
kubectl -n capi-ipam-netbox-system create serviceaccount metrics-check
kubectl create clusterrolebinding metrics-check --clusterrole=capi-ipam-netbox-metrics-reader --serviceaccount=capi-ipam-netbox-system:metrics-check
kubectl -n capi-ipam-netbox-system port-forward deployment/capi-ipam-netbox-controller-manager 8443:8443 >/dev/null & pf=$!
until curl -sk -o /dev/null https://localhost:8443/metrics; do sleep 1; done
curl -sk -H "Authorization: Bearer $(kubectl -n capi-ipam-netbox-system create token metrics-check)" https://localhost:8443/metrics | grep '^netbox_ipam_requests_total'
```

The counters grow with every reconcile that reads NetBox. Stop the port-forward and remove the check afterwards:

```bash
kill $pf
kubectl delete clusterrolebinding metrics-check
kubectl -n capi-ipam-netbox-system delete serviceaccount metrics-check
```

## Next

- [Troubleshooting](troubleshooting.md).
- [Conditions](../reference/conditions.md).
- [Configuration](configuration.md).
