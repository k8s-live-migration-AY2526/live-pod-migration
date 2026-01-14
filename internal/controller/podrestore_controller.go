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
)

// PodRestoreReconciler reconciles a PodRestore object
type PodRestoreReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	AgentClient *agent.Client
}

// RBAC
// +kubebuilder:rbac:groups=lpm.my.domain,resources=podrestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=lpm.my.domain,resources=podrestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=lpm.my.domain,resources=podrestores/finalizers,verbs=update
// +kubebuilder:rbac:groups=lpm.my.domain,resources=podcheckpointcontents,verbs=get;list;watch
// +kubebuilder:rbac:groups=lpm.my.domain,resources=containercheckpointcontents,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch

func NewPodRestoreReconciler(c client.Client, scheme *runtime.Scheme) *PodRestoreReconciler {
	return &PodRestoreReconciler{
		Client:      c,
		Scheme:      scheme,
		AgentClient: agent.NewClient(c),
	}
}

func (r *PodRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var podRestore lpmv1.PodRestore
	if err := r.Get(ctx, req.NamespacedName, &podRestore); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// default phase
	if podRestore.Status.Phase == "" {
		podRestore.Status.Phase = lpmv1.PodRestorePhasePending
		if err := r.Status().Update(ctx, &podRestore); err != nil {
			return ctrl.Result{}, err
		}
	}

	switch podRestore.Status.Phase {
	case lpmv1.PodRestorePhasePending:
		return r.handlePending(ctx, &podRestore)
	case lpmv1.PodRestorePhasePreparing:
		return r.handlePreparingImages(ctx, &podRestore)
	case lpmv1.PodRestorePhaseRestoring:
		return r.handleRestoring(ctx, &podRestore)
	case lpmv1.PodRestorePhaseSucceeded, lpmv1.PodRestorePhaseFailed:
		return r.handleCompleted(ctx, &podRestore)
	default:
		logger.Info("Unknown phase, nothing to do", "phase", podRestore.Status.Phase)
		return ctrl.Result{}, nil
	}
}

func (r *PodRestoreReconciler) handlePending(ctx context.Context, podRestore *lpmv1.PodRestore) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("PodRestore Pending", "name", podRestore.Name, "checkpointRef", podRestore.Spec.PodCheckpointContentRef)

	// Validate referenced PodCheckpointContent exists
	if podRestore.Spec.PodCheckpointContentRef.Name == "" {
		return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, "invalid podCheckpointContentRef")
	}

	var cpc lpmv1.PodCheckpointContent
	err := r.Get(ctx, client.ObjectKey{
		Namespace: podRestore.Namespace,
		Name:      podRestore.Spec.PodCheckpointContentRef.Name,
	}, &cpc)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, "podCheckpointContent not found")
		}
		return ctrl.Result{}, err
	}

	// Ensure content is ready
	if !cpc.Status.Ready {
		// requeue and wait for content to become ready
		podRestore.Status.Message = "waiting for podCheckpointContent to be ready"
		if err := r.Status().Update(ctx, podRestore); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Move to PreparingImages
	podRestore.Status.Phase = lpmv1.PodRestorePhasePreparing
	podRestore.Status.Message = "preparing checkpoint images"
	podRestore.Status.StartTime = &metav1.Time{Time: time.Now()}
	if err := r.Status().Update(ctx, podRestore); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
}

// Node-level agent handles image preparation for containers on the target node
// We monitor when all images are ready, then transition to Restoring phase
func (r *PodRestoreReconciler) handlePreparingImages(ctx context.Context, podRestore *lpmv1.PodRestore) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Preparing images for PodRestore", "name", podRestore.Name)

	// Fetch PodCheckpointContent
	cpc, err := r.getPodCheckpointContent(ctx, podRestore)
	if err != nil {
		return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, fmt.Sprintf("failed to get checkpoint content: %v", err))
	}

	// Ensure spec.podSnapshot exists
	if cpc.Spec.PodSnapshot == nil {
		return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, "podSpecSnapshot not found in PodCheckpointContent")
	}

	// Parse pod snapshot to determine expected container count
	var podSnapshot corev1.Pod
	if err := json.Unmarshal(cpc.Spec.PodSnapshot.Raw, &podSnapshot); err != nil {
		return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, fmt.Sprintf("failed to parse podSnapshot: %v", err))
	}

	// Check if all containers have their images prepared
	expectedContainers := len(podSnapshot.Spec.Containers)
	if podRestore.Status.ImageMapping == nil {
		podRestore.Status.ImageMapping = make(map[string]string)
	}
	preparedContainers := len(podRestore.Status.ImageMapping)

	logger.Info("Image preparation progress",
		"prepared", preparedContainers,
		"expected", expectedContainers,
		"mapping", podRestore.Status.ImageMapping)

	// Validate that each container has a non-empty image reference
	for _, container := range podSnapshot.Spec.Containers {
		imageRef, exists := podRestore.Status.ImageMapping[container.Name]
		if !exists || imageRef == "" {
			return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
		}
	}

	// move to restoring only when all containers have images mapped
	podRestore.Status.Phase = lpmv1.PodRestorePhaseRestoring
	podRestore.Status.Message = "creating restored pod"
	if err := r.Status().Update(ctx, podRestore); err != nil {
		return ctrl.Result{}, err

	}
	return ctrl.Result{}, nil
}

