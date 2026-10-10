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
	// inside the prefix, else the highest free address. The VIP is written to Cluster.spec.controlPlaneEndpoint
	// while that has no host or no port; another endpoint is reported, not changed.
	// Without it no VIP is reserved; removing it later leaves the VIP in NetBox and in the status.
	// A VIP that cannot be reserved shows in the ControlPlaneEndpointReady condition and leaves Ready alone.
	// +optional
	ControlPlaneEndpoint *ControlPlaneEndpoint `json:"controlPlaneEndpoint,omitempty"`

	// prefix adopts an existing prefix inside parentPrefix instead of carving one.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=43
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="must be a CIDR"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="prefix is immutable"
	Prefix string `json:"prefix,omitempty"`

	// deletionPolicy for the NetBox prefix and the pool ranges inside it.
	// +optional
	// +kubebuilder:default=Retain
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// customFields are set on the NetBox prefix. A JSON-object value is merged key by key into the
	// field's current object: keys given here are written, keys written before but no longer given
	// are removed, other keys are left alone. Any other value replaces the field.
	// capi_ref and capi_cluster are managed by the provider; setting them fails the MetadataSynced condition with reason InvalidSpec.
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
}

// ControlPlaneEndpoint asks for the cluster's VIP in the network's prefix.
type ControlPlaneEndpoint struct {
	// port of the API server behind the VIP. Defaults to 6443.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// dnsName of the VIP in NetBox, which stores it in lower case.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^([0-9A-Za-z_-]+|\*)(\.[0-9A-Za-z_-]+)*\.?$`
	DNSName string `json:"dnsName,omitempty"`
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
	// Known condition types are Ready, MetadataSynced and ControlPlaneEndpointReady.
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
