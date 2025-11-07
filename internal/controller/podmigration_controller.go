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
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	lpmv1 "my.domain/guestbook/api/v1"
	"my.domain/guestbook/internal/agent"
	"my.domain/guestbook/internal/utils"
)

// PodMigrationReconciler reconciles a PodMigration object
type PodMigrationReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	AgentClient *agent.Client
	Puller      utils.Puller
}

// +kubebuilder:rbac:groups=lpm.my.domain,resources=podmigrations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=lpm.my.domain,resources=podmigrations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=lpm.my.domain,resources=podmigrations/finalizers,verbs=update
// +kubebuilder:rbac:groups=lpm.my.domain,resources=podcheckpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=lpm.my.domain,resources=podcheckpointcontents,verbs=get;list;watch
// +kubebuilder:rbac:groups=lpm.my.domain,resources=containercheckpointcontents,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;update;patch

func NewPodMigrationReconciler(c client.Client, scheme *runtime.Scheme) *PodMigrationReconciler {
	return &PodMigrationReconciler{
		Client:      c,
		Scheme:      scheme,
		AgentClient: agent.NewClient(c),
		Puller:      utils.NewEphemeralPuller(c, "default"),
	}
}

func (r *PodMigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var podMigration lpmv1.PodMigration
	if err := r.Get(ctx, req.NamespacedName, &podMigration); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if podMigration.Status.Phase == "" {
		podMigration.Status.Phase = lpmv1.MigrationPhasePrePullImages
	}

	switch podMigration.Status.Phase {
	case lpmv1.MigrationPhasePrePullImages:
		return r.handlePrePullImagesPhase(ctx, &podMigration)
	case lpmv1.MigrationPhasePending:
		return r.handlePendingPhase(ctx, &podMigration)
	case lpmv1.MigrationPhaseCheckpointing:
		return r.handleCheckpointingPhase(ctx, &podMigration)
	case lpmv1.MigrationPhaseCheckpointComplete:
		return r.handleCheckpointCompletePhase(ctx, &podMigration)
	case lpmv1.MigrationPhaseRestoring:
		return r.handleRestoringPhase(ctx, &podMigration)
	case lpmv1.MigrationPhaseSucceeded, lpmv1.MigrationPhaseFailed:
		return r.handleCompletedOrFailedPhase(ctx, &podMigration)
	default:
		logger.Info("Unknown phase, nothing to do", "phase", podMigration.Status.Phase)
		return ctrl.Result{}, nil
	}
}

