# CAPV with kube-vip

Goal: a CAPV cluster whose control-plane VIP comes from NetBox and is run by kube-vip as a static pod. The network reserves the VIP and writes it to the Cluster; a command on the node puts it into the kube-vip manifest.

You need the network and pools of [First cluster](../getting-started/first-cluster.md), and a CAPV template with a kube-vip manifest. The concepts (why placeholders, what the provider does) are in [Control-plane VIP](../guides/control-plane-vip.md).

## Steps

1. **Ask for the VIP** on one network of the cluster:

   ```yaml
   spec:
     controlPlaneEndpoint: {dnsName: demo-api.example}   # port defaults to 6443
   ```

2. **Leave the endpoint out of the `VSphereCluster`.** Omit `spec.controlPlaneEndpoint` entirely rather than leaving it empty. In the CAPV template it is `host: ${CONTROL_PLANE_ENDPOINT_IP}`; the same variable also fills the kube-vip `address` there.

3. **Take the kube-vip manifest and its helper files, and nothing else,** from the CAPV cluster template [`templates/cluster-template-node-ipam.yaml`](https://github.com/kubernetes-sigs/cluster-api-provider-vsphere/blob/main/templates/cluster-template-node-ipam.yaml), into `kubeadmConfigSpec.files` of your KubeadmControlPlane:

   - the Pod manifest `/etc/kubernetes/manifests/kube-vip.yaml`;
   - the `/etc/hosts` mount that kube-vip needs, `/etc/kube-vip.hosts`;
   - for Kubernetes 1.29 and later, the workaround for `kubeadm init`, `/etc/pre-kubeadm-commands/50-kube-vip-prepare.sh`, which points kube-vip at `super-admin.conf` during the init. Leave neither out.

4. **Write the placeholders into the environment of the manifest** instead of `${CONTROL_PLANE_ENDPOINT_IP}` and the fixed port. Quote the port: an unquoted number is not a string for the Pod.

   ```yaml
   - {name: address, value: __CONTROL_PLANE_ENDPOINT__}
   - {name: port, value: "__CONTROL_PLANE_PORT__"}
   ```

5. **Add the command to `preKubeadmCommands`** of the KubeadmControlPlane: the single entry of [`docs/examples/control-plane-vip.yaml`](../examples/control-plane-vip.yaml), whose first line is `f=/etc/kubernetes/manifests/kube-vip.yaml`. It replaces the two placeholders from the kubeadm config on the node and fails without an address. Add it to the list that is there; do not replace the list. The node needs GNU sed.

6. **Pin the kube-vip image** to a version you have tested. The CAPV template uses v0.6.4; v1.2.2 deadlocked the cold start of a control plane ([kube-vip#1654](https://github.com/kube-vip/kube-vip/issues/1654)).

7. **Choose ARP or BGP.** kube-vip in ARP mode (`vip_arp: "true"` in the CAPV manifest) needs all control plane machines in the L2 network of the VIP; otherwise use BGP ([CAPV#1120](https://github.com/kubernetes-sigs/cluster-api-provider-vsphere/issues/1120)). With a second NIC set `vip_interface`: [a second NIC](second-nic.md).

Create the cluster as usual.

## Check

```bash
kubectl -n demo get netboxnetwork demo -o jsonpath='{.status.controlPlaneEndpoint} {.status.conditions}'
kubectl -n demo get cluster demo -o jsonpath='{.spec.controlPlaneEndpoint}'
```

`ControlPlaneEndpointReady` is `True` and the Cluster has the VIP and the port. On a control plane node, `grep __CONTROL_PLANE /etc/kubernetes/manifests/kube-vip.yaml` finds nothing and the `address` value is the VIP. In NetBox the VIP is an IP address with role `vip`.

## Next

- [Control-plane VIP](../guides/control-plane-vip.md): what the provider does with the VIP.
- [keepalived](keepalived.md), [ClusterClass](clusterclass.md), [a second NIC](second-nic.md).
- [Migrating an existing cluster](migrate-existing-cluster.md).
