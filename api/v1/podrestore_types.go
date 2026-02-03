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

package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodRestorePhase is the lifecycle phase of a PodRestore.
type PodRestorePhase string

const (
	PodRestorePhasePending   PodRestorePhase = "Pending"
	PodRestorePhasePreparing PodRestorePhase = "PreparingImages"
	PodRestorePhaseRestoring PodRestorePhase = "Restoring"
	PodRestorePhaseSucceeded PodRestorePhase = "Succeeded"
	PodRestorePhaseFailed    PodRestorePhase = "Failed"
)

// StatefulSetRestoreInfo contains information needed for StatefulSet pod restoration.
type StatefulSetRestoreInfo struct {
	// OriginalPodUID stores the UID of the original pod to distinguish it
	// from the recreated pod during StatefulSet migration.
	OriginalPodUID string `json:"originalPodUID"`
}

// PodRestoreSpec defines the desired state of PodRestore.
type PodRestoreSpec struct {
	// PodCheckpointContentRef references the PodCheckpointContent that holds
	// checkpoint artifacts and the podSpecSnapshot used for restore.
	PodCheckpointContentRef corev1.LocalObjectReference `json:"podCheckpointContentRef"`

	// TargetNode optionally pins the restored Pod to a specific node.
	// If empty, the scheduler will decide placement.
	TargetNode string `json:"targetNode,omitempty"`

	// RestoredPodName is the name to assign to the restored Pod.
	RestoredPodName string `json:"restoredPodName,omitempty"`

	// IsStatefulSet indicates whether this is a StatefulSet pod restoration.
	// When true, the controller will patch the StatefulSet template and manage
	// pod recreation through the StatefulSet controller.
	IsStatefulSet bool `json:"isStatefulSet,omitempty"`
}

// PodRestoreStatus defines the observed state of PodRestore.
type PodRestoreStatus struct {
	// Phase is the high-level lifecycle state.
	Phase PodRestorePhase `json:"phase,omitempty"`

	// Message is a human-readable status or error.
	Message string `json:"message,omitempty"`

	// StartTime indicates when restore began.
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime indicates when restore finished (on terminal states).
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// RestoredPodName is the name of the Pod created by the restore controller.
	RestoredPodName string `json:"restoredPodName,omitempty"`

	// ImageMapping maps container name -> prepared image reference used during restore.
	ImageMapping map[string]string `json:"imageMapping,omitempty"`

	// StatefulSetRestore contains information needed to restore the original StatefulSet
	// template after migration completion. This field is only populated for StatefulSet pods.
	StatefulSetRestore *StatefulSetRestoreInfo `json:"statefulSetRestore,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="PodCheckpointContentName",type=string,JSONPath=`.spec.podCheckpointContentRef.name`,description="Referenced PodCheckpointContent"
// +kubebuilder:printcolumn:name="TargetNode",type=string,JSONPath=`.spec.targetNode`,description="Target node for restore"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`,description="Current phase of the restore"

// PodRestore is the Schema for the podrestores API
type PodRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PodRestoreSpec   `json:"spec,omitempty"`
	Status PodRestoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PodRestoreList contains a list of PodRestore
type PodRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodRestore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PodRestore{}, &PodRestoreList{})
}
