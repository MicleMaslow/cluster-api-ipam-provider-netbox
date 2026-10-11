# keepalived

Goal: the control-plane VIP from NetBox run by keepalived instead of kube-vip. The provider does not care which load balancer you run: it reserves the VIP and writes it to the Cluster, and the same command as for kube-vip puts the address into the configuration file of your choice.

You need the network and pools of [First cluster](../getting-started/first-cluster.md). keepalived itself, its package or image, how it is started on the node and its VRRP settings are yours; the pages of [CAPV with kube-vip](capv-kube-vip.md) apply for everything about the VIP.

## Steps

1. **Ask for the VIP** on one network of the cluster (`controlPlaneEndpoint`) and leave the endpoint out of the infrastructure cluster, as in [Control-plane VIP](../guides/control-plane-vip.md#steps).

2. **Put the placeholders into `keepalived.conf`** in `kubeadmConfigSpec.files` of the KubeadmControlPlane, where the address and, if you use it, the port belong. The VIP is the bare address; give it the prefix length of your L2 network, or `/32`:

   ```yaml
   files:
   - path: /etc/keepalived/keepalived.conf
     owner: root:root
     permissions: "0644"
     content: |
       vrrp_instance VI_1 {
         state BACKUP
         interface eth0                  # yours
         virtual_router_id 51            # yours
         priority 100
         virtual_ipaddress {
           __CONTROL_PLANE_ENDPOINT__/32
         }
       }
   ```

   The port `__CONTROL_PLANE_PORT__` is for what checks the API server, such as a health-check script run by keepalived (`https://localhost:__CONTROL_PLANE_PORT__/healthz`); leave it out where the file does not name the port.

3. **Add the command to `preKubeadmCommands`:** the single entry of [`docs/examples/control-plane-vip.yaml`](../examples/control-plane-vip.yaml) with the first line changed to the file you wrote:

   ```
   f=/etc/keepalived/keepalived.conf;
   ```

   The rest of the command is the same: it reads the host and port from the kubeadm config that Cluster API renders on the node (`/run/kubeadm/kubeadm.yaml`, `/run/kubeadm/kubeadm-join-config.yaml` or `/etc/kubeadm.yml`), replaces the two placeholders in `f` and fails without an address. Add it to the list; do not replace the list. The node needs GNU sed.

4. **Start keepalived after the command**, in a later `preKubeadmCommands` entry (for example `systemctl restart keepalived`), so that it reads the finished file and the VIP is up before `kubeadm init` talks to the API at the VIP. Any entry that starts it earlier reads the placeholders.

Create the cluster as usual.

## Check

```bash
kubectl -n demo get netboxnetwork demo -o jsonpath='{.status.controlPlaneEndpoint} {.status.conditions}'
kubectl -n demo get cluster demo -o jsonpath='{.spec.controlPlaneEndpoint}'
```

`ControlPlaneEndpointReady` is `True` and the Cluster has the VIP. On a control plane node, `grep __CONTROL_PLANE /etc/keepalived/keepalived.conf` finds nothing and `ip -4 addr` shows the VIP on the interface once keepalived is the master. `make test` runs the command of the example on kubeadm configs as Cluster API v1.14 renders them; the keepalived file is yours to test.

## Next

- [Control-plane VIP](../guides/control-plane-vip.md).
- [CAPV with kube-vip](capv-kube-vip.md), [ClusterClass](clusterclass.md).
