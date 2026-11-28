/*
Copyright 2025.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MonitorSpec defines the desired state of Monitor
type MonitorSpec struct {
	// Target is what the monitor checks: a URL for http, a host for ping and dns, host:port for tcp
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Target string `json:"target"`

	// Interval between two checks, in seconds
	// +kubebuilder:validation:Enum="30";"60";"120";"300";"600"
	Interval string `json:"interval"`

	// Type of the monitor
	// +kubebuilder:validation:Enum=http;ping;tcp;dns
	Type string `json:"type"`
}

// MonitorStatus defines the observed state of Monitor.
type MonitorStatus struct {
	// ExternalID is the ID of the monitor in Upbot
	// +optional
	ExternalID string `json:"externalID,omitempty"`

	// ObservedGeneration is the generation last sent to Upbot
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastSyncTime is when the monitor in Upbot was last checked against the spec
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// Conditions hold the Ready condition, with the Upbot API error when the sync fails
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target`
// +kubebuilder:printcolumn:name="Interval",type=string,JSONPath=`.spec.interval`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="ID",type=string,JSONPath=`.status.externalID`
// +kubebuilder:printcolumn:name="Synced",type=date,JSONPath=`.status.lastSyncTime`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Monitor is the Schema for the monitors API
type Monitor struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Monitor
	// +required
	Spec MonitorSpec `json:"spec"`

	// status defines the observed state of Monitor
	// +optional
	Status MonitorStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// MonitorList contains a list of Monitor
type MonitorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Monitor `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Monitor{}, &MonitorList{})
}
