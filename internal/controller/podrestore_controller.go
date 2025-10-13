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

	// Initialize imageMapping in status if nil
	if podRestore.Status.ImageMapping == nil {
		podRestore.Status.ImageMapping = map[string]string{}
	}

	imagesReady := true

	var podSnapshot corev1.Pod
	if err := json.Unmarshal(cpc.Spec.PodSnapshot.Raw, &podSnapshot); err != nil {
		return ctrl.Result{}, r.updatePhase(ctx, podRestore, lpmv1.PodRestorePhaseFailed, fmt.Sprintf("failed to parse podSnapshot: %v", err))
	}

	// Iterate containers from podSnapshot
	for _, c := range podSnapshot.Spec.Containers {
		// skip if already resolved
		if _, ok := podRestore.Status.ImageMapping[c.Name]; ok {
			continue
		}

		// find checkpoint artifact for this container
		checkpointURI := r.getCheckpointPathForContainer(ctx, &cpc, c.Name)
		if checkpointURI == "" {
			// if not found, fail early
			imagesReady = false
			logger.Info("No checkpoint found for container", "container", c.Name)
			continue
		}

		imageRef, err := r.convertToOCIImage(ctx, checkpointURI, c.Name, podRestore.Spec.TargetNode)
		if err != nil {
			// conversion failed; mark not ready and retry later
			imagesReady = false
			logger.Error(err, "Failed to convert checkpoint to OCI image", "container", c.Name, "checkpointURI", checkpointURI)
			continue
		}

		// store mapping
		podRestore.Status.ImageMapping[c.Name] = imageRef
		logger.Info("Prepared image for container", "container", c.Name, "image", imageRef)
	}

	// persist status again once done/partially done
	if err := r.Status().Update(ctx, podRestore); err != nil {
		return ctrl.Result{}, err
	}

	// move to restoring only when all containers have images mapped
	if imagesReady && len(podRestore.Status.ImageMapping) == len(podSnapshot.Spec.Containers) {
		podRestore.Status.Phase = lpmv1.PodRestorePhaseRestoring
		podRestore.Status.Message = "creating restored pod"
		if err := r.Status().Update(ctx, podRestore); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// still preparing images; requeue
	return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
}

func (r *PodRestoreReconciler) handleRestoring(ctx context.Context, podRestore *lpmv1.PodRestore) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling Restoring phase for PodRestore", "name", podRestore.Name)

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
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	default:
		// still starting
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
}

func (r *PodRestoreReconciler) handleCompleted(ctx context.Context, podRestore *lpmv1.PodRestore) (ctrl.Result, error) {
	// terminal states; nothing to do
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
	restoredPod.ObjectMeta.Name = fmt.Sprintf("%s-restored", podSnapshot.Name)
	restoredPod.ObjectMeta.ResourceVersion = ""            // Required for creation
	restoredPod.ObjectMeta.UID = ""                        // Required for creation
	restoredPod.Spec.NodeName = podRestore.Spec.TargetNode // Target node

	// Add migration tracking annotations
	if restoredPod.ObjectMeta.Annotations == nil {
		restoredPod.ObjectMeta.Annotations = make(map[string]string)
	}
	restoredPod.ObjectMeta.Annotations["migration.source-pod"] = podSnapshot.Name
	restoredPod.ObjectMeta.Annotations["migration.target-node"] = podRestore.Spec.TargetNode

	// Set owner reference
	restoredPod.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
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
