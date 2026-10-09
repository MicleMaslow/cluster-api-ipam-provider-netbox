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

// NetBoxNetworkSpec is one subnet of a cluster: a prefix carved from, or adopted in, a parent L2 network.
// +kubebuilder:validation:XValidation:rule="has(self.prefix) != has(self.prefixLength)",message="exactly one of prefix or prefixLength must be set"
// +kubebuilder:validation:XValidation:rule="has(self.prefix) == has(oldSelf.prefix)",message="cannot switch between prefix and prefixLength"
// +kubebuilder:validation:XValidation:rule="!isCIDR(self.parentPrefix) || !isIP(self.gateway) || cidr(self.parentPrefix).containsIP(self.gateway)",message="gateway must be inside parentPrefix"
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

	// parentPrefix is the L2 network the prefix lives in; addresses get its mask.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=43
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="must be a CIDR"
	// +kubebuilder:validation:XValidation:rule="!isCIDR(self) || cidr(self).ip().family() == 4",message="only IPv4 is supported"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="parentPrefix is immutable"
	ParentPrefix string `json:"parentPrefix,omitempty"`

	// gateway of the parent network.
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

// NetBoxNetworkStatus is the observed state of a NetBoxNetwork.
// +kubebuilder:validation:MinProperties=1
type NetBoxNetworkStatus struct {
	// conditions represents the observations of a NetBoxNetwork's current state.
	// Known condition types are Ready and MetadataSynced.
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

	// prefixID is the NetBox ID of the prefix.
	// +optional
	// +kubebuilder:validation:Minimum=1
	PrefixID int64 `json:"prefixID,omitempty"`

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
