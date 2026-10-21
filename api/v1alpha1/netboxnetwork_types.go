/*
Copyright 2026 The cluster-api-ipam-provider-netbox Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// DeletionPolicy says what happens in NetBox when the Kubernetes object goes away.
// +kubebuilder:validation:Enum=Retain;Delete
type DeletionPolicy string

const (
	// DeletionPolicyRetain keeps the NetBox object as deprecated; recreating the object reclaims it.
	DeletionPolicyRetain DeletionPolicy = "Retain"
	// DeletionPolicyDelete removes the NetBox object.
	DeletionPolicyDelete DeletionPolicy = "Delete"
)

// Allocator says what hands out the addresses of the pools of a network.
// +kubebuilder:validation:Enum=NetBox;InCluster
type Allocator string

const (
	// AllocatorNetBox hands out addresses from the NetBox ranges of the pools, through NetBoxIPPools.
	AllocatorNetBox Allocator = "NetBox"
	// AllocatorInCluster hands out addresses through an InClusterIPPool (cluster-api-ipam-provider-in-cluster) per pool.
	AllocatorInCluster Allocator = "InCluster"
)

// NetBoxNetworkSpec is one subnet of a cluster: a prefix carved from, or adopted in, parentPrefix.
// +kubebuilder:validation:XValidation:rule="has(self.prefix) != has(self.prefixLength)",message="exactly one of prefix or prefixLength must be set"
// +kubebuilder:validation:XValidation:rule="has(self.prefix) == has(oldSelf.prefix)",message="cannot switch between prefix and prefixLength"
// +kubebuilder:validation:XValidation:rule="!isCIDR(self.parentPrefix) || !isIP(self.gateway) || (has(self.addressPrefixLength) ? cidr(self.gateway + ['/1', '/2', '/3', '/4', '/5', '/6', '/7', '/8', '/9', '/10', '/11', '/12', '/13', '/14', '/15', '/16', '/17', '/18', '/19', '/20', '/21', '/22', '/23', '/24', '/25', '/26', '/27', '/28', '/29', '/30', '/31', '/32'][self.addressPrefixLength - 1]).masked().containsCIDR(self.parentPrefix) : cidr(self.parentPrefix).containsIP(self.gateway))",message="gateway/addressPrefixLength must contain parentPrefix"
// +kubebuilder:validation:XValidation:rule="!has(self.addressPrefixLength) || !isCIDR(self.parentPrefix) || self.addressPrefixLength <= cidr(self.parentPrefix).prefixLength()",message="addressPrefixLength must not be longer than the parentPrefix mask"
// +kubebuilder:validation:XValidation:rule="has(self.addressPrefixLength) == has(oldSelf.addressPrefixLength)",message="addressPrefixLength cannot be added or removed"
// +kubebuilder:validation:XValidation:rule="!has(self.isPool) || !self.isPool || has(self.prefixLength)",message="isPool needs prefixLength"
// +kubebuilder:validation:XValidation:rule="!has(self.prefix) || !isCIDR(self.parentPrefix) || !isCIDR(self.prefix) || cidr(self.parentPrefix).containsCIDR(self.prefix)",message="prefix must be inside parentPrefix"
// +kubebuilder:validation:XValidation:rule="!has(self.prefixLength) || !isCIDR(self.parentPrefix) || self.prefixLength > cidr(self.parentPrefix).prefixLength()",message="prefixLength must be longer than the parentPrefix mask"
// +kubebuilder:validation:XValidation:rule="!has(self.prefix) || !has(self.deletionPolicy) || self.deletionPolicy != 'Delete'",message="an adopted prefix cannot use deletionPolicy Delete"
// +kubebuilder:validation:XValidation:rule="!has(self.pools) || !has(self.addresses) || self.pools.all(p, !self.addresses.exists(a, a.name == p.name))",message="pool and address group names must differ"
// +kubebuilder:validation:XValidation:rule="has(self.allocator) == has(oldSelf.allocator)",message="allocator cannot be added or removed"
// +kubebuilder:validation:XValidation:rule="!has(self.pools) || has(self.allocator) && self.allocator == 'InCluster' || self.pools.all(p, !has(p.sync) || !has(p.sync.enabled) || !p.sync.enabled)",message="sync needs allocator InCluster"
type NetBoxNetworkSpec struct {
	// clusterName is the CAPI Cluster the network belongs to.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterName is immutable"
	ClusterName string `json:"clusterName,omitempty"`

	// parentPrefix is the NetBox prefix the cluster prefix is carved from, or adopted in.
	// It lies inside the L2 network (see addressPrefixLength) and may be a logical pool in it.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=43
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="must be a CIDR"
	// +kubebuilder:validation:XValidation:rule="!isCIDR(self) || cidr(self).ip().family() == 4",message="only IPv4 is supported"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="parentPrefix is immutable"
	ParentPrefix string `json:"parentPrefix,omitempty"`

	// gateway of the L2 network; with addressPrefixLength it gives that network.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=39
	// +kubebuilder:validation:XValidation:rule="isIP(self)",message="must be an IP address"
	// +kubebuilder:validation:XValidation:rule="!isIP(self) || ip(self).family() == 4",message="only IPv4 is supported"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="gateway is immutable"
	Gateway string `json:"gateway,omitempty"`

	// prefixLength of the prefix to carve from parentPrefix.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="prefixLength is immutable"
	PrefixLength int32 `json:"prefixLength,omitempty"`

	// addressPrefixLength is the mask of the nodes' subnet; with gateway it gives that subnet (the L2 network).
	// Defaults to the mask of parentPrefix. Addresses get this mask.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="addressPrefixLength is immutable"
	AddressPrefixLength int32 `json:"addressPrefixLength,omitempty"`

	// isPool marks the carved prefix as a pool in NetBox: all its addresses are usable hosts.
	// It needs prefixLength; false is the same as unset.
	// +optional
	IsPool *bool `json:"isPool,omitempty"`

	// controlPlaneEndpoint reserves the cluster's VIP in the prefix: the address of the Cluster's endpoint if that is
	// inside the prefix, else the highest free address outside the pools and address groups, which the layout keeps
	// for it. The VIP is written to Cluster.spec.controlPlaneEndpoint while that has no host or no port; another
	// endpoint is reported, not changed.
	// Without it no VIP is reserved; removing it later leaves the VIP in NetBox and in the status.
	// A VIP that cannot be reserved shows in the ControlPlaneEndpointReady condition and leaves Ready alone.
	// +optional
	ControlPlaneEndpoint *ControlPlaneEndpoint `json:"controlPlaneEndpoint,omitempty"`

	// allocator hands out the addresses of the pools: NetBox, through NetBoxIPPools over the NetBox ranges of the pools,
	// or InCluster, through an InClusterIPPool per pool, named as the pool, which the network creates and keeps in sync.
	// Unset is NetBox. Immutable, cannot be added or removed: another allocator is another network.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="allocator is immutable"
	Allocator Allocator `json:"allocator,omitempty"`

	// pools lay out the prefix: in order, each takes the next free addresses from the start of the prefix.
	// A free address is not the network, broadcast or gateway of the L2 network, the VIP, nor held by a NetBox
	// address or range of anything else. The layout is fixed in status.layout and placed again only when the names
	// or sizes here change; a change that would move an allocated address to another pool is refused as a whole.
	// Each pool is one or more NetBox IP ranges, which the allocator hands out addresses from.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:XValidation:rule="self.filter(p, type(p.size) == string).size() <= 1",message="at most one pool of size rest"
	Pools []NetworkPool `json:"pools,omitempty"`

	// addresses are groups of addresses reserved in NetBox at the top of the prefix, under the VIP: each group takes
	// count free addresses outside the pools, from the top down. Placed addresses stay; a group that grows takes the
	// highest free ones, one that shrinks gives up its last ones, deleted in NetBox whatever the deletionPolicy.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=8
	Addresses []AddressGroup `json:"addresses,omitempty"`

	// prefix adopts an existing prefix inside parentPrefix instead of carving one.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=43
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="must be a CIDR"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="prefix is immutable"
	Prefix string `json:"prefix,omitempty"`

	// deletionPolicy for the NetBox prefix, the VIP, the reserved addresses and the pool ranges when the network is deleted.
	// +optional
	// +kubebuilder:default=Retain
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// customFields are set on the NetBox prefix. A JSON-object value is merged key by key into the
	// field's current object: keys given here are written, keys written before but no longer given
	// are removed, other keys are left alone. Any other value replaces the field.
	// The key field (capi_ref by default) is managed by the provider; setting it fails the MetadataSynced condition with reason InvalidSpec.
	// +optional
	// +kubebuilder:validation:MaxProperties=32
	CustomFields map[string]apiextensionsv1.JSON `json:"customFields,omitempty"`

	// tags are slugs of existing NetBox tags set on the prefix. Tags added by others are kept.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=100
	// +kubebuilder:validation:items:Pattern=`^[-a-zA-Z0-9_]+$`
	Tags []string `json:"tags,omitempty"`

	// owner of the NetBox objects of the network and its pools, kept in sync; an adopted prefix keeps its own.
	// Unset: never written nor cleared.
	// +optional
	Owner *OwnerRef `json:"owner,omitempty"`

	// tenant of the NetBox objects of the network and its pools, kept in sync; an adopted prefix keeps its own.
	// Unset: never written nor cleared.
	// +optional
	Tenant *TenantRef `json:"tenant,omitempty"`

	// description template of the prefix, kept in sync and cut to 200 characters; an adopted prefix keeps its own.
	// Variables: .Cluster, .Namespace, .Name. Default "{{ .Namespace }}/{{ .Name }} // cluster {{ .Cluster }}".
	// Rules as for the pool's ipAddress templates.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// comments template of the prefix, kept in sync; an adopted prefix keeps its own. Variables as for description.
	// Default "// managed by cluster-api-ipam-provider-netbox, please don't edit it in NetBox unless you know what you're doing".
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Comments string `json:"comments,omitempty"`
}

// NetworkPool is a pool of the network's layout.
type NetworkPool struct {
	// name of the pool, which NetBoxIPPool ranges refer to; with allocator InCluster, the name of its InClusterIPPool.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name,omitempty"`

	// size is the number of free addresses of the pool, or rest: the free addresses the other pools leave.
	// +required
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:validation:XValidation:rule="type(self) == string ? self == 'rest' : self >= 1 && self <= 65535",message="must be a positive number or rest"
	Size intstr.IntOrString `json:"size,omitempty"`

	// publish writes the addresses of the pool, under its name, to a Secret.
	// +optional
	Publish Publish `json:"publish,omitempty,omitzero"`

	// sync writes each address the InClusterIPPool of the pool hands out (an IPAddress) to NetBox, and deletes it with
	// the IPAddress. Only with allocator InCluster.
	// +optional
	Sync PoolSync `json:"sync,omitempty,omitzero"`
}

// PoolSync writes the IPAddresses of an InClusterIPPool to NetBox: each an active NetBox address with the mask of the
// L2 network, the key .../sync/<pool>/<IPAddress>, the fields of ipAddress and the owner and tenant of the network.
// The NetBox addresses follow the templates and the network; one of someone else at the address of an IPAddress is
// AddressConflict and is left alone. They are deleted with their IPAddress, when the sync is turned off, and with the
// network, whatever its deletionPolicy.
// +kubebuilder:validation:MinProperties=1
type PoolSync struct {
	// enabled turns the sync on; unset is false.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// ipAddress are the templates of the NetBox addresses, as the ipAddress of a NetBoxIPPool: .Owner is the first
	// owner of the claim of the IPAddress (the machine), .Pool the name of the pool.
	// +optional
	IPAddress IPAddressFields `json:"ipAddress,omitempty,omitzero"`
}

// Publish writes addresses to the Secret secretName in the namespace of the network, one key per pool or group.
// The Secret exists while at least one pool or group that publishes in it is enabled, and holds only those.
// +kubebuilder:validation:MinProperties=1
// +kubebuilder:validation:XValidation:rule="!has(self.enabled) || !self.enabled || has(self.secretName)",message="enabled needs secretName"
type Publish struct {
	// enabled turns publishing on; unset is false.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// secretName is the Secret to write to. Several pools and groups of a network may share one, a Secret belongs to
	// one network: one that exists and is not controlled by it is left alone (SecretConflict).
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	SecretName string `json:"secretName,omitempty"`
}

// AddressGroup is a named group of addresses reserved in NetBox at the top of the prefix.
type AddressGroup struct {
	// name of the group; it differs from the name of every pool.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name,omitempty"`

	// count is the number of addresses of the group.
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	Count int32 `json:"count,omitempty"`

	// dnsName template of each address, kept in sync; NetBox stores it in lower case. Unset: no dns_name.
	// Variables: .Cluster, .Namespace, .Network, .Group, .Index (1 for the highest address of the group, counting down).
	// Rules as for the pool's ipAddress templates.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	DNSName string `json:"dnsName,omitempty"`

	// description template of each address, kept in sync and cut to 200 characters. Variables as for dnsName.
	// Default "{{ .Namespace }}/{{ .Network }} // {{ .Cluster }} {{ .Group }} {{ .Index }}".
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// publish writes the addresses of the group, under its name, to a Secret.
	// +optional
	Publish Publish `json:"publish,omitempty,omitzero"`
}

// NetworkLayout is the placement of the pools and address groups of a network in its prefix.
// +kubebuilder:validation:MinProperties=1
type NetworkLayout struct {
	// pools are the placed pools in the order of spec.pools.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	Pools []PoolLayout `json:"pools,omitempty"`

	// addresses are the placed address groups in the order of spec.addresses.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=8
	Addresses []AddressGroupLayout `json:"addresses,omitempty"`
}

// AddressGroupLayout is an address group placed in the prefix.
type AddressGroupLayout struct {
	// name of the group.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name,omitempty"`

	// count is the count spec.addresses asked for when the group was placed.
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	Count int32 `json:"count,omitempty"`

	// addresses of the group from the top down: the first has .Index 1.
	// +required
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=39
	Addresses []string `json:"addresses,omitempty"`
}

// PoolLayout is a pool placed in the prefix.
type PoolLayout struct {
	// name of the pool.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name,omitempty"`

	// size is the size spec.pools asked for when the pool was placed.
	// +required
	// +kubebuilder:validation:XIntOrString
	Size intstr.IntOrString `json:"size,omitempty"`

	// ranges hold the addresses of the pool, ascending; the addresses between them are not the pool's.
	// +required
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	Ranges []LayoutRange `json:"ranges,omitempty"`
}

// LayoutRange is one NetBox IP range of a placed pool.
type LayoutRange struct {
	// start is the first address of the range.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=39
	Start string `json:"start,omitempty"`

	// end is the last address of the range.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=39
	End string `json:"end,omitempty"`

	// id is the NetBox ID of the IP range, unset until it is created.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ID int64 `json:"id,omitempty"`
}

// OwnerRef names a NetBox Owner by name or id.
// +kubebuilder:validation:XValidation:rule="has(self.name) != has(self.id)",message="set exactly one of name and id"
type OwnerRef struct {
	// name of the owner.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=100
	Name string `json:"name,omitempty"`

	// id of the owner.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ID int64 `json:"id,omitempty"`
}

// TenantRef names a NetBox Tenant by slug or id.
// +kubebuilder:validation:XValidation:rule="has(self.slug) != has(self.id)",message="set exactly one of slug and id"
type TenantRef struct {
	// slug of the tenant.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=100
	// +kubebuilder:validation:Pattern=`^[-a-zA-Z0-9_]+$`
	Slug string `json:"slug,omitempty"`

	// id of the tenant.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ID int64 `json:"id,omitempty"`
}

// ControlPlaneEndpoint asks for the cluster's VIP in the network's prefix.
// +kubebuilder:validation:XValidation:rule="has(self.port) == has(oldSelf.port)",message="port cannot be added or removed"
type ControlPlaneEndpoint struct {
	// port of the API server behind the VIP. Defaults to 6443.
	// Immutable while controlPlaneEndpoint is set: the Cluster keeps the endpoint it got.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="port is immutable"
	Port int32 `json:"port,omitempty"`

	// dnsName template of the VIP in NetBox, which stores it in lower case. Variables as for spec.description;
	// a value that is not a DNS name fails Ready with reason InvalidSpec.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	DNSName string `json:"dnsName,omitempty"`

	// description template of the VIP, as spec.description.
	// Default "{{ .Namespace }}/{{ .Name }} // {{ .Cluster }} control plane endpoint".
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// comments template of the VIP, as spec.comments.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Comments string `json:"comments,omitempty"`
}

// APIEndpoint is the host and port the API server is reached at.
type APIEndpoint struct {
	// host is the VIP.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=45
	Host string `json:"host,omitempty"`

	// port of the API server behind the VIP.
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
}

// NetBoxNetworkStatus is the observed state of a NetBoxNetwork.
// +kubebuilder:validation:MinProperties=1
type NetBoxNetworkStatus struct {
	// conditions represents the observations of a NetBoxNetwork's current state.
	// Known condition types are Ready, MetadataSynced, ControlPlaneEndpointReady and LayoutReady.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// prefix is the CIDR held in NetBox.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=43
	Prefix string `json:"prefix,omitempty"`

	// addressPrefixLength is the mask handed to addresses: spec.addressPrefixLength or the parentPrefix mask.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32
	AddressPrefixLength int32 `json:"addressPrefixLength,omitempty"`

	// prefixID is the NetBox ID of the prefix.
	// +optional
	// +kubebuilder:validation:Minimum=1
	PrefixID int64 `json:"prefixID,omitempty"`

	// controlPlaneEndpoint is the reserved VIP and the port of the API server behind it.
	// +optional
	ControlPlaneEndpoint APIEndpoint `json:"controlPlaneEndpoint,omitempty,omitzero"`

	// customFieldKeys are, per JSON-object custom field, the keys the provider has written.
	// +optional
	CustomFieldKeys map[string][]string `json:"customFieldKeys,omitempty"`

	// tags are the slugs of the tags the provider has set on the prefix.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=100
	Tags []string `json:"tags,omitempty"`

	// ownerID is the NetBox ID spec.owner resolves to; unset without spec.owner.
	// +optional
	// +kubebuilder:validation:Minimum=1
	OwnerID int64 `json:"ownerID,omitempty"`

	// tenantID is the NetBox ID spec.tenant resolves to; unset without spec.tenant.
	// +optional
	// +kubebuilder:validation:Minimum=1
	TenantID int64 `json:"tenantID,omitempty"`

	// layout is the placement of spec.pools and spec.addresses in the prefix, with the NetBox ranges of the pools.
	// +optional
	Layout NetworkLayout `json:"layout,omitempty,omitzero"`

	// publishedSecrets are the Secrets the network has written its addresses to, which it deletes once no enabled
	// pool or group publishes in them.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=253
	PublishedSecrets []string `json:"publishedSecrets,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterName",description="Cluster the network belongs to"
// +kubebuilder:printcolumn:name="Prefix",type="string",JSONPath=".status.prefix",description="CIDR held in NetBox"
// +kubebuilder:printcolumn:name="VIP",type="string",JSONPath=".status.controlPlaneEndpoint.host",description="Control plane VIP"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="Whether the network is reflected in NetBox"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].reason",description="Reason of the Ready condition"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time duration since creation of NetBoxNetwork"

// NetBoxNetwork is a cluster subnet held in NetBox.
type NetBoxNetwork struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is the standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state of NetBoxNetwork.
	// +required
	Spec NetBoxNetworkSpec `json:"spec,omitempty,omitzero"`
	// status is the observed state of NetBoxNetwork.
	// +optional
	Status NetBoxNetworkStatus `json:"status,omitempty,omitzero"`
}

// GetConditions returns the set of conditions for this object.
func (n *NetBoxNetwork) GetConditions() []metav1.Condition { return n.Status.Conditions }

// SetConditions sets conditions for an API object.
func (n *NetBoxNetwork) SetConditions(c []metav1.Condition) { n.Status.Conditions = c }

// +kubebuilder:object:root=true

// NetBoxNetworkList is a list of NetBoxNetworks.
type NetBoxNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is the standard list's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#lists-and-simple-kinds
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	// items is the list of NetBoxNetworks.
	Items []NetBoxNetwork `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &NetBoxNetwork{}, &NetBoxNetworkList{})
}
