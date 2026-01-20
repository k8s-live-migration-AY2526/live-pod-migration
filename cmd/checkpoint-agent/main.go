package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	lpmv1 "my.domain/guestbook/api/v1"
)

const (
	port                     = ":50051"
	checkpointDir            = "/var/lib/kubelet/checkpoints"
	maxMessageSize           = 100 * 1024 * 1024 // 100MB
	checkpointTimeout        = 30 * time.Second
	checkpointBackoffSteps   = 5
	checkpointBackoffInitial = 2 * time.Second
	checkpointBackoffFactor  = 2.0

	// Kubelet certificate paths
	checkpointCertFile = "/etc/kubernetes/pki/apiserver-kubelet-client.crt"
	checkpointKeyFile  = "/etc/kubernetes/pki/apiserver-kubelet-client.key"
	checkpointCAFile   = "/var/lib/kubelet/pki/kubelet.crt"

	checkpointsMountPath = "/mnt/checkpoints"
)

func main() {
	log.SetLogger(zap.New(zap.UseDevMode(true)))
	logger := log.Log.WithName("setup")

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		logger.Error(nil, "NODE_NAME environment variable must be set")
		os.Exit(1)
	}
	logger.Info("Starting checkpoint agent on node " + nodeName)

	// Ensure checkpoint directory exists
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		logger.Error(err, "Failed to create checkpoint directory")
		os.Exit(1)
	}

	// Build scheme
	scheme := runtime.NewScheme()
	if err := lpmv1.AddToScheme(scheme); err != nil {
		logger.Error(err, "Failed to add lpm types to scheme")
		os.Exit(1)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		logger.Error(err, "Failed to add core types to scheme")
		os.Exit(1)
	}

	// Create manager
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: ":8081",
	})
	if err != nil {
		logger.Error(err, "Failed to create manager")
		os.Exit(1)
	}

	// Setup Checkpoint reconciler
	reconciler := &CheckpointReconciler{
		Client:   mgr.GetClient(),
		NodeName: nodeName,
	}

	// Watch ContainerCheckpoint resources
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&lpmv1.ContainerCheckpoint{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object client.Object) bool {
			checkpoint := object.(*lpmv1.ContainerCheckpoint)
			return checkpoint.Status.Phase == lpmv1.ContainerCheckpointPhaseRunning
		})).
		Complete(reconciler); err != nil {
		logger.Error(err, "Failed to create ContainerCheckpoint controller")
		os.Exit(1)
	}

	// Setup PodRestore reconciler
	podRestoreReconciler := &PodRestoreReconciler{
		Client:   mgr.GetClient(),
		NodeName: nodeName,
	}

	// Watch PodRestore resources
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&lpmv1.PodRestore{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object client.Object) bool {
			podRestore := object.(*lpmv1.PodRestore)
			return podRestore.Status.Phase == lpmv1.PodRestorePhasePreparing &&
				podRestore.Spec.TargetNode == nodeName
		})).
		Complete(podRestoreReconciler); err != nil {
		logger.Error(err, "Failed to create PodRestore controller")
		os.Exit(1)
	}

	// Add health check endpoint
	if err := mgr.AddHealthzCheck("healthz", func(req *http.Request) error {
		return nil
	}); err != nil {
		logger.Error(err, "Failed to add health check")
		os.Exit(1)
	}

	if err := mgr.AddReadyzCheck("readyz", func(req *http.Request) error {
		return nil
	}); err != nil {
		logger.Error(err, "Failed to add ready check")
		os.Exit(1)
	}

	// Setup signal handler
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start manager in background
	go func() {
		logger.Info("Starting manager")
		if err := mgr.Start(ctx); err != nil {
			logger.Error(err, "Failed to start manager")
			os.Exit(1)
		}
	}()

	// Wait for manager cache sync
	if ok := mgr.GetCache().WaitForCacheSync(ctx); !ok {
		logger.Error(fmt.Errorf("cache sync failed"), "Failed to sync manager cache")
		os.Exit(1)
	}

	logger.Info("Checkpoint agent started successfully")
	<-ctx.Done()
	logger.Info("Shutting down checkpoint agent...")
}

// CheckpointReconciler handles ContainerCheckpoint events
type CheckpointReconciler struct {
	client.Client
	NodeName string
}

