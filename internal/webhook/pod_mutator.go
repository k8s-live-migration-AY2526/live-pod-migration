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

package webhook

import (
	"context"
	"encoding/json"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	lpmv1 "my.domain/guestbook/api/v1"
)

type PodMutator struct {
	Client  client.Client
	decoder admission.Decoder
}

func (m *PodMutator) InjectDecoder(d admission.Decoder) error {
	m.decoder = d
	return nil
}

func (m *PodMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx)

	pod := &corev1.Pod{}
	if err := m.decoder.Decode(req, pod); err != nil {
		logger.Error(err, "Failed to decode pod")
		return admission.Errored(http.StatusBadRequest, err)
	}

	logger.Info("Pod mutation webhook invoked",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"operation", req.Operation)

	ownerKind := getOwnerKind(pod)
	if ownerKind == "" {
		return admission.Allowed("not a StatefulSet or Deployment/ReplicaSet pod")
	}

	logger.Info("Detected StatefulSet or Deployment/ReplicaSet pod creation",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"ownerKind", ownerKind,
		"operation", req.Operation)

	podRestore, err := m.findActivePodRestore(ctx, pod)
	if err != nil {
		logger.Error(err, "Failed to find active PodRestore")
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if podRestore == nil {
		return admission.Allowed("no related active migration")
	}

	logger.Info("Active PodRestore found for pod", "podRestore", podRestore.Name)

	for i, container := range pod.Spec.Containers {
		if checkpointImage, ok := podRestore.Status.ImageMapping[container.Name]; ok {
			pod.Spec.Containers[i].Image = checkpointImage
			pod.Spec.Containers[i].ImagePullPolicy = corev1.PullNever
		}
	}

	if podRestore.Spec.TargetNode != "" {
		if pod.Spec.NodeSelector == nil {
			pod.Spec.NodeSelector = make(map[string]string)
		}
		pod.Spec.NodeSelector["kubernetes.io/hostname"] = podRestore.Spec.TargetNode
	}

	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels["migration-job"] = podRestore.Name

	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		logger.Error(err, "Failed to marshal mutated pod")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	logger.Info("Pod mutated successfully with checkpoint images", "pod", pod.Name)
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

func (m *PodMutator) findActivePodRestore(ctx context.Context, pod *corev1.Pod) (*lpmv1.PodRestore, error) {
	logger := log.FromContext(ctx)

	var podRestoreList lpmv1.PodRestoreList
	if err := m.Client.List(ctx, &podRestoreList, client.InNamespace(pod.Namespace)); err != nil {
		return nil, err
	}

	ownerKind := getOwnerKind(pod)

	for i := range podRestoreList.Items {
		pr := &podRestoreList.Items[i]
		if pr.Status.Phase != lpmv1.PodRestorePhaseRestoring {
			continue
		}

		// For Deployment pods, we cannot match by name because ReplicaSet generates a random suffix.
		// Instead, match by ReplicaSet owner name and ensure the PodRestore has initiated
		// deletion (DeploymentRestore != nil) and hasn't already been claimed (RestoredPodName still empty).
		if pr.Spec.IsDeployment && ownerKind == "ReplicaSet" {
			if pr.Status.DeploymentRestore == nil {
				continue
			}
			if pr.Status.RestoredPodName != "" {
				continue
			}
			if pr.Status.DeploymentRestore.ReplicaSetName != getReplicaSetOwner(pod) {
				continue
			}
			logger.Info("Found matching Deployment PodRestore",
				"podRestore", pr.Name,
				"pod", pod.Name,
				"replicaSet", pr.Status.DeploymentRestore.ReplicaSetName,
				"phase", pr.Status.Phase)
			return pr, nil
		}

		// For StatefulSet and standalone pods, name is expected and can be directly matched
		if pr.Status.RestoredPodName != pod.Name {
			continue
		}

		logger.Info("Found matching PodRestore",
			"podRestore", pr.Name,
			"pod", pod.Name,
			"phase", pr.Status.Phase)

		return pr, nil
	}

	return nil, nil
}

func getOwnerKind(pod *corev1.Pod) string {
	controller := metav1.GetControllerOf(pod)
	if controller == nil {
		return ""
	}
	if controller.Kind == "StatefulSet" || controller.Kind == "ReplicaSet" {
		return controller.Kind
	}
	return ""
}

func getReplicaSetOwner(pod *corev1.Pod) string {
	controller := metav1.GetControllerOf(pod)
	if controller != nil && controller.Kind == "ReplicaSet" {
		return controller.Name
	}
	return ""
}
