package utils

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PullStatus represents the state of an image pull
type PullStatus int

const (
	PullPending PullStatus = iota
	PullSucceeded
	PullFailed
)

// Puller interface defines methods for ephemeral image pulling
type Puller interface {
	PullImages(ctx context.Context, nodeName string, images []string) (string, error)
	CheckPullStatusAndCleanup(ctx context.Context, pullPodName string) (PullStatus, error)
}

// EphemeralPuller implements Puller using ephemeral pods
type EphemeralPuller struct {
	Client    client.Client
	Namespace string // namespace to create ephemeral pods
}

// NewEphemeralPuller creates a new EphemeralPuller
func NewEphemeralPuller(c client.Client, namespace string) *EphemeralPuller {
	return &EphemeralPuller{
		Client:    c,
		Namespace: namespace,
	}
}

// PullImage creates an ephemeral pod to pull the image
func (p *EphemeralPuller) PullImages(ctx context.Context, nodeName string, images []string) (string, error) {
	podName := p.getPodName(nodeName, images)

	// Check if pod already exists
	var existingPod corev1.Pod
	err := p.Client.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: podName}, &existingPod)
	if err == nil {
		// Pod exists, nothing to do
		return podName, nil
	}

	// Build container specs for all images
	containers := []corev1.Container{}
	for i, image := range images {
		containers = append(containers, corev1.Container{
			Name:    fmt.Sprintf("puller-%d", i),
			Image:   image,
			Command: []string{"sleep", "1"}, // dummy command
		})
	}

	// Create ephemeral pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: p.Namespace,
			Labels: map[string]string{
				"app": "ephemeral-puller",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:                      nodeName,
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: int64Ptr(0),
			Containers:                    containers,
		},
	}

	if err := p.Client.Create(ctx, pod); err != nil {
		return "", fmt.Errorf("failed to create ephemeral pod: %w", err)
	}

	return podName, nil
}

// CheckPullStatusAndCleanup checks pod status and deletes it if finished
func (p *EphemeralPuller) CheckPullStatusAndCleanup(ctx context.Context, pullPodName string) (PullStatus, error) {
	var pod corev1.Pod
	err := p.Client.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: pullPodName}, &pod)
	if err != nil {
		return PullFailed, fmt.Errorf("failed to get ephemeral pod: %w", err)
	}

	switch pod.Status.Phase {
	case corev1.PodPending, corev1.PodUnknown:
		return PullPending, nil
	case corev1.PodSucceeded:
		// Clean up pod
		_ = p.Client.Delete(ctx, &pod)
		return PullSucceeded, nil
	case corev1.PodFailed:
		// Clean up pod
		_ = p.Client.Delete(ctx, &pod)
		return PullFailed, nil
	default:
		return PullPending, nil
	}
}

func hashStringList(list []string) string {
	// Sort to ensure deterministic order
	sort.Strings(list)
	concat := strings.Join(list, ",")

	h := fnv.New32a() // 32-bit FNV-1a hash
	h.Write([]byte(concat))
	return fmt.Sprintf("%x", h.Sum32())
}

// getPodName generates a deterministic pod name for ephemeral pull
func (p *EphemeralPuller) getPodName(nodeName string, images []string) string {
	// Replace slashes and colons to make a valid pod name
	return fmt.Sprintf("pull-%s-%s", nodeName, hashStringList(images))
}

// helper functions
func int64Ptr(i int64) *int64 { return &i }