func (r *PodMigrationReconciler) handlePrePullImagesPhase(ctx context.Context, podMigration *lpmv1.PodMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling PrePullImages phase for PodMigration", "name", podMigration.Name)

	// Trigger image pulling if not already started
	if podMigration.Status.EphemeralPullPodName == "" {
		originalPod, err := r.getOriginalPod(ctx, podMigration)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get original pod: %w", err)
		}

		// Collect all images from containers and initContainers
		imagesToPull := []string{}
		for _, c := range append(originalPod.Spec.Containers, originalPod.Spec.InitContainers...) {
			imagesToPull = append(imagesToPull, c.Image)
		}

		podName, err := r.Puller.PullImages(ctx, podMigration.Spec.TargetNode, imagesToPull)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create pull pod: %w", err)
		}

		podMigration.Status.EphemeralPullPodName = podName
		if err := r.Status().Update(ctx, podMigration); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update podMigration status: %w", err)
		}
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
	}

	// Check pull status
	pullStatus, err := r.Puller.CheckPullStatusAndCleanup(ctx, podMigration.Status.EphemeralPullPodName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check pull pod status: %w", err)
	}

	switch pullStatus {
	case utils.PullSucceeded:
		podMigration.Status.Phase = lpmv1.MigrationPhasePending
		podMigration.Status.Message = "Base images pulled successfully"
	case utils.PullFailed:
		podMigration.Status.Phase = lpmv1.MigrationPhaseFailed
		podMigration.Status.Message = "Base images pull failed"
	case utils.PullPending:
		// Still pulling, requeue
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
	}

	// Update status once if needed
	if err := r.Status().Update(ctx, podMigration); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update podMigration status: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *PodMigrationReconciler) handlePendingPhase(ctx context.Context, podMigration *lpmv1.PodMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	logger.Info("Handling Pending phase for PodMigration", "name", podMigration.Name)

	// 1. Validate source Pod exists
	var srcPod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: podMigration.Namespace, Name: podMigration.Spec.PodName}, &srcPod); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, "source pod not found")
		}
		return ctrl.Result{}, err
	}

	// 2. Validate source Pod running
	if srcPod.Status.Phase != corev1.PodRunning {
		return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, "source pod not running")
	}

	// 2.5. Detect StatefulSet pod
	if !podMigration.Spec.IsStatefulSet {
		if owner := metav1.GetControllerOf(&srcPod); owner != nil && owner.Kind == "StatefulSet" {
			podMigration.Spec.IsStatefulSet = true
			if err := r.Update(ctx, podMigration); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to update StatefulSet flag: %w", err)
			}
			logger.Info("Detected StatefulSet pod, updated migration spec", "pod", srcPod.Name, "statefulSet", owner.Name)
		}
	}

	// 3. If target node requested, validate it exists
	if podMigration.Spec.TargetNode != "" {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: podMigration.Spec.TargetNode}, &node); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, "target node not found")
			}
			return ctrl.Result{}, err
		}
	}

	// 4/5. Ensure PodCheckpoint exists and update status accordingly
	checkpointName := podMigration.Name + "-" + strconv.FormatInt(time.Now().Unix(), 10)
	var podCheckpoint lpmv1.PodCheckpoint
	err := r.Get(ctx, client.ObjectKey{Namespace: podMigration.Namespace, Name: checkpointName}, &podCheckpoint)

	if apierrors.IsNotFound(err) {
		// Create new checkpoint
		podCheckpoint = lpmv1.PodCheckpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:      checkpointName,
				Namespace: podMigration.Namespace,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(podMigration, lpmv1.GroupVersion.WithKind("PodMigration")),
				},
			},
			Spec: lpmv1.PodCheckpointSpec{
				PodName: &podMigration.Spec.PodName,
			},
		}
		if err := r.Create(ctx, &podCheckpoint); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("PodCheckpoint created from Pending phase", "name", podCheckpoint.Name)

		podMigration.Status.PodCheckpointRef = &corev1.LocalObjectReference{Name: checkpointName}
		podMigration.Status.Phase = lpmv1.MigrationPhaseCheckpointing
		podMigration.Status.Message = "checkpoint requested"
		if err := r.Status().Update(ctx, podMigration); err != nil {
			return ctrl.Result{}, err
		}
		// requeue soon to start monitoring
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}

	// checkpoint already exists
	if podMigration.Status.PodCheckpointRef == nil {
		podMigration.Status.PodCheckpointRef = &corev1.LocalObjectReference{Name: podCheckpoint.Name}
	}
	podMigration.Status.Phase = lpmv1.MigrationPhaseCheckpointing
	podMigration.Status.Message = "checkpoint in progress"
	if err := r.Status().Update(ctx, podMigration); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *PodMigrationReconciler) handleCheckpointingPhase(ctx context.Context, podMigration *lpmv1.PodMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling Checkpointing phase for PodMigration", "name", podMigration.Name)

	// Determine PodCheckpoint name: status ref if set, else fall back to migration name
	podCheckpointName := podMigration.Name
	if podMigration.Status.PodCheckpointRef != nil && podMigration.Status.PodCheckpointRef.Name != "" {
		podCheckpointName = podMigration.Status.PodCheckpointRef.Name
	}

	var podCheckpoint lpmv1.PodCheckpoint
	err := r.Get(ctx, client.ObjectKey{Namespace: podMigration.Namespace, Name: podCheckpointName}, &podCheckpoint)

	if apierrors.IsNotFound(err) {
		// Re-create checkpoint request
		podCheckpoint = lpmv1.PodCheckpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podCheckpointName,
				Namespace: podMigration.Namespace,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(podMigration, lpmv1.GroupVersion.WithKind("PodMigration")),
				},
			},
			Spec: lpmv1.PodCheckpointSpec{
				PodName: &podMigration.Spec.PodName,
			},
		}
		if err := r.Create(ctx, &podCheckpoint); err != nil {
			return ctrl.Result{}, err
		}
		if podMigration.Status.PodCheckpointRef == nil {
			podMigration.Status.PodCheckpointRef = &corev1.LocalObjectReference{Name: podCheckpointName}
			if err := r.Status().Update(ctx, podMigration); err != nil {
				return ctrl.Result{}, err
			}
		}
		logger.Info("PodCheckpoint (re)created", "name", podCheckpointName)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}

	// Switch based on checkpoint status
	switch podCheckpoint.Status.Phase {
	case lpmv1.PodCheckpointPhaseFailed:
		return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, "checkpoint failed: "+podCheckpoint.Status.Message)

	case lpmv1.PodCheckpointPhaseSucceeded:
		// Ensure checkpoint is truly ready
		if podCheckpoint.Status.Ready {
			podMigration.Status.Phase = lpmv1.MigrationPhaseCheckpointComplete
			podMigration.Status.Message = "checkpoint complete"
			if err := r.Status().Update(ctx, podMigration); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		// Not ready yet; fallthrough to default requeue
	}

	// Pending / Running / default
	logger.Info("Checkpoint in progress", "phase", podCheckpoint.Status.Phase)
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