func (r *PodRestoreReconciler) handleRestoring(ctx context.Context, podRestore *lpmv1.PodRestore) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling Restoring phase for PodRestore", "name", podRestore.Name)

	if podRestore.Spec.IsStatefulSet {
		return r.handleStatefulSetRestoration(ctx, podRestore)
	}
	return r.handlePodRestoration(ctx, podRestore)
}

func (r *PodRestoreReconciler) handlePodRestoration(ctx context.Context, podRestore *lpmv1.PodRestore) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling Pod restoration", "name", podRestore.Name)

	// Create restored pod if not already created
	if podRestore.Status.RestoredPodName == "" {
		cpc, err := r.getPodCheckpointContent(ctx, podRestore)
		if err != nil {
			return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, fmt.Sprintf("failed to get checkpoint content: %v", err))
		}

		// Ensure podSnapshot exists
		if cpc.Spec.PodSnapshot == nil {
			return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, "podSnapshot missing in checkpoint content")
		}

		restoredPod, err := r.createRestoredPod(podRestore, &cpc)
		if err != nil {
			return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, fmt.Sprintf("failed to build restored pod: %v", err))
		}

		// Create pod
		if err := r.Create(ctx, restoredPod); err != nil {
			if apierrors.IsAlreadyExists(err) {
				podRestore.Status.RestoredPodName = restoredPod.Name
				podRestore.Status.Message = "restored pod already exists"
				if err := r.Status().Update(ctx, podRestore); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
			}
			return ctrl.Result{}, r.updatePhase(ctx, podRestore, "Failed", fmt.Sprintf("failed to create restored pod: %v", err))
		}

		// record restored pod name and continue monitoring
		podRestore.Status.RestoredPodName = restoredPod.Name
		podRestore.Status.Message = "restored pod created"
		if err := r.Status().Update(ctx, podRestore); err != nil {
			return ctrl.Result{}, err
		}

		logger.Info("Restored pod created", "pod", restoredPod.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	var restored corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: podRestore.Namespace, Name: podRestore.Status.RestoredPodName}, &restored); err != nil {
		if apierrors.IsNotFound(err) {
			// pod disappeared unexpectedly -> mark failed
			return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, "restored pod not found")
		}
		return ctrl.Result{}, err
	}

	switch restored.Status.Phase {
	case corev1.PodRunning:
		podRestore.Status.Phase = lpmv1.PodRestorePhaseSucceeded
		podRestore.Status.Message = "restored pod running"
		podRestore.Status.CompletionTime = &metav1.Time{Time: time.Now()}
		if err := r.Status().Update(ctx, podRestore); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	case corev1.PodFailed:
		return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, "restored pod failed to start")
	case corev1.PodPending:
		// 1. Check for Container Errors (ImagePullBackOff, etc.)
		for _, status := range append(restored.Status.InitContainerStatuses, restored.Status.ContainerStatuses...) {
			if status.State.Waiting != nil {
				reason := status.State.Waiting.Reason
				message := status.State.Waiting.Message

				// List of fatal waiting states,
				// not the cleanest way to detect error but seems like there isn't a better way
				if reason == "ImagePullBackOff" ||
					reason == "ErrImagePull" ||
					reason == "CreateContainerError" ||
					reason == "RunContainerError" ||
					reason == "InvalidImageName" {

					errMsg := fmt.Sprintf("Restored pod stuck in %s: %s", reason, message)
					logger.Error(nil, errMsg, "pod", restored.Name)
					return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, errMsg)
				}
			}
		}

		// 2. Check for Scheduling Errors
		for _, cond := range restored.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
				if cond.Reason == "Unschedulable" {
					errMsg := fmt.Sprintf("Restored pod cannot be scheduled: %s", cond.Message)
					logger.Error(nil, errMsg, "pod", restored.Name)
					return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, errMsg)
				}
			}
		}

		// 3. Hard Timeout Safety Net (e.g., 5 minutes)
		// This catches edge cases not covered above (e.g., pending PVC binding)
		if time.Since(restored.CreationTimestamp.Time) > 5*time.Minute {
			errMsg := "Restored pod timed out in Pending state ( > 5m)"
			logger.Error(nil, errMsg, "pod", restored.Name)
			return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, errMsg)
		}

		// If no fatal errors found, keep waiting
		logger.Info("Restored pod is pending", "pod", restored.Name, "status", restored.Status)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	default:
		// still starting
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
}

