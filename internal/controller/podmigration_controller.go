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
	"strconv"
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
	case lpmv1.MigrationPhaseFailed:
		return r.handleFailedPhase(ctx, &podMigration)
	case lpmv1.MigrationPhaseSucceeded:
		return r.handleCompletedPhase(ctx, &podMigration)
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
	if !podMigration.Status.IsStatefulSet {
		if owner := metav1.GetControllerOf(&srcPod); owner != nil && owner.Kind == "StatefulSet" {
			podMigration.Status.IsStatefulSet = true
			if err := r.Status().Update(ctx, podMigration); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to update StatefulSet flag: %w", err)
			}
			logger.Info("Detected StatefulSet pod, updated migration status", "pod", srcPod.Name, "statefulSet", owner.Name)
		}
	}

	// Add annotation to freeze restart if deferTermination is not set
	if !podMigration.Spec.DeferTermination {
		if srcPod.Annotations == nil || srcPod.Annotations["migration.my.domain/freeze-restart"] != "true" {
			patch := client.MergeFrom(srcPod.DeepCopy())

			if srcPod.Annotations == nil {
				srcPod.Annotations = make(map[string]string)
			}
			srcPod.Annotations["migration.my.domain/freeze-restart"] = "true"

			if err := r.Patch(ctx, &srcPod, patch); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to apply freeze annotation: %w", err)
			}

			logger.Info("Applied freeze-restart annotation to source pod", "pod", srcPod.Name)

			// Requeue to give the Kubelet time to see the annotation
			// If you proceed to checkpoint immediately, the Kubelet might not have synced this update yet.
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
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
				PodName:          &podMigration.Spec.PodName,
				DeferTermination: podMigration.Spec.DeferTermination,
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
				PodName:          &podMigration.Spec.PodName,
				DeferTermination: podMigration.Spec.DeferTermination,
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

func (r *PodMigrationReconciler) handleRestoringPhase(ctx context.Context, podMigration *lpmv1.PodMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling Restoring phase for PodMigration", "name", podMigration.Name, "isStatefulSet", podMigration.Status.IsStatefulSet)

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
		var podCheckpoint lpmv1.PodCheckpoint
		if err := r.Get(ctx, client.ObjectKey{Namespace: podMigration.Namespace, Name: podCheckpointName}, &podCheckpoint); err != nil {
			return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, fmt.Sprintf("failed to get PodCheckpoint %s: %v", podCheckpointName, err))
		}
		if podCheckpoint.Status.BoundContentName == "" {
			return ctrl.Result{}, r.updatePhase(ctx, podMigration, lpmv1.MigrationPhaseFailed, "PodCheckpointContent has no bound content")
		}

		// Create restore request
		podRestore = lpmv1.PodRestore{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podRestoreName,
				Namespace: podMigration.Namespace,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(podMigration, lpmv1.GroupVersion.WithKind("PodMigration")),
				},
			},
			Spec: lpmv1.PodRestoreSpec{
				PodCheckpointContentRef: corev1.LocalObjectReference{Name: podCheckpoint.Status.BoundContentName},
				TargetNode:              podMigration.Spec.TargetNode,
				IsStatefulSet:           podMigration.Status.IsStatefulSet,
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

func (r *PodMigrationReconciler) handleFailedPhase(ctx context.Context, podMigration *lpmv1.PodMigration) (ctrl.Result, error) {
	originalPod, err := r.getOriginalPod(ctx, podMigration)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get original pod in Failed Phase: %w", err)
	}

	// Remove freezeRestart annotation if present to try and recover from failure
	if originalPod.Annotations != nil {
		if _, exists := originalPod.Annotations["migration.my.domain/freeze-restart"]; exists {
			patchBase := client.MergeFrom(originalPod.DeepCopy())
			delete(originalPod.Annotations, "migration.my.domain/freeze-restart")
			if err := r.Patch(ctx, originalPod, patchBase); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to remove freeze annotation: %w", err)
			}

			log.FromContext(ctx).Info("Removed freeze annotation from source pod during failure recovery", "pod", originalPod.Name)
		}
	}

	return ctrl.Result{}, nil
}

func (r *PodMigrationReconciler) handleCompletedPhase(ctx context.Context, podMigration *lpmv1.PodMigration) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Do not cleanup pods belonging to StatefulSets.
	// Deletion behaviour is built into StatefulSet migration flow within PodRestore controller.
	if !podMigration.Status.IsStatefulSet && !podMigration.Spec.RetainOriginalPod {
		if err := r.deleteOriginalPod(ctx, podMigration); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Original pod deleted after migration", "pod", podMigration.Spec.PodName)
	}

	return ctrl.Result{}, nil
}

func (r *PodMigrationReconciler) deleteOriginalPod(ctx context.Context, podMigration *lpmv1.PodMigration) error {
	var originalPod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: podMigration.Namespace, Name: podMigration.Spec.PodName}, &originalPod)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get original pod for deletion: %w", err)
		}
		return nil
	}
	if err := r.Delete(ctx, &originalPod); err != nil {
		return fmt.Errorf("failed to delete original pod: %w", err)
	}
	return nil
}

func (r *PodMigrationReconciler) updatePhase(ctx context.Context, podMigration *lpmv1.PodMigration, phase lpmv1.PodMigrationPhase, message string) error {
	podMigration.Status.Phase = phase
	podMigration.Status.Message = message
	return r.Status().Update(ctx, podMigration)
}

func (r *PodMigrationReconciler) getOriginalPod(ctx context.Context, podMigration *lpmv1.PodMigration) (*corev1.Pod, error) {
	var originalPod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{
		Namespace: podMigration.Namespace,
		Name:      podMigration.Spec.PodName,
	}, &originalPod)

	return &originalPod, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&lpmv1.PodMigration{}).
		Named("podmigration").
		Complete(r)
}