func (r *PodMigrationReconciler) handleCheckpointCompletePhase(ctx context.Context, podMigration *lpmv1.PodMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling CheckpointComplete phase for PodMigration", "name", podMigration.Name)

	// Move to restoring phase
	podMigration.Status.Phase = lpmv1.MigrationPhaseRestoring
	podMigration.Status.Message = "restoring from checkpoint"
	if err := r.Status().Update(ctx, podMigration); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
}

func (r *PodMigrationReconciler) handlePreparingImagesPhase(ctx context.Context, podMigration *lpmv1.PodMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling PreparingImages phase for PodMigration", "name", podMigration.Name)

	// Get checkpoint content to find container checkpoints
	checkpointContent, err := r.getCheckpointContent(ctx, podMigration)
	if err != nil {
		return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, fmt.Sprintf("failed to get checkpoint content: %v", err))
	}

	// Get original pod to know what containers we need images for
	var originalPod corev1.Pod
	err = r.Get(ctx, client.ObjectKey{
		Namespace: podMigration.Namespace,
		Name:      podMigration.Spec.PodName,
	}, &originalPod)
	if err != nil {
		return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, fmt.Sprintf("failed to get original pod: %v", err))
	}

	// Convert all container checkpoints to OCI images
	if podMigration.Status.CheckpointImages == nil {
		podMigration.Status.CheckpointImages = make(map[string]string)
	}

	imagesReady := true
	for _, container := range originalPod.Spec.Containers {
		// Check if image already prepared
		if _, exists := podMigration.Status.CheckpointImages[container.Name]; exists {
			continue
		}

		// Get checkpoint path for this container
		checkpointPath := r.getCheckpointPathForContainer(ctx, checkpointContent, container.Name)
		if checkpointPath == "" {
			return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, fmt.Sprintf("no checkpoint found for container %s", container.Name))
		}

		// Convert to OCI image
		checkpointImage, err := r.convertToOCIImage(ctx, checkpointPath, container.Name, podMigration.Spec.TargetNode)
		if err != nil {
			logger.Error(err, "Failed to convert checkpoint to OCI image", "container", container.Name)
			imagesReady = false
			continue
		}

		// Store the image reference
		podMigration.Status.CheckpointImages[container.Name] = checkpointImage
		logger.Info("Checkpoint image prepared", "container", container.Name, "image", checkpointImage)
	}

	// Update status with current image state
	if err := r.Status().Update(ctx, podMigration); err != nil {
		return ctrl.Result{}, err
	}

	// If all images are ready, move to restoring phase
	if imagesReady && len(podMigration.Status.CheckpointImages) == len(originalPod.Spec.Containers) {
		podMigration.Status.Phase = lpmv1.MigrationPhaseRestoring
		podMigration.Status.Message = "checkpoint images ready, creating restored pod"
		if err := r.Status().Update(ctx, podMigration); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Still preparing images, requeue to continue
	return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
}