func (r *PodRestoreReconciler) handleStatefulSetRestoration(ctx context.Context, podRestore *lpmv1.PodRestore) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling StatefulSet pod restoration", "name", podRestore.Name)

	cpc, err := r.getPodCheckpointContent(ctx, podRestore)
	if err != nil {
		return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, fmt.Sprintf("failed to get pod checkpoint content: %v", err))
	}

	// Check if pod exists
	var srcPod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: podRestore.Namespace, Name: cpc.Spec.PodName}, &srcPod); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Pod not found during StatefulSet restoration", "pod", cpc.Spec.PodName)
			podRestore.Status.Message = "waiting for StatefulSet to recreate pod"
			if statusErr := r.Status().Update(ctx, podRestore); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// Patch StatefulSet template if not already done
	if podRestore.Status.StatefulSetRestore == nil {
		if err := r.patchStatefulSetTemplate(ctx, &srcPod, podRestore); err != nil {
			return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed,
				fmt.Sprintf("failed to patch StatefulSet: %v", err))
		}
		logger.Info("StatefulSet template patched", "statefulSet", podRestore.Status.StatefulSetRestore.Name)
	}

	// Check if this is the original pod or a recreated pod using UID
	if string(srcPod.UID) == podRestore.Status.StatefulSetRestore.OriginalPodUID {
		// Delete the original pod (StatefulSet will recreate it)
		logger.Info("Deleting original pod", "pod", srcPod.Name, "uid", srcPod.UID)
		if err := r.Delete(ctx, &srcPod); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to delete original pod: %w", err)
		}

		podRestore.Status.RestoredPodName = srcPod.Name // Same ordinal name required
		podRestore.Status.Message = "original pod deletion requested, waiting for StatefulSet to recreate"
		if err := r.Status().Update(ctx, podRestore); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Pod recreated with new template, observe status
	logger.Info("Observing recreated pod", "pod", srcPod.Name, "uid", srcPod.UID, "phase", srcPod.Status.Phase)

	switch srcPod.Status.Phase {
	case corev1.PodRunning:
		// Verify that the pod is using checkpoint images
		if r.isPodUsingCheckpointImages(&srcPod, podRestore) {
			logger.Info("StatefulSet pod successfully recreated with checkpoint images", "pod", srcPod.Name)
			return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseSucceeded, "StatefulSet pod successfully restored and running")
		} else {
			logger.Error(nil, "Recreated pod is not using checkpoint images", "pod", srcPod.Name)
			return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, "recreated pod is not using checkpoint images")
		}

	case corev1.PodFailed:
		return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, "recreated pod failed to start")

	default:
		logger.Info("Recreated pod in progress", "pod", srcPod.Name, "phase", srcPod.Status.Phase)
		podRestore.Status.Message = fmt.Sprintf("recreated pod phase: %s", srcPod.Status.Phase)
		if err := r.Status().Update(ctx, podRestore); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
}

