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

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	lpmv1 "my.domain/guestbook/api/v1"
	"my.domain/guestbook/internal/agent"
)

// ContainerCheckpointReconciler reconciles a ContainerCheckpoint object
type ContainerCheckpointReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Agent  agent.Client
}

// +kubebuilder:rbac:groups=lpm.my.domain,resources=containercheckpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=lpm.my.domain,resources=containercheckpoints/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=lpm.my.domain,resources=containercheckpoints/finalizers,verbs=update
// +kubebuilder:rbac:groups=lpm.my.domain,resources=containercheckpointcontents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=lpm.my.domain,resources=containercheckpointcontents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

func (r *ContainerCheckpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var containerCheckpoint lpmv1.ContainerCheckpoint
	if err := r.Get(ctx, req.NamespacedName, &containerCheckpoint); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if containerCheckpoint.Status.Phase == "" {
		containerCheckpoint.Status.Phase = lpmv1.ContainerCheckpointPhasePending
	}

	switch containerCheckpoint.Status.Phase {
	case lpmv1.ContainerCheckpointPhasePending:
		return r.handlePendingPhase(ctx, &containerCheckpoint)
	case lpmv1.ContainerCheckpointPhaseRunning:
		return r.handleCheckpointingPhase(ctx, &containerCheckpoint)
	case lpmv1.ContainerCheckpointPhaseSucceeded, lpmv1.ContainerCheckpointPhaseFailed:
		return r.handleCompletedOrFailedPhase(ctx, &containerCheckpoint)
	default:
		logger.Info("Unknown phase, nothing to do", "phase", containerCheckpoint.Status.Phase)
		return ctrl.Result{}, nil
	}
}

func (r *ContainerCheckpointReconciler) handlePendingPhase(ctx context.Context, containerCheckpoint *lpmv1.ContainerCheckpoint) (ctrl.Result, error) {
	// Get the source pod
	srcPod := &corev1.Pod{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: containerCheckpoint.Namespace,
		Name:      containerCheckpoint.Spec.PodName,
	}, srcPod)

	// Return failure if pod not found
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.updatePhase(ctx, containerCheckpoint, lpmv1.ContainerCheckpointPhaseFailed, "pod not found")
		}
		return ctrl.Result{}, err
	}

	// Verify container exists in pod
	containerExists := false
	for _, container := range srcPod.Spec.Containers {
		if container.Name == containerCheckpoint.Spec.ContainerName {
			containerExists = true
			break
		}
	}
	if !containerExists {
		return ctrl.Result{}, r.updatePhase(ctx, containerCheckpoint, lpmv1.ContainerCheckpointPhaseFailed, "container not found in pod")
	}

	// Ensure pod is running
	if srcPod.Status.Phase != corev1.PodRunning {
		return ctrl.Result{}, r.updatePhase(ctx, containerCheckpoint, lpmv1.ContainerCheckpointPhaseFailed, "pod not running")
	}

	// Update status to running phase
	containerCheckpoint.Status.Phase = lpmv1.ContainerCheckpointPhaseRunning
	containerCheckpoint.Status.Message = "checkpointing container"
	return ctrl.Result{}, r.Status().Update(ctx, containerCheckpoint)
}

func (r *ContainerCheckpointReconciler) handleCheckpointingPhase(ctx context.Context, containerCheckpoint *lpmv1.ContainerCheckpoint) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if ContainerCheckpointContent exists (created by agent asynchronously)
	contentName := containerCheckpoint.Name
	containerCheckpointContent := &lpmv1.ContainerCheckpointContent{}
	err := r.Get(ctx, client.ObjectKey{Name: contentName}, containerCheckpointContent)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// Content not created yet by agent, keep waiting
			logger.Info("Waiting for ContainerCheckpointContent to be created", "name", contentName)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// Content exists, check for error annotation
	if errMsg, hasError := containerCheckpointContent.Annotations["error"]; hasError {
		logger.Info("Checkpoint failed", "error", errMsg)
		now := metav1.Now()
		containerCheckpoint.Status.Phase = lpmv1.ContainerCheckpointPhaseFailed
		containerCheckpoint.Status.Message = "checkpointing failed: " + errMsg
		containerCheckpoint.Status.Ready = false
		containerCheckpoint.Status.CompletionTime = &now
		return ctrl.Result{}, r.Status().Update(ctx, containerCheckpoint)
	}

	// Track checkpoint success
	logger.Info("Checkpoint succeeded", "artifactURI", containerCheckpointContent.Spec.ArtifactURI)
	now := metav1.Now()
	containerCheckpoint.Status.BoundContentName = containerCheckpointContent.Name
	containerCheckpoint.Status.Ready = true
	containerCheckpoint.Status.Phase = lpmv1.ContainerCheckpointPhaseSucceeded
	containerCheckpoint.Status.Message = "done"
	containerCheckpoint.Status.CompletionTime = &now
	return ctrl.Result{}, r.Status().Update(ctx, containerCheckpoint)
}

func (r *ContainerCheckpointReconciler) handleCompletedOrFailedPhase(ctx context.Context, checkpoint *lpmv1.ContainerCheckpoint) (ctrl.Result, error) {
	// Logic to handle the Succeeded or Failed phase
	return ctrl.Result{}, nil
}

func (r *ContainerCheckpointReconciler) updatePhase(ctx context.Context, containerCheckpoint *lpmv1.ContainerCheckpoint, phase lpmv1.ContainerCheckpointPhase, message string) error {
	containerCheckpoint.Status.Phase = phase
	containerCheckpoint.Status.Message = message
	return r.Status().Update(ctx, containerCheckpoint)
}

func (r *ContainerCheckpointReconciler) performContainerCheckpoint(ctx context.Context, containerCheckpoint *lpmv1.ContainerCheckpoint) (string, error) {
	// Get the pod to extract node name and UID
	pod := &corev1.Pod{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: containerCheckpoint.Namespace,
		Name:      containerCheckpoint.Spec.PodName,
	}, pod)
	if err != nil {
		return "", fmt.Errorf("failed to get pod %s/%s: %w", containerCheckpoint.Namespace, containerCheckpoint.Spec.PodName, err)
	}

	// Ensure pod is scheduled to a node
	if pod.Spec.NodeName == "" {
		return "", fmt.Errorf("pod %s/%s is not scheduled to any node", containerCheckpoint.Namespace, containerCheckpoint.Spec.PodName)
	}

	// Call the agent to perform the container checkpoint operation
	return r.Agent.CheckpointContainer(ctx,
		pod.Spec.NodeName,
		containerCheckpoint.Namespace,
		containerCheckpoint.Spec.PodName,
		containerCheckpoint.Spec.ContainerName,
		string(pod.UID),
	)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ContainerCheckpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&lpmv1.ContainerCheckpoint{}).
		Owns(&lpmv1.ContainerCheckpointContent{}).
		Named("containercheckpoint").
		Complete(r)
}