func (r *PodMigrationReconciler) handleRestoringPhase(ctx context.Context, podMigration *lpmv1.PodMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling Restoring phase for PodMigration", "name", podMigration.Name, "isStatefulSet", podMigration.Spec.IsStatefulSet)

	// Validate PodCheckpointRef
	podCheckpointRef := podMigration.Status.PodCheckpointRef
	if podCheckpointRef == nil || podCheckpointRef.Name == "" {
		return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, "missing PodCheckpointRef in status")
	}

	podCheckpointName := podCheckpointRef.Name
	podRestoreName := podCheckpointName + "-restore"

	var podRestore lpmv1.PodRestore
	err := r.Get(ctx, client.ObjectKey{Namespace: podMigration.Namespace, Name: podRestoreName}, &podRestore)

	if apierrors.IsNotFound(err) {
		// Re-create restore request
		podRestore = lpmv1.PodRestore{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podRestoreName,
				Namespace: podMigration.Namespace,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(podMigration, lpmv1.GroupVersion.WithKind("PodMigration")),
				},
			},
			Spec: lpmv1.PodRestoreSpec{
				PodCheckpointContentRef: corev1.LocalObjectReference{Name: podCheckpointName},
				TargetNode:              podMigration.Spec.TargetNode,
				IsStatefulSet:           podMigration.Spec.IsStatefulSet,
			},
		}
		if err := r.Create(ctx, &podRestore); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("PodRestore created", "name", podRestoreName)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}

	// Switch based on restore status
	switch podRestore.Status.Phase {
	case lpmv1.PodRestorePhaseFailed:
		return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, "restore failed: "+podRestore.Status.Message)

	case lpmv1.PodRestorePhaseSucceeded:
			podMigration.Status.Phase = lpmv1.MigrationPhaseSucceeded
		podMigration.Status.Message = "pod successfully restored"
			if err := r.Status().Update(ctx, podMigration); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
	}

	// Pending / Preparing / Restoring / default
	logger.Info("Restore in progress", "phase", podRestore.Status.Phase)
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

func (r *PodMigrationReconciler) handlePodRestoration(ctx context.Context, podMigration *lpmv1.PodMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling pod restoration", "podName", podMigration.Spec.PodName)

	// Create restored pod if not already created
	if podMigration.Status.RestoredPodName == "" {
		restoredPod, err := r.createRestoredPod(ctx, podMigration)
		if err != nil {
			return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, fmt.Sprintf("failed to create restored pod: %v", err))
		}

		err = r.Create(ctx, restoredPod)
		if err != nil {
			if apierrors.IsAlreadyExists(err) {
				logger.Info("Restored pod already exists", "pod", restoredPod.Name)
			} else {
				return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, fmt.Sprintf("failed to create restored pod: %v", err))
			}
		}

		// Update status with restored pod name
		podMigration.Status.RestoredPodName = restoredPod.Name
		podMigration.Status.Message = "restored pod created"
		if err := r.Status().Update(ctx, podMigration); err != nil {
			return ctrl.Result{}, err
		}

		logger.Info("Restored pod created", "pod", restoredPod.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Check restored pod status
	var restoredPod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{
		Name:      podMigration.Status.RestoredPodName,
		Namespace: podMigration.Namespace,
	}, &restoredPod)

	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, "restored pod not found")
		}
		return ctrl.Result{}, err
	}

	// Check pod status
	switch restoredPod.Status.Phase {
	case corev1.PodRunning:
		// Delete original pod after successful restoration
		if err := r.deleteOriginalPod(ctx, podMigration); err != nil {
			logger.Error(err, "Failed to delete original pod, but migration succeeded")
		}
		return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseSucceeded, "pod successfully restored and running")

	case corev1.PodFailed:
		return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, "restored pod failed to start")

	case corev1.PodPending:
		logger.Info("Restored pod is pending", "pod", restoredPod.Name, "reason", restoredPod.Status.Reason)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil

	default:
		logger.Info("Restored pod in progress", "pod", restoredPod.Name, "phase", restoredPod.Status.Phase)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
}