func (r *PodRestoreReconciler) patchStatefulSetTemplate(ctx context.Context, srcPod *corev1.Pod, podRestore *lpmv1.PodRestore) error {
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
	if podRestore.Status.StatefulSetRestore == nil {
		originalTemplateBytes, err := json.Marshal(sts.Spec.Template)
		if err != nil {
			return fmt.Errorf("failed to encode original StatefulSet template: %w", err)
		}
		originalTemplate := string(originalTemplateBytes)
		podRestore.Status.StatefulSetRestore = &lpmv1.StatefulSetRestoreInfo{
			Name:             sts.Name,
			OriginalTemplate: originalTemplate,
			OriginalPodUID:   string(srcPod.UID),
		}

		// Update the status to persist the original template
		if err := r.Status().Update(ctx, podRestore); err != nil {
			return fmt.Errorf("failed to update podRestore with original template: %w", err)
		}
		logger.Info("Stored original StatefulSet template", "statefulSet", sts.Name)
	}

	patched := sts.DeepCopy()

	// Ensure update strategy is OnDelete so deleting a single pod results in recreation
	patched.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
		Type: appsv1.OnDeleteStatefulSetStrategyType,
	}

	// Patch container images to use checkpoint images
	for i, c := range patched.Spec.Template.Spec.Containers {
		if img, ok := podRestore.Status.ImageMapping[c.Name]; ok {
			patched.Spec.Template.Spec.Containers[i].Image = img
			patched.Spec.Template.Spec.Containers[i].ImagePullPolicy = corev1.PullNever
		}
	}

	if patched.Spec.Template.Labels == nil {
		patched.Spec.Template.Labels = map[string]string{}
	}
	patched.Spec.Template.Labels["migration-job"] = podRestore.Name

	// Constrain placement to target node (via nodeSelector)
	if podRestore.Spec.TargetNode != "" {
		if patched.Spec.Template.Spec.NodeSelector == nil {
			patched.Spec.Template.Spec.NodeSelector = map[string]string{}
		}
		patched.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] = podRestore.Spec.TargetNode
	}

	return r.Patch(ctx, patched, client.MergeFrom(sts))
}

func (r *PodRestoreReconciler) restoreStatefulSetTemplate(ctx context.Context, podRestore *lpmv1.PodRestore) error {
	logger := log.FromContext(ctx)

	if podRestore.Status.StatefulSetRestore == nil {
		logger.Info("No StatefulSet restore info stored, skipping restore")
		return nil
	}

	sts := &appsv1.StatefulSet{}
	statefulSetKey := client.ObjectKey{
		Namespace: podRestore.Namespace,
		Name:      podRestore.Status.StatefulSetRestore.Name,
	}

	if err := r.Get(ctx, statefulSetKey, sts); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("StatefulSet not found, skipping template restore", "statefulSet", statefulSetKey.Name)
			return nil
		}
		return fmt.Errorf("failed to get StatefulSet: %w", err)
	}

	var originalTemplate corev1.PodTemplateSpec
	if err := json.Unmarshal([]byte(podRestore.Status.StatefulSetRestore.OriginalTemplate), &originalTemplate); err != nil {
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

func (r *PodRestoreReconciler) handleCompleted(ctx context.Context, podRestore *lpmv1.PodRestore) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if podRestore.Spec.IsStatefulSet {
		if err := r.restoreStatefulSetTemplate(ctx, podRestore); err != nil {
			logger.Error(err, "Failed to restore StatefulSet template", "podRestore", podRestore.Name)
			// Don't fail the migration just because template restore failed
		} else {
			logger.Info("StatefulSet template restored successfully", "statefulSet", podRestore.Status.StatefulSetRestore.Name)
		}
	}

	return ctrl.Result{}, nil
}

