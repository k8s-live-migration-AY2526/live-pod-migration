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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	lpmv1 "my.domain/guestbook/api/v1"
)

// PodRestoreReconciler reconciles a PodRestore object
type PodRestoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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
		Client: c,
		Scheme: scheme,
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
	return ctrl.Result{}, nil
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
	if podRestore.Spec.IsDeployment {
		return r.handleDeploymentRestoration(ctx, podRestore)
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
			return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, fmt.Sprintf("failed to create restored pod: %v", err))
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

	if podRestore.Status.RestoredPodName == "" {
		podRestore.Status.RestoredPodName = cpc.Spec.PodName
		if err := r.Status().Update(ctx, podRestore); err != nil {
			return ctrl.Result{}, err
		}
	}

	var srcPod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: podRestore.Namespace, Name: cpc.Spec.PodName}, &srcPod); err != nil {
		if apierrors.IsNotFound(err) {
			// Pod doesn't exist - either not deleted yet or already deleted and awaiting recreation
			if podRestore.Status.StatefulSetRestore != nil && podRestore.Status.StatefulSetRestore.OriginalPodUID != "" {
				logger.Info("Original pod deleted, waiting for StatefulSet recreation", "pod", cpc.Spec.PodName)
				podRestore.Status.Message = "waiting for StatefulSet to recreate pod"
				if statusErr := r.Status().Update(ctx, podRestore); statusErr != nil {
					return ctrl.Result{}, statusErr
				}
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			// Haven't deleted yet, pod genuinely doesn't exist - something is wrong
			return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, "original pod not found")
		}
		return ctrl.Result{}, err
	}

	if podRestore.Status.StatefulSetRestore == nil {
		podRestore.Status.StatefulSetRestore = &lpmv1.StatefulSetRestoreInfo{
			OriginalPodUID: string(srcPod.UID),
		}
		if err := r.Status().Update(ctx, podRestore); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Recorded original pod UID", "pod", srcPod.Name, "uid", srcPod.UID)
	}

	if string(srcPod.UID) != podRestore.Status.StatefulSetRestore.OriginalPodUID {
		if !r.isPodUsingCheckpointImages(&srcPod, podRestore) {
			errMsg := "recreated pod is not using checkpoint images - webhook may have failed to inject them"
			logger.Error(nil, errMsg, "pod", srcPod.Name, "uid", srcPod.UID)
			return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, errMsg)
		}

		if srcPod.Status.Phase == corev1.PodRunning {
			logger.Info("StatefulSet pod successfully restored with checkpoint images", "pod", srcPod.Name)
			return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseSucceeded, "StatefulSet pod restored and running")
		}

		logger.Info("Recreated pod is starting", "pod", srcPod.Name, "phase", srcPod.Status.Phase)
		podRestore.Status.Message = "waiting for recreated pod to start"
		if err := r.Status().Update(ctx, podRestore); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// This is the original pod - delete it to trigger StatefulSet recreation
	logger.Info("Deleting original pod to trigger StatefulSet recreation", "pod", srcPod.Name, "uid", srcPod.UID)
	if err := r.Delete(ctx, &srcPod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to delete original pod: %w", err)
	}

	podRestore.Status.Message = "pod deletion requested, webhook will inject checkpoint images on recreation"
	if err := r.Status().Update(ctx, podRestore); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Original pod deleted, waiting for recreation", "pod", srcPod.Name)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *PodRestoreReconciler) handleDeploymentRestoration(ctx context.Context, podRestore *lpmv1.PodRestore) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling Deployment pod restoration", "name", podRestore.Name)

	cpc, err := r.getPodCheckpointContent(ctx, podRestore)
	if err != nil {
		return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, fmt.Sprintf("failed to get pod checkpoint content: %v", err))
	}

	// Record original pod UID and delete it
	if podRestore.Status.DeploymentRestore == nil {
		var srcPod corev1.Pod
		if err := r.Get(ctx, client.ObjectKey{Namespace: podRestore.Namespace, Name: cpc.Spec.PodName}, &srcPod); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, "original pod not found")
			}
			return ctrl.Result{}, err
		}

		podRestore.Status.DeploymentRestore = &lpmv1.DeploymentRestoreInfo{
			OriginalPodUID: string(srcPod.UID),
			ReplicaSetName: getReplicaSetOwner(&srcPod),
		}
		if err := r.Status().Update(ctx, podRestore); err != nil {
			return ctrl.Result{}, err
		}

		logger.Info("Deleting original pod to trigger Deployment recreation", "pod", srcPod.Name, "uid", srcPod.UID)
		if err := r.Delete(ctx, &srcPod); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to delete original pod: %w", err)
		}

		podRestore.Status.Message = "pod deletion requested, webhook will inject checkpoint images on recreation"
		if err := r.Status().Update(ctx, podRestore); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Find the recreated pod by migration-job label. We cannot find via name because Deployment adds a random suffix
	// to recreated pod's name.
	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(podRestore.Namespace),
		client.MatchingLabels{"migration-job": podRestore.Name},
	); err != nil {
		return ctrl.Result{}, err
	}

	// Filter out the original pod by UID
	var restoredPod *corev1.Pod
	for i := range podList.Items {
		if string(podList.Items[i].UID) != podRestore.Status.DeploymentRestore.OriginalPodUID {
			restoredPod = &podList.Items[i]
			break
		}
	}

	if restoredPod == nil {
		logger.Info("Waiting for Deployment to recreate pod with checkpoint images")
		podRestore.Status.Message = "waiting for Deployment to recreate pod"
		if err := r.Status().Update(ctx, podRestore); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if !r.isPodUsingCheckpointImages(restoredPod, podRestore) {
		errMsg := "recreated pod is not using checkpoint images - webhook may have failed to inject them"
		logger.Error(nil, errMsg, "pod", restoredPod.Name, "uid", restoredPod.UID)
		return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, errMsg)
	}

	if podRestore.Status.RestoredPodName != restoredPod.Name {
		podRestore.Status.RestoredPodName = restoredPod.Name
		if err := r.Status().Update(ctx, podRestore); err != nil {
			return ctrl.Result{}, err
		}
	}

	if restoredPod.Status.Phase == corev1.PodRunning {
		logger.Info("Deployment pod successfully restored with checkpoint images", "pod", restoredPod.Name)
		return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseSucceeded, "Deployment pod restored and running")
	}

	logger.Info("Recreated pod is starting", "pod", restoredPod.Name, "phase", restoredPod.Status.Phase)
	podRestore.Status.Message = "waiting for recreated pod to start"
	if err := r.Status().Update(ctx, podRestore); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *PodRestoreReconciler) handleCompleted(_ context.Context, _ *lpmv1.PodRestore) (ctrl.Result, error) {
	return ctrl.Result{}, nil
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

func getReplicaSetOwner(pod *corev1.Pod) string {
	controller := metav1.GetControllerOf(pod)
	if controller != nil && controller.Kind == "ReplicaSet" {
		return controller.Name
	}
	return ""
}
