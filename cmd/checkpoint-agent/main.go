package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	pb "my.domain/guestbook/api/proto"
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

// CheckpointServer implements the CheckpointService
type CheckpointServer struct {
	pb.UnimplementedCheckpointServiceServer
	nodeName string
}

// NewCheckpointServer creates a new checkpoint server
func NewCheckpointServer() *CheckpointServer {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		nodeName = "unknown"
	}

	return &CheckpointServer{
		nodeName: nodeName,
	}
}

// Checkpoint implements the checkpoint operation
func (s *CheckpointServer) Checkpoint(ctx context.Context, req *pb.CheckpointRequest) (*pb.CheckpointResponse, error) {
	log.Printf("Checkpoint request: namespace=%s, pod=%s, container=%s, uid=%s",
		req.PodNamespace, req.PodName, req.ContainerName, req.PodUid)

	// Ensure checkpoint directory exists
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		log.Printf("Failed to create checkpoint directory: %v", err)
		return &pb.CheckpointResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to create checkpoint directory: %v", err),
		}, nil
	}

	// Create checkpoint using kubelet API
	url := fmt.Sprintf("https://%s:10250/checkpoint/%s/%s/%s",
		s.nodeName, req.PodNamespace, req.PodName, req.ContainerName)

	httpClient, err := s.makeTLSClient()
	if err != nil {
		log.Printf("Failed to create TLS client: %v", err)
		return &pb.CheckpointResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to create TLS client: %v", err),
		}, nil
	}

	checkpointFiles, err := s.doCheckpointWithBackoff(ctx, httpClient, url)
	if err != nil {
		log.Printf("Failed to create checkpoint: %v", err)
		return &pb.CheckpointResponse{
			Success: false,
			Error:   fmt.Sprintf("checkpoint failed: %v", err),
		}, nil
	}

	if len(checkpointFiles) == 0 {
		return &pb.CheckpointResponse{
			Success: false,
			Error:   "no checkpoint files created",
		}, nil
	}

	// Copy checkpoint to shared storage
	sharedPath, err := s.copyToSharedStorage(req.PodUid, req.ContainerName, checkpointFiles[0])
	if err != nil {
		log.Printf("Failed to copy to shared storage: %v", err)
		// Return local path as fallback
		artifactURI := fmt.Sprintf("file://%s", checkpointFiles[0])
		log.Printf("Checkpoint created successfully: %s", artifactURI)
		return &pb.CheckpointResponse{
			Success:     true,
			ArtifactUri: artifactURI,
			Message:     "checkpoint created successfully",
		}, nil
	}

	// Return shared path
	artifactURI := fmt.Sprintf("shared://%s", sharedPath)
	log.Printf("Checkpoint created successfully: %s", artifactURI)
	return &pb.CheckpointResponse{
		Success:     true,
		ArtifactUri: artifactURI,
		Message:     "checkpoint created successfully",
	}, nil
}

// ConvertCheckpointToImage converts a checkpoint tar file to OCI image format
func (s *CheckpointServer) ConvertCheckpointToImage(ctx context.Context, req *pb.ConvertRequest) (*pb.ConvertResponse, error) {
	logger := log.FromContext(ctx)
	logger.Info("Convert request",
		"checkpoint_path", req.CheckpointPath,
		"container_name", req.ContainerName,
		"image_name", req.ImageName)

	// Validate input
	if req.CheckpointPath == "" {
		return &pb.ConvertResponse{
			Success: false,
			Error:   "checkpoint path is required",
		}, nil
	}

	if req.ImageName == "" {
		return &pb.ConvertResponse{
			Success: false,
			Error:   "image name is required",
		}, nil
	}

	// Convert shared:// URI to local path
	checkpointPath := req.CheckpointPath
	if strings.HasPrefix(checkpointPath, "shared://") {
		filename := strings.TrimPrefix(checkpointPath, "shared://")
		checkpointPath = filepath.Join(checkpointsMountPath, filename)
	}

	// Verify checkpoint file exists
	if _, err := os.Stat(checkpointPath); os.IsNotExist(err) {
		return &pb.ConvertResponse{
			Success: false,
			Error:   fmt.Sprintf("checkpoint file not found: %s", checkpointPath),
		}, nil
	}

	// Convert checkpoint to OCI image using buildah
	imageRef, err := s.convertCheckpointToOCI(ctx, checkpointPath, req.ContainerName, req.ImageName)
	if err != nil {
		logger.Error(err, "Failed to convert checkpoint to OCI")
		return &pb.ConvertResponse{
			Success: false,
			Error:   fmt.Sprintf("conversion failed: %v", err),
		}, nil
	}

	logger.Info("Successfully converted checkpoint to OCI image", "imageRef", imageRef)
	return &pb.ConvertResponse{
		Success:        true,
		ImageReference: imageRef,
		Message:        "checkpoint successfully converted to OCI image",
	}, nil
}