// Reconcile processes ContainerCheckpoint events
func (r *CheckpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var checkpoint lpmv1.ContainerCheckpoint
	if err := r.Get(ctx, req.NamespacedName, &checkpoint); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Get pod and check if deployed on this node. If yes, responsible for checkpointing.
	pod := &corev1.Pod{}
	err := r.Get(ctx, client.ObjectKey{Namespace: checkpoint.Namespace, Name: checkpoint.Spec.PodName}, pod)
	if err != nil {
		logger.Error(err, "Failed to get pod")
		return ctrl.Result{}, err
	}

	// Not for this node, skip
	if pod.Spec.NodeName != r.NodeName {
		return ctrl.Result{}, nil
	}

	logger.Info("Detected ContainerCheckpoint CR for checkpointing",
		"namespace", checkpoint.Namespace,
		"name", checkpoint.Name,
		"pod", checkpoint.Spec.PodName,
		"container", checkpoint.Spec.ContainerName,
		"phase", checkpoint.Status.Phase)

	// Check if content already exists (idempotency)
	contentName := checkpoint.Name
	containerCheckpointContent := &lpmv1.ContainerCheckpointContent{}
	err = r.Get(ctx, client.ObjectKey{Name: contentName}, containerCheckpointContent)
	if err == nil {
		logger.Info("ContainerCheckpointContent already exists", "name", contentName)
		return ctrl.Result{}, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// Perform checkpoint operation
	artifactURI, err := r.performCheckpoint(ctx, pod, checkpoint.Spec.ContainerName, checkpoint.Spec.DeferTermination)
	if err != nil {
		logger.Error(err, "Failed to perform checkpoint",
			"pod", checkpoint.Spec.PodName,
			"container", checkpoint.Spec.ContainerName,
		)

		// Create content with error status to signal failure
		containerCheckpointContent = &lpmv1.ContainerCheckpointContent{
			ObjectMeta: metav1.ObjectMeta{
				Name: contentName,
				Annotations: map[string]string{
					"error": err.Error(),
				},
			},
			Spec: lpmv1.ContainerCheckpointContentSpec{
				ContainerCheckpointRef: corev1.ObjectReference{
					Namespace: checkpoint.Namespace,
					Name:      checkpoint.Name,
				},
				PodNamespace:  checkpoint.Namespace,
				PodName:       checkpoint.Spec.PodName,
				ContainerName: checkpoint.Spec.ContainerName,
				ArtifactURI:   "", // Empty on error
			},
		}
		if createErr := r.Create(ctx, containerCheckpointContent); createErr != nil {
			logger.Error(createErr, "Failed to create ContainerCheckpointContent after checkpoint error")
			return ctrl.Result{}, fmt.Errorf("checkpoint failed: %v, create ContainerCheckpointContent failed: %v", err, createErr)
		}
		return ctrl.Result{}, err
	}

	containerCheckpointContent = &lpmv1.ContainerCheckpointContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: contentName,
		},
		Spec: lpmv1.ContainerCheckpointContentSpec{
			ContainerCheckpointRef: corev1.ObjectReference{
				Namespace: checkpoint.Namespace,
				Name:      checkpoint.Name,
			},
			PodNamespace:  checkpoint.Namespace,
			PodName:       checkpoint.Spec.PodName,
			ContainerName: checkpoint.Spec.ContainerName,
			ArtifactURI:   artifactURI,
		},
	}

	logger.Info("Creating ContainerCheckpointContent", "name", contentName, "artifactURI", artifactURI)
	return ctrl.Result{}, r.Create(ctx, containerCheckpointContent)
}