func (r *PodMigrationReconciler) handleStatefulSetRestoration(ctx context.Context, podMigration *lpmv1.PodMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling StatefulSet pod restoration", "podName", podMigration.Spec.PodName)

	var srcPod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: podMigration.Namespace, Name: podMigration.Spec.PodName}, &srcPod); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Pod not found during StatefulSet restoration", "pod", podMigration.Spec.PodName)
			podMigration.Status.Message = "waiting for StatefulSet to recreate pod"
			if statusErr := r.Status().Update(ctx, podMigration); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// Patch StatefulSet template if not already done
	if podMigration.Status.StatefulSetRestore == nil {
		if err := r.patchStatefulSetTemplate(ctx, &srcPod, podMigration); err != nil {
			return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed,
				fmt.Sprintf("failed to patch StatefulSet: %v", err))
		}
		logger.Info("StatefulSet template patched", "statefulSet", podMigration.Status.StatefulSetRestore.Name)
	}

	// Check if this is the original pod or a recreated pod using UID
	originalPodUID := podMigration.Status.StatefulSetRestore.OriginalPodUID

	if string(srcPod.UID) == originalPodUID {
		// Delete the original pod (StatefulSet will recreate it)
		logger.Info("Deleting original pod", "pod", srcPod.Name, "uid", srcPod.UID)
		if err := r.Delete(ctx, &srcPod); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to delete original pod: %w", err)
		}

		podMigration.Status.RestoredPodName = srcPod.Name // Same ordinal name required
		podMigration.Status.Message = "original pod deletion requested, waiting for StatefulSet to recreate"
		if err := r.Status().Update(ctx, podMigration); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Pod recreated with new template, observe status
	logger.Info("Observing recreated pod", "pod", srcPod.Name, "uid", srcPod.UID, "phase", srcPod.Status.Phase)

	switch srcPod.Status.Phase {
	case corev1.PodRunning:
		// Verify that the pod is using checkpoint images
		if r.isPodUsingCheckpointImages(&srcPod, podMigration) {
			logger.Info("StatefulSet pod successfully recreated with checkpoint images", "pod", srcPod.Name)
			return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseSucceeded, "StatefulSet pod successfully restored and running")
		} else {
			logger.Error(nil, "Recreated pod is not using checkpoint images", "pod", srcPod.Name)
			return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, "recreated pod is not using checkpoint images")
		}

	case corev1.PodFailed:
		return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, "recreated pod failed to start")

	case corev1.PodPending:
		logger.Info("Recreated pod is pending", "pod", srcPod.Name, "reason", srcPod.Status.Reason)
		podMigration.Status.Message = "recreated pod is starting"
		if err := r.Status().Update(ctx, podMigration); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil

	default:
		logger.Info("Recreated pod in progress", "pod", srcPod.Name, "phase", srcPod.Status.Phase)
		podMigration.Status.Message = fmt.Sprintf("recreated pod phase: %s", srcPod.Status.Phase)
		if err := r.Status().Update(ctx, podMigration); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
}

func (r *PodMigrationReconciler) patchStatefulSetTemplate(ctx context.Context, srcPod *corev1.Pod, podMigration *lpmv1.PodMigration) error {
	logger := log.FromContext(ctx)

	owner := metav1.GetControllerOf(srcPod)
	if owner == nil {
		return fmt.Errorf("pod %s/%s has no controller", srcPod.Namespace, srcPod.Name)
	}

	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: srcPod.Namespace, Name: owner.Name}, sts); err != nil {
		return err
	}

	// Store the original template
	if podMigration.Status.StatefulSetRestore == nil {
		originalTemplateBytes, err := json.Marshal(sts.Spec.Template)
		if err != nil {
			return fmt.Errorf("failed to encode original StatefulSet template: %w", err)
		}
		originalTemplate := string(originalTemplateBytes)
		podMigration.Status.StatefulSetRestore = &lpmv1.StatefulSetRestoreInfo{
			Name:             sts.Name,
			OriginalTemplate: originalTemplate,
			OriginalPodUID:   string(srcPod.UID),
		}

		// Update the status to persist the original template
		if err := r.Status().Update(ctx, podMigration); err != nil {
			return fmt.Errorf("failed to update podMigration with original template: %w", err)
		}
		logger.Info("Stored original StatefulSet template", "statefulSet", sts.Name)
	}

	patched := sts.DeepCopy()

	// Ensure update strategy is OnDelete so deleting a single pod results in recreation
	patched.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
		Type: appsv1.OnDeleteStatefulSetStrategyType,
	}

	for i, c := range patched.Spec.Template.Spec.Containers {
		if img, ok := podMigration.Status.CheckpointImages[c.Name]; ok {
			patched.Spec.Template.Spec.Containers[i].Image = img
			patched.Spec.Template.Spec.Containers[i].ImagePullPolicy = corev1.PullNever
		}
	}

	if patched.Spec.Template.Labels == nil {
		patched.Spec.Template.Labels = map[string]string{}
	}
	patched.Spec.Template.Labels["migration-job"] = podMigration.Name

	// Constrain placement to target node (via nodeSelector)
	if podMigration.Spec.TargetNode != "" {
		if patched.Spec.Template.Spec.NodeSelector == nil {
			patched.Spec.Template.Spec.NodeSelector = map[string]string{}
		}
		patched.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] = podMigration.Spec.TargetNode
	}

	return r.Patch(ctx, patched, client.MergeFrom(sts))
}