// Health implements the health check
func (s *CheckpointServer) Health(_ context.Context, _ *pb.HealthRequest) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{
		Healthy: true,
		Message: fmt.Sprintf("checkpoint agent healthy on node %s", s.nodeName),
	}, nil
}

// makeTLSClient creates an HTTP client with TLS configuration for kubelet
func (s *CheckpointServer) makeTLSClient() (*http.Client, error) {
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
			log.Printf("Certificate file not found: %s", paths.cert)
			continue
		}
		if _, err := os.Stat(paths.key); os.IsNotExist(err) {
			log.Printf("Key file not found: %s", paths.key)
			continue
		}
		if _, err := os.Stat(paths.ca); os.IsNotExist(err) {
			log.Printf("CA file not found: %s", paths.ca)
			continue
		}

		// Try to load the certificate
		cert, err = tls.LoadX509KeyPair(paths.cert, paths.key)
		if err != nil {
			log.Printf("Failed to load certificates from %s/%s (%s): %v", paths.cert, paths.key, paths.desc, err)
			continue
		}

		// Try to load the CA
		caBytes, err = os.ReadFile(paths.ca)
		if err != nil {
			log.Printf("Failed to load CA from %s (%s): %v", paths.ca, paths.desc, err)
			continue
		}

		workingPaths = fmt.Sprintf("%s (cert=%s, key=%s, ca=%s)", paths.desc, paths.cert, paths.key, paths.ca)
		log.Printf("Successfully loaded certificates: %s", workingPaths)
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
func (s *CheckpointServer) doCheckpointWithBackoff(ctx context.Context, httpClient *http.Client, url string) ([]string, error) {
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
			log.Printf("Kubelet request failed, retrying: %v", err)
			return false, nil
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				log.Printf("Failed to close response body: %v", err)
			}
		}()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, _ := io.ReadAll(resp.Body)
			lastErr = fmt.Errorf("kubelet responded %d: %s", resp.StatusCode, string(data))
			log.Printf("Non-2xx from kubelet, retrying: %s", lastErr)
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
		log.Printf("Checkpoint created successfully, files: %v", checkpointFiles)
		return true, nil
	})

	if err != nil {
		return nil, fmt.Errorf("checkpoint failed after retries: %w", lastErr)
	}

	return checkpointFiles, nil
}

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

	// Create manager with field selector to only watch pods on this node
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Pod{}: {
					Field: fields.SelectorFromSet(fields.Set{"spec.nodeName": nodeName}),
				},
			},
		},
		HealthProbeBindAddress: ":8081",
	})
	if err != nil {
		logger.Error(err, "Failed to create manager")
		os.Exit(1)
	}

	// Setup reconciler
	reconciler := &CheckpointReconciler{
		Client:   mgr.GetClient(),
		NodeName: nodeName,
	}

	// Watch ContainerCheckpoint resources
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&lpmv1.ContainerCheckpoint{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object client.Object) bool {
			// Only process checkpoints for pods on this node
			checkpoint := object.(*lpmv1.ContainerCheckpoint)
			return reconciler.isForThisNode(context.Background(), checkpoint)
		})).
		Complete(reconciler); err != nil {
		logger.Error(err, "Failed to create controller")
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

	// Create gRPC server
	lis, err := net.Listen("tcp", port)
	if err != nil {
		logger.Error(err, "Failed to listen")
		os.Exit(1)
	}

	// Configure gRPC server with larger message size
	s := grpc.NewServer(
		grpc.MaxRecvMsgSize(maxMessageSize),
		grpc.MaxSendMsgSize(maxMessageSize),
	)

	// Register services
	checkpointServer := NewCheckpointServer()
	pb.RegisterCheckpointServiceServer(s, checkpointServer)

	// Register health service
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(s, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	// Enable reflection for debugging
	reflection.Register(s)

	// Handle graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan

		logger.Info("Shutting down checkpoint agent...")
		cancel() // Stop the CheckpointReconciler watcher
		s.GracefulStop()
	}()

	logger.Info("Checkpoint agent listening", "port", port)
	if err := s.Serve(lis); err != nil {
		logger.Error(err, "Failed to serve")
		os.Exit(1)
	}
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

	logger.Info("Detected ContainerCheckpoint CR",
		"namespace", checkpoint.Namespace,
		"name", checkpoint.Name,
		"pod", checkpoint.Spec.PodName,
		"container", checkpoint.Spec.ContainerName,
		"phase", checkpoint.Status.Phase)

	// Only process if Running phase (main controller sets this)
	if checkpoint.Status.Phase != lpmv1.ContainerCheckpointPhaseRunning {
		return ctrl.Result{}, nil
	}

	// Check if content already exists (idempotency)
	contentName := checkpoint.Name
	containerCheckpointContent := &lpmv1.ContainerCheckpointContent{}
	err := r.Get(ctx, client.ObjectKey{Name: contentName}, containerCheckpointContent)
	if err == nil {
		// Content already exists, nothing to do
		logger.Info("ContainerCheckpointContent already exists", "name", contentName)
		return ctrl.Result{}, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// Get pod to extract UID
	pod := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: checkpoint.Namespace,
		Name:      checkpoint.Spec.PodName,
	}, pod); err != nil {
		logger.Error(err, "Failed to get pod")
		return ctrl.Result{}, err
	}

	// Ensure checkpoint directory exists
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		logger.Error(err, "Failed to create checkpoint directory")
		return ctrl.Result{}, err
	}

	// Perform checkpoint operation
	artifactURI, err := r.performCheckpoint(ctx, pod, checkpoint.Spec.ContainerName)
	if err != nil {
		logger.Error(err, "Failed to perform checkpoint")
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
		return ctrl.Result{}, r.Create(ctx, containerCheckpointContent)
	}

	// Create ContainerCheckpointContent with success
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