// performCheckpoint executes the checkpoint operation via kubelet API
func (r *CheckpointReconciler) performCheckpoint(ctx context.Context, pod *corev1.Pod, containerName string, deferTermination bool) (string, error) {
	// Create checkpoint using kubelet API
	url := fmt.Sprintf("https://%s:10250/checkpoint/%s/%s/%s?leaveStopped=%t",
		r.NodeName, pod.Namespace, pod.Name, containerName, !deferTermination)

	httpClient, err := r.makeTLSClient(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to create TLS client: %w", err)
	}

	checkpointFiles, err := r.doCheckpointWithBackoff(ctx, httpClient, url)
	if err != nil {
		return "", fmt.Errorf("checkpoint operation failed: %w", err)
	}

	if len(checkpointFiles) == 0 {
		return "", fmt.Errorf("no checkpoint files created")
	}

	// Copy checkpoint to shared storage
	sharedPath, err := r.copyToSharedStorage(string(pod.UID), containerName, checkpointFiles[0])
	if err != nil {
		// Return local path as fallback
		return fmt.Sprintf("file://%s", checkpointFiles[0]), nil
	}
	return fmt.Sprintf("shared://%s", sharedPath), nil
}

// makeTLSClient creates an HTTP client with TLS configuration for kubelet
func (r *CheckpointReconciler) makeTLSClient(ctx context.Context) (*http.Client, error) {
	logger := log.FromContext(ctx)
	// Try different certificate path combinations
	certPaths := []struct {
		cert string
		key  string
		ca   string
		desc string
	}{
		// Worker node paths (kubelet auto-generated)
		{
			cert: "/var/lib/kubelet/pki/kubelet-client-current.pem",
			key:  "/var/lib/kubelet/pki/kubelet-client-current.pem",
			ca:   "/etc/kubernetes/pki/ca.crt",
			desc: "worker node (kubelet auto-generated)",
		},
		// Master node paths (kubeadm generated)
		{
			cert: "/etc/kubernetes/pki/apiserver-kubelet-client.crt",
			key:  "/etc/kubernetes/pki/apiserver-kubelet-client.key",
			ca:   "/etc/kubernetes/pki/ca.crt",
			desc: "master node (kubeadm generated)",
		},
		// Alternative master node paths
		{
			cert: "/etc/kubernetes/pki/apiserver-kubelet-client.crt",
			key:  "/etc/kubernetes/pki/apiserver-kubelet-client.key",
			ca:   "/var/lib/kubelet/pki/kubelet.crt",
			desc: "master node (alternative CA)",
		},
	}

	var cert tls.Certificate
	var caBytes []byte
	var err error
	var workingPaths string

	// Try each certificate path combination
	for _, paths := range certPaths {
		// Check if all required files exist
		if _, err := os.Stat(paths.cert); os.IsNotExist(err) {
			logger.Info("Certificate file not found", "path", paths.cert)
			continue
		}
		if _, err := os.Stat(paths.key); os.IsNotExist(err) {
			logger.Info("Key file not found", "path", paths.key)
			continue
		}
		if _, err := os.Stat(paths.ca); os.IsNotExist(err) {
			logger.Info("CA file not found", "path", paths.ca)
			continue
		}

		// Try to load the certificate
		cert, err = tls.LoadX509KeyPair(paths.cert, paths.key)
		if err != nil {
			logger.Info("Failed to load certificates",
				"cert", paths.cert,
				"key", paths.key,
				"desc", paths.desc,
				"error", err)
			continue
		}

		// Try to load the CA
		caBytes, err = os.ReadFile(paths.ca)
		if err != nil {
			logger.Info("Failed to load CA",
				"ca", paths.ca,
				"desc", paths.desc,
				"error", err)
			continue
		}

		workingPaths = fmt.Sprintf("%s (cert=%s, key=%s, ca=%s)", paths.desc, paths.cert, paths.key, paths.ca)
		logger.Info("Successfully loaded certificates", "paths", workingPaths)
		break
	}

	if err != nil {
		return nil, fmt.Errorf("failed to load client certificate from any known location: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("failed to parse CA certificate from %s", workingPaths)
	}

	return &http.Client{
		Timeout: checkpointTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates:       []tls.Certificate{cert},
				RootCAs:            pool,
				InsecureSkipVerify: true, // Skip verification due to IP SAN issues
			},
		},
	}, nil
}