// helper to convert shared:// checkpoint URI to OCI image via agent
func (r *PodRestoreReconciler) convertToOCIImage(ctx context.Context, checkpointURI, containerName, targetNode string) (string, error) {
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

// helper to construct restored pod using podSnapshot and imageMapping
func (r *PodRestoreReconciler) createRestoredPod(podRestore *lpmv1.PodRestore, cpc *lpmv1.PodCheckpointContent) (*corev1.Pod, error) {
	// Parse podSnapshot
	var podSnapshot corev1.Pod
	if err := json.Unmarshal(cpc.Spec.PodSnapshot.Raw, &podSnapshot); err != nil {
		return nil, fmt.Errorf("failed to parse podSnapshot: %w", err)
	}

	// START WITH THE ORIGINAL POD - preserve all runtime context
	restoredPod := podSnapshot.DeepCopy()

	// Change only what's absolutely necessary
	restoredPodName := podRestore.Spec.RestoredPodName
	if podRestore.Spec.RestoredPodName == "" {
		restoredPodName = fmt.Sprintf("%s-restored", podSnapshot.Name)
	}
	restoredPod.Name = restoredPodName
	restoredPod.ResourceVersion = ""                       // Required for creation
	restoredPod.UID = ""                                   // Required for creation
	restoredPod.Spec.NodeName = podRestore.Spec.TargetNode // Target node

	// Add migration tracking annotations
	if restoredPod.Annotations == nil {
		restoredPod.Annotations = make(map[string]string)
	}
	restoredPod.Annotations["migration.source-pod"] = podSnapshot.Name
	restoredPod.Annotations["migration.target-node"] = podRestore.Spec.TargetNode
	// Remove freeze-restart if present
	delete(restoredPod.Annotations, "migration.my.domain/freeze-restart")

	// Set owner reference
	restoredPod.OwnerReferences = []metav1.OwnerReference{
		*metav1.NewControllerRef(podRestore, lpmv1.GroupVersion.WithKind("PodRestore")),
	}

	// Apply checkpoint images to containers (existing logic)
	if podRestore.Status.ImageMapping == nil {
		return nil, fmt.Errorf("checkpoint images not prepared for migration")
	}

	for i, container := range restoredPod.Spec.Containers {
		checkpointImage, exists := podRestore.Status.ImageMapping[container.Name]
		if !exists {
			return nil, fmt.Errorf("no checkpoint image prepared for container %s", container.Name)
		}

		restoredPod.Spec.Containers[i].Image = checkpointImage
		restoredPod.Spec.Containers[i].ImagePullPolicy = corev1.PullNever
	}

	return restoredPod, nil
}

// getPodCheckpointContent resolves the referenced PodCheckpointContent
func (r *PodRestoreReconciler) getPodCheckpointContent(ctx context.Context, podRestore *lpmv1.PodRestore) (lpmv1.PodCheckpointContent, error) {
	var cpc lpmv1.PodCheckpointContent
	err := r.Get(ctx, client.ObjectKey{
		Namespace: podRestore.Namespace,
		Name:      podRestore.Spec.PodCheckpointContentRef.Name,
	}, &cpc)
	return cpc, err
}

func (r *PodRestoreReconciler) isPodUsingCheckpointImages(pod *corev1.Pod, podRestore *lpmv1.PodRestore) bool {
	if podRestore.Status.ImageMapping == nil {
		return false
	}
	for _, container := range pod.Spec.Containers {
		expectedImage, exists := podRestore.Status.ImageMapping[container.Name]
		if !exists {
			return false
		}
		if container.Image != expectedImage {
			return false
		}
	}
	return true
}

func (r *PodRestoreReconciler) deleteOriginalPod(ctx context.Context, podRestore *lpmv1.PodRestore) error {
	logger := log.FromContext(ctx)

	cpc, err := r.getPodCheckpointContent(ctx, podRestore)
	if err != nil {
		return fmt.Errorf("failed to get pod checkpoint content: %w", err)
	}

	originalPodName := cpc.Spec.PodName
	if podRestore.Status.RestoredPodName == originalPodName {
		// Avoid deleting the restored pod if it has the same name as the original
		logger.Info("Restored pod name matches original pod name; skipping deletion", "pod", originalPodName)
		return nil
	}

	var originalPod corev1.Pod
	err = r.Get(ctx, client.ObjectKey{
		Namespace: cpc.Spec.PodNamespace,
		Name:      originalPodName,
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

// getCheckpointPathForContainer locates the artifactURI inside PodCheckpointContent for a container name
func (r *PodRestoreReconciler) getCheckpointPathForContainer(ctx context.Context, checkpointContent *lpmv1.PodCheckpointContent, containerName string) string {
	for _, containerContent := range checkpointContent.Spec.ContainerContents {
		var content lpmv1.ContainerCheckpointContent
		err := r.Get(ctx, client.ObjectKey{
			Name:      containerContent.Name,
			Namespace: checkpointContent.Namespace,
		}, &content)
		if err != nil {
			// skip missing content; caller will handle not-found case
			continue
		}

		// simple heuristic: content name contains container name OR match by ContainerName field
		if strings.Contains(content.Name, containerName) || content.Spec.ContainerName == containerName {
			return content.Spec.ArtifactURI
		}
	}
	return ""
}

func (r *PodRestoreReconciler) updatePhase(ctx context.Context, podRestore *lpmv1.PodRestore, phase lpmv1.PodRestorePhase, message string) error {
	podRestore.Status.Phase = phase
	podRestore.Status.Message = message
	// mark completion time on terminal phases
	if phase == "Succeeded" || phase == "Failed" {
		now := metav1.Now()
		podRestore.Status.CompletionTime = &now
	}
	return r.Status().Update(ctx, podRestore)
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&lpmv1.PodRestore{}).
		Named("podrestore").
		Complete(r)
}
