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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// NetBoxIPPoolKind is the kind IPAddressClaims reference.
const NetBoxIPPoolKind = "NetBoxIPPool"

// NetBoxIPPoolSpec is a pool of addresses made of ranges inside NetBoxNetworks of one L2 network.
type NetBoxIPPoolSpec struct {
	// ranges are used in order. A range can be changed or removed; a change that would leave
	// allocated addresses outside the range waits until they are released.
	// +required
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:XValidation:rule="self.all(r, self.exists_one(o, o.networkName == r.networkName))",message="one range per network"
	// +kubebuilder:validation:XValidation:rule="self.all(r, (r.start >= 0) != (r.end >= 0) || r.start <= r.end)",message="start must not be after end"
	Ranges []IPPoolRange `json:"ranges,omitempty"`

	// ipAddress sets dns_name and description of the pool's NetBox IP addresses with Go templates, rendered per claim.
	// Variables: .Owner (first owner of the claim, else the claim), .Claim, .Cluster, .Namespace, .Pool.
	// dnsName defaults to "{{ .Owner }}", is lowercased and must be a valid DNS name;
	// description defaults to "{{ .Cluster }} {{ .Pool }} {{ .Owner }}" and is cut to 200 characters.
	// Results are trimmed; {{ "" }} leaves a field empty. range, define, block and template are not allowed,
	// and a value is limited to 1024 bytes.
	// +optional
	IPAddress IPAddressFields `json:"ipAddress,omitempty,omitzero"`

	// addressReuseGracePeriodSeconds keeps the address of a deleted claim in NetBox this long, so that it is not handed
	// to a new machine while caches of the old one still point at it, at most a day. Not applied while the pool
	// is being deleted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=86400
	AddressReuseGracePeriodSeconds *int32 `json:"addressReuseGracePeriodSeconds,omitempty"`

	// omitGateway hands out addresses without a gateway, e.g. for a second NIC whose default route is on the first.
	// Applies to addresses allocated after the change.
	// +optional
	OmitGateway *bool `json:"omitGateway,omitempty"`
}

// IPAddressFields are Go templates of NetBox IP address fields, rendered per claim.
// +kubebuilder:validation:MinProperties=1
type IPAddressFields struct {
	// dnsName template, lowercased; default "{{ .Owner }}".
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	DNSName string `json:"dnsName,omitempty"`

	// description template, cut to 200 characters; default "{{ .Cluster }} {{ .Pool }} {{ .Owner }}".
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`
}

// IPPoolRange is a span of a NetBoxNetwork given as offsets from its first address.
type IPPoolRange struct {
	// networkName is a NetBoxNetwork in the pool's namespace.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	NetworkName string `json:"networkName,omitempty"`

	// start offset; a negative one counts back from the last address, -1 is the last one.
	// +required
	Start int32 `json:"start"`

	// end offset, same rules as start.
	// +required
	End int32 `json:"end"`
}

// NetBoxIPPoolStatus is the observed state of a NetBoxIPPool.
// +kubebuilder:validation:MinProperties=1
type NetBoxIPPoolStatus struct {
	// conditions represents the observations of a NetBoxIPPool's current state.
	// Known condition types are Ready and RangesReady.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ranges are the NetBox IP ranges of the pool, in spec order, then the removed ones that still hold addresses.
	// +optional
	// +listType=map
	// +listMapKey=networkName
	// +kubebuilder:validation:MaxItems=32
	Ranges []IPPoolRangeStatus `json:"ranges,omitempty"`

	// gateway handed to every address of the pool.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=39
	Gateway string `json:"gateway,omitempty"`

	// addressPrefixLength is the mask length (that of the L2 network) handed to every address of the pool.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	AddressPrefixLength int32 `json:"addressPrefixLength,omitempty"`

	// ipAddresses reports the count of total, used and free addresses in the pool.
	// +optional
	IPAddresses *NetBoxIPPoolStatusIPAddresses `json:"ipAddresses,omitempty"`
}

// IPPoolRangeStatus is one range of a NetBoxIPPool as it is in NetBox.
type IPPoolRangeStatus struct {
	// networkName is the NetBoxNetwork the range lies in.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	NetworkName string `json:"networkName,omitempty"`

	// id is the NetBox ID of the IP range.
	// +required
	// +kubebuilder:validation:Minimum=1
	ID int64 `json:"id,omitempty"`

	// startAddress is the first address of the NetBox IP range.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=39
	StartAddress string `json:"startAddress,omitempty"`

	// endAddress is the last address of the NetBox IP range.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=39
	EndAddress string `json:"endAddress,omitempty"`

	// ready is true when claims get addresses from the range.
	// +required
	Ready bool `json:"ready"`

	// reason is why the range is not ready or not as the spec asks.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Reason string `json:"reason,omitempty"`

	// message is a human readable explanation of reason.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32768
	Message string `json:"message,omitempty"`
}

// NetBoxIPPoolStatusIPAddresses contains the count of total, used and free addresses in a pool.
type NetBoxIPPoolStatusIPAddresses struct {
	// total is the number of addresses in the ready ranges of the pool.
	// +required
	Total int32 `json:"total"`

	// used is the number of IPAddress objects of the pool in the ready ranges.
	// +required
	Used int32 `json:"used"`

	// free is the number of addresses not yet used, total minus used.
	// +required
	Free int32 `json:"free"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Total",type="integer",JSONPath=".status.ipAddresses.total",description="Count of addresses in the pool"
// +kubebuilder:printcolumn:name="Used",type="integer",JSONPath=".status.ipAddresses.used",description="Count of allocated addresses in the pool"
// +kubebuilder:printcolumn:name="Free",type="integer",JSONPath=".status.ipAddresses.free",description="Count of unallocated addresses in the pool"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="Whether the pool is reflected in NetBox"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].reason",description="Reason of the Ready condition"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time duration since creation of NetBoxIPPool"

// NetBoxIPPool is a pool of addresses held in NetBox.
type NetBoxIPPool struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is the standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state of NetBoxIPPool.
	// +required
	Spec NetBoxIPPoolSpec `json:"spec,omitempty,omitzero"`
	// status is the observed state of NetBoxIPPool.
	// +optional
	Status NetBoxIPPoolStatus `json:"status,omitempty,omitzero"`
}

// GetConditions returns the set of conditions for this object.
func (p *NetBoxIPPool) GetConditions() []metav1.Condition { return p.Status.Conditions }

// SetConditions sets conditions for an API object.
func (p *NetBoxIPPool) SetConditions(c []metav1.Condition) { p.Status.Conditions = c }

// +kubebuilder:object:root=true

// NetBoxIPPoolList is a list of NetBoxIPPools.
type NetBoxIPPoolList struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is the standard list's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#lists-and-simple-kinds
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	// items is the list of NetBoxIPPools.
	Items []NetBoxIPPool `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &NetBoxIPPool{}, &NetBoxIPPoolList{})
}