func (r *PodMigrationReconciler) restoreStatefulSetTemplate(ctx context.Context, podMigration *lpmv1.PodMigration) error {
	logger := log.FromContext(ctx)

	if podMigration.Status.StatefulSetRestore == nil {
		logger.Info("No StatefulSet restore info stored, skipping restore")
		return nil
	}

	sts := &appsv1.StatefulSet{}
	statefulSetKey := client.ObjectKey{
		Namespace: podMigration.Namespace,
		Name:      podMigration.Status.StatefulSetRestore.Name,
	}

	if err := r.Get(ctx, statefulSetKey, sts); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("StatefulSet not found, skipping template restore", "statefulSet", statefulSetKey.Name)
			return nil
		}
		return fmt.Errorf("failed to get StatefulSet: %w", err)
	}

	var originalTemplate corev1.PodTemplateSpec
	if err := json.Unmarshal([]byte(podMigration.Status.StatefulSetRestore.OriginalTemplate), &originalTemplate); err != nil {
		return fmt.Errorf("failed to unmarshal original StatefulSet template: %w", err)
	}

	patched := sts.DeepCopy()
	patched.Spec.Template = originalTemplate

	if err := r.Patch(ctx, patched, client.MergeFrom(sts)); err != nil {
		return fmt.Errorf("failed to patch StatefulSet with original template: %w", err)
	}

	logger.Info("Successfully restored original StatefulSet template", "statefulSet", sts.Name)
	return nil
}

func (r *PodMigrationReconciler) handleCompletedOrFailedPhase(ctx context.Context, podMigration *lpmv1.PodMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if podMigration.Status.StatefulSetRestore != nil {
		if err := r.restoreStatefulSetTemplate(ctx, podMigration); err != nil {
			logger.Error(err, "Failed to restore StatefulSet template", "statefulSet", podMigration.Status.StatefulSetRestore.Name)
			// Don't fail the migration just because template restore failed
		} else {
			logger.Info("StatefulSet template restored successfully", "statefulSet", podMigration.Status.StatefulSetRestore.Name)

			// Clear the stored template since we've restored it
			podMigration.Status.StatefulSetRestore = nil
			if err := r.Status().Update(ctx, podMigration); err != nil {
				logger.Error(err, "Failed to clear StatefulSet restore info from status")
			}
		}
	}

	// No further action needed for completed migrations
	return ctrl.Result{}, nil
}

func (r *PodMigrationReconciler) updatePhase(ctx context.Context, podMigration *lpmv1.PodMigration, phase lpmv1.PodMigrationPhase, message string) error {
	podMigration.Status.Phase = phase
	podMigration.Status.Message = message
	return r.Status().Update(ctx, podMigration)
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodMigrationReconciler) createRestoredPod(ctx context.Context, podMigration *lpmv1.PodMigration) (*corev1.Pod, error) {
	// Get original pod
	originalPod, err := r.getOriginalPod(ctx, podMigration)
	if err != nil {
		return nil, fmt.Errorf("failed to get original pod: %w", err)
	}

	// START WITH THE ORIGINAL POD - preserve all runtime context
	restoredPod := originalPod.DeepCopy()

	// Change only what's absolutely necessary
	restoredPod.ObjectMeta.Name = fmt.Sprintf("%s-restored", originalPod.Name)
	restoredPod.ObjectMeta.ResourceVersion = ""              // Required for creation
	restoredPod.ObjectMeta.UID = ""                          // Required for creation
	restoredPod.Spec.NodeName = podMigration.Spec.TargetNode // Target node

	// Add migration tracking annotations
	if restoredPod.ObjectMeta.Annotations == nil {
		restoredPod.ObjectMeta.Annotations = make(map[string]string)
	}
	restoredPod.ObjectMeta.Annotations["migration.source-pod"] = originalPod.Name
	restoredPod.ObjectMeta.Annotations["migration.target-node"] = podMigration.Spec.TargetNode

	// Set owner reference
	restoredPod.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
		*metav1.NewControllerRef(podMigration, lpmv1.GroupVersion.WithKind("PodMigration")),
	}

	// Apply checkpoint images to containers (existing logic)
	if podMigration.Status.CheckpointImages == nil {
		return nil, fmt.Errorf("checkpoint images not prepared for migration")
	}

	for i, container := range restoredPod.Spec.Containers {
		checkpointImage, exists := podMigration.Status.CheckpointImages[container.Name]
		if !exists {
			return nil, fmt.Errorf("no checkpoint image prepared for container %s", container.Name)
		}

		restoredPod.Spec.Containers[i].Image = checkpointImage
		restoredPod.Spec.Containers[i].ImagePullPolicy = corev1.PullNever
	}

	return restoredPod, nil
}