func (r *CheckpointReconciler) isForThisNode(ctx context.Context, checkpoint *lpmv1.ContainerCheckpoint) bool {
	pod := &corev1.Pod{}
	err := r.Get(ctx, client.ObjectKey{
		Namespace: checkpoint.Namespace,
		Name:      checkpoint.Spec.PodName,
	}, pod)

	if err != nil {
		return false
	}
	return pod.Spec.NodeName == r.NodeName
}

// performCheckpoint executes the checkpoint operation via kubelet API
func (r *CheckpointReconciler) performCheckpoint(ctx context.Context, pod *corev1.Pod, containerName string) (string, error) {
	// Create checkpoint using kubelet API
	url := fmt.Sprintf("https://%s:10250/checkpoint/%s/%s/%s",
		r.NodeName, pod.Namespace, pod.Name, containerName)

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

// convertCheckpointToOCI converts a checkpoint tar file to OCI image format using buildah
func (s *CheckpointServer) convertCheckpointToOCI(ctx context.Context, checkpointPath, containerName, imageName string) (string, error) {
	logger := log.FromContext(ctx)
	logger.Info("Converting checkpoint to OCI image", "checkpointPath", checkpointPath, "imageName", imageName)

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
	cmd = exec.Command("buildah", append(buildahFlags, "add", containerID, checkpointPath, "/")...)
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

// copyToSharedStorage copies checkpoint to shared NFS mount
func (s *CheckpointServer) copyToSharedStorage(podUID, containerName, localPath string) (string, error) {
	// Simple path: <checkpointsMountPath>/<podUID>-<container>-<timestamp>.tar
	timestamp := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("%s-%s-%s.tar", podUID, containerName, timestamp)
	sharedPath := filepath.Join(checkpointsMountPath, filename)

	// Copy file
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