// doCheckpointWithBackoff calls kubelet checkpoint API with exponential backoff
func (r *CheckpointReconciler) doCheckpointWithBackoff(ctx context.Context, httpClient *http.Client, url string) ([]string, error) {
	var checkpointFiles []string
	var lastErr error

	bo := wait.Backoff{
		Steps:    checkpointBackoffSteps,
		Duration: checkpointBackoffInitial,
		Factor:   checkpointBackoffFactor,
	}

	err := wait.ExponentialBackoff(bo, func() (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			lastErr = fmt.Errorf("failed to create request: %w", err)
			return false, nil
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("kubelet request failed: %w", err)
			return false, nil
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, _ := io.ReadAll(resp.Body)
			lastErr = fmt.Errorf("kubelet responded %d: %s", resp.StatusCode, string(data))
			return false, nil
		}

		data, err := io.ReadAll(resp.Body)
		if err != nil {
			lastErr = fmt.Errorf("failed to read response: %w", err)
			return false, nil
		}

		var parsed struct {
			Items []string `json:"items"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			lastErr = fmt.Errorf("failed to parse kubelet JSON response: %w", err)
			return false, nil
		}

		if len(parsed.Items) == 0 {
			lastErr = fmt.Errorf("no checkpoint files returned by kubelet")
			return false, nil
		}

		checkpointFiles = parsed.Items
		logger := log.FromContext(ctx)
		logger.Info("Checkpoint created successfully", "files", checkpointFiles)
		return true, nil
	})

	if err != nil {
		return nil, fmt.Errorf("checkpoint failed after retries: %w", lastErr)
	}

	return checkpointFiles, nil
}

// copyToSharedStorage copies checkpoint to shared NFS mount
func (r *CheckpointReconciler) copyToSharedStorage(podUID, containerName, localPath string) (string, error) {
	timestamp := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("%s-%s-%s.tar", podUID, containerName, timestamp)
	sharedPath := filepath.Join(checkpointsMountPath, filename)

	sourceFile, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(sharedPath)
	if err != nil {
		return "", err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	if err != nil {
		return "", err
	}

	// Return relative path for shared:// URI
	return filename, destFile.Sync()
}

// PodRestoreReconciler handles PodRestore events
type PodRestoreReconciler struct {
	client.Client
	NodeName string
}

// Reconcile processes PodRestore events
func (r *PodRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var podRestore lpmv1.PodRestore
	if err := r.Get(ctx, req.NamespacedName, &podRestore); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Detected PodRestore CR for restoration",
		"namespace", podRestore.Namespace,
		"name", podRestore.Name,
	)

	// Fetch PodCheckpointContent
	var cpc lpmv1.PodCheckpointContent
	err := r.Get(ctx, client.ObjectKey{
		Namespace: podRestore.Namespace,
		Name:      podRestore.Spec.PodCheckpointContentRef.Name,
	}, &cpc)
	if err != nil {
		logger.Error(err, "Failed to get PodCheckpointContent", "name", podRestore.Spec.PodCheckpointContentRef.Name)
		return ctrl.Result{}, r.updatePhase(ctx, &podRestore, lpmv1.PodRestorePhaseFailed, fmt.Sprintf("failed to get PodCheckpointContent: %v", err))
	}

	// Ensure spec.podSnapshot exists
	if cpc.Spec.PodSnapshot == nil {
		logger.Error(fmt.Errorf("podSnapshot is nil"), "PodSnapshot is required in PodCheckpointContent")
		return ctrl.Result{}, r.updatePhase(ctx, &podRestore, lpmv1.PodRestorePhaseFailed, "podSnapshot is required in PodCheckpointContent")
	}

	// Initialize imageMapping in status if nil
	if podRestore.Status.ImageMapping == nil {
		podRestore.Status.ImageMapping = map[string]string{}
	}

	var podSnapshot corev1.Pod
	if err := json.Unmarshal(cpc.Spec.PodSnapshot.Raw, &podSnapshot); err != nil {
		return ctrl.Result{}, r.updatePhase(ctx, &podRestore, lpmv1.PodRestorePhaseFailed, fmt.Sprintf("failed to parse podSnapshot: %v", err))
	}

	logger.Info("Preparing images for pod restoration", "pod", podSnapshot.Name)

	// Iterate containers from podSnapshot
	imagesReady := true
	statusChanged := false
	for _, c := range podSnapshot.Spec.Containers {
		// skip if already resolved
		if _, ok := podRestore.Status.ImageMapping[c.Name]; ok {
			logger.Info("Image already prepared for container", "container", c.Name)
			continue
		}

		// find checkpoint artifact for this container
		checkpointURI := r.getCheckpointPathForContainer(ctx, &cpc, c.Name)
		if checkpointURI == "" {
			imagesReady = false
			logger.Info("No checkpoint found for container", "container", c.Name)
			continue
		}

		imageRef, err := r.convertCheckpointToOCI(ctx, checkpointURI, c.Name)
		if err != nil {
			imagesReady = false
			logger.Error(err, "Failed to convert checkpoint to OCI image", "container", c.Name, "checkpointURI", checkpointURI)
			continue
		}

		// store mapping
		podRestore.Status.ImageMapping[c.Name] = imageRef
		statusChanged = true
		logger.Info("Prepared image for container", "container", c.Name, "image", imageRef)
	}

	// Only update status if we actually made changes
	if statusChanged {
		if err := r.Status().Update(ctx, &podRestore); err != nil {
			logger.Error(err, "Failed to update PodRestore status after image preparation")
			// Return with requeue to retry on conflict
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		}
		logger.Info("Updated PodRestore status with new image mappings", "pod", podSnapshot.Name)
	}

	if !imagesReady {
		logger.Info("Not all images are ready yet for pod, requeueing", "pod", podSnapshot.Name)
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
	}

	// Let main controller update phase to PodRestorePhaseRestoring
	logger.Info("All images prepared for pod restoration", "pod", podSnapshot.Name)
	return ctrl.Result{}, nil
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

func (r *PodRestoreReconciler) convertCheckpointName(checkpointURI string) (string, string, error) {
	if !strings.HasPrefix(checkpointURI, "shared://") {
		return "", "", fmt.Errorf("Expected shared:// URI prefix, got: %s", checkpointURI)
	}

	// Generate OCI image name
	filename := strings.TrimPrefix(checkpointURI, "shared://")
	imageName := fmt.Sprintf("localhost/checkpoint:%s", strings.TrimSuffix(filename, ".tar"))
	checkpointURI = filepath.Join(checkpointsMountPath, filename)
	return checkpointURI, imageName, nil
}

// convertCheckpointToOCI converts a checkpoint tar file to OCI image format using buildah
func (r *PodRestoreReconciler) convertCheckpointToOCI(ctx context.Context, checkpointURI, containerName string) (string, error) {
	checkpointURI, imageName, err := r.convertCheckpointName(checkpointURI)
	if err != nil {
		return "", err
	}

	// Verify checkpoint file exists
	if _, err := os.Stat(checkpointURI); os.IsNotExist(err) {
		return "", fmt.Errorf("checkpoint file not found: %s", checkpointURI)
	}

	logger := log.FromContext(ctx)
	logger.Info("Converting checkpoint to OCI image", "checkpointURI", checkpointURI, "imageName", imageName)

	// Common buildah flags to use the mounted container storage
	buildahFlags := []string{"--root", "/var/lib/containers/storage"}

	// Create a working container from scratch
	cmd := exec.Command("buildah", append(buildahFlags, "from", "scratch")...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to create working container: %v, output: %s", err, output)
	}

	containerID := strings.TrimSpace(string(output))
	logger.Info("Created working container", "containerID", containerID)

	// Clean up working container on exit
	defer func() {
		cmd := exec.Command("buildah", append(buildahFlags, "rm", containerID)...)
		if err := cmd.Run(); err != nil {
			logger.Info("Warning: failed to remove working container",
				"containerID", containerID,
				"error", err)
		}
	}()

	// Add checkpoint file to container
	cmd = exec.Command("buildah", append(buildahFlags, "add", containerID, checkpointURI, "/")...)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to add checkpoint to container: %v", err)
	}

	// Add checkpoint annotation
	cmd = exec.Command("buildah", append(buildahFlags, "config",
		fmt.Sprintf("--annotation=io.kubernetes.cri-o.annotations.checkpoint.name=%s", containerName),
		containerID)...)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to add checkpoint annotation: %v", err)
	}

	// Commit the container as an image
	cmd = exec.Command("buildah", append(buildahFlags, "commit", containerID, imageName)...)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to commit container as image: %v", err)
	}

	logger.Info("Successfully created OCI image", "imageName", imageName)
	return imageName, nil
}
