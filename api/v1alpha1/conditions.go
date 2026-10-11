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

// ReadyCondition is true once the object is fully reflected in NetBox.
const ReadyCondition = "Ready"

// MetadataSyncedCondition is true once spec.customFields and spec.tags are applied to the NetBox prefix, and spec.owner,
// spec.tenant and the description and comments to the prefix and the VIP. It never changes Ready.
const MetadataSyncedCondition = "MetadataSynced"

// ControlPlaneEndpointReadyCondition is true once the VIP is reserved in NetBox and Cluster.spec.controlPlaneEndpoint is the VIP.
// False with WaitingForCluster until the Cluster exists, EndpointConflict when the Cluster has another endpoint or the address
// cannot be the VIP, Exhausted when no address is left for it. It never changes Ready.
const ControlPlaneEndpointReadyCondition = "ControlPlaneEndpointReady"

// RangesReadyCondition is true when every range of a NetBoxIPPool is in NetBox as the spec asks.
const RangesReadyCondition = "RangesReady"

// Reasons of the Ready condition.
const (
	ReasonReady           = "Ready"
	ReasonSynced          = "Synced"
	ReasonNetBoxError     = "NetBoxError"
	ReasonParentNotFound  = "ParentNotFound"
	ReasonExhausted       = "Exhausted"
	ReasonPrefixMismatch  = "PrefixMismatch"
	ReasonPrefixNotFound  = "PrefixNotFound"
	ReasonDuplicate       = "Duplicate"
	ReasonNetworkNotReady = "NetworkNotReady"
	ReasonMixedNetworks   = "MixedNetworks"
	ReasonInvalidRange    = "InvalidRange"
	ReasonInvalidSpec     = "InvalidSpec"
	ReasonRangeInUse      = "RangeInUse"
	ReasonRangeOverlap    = "RangeOverlap"

	ReasonDeleting            = "Deleting"
	ReasonWaitingForPools     = "WaitingForPools"
	ReasonWaitingForAddresses = "WaitingForAddresses"
	ReasonWaitingForPrefix    = "WaitingForPrefix"
	ReasonWaitingForCluster   = "WaitingForCluster"
	ReasonEndpointConflict    = "EndpointConflict"
	ReasonForbidden           = "Forbidden"
	ReasonOwnerNotFound       = "OwnerNotFound"
	ReasonTenantNotFound      = "TenantNotFound"
	ReasonInternalError       = "InternalError"
	ReasonReleasing           = "Releasing"
	ReasonReleaseFailed       = "ReleaseFailed"
)