func (r *PodMigrationReconciler) getCheckpointContent(ctx context.Context, podMigration *lpmv1.PodMigration) (*lpmv1.PodCheckpointContent, error) {
	if podMigration.Status.PodCheckpointRef == nil {
		return nil, fmt.Errorf("no checkpoint reference in migration status")
	}

	checkpointName := podMigration.Status.PodCheckpointRef.Name

	var podCheckpoint lpmv1.PodCheckpoint
	err := r.Get(ctx, client.ObjectKey{
		Namespace: podMigration.Namespace,
		Name:      checkpointName,
	}, &podCheckpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod checkpoint: %w", err)
	}

	if podCheckpoint.Status.BoundContentName == "" {
		return nil, fmt.Errorf("checkpoint has no bound content")
	}

	var checkpointContent lpmv1.PodCheckpointContent
	err = r.Get(ctx, client.ObjectKey{
		Namespace: podMigration.Namespace,
		Name:      podCheckpoint.Status.BoundContentName,
	}, &checkpointContent)
	if err != nil {
		return nil, fmt.Errorf("failed to get checkpoint content: %w", err)
	}

	return &checkpointContent, nil
}

func (r *PodMigrationReconciler) getCheckpointPathForContainer(ctx context.Context, checkpointContent *lpmv1.PodCheckpointContent, containerName string) string {
	for _, containerContent := range checkpointContent.Spec.ContainerContents {
		var content lpmv1.ContainerCheckpointContent
		err := r.Get(ctx, client.ObjectKey{
			Name:      containerContent.Name,
			Namespace: checkpointContent.Namespace,
		}, &content)
		if err != nil {
			continue
		}

		if strings.Contains(content.Name, containerName) {
			return content.Spec.ArtifactURI
		}
	}
	return ""
}

func (r *PodMigrationReconciler) convertToOCIImage(ctx context.Context, checkpointURI, containerName, targetNode string) (string, error) {
	if !strings.HasPrefix(checkpointURI, "shared://") {
		return checkpointURI, nil
	}

	// Generate OCI image name
	filename := strings.TrimPrefix(checkpointURI, "shared://")
	imageName := fmt.Sprintf("localhost/checkpoint:%s", strings.TrimSuffix(filename, ".tar"))

	// Use agent to convert checkpoint to OCI image
	imageRef, err := r.AgentClient.ConvertCheckpointToImage(ctx, targetNode, checkpointURI, containerName, imageName)
	if err != nil {
		return "", fmt.Errorf("failed to convert checkpoint to OCI image: %w", err)
	}

	return imageRef, nil
}

func (r *PodMigrationReconciler) getOriginalPod(ctx context.Context, podMigration *lpmv1.PodMigration) (*corev1.Pod, error) {
	var originalPod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{
		Namespace: podMigration.Namespace,
		Name:      podMigration.Spec.PodName,
	}, &originalPod)

	return &originalPod, err
}

func (r *PodMigrationReconciler) isPodUsingCheckpointImages(pod *corev1.Pod, podMigration *lpmv1.PodMigration) bool {
	if podMigration.Status.CheckpointImages == nil {
		return false
	}

	for _, container := range pod.Spec.Containers {
		expectedImage, exists := podMigration.Status.CheckpointImages[container.Name]
		if !exists {
			return false
		}
		if container.Image != expectedImage {
			return false
		}
	}

	return true
}

func (r *PodMigrationReconciler) deleteOriginalPod(ctx context.Context, podMigration *lpmv1.PodMigration) error {
	var originalPod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{
		Namespace: podMigration.Namespace,
		Name:      podMigration.Spec.PodName,
	}, &originalPod)

	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get original pod for deletion: %w", err)
	}

	err = r.Delete(ctx, &originalPod)
	if err != nil {
		return fmt.Errorf("failed to delete original pod: %w", err)
	}

	return nil
}

func (r *PodMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&lpmv1.PodMigration{}).
		Named("podmigration").
		Complete(r)
}
