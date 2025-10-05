# Manual Pod Checkpoint & Restore

A minimal guide to manually checkpointing and restoring the counter pod.

## Steps

### Create counter Pod

```bash
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: counter-migration-test
spec:
  containers:
  - name: counter
    image: busybox:1.35
    command: 
    - /bin/sh
    - -c  
    - |
      echo 'Counter script starting'
      COUNT=0
      while true; do
        COUNT=$((COUNT + 1))
        TIMESTAMP=$(date)
        echo "$TIMESTAMP: Count=$COUNT" | tee -a /data/counter.log
        sleep 3
      done
    volumeMounts:
    - name: data
      mountPath: /data
  volumes:
  - name: data
    emptyDir: {}
EOF

kubectl wait --for=condition=Ready pod counter --timeout=120s
kubectl logs counter
```

### Checkpoint the pod

```bash
kubectl apply -f - <<'EOF'
apiVersion: lpm.my.domain/v1
kind: PodCheckpoint
metadata:
  name: counter-checkpoint
  namespace: default
spec:
  podName: counter
EOF
```

### Delete the pod (manual step for now)

```bash
kubectl delete pod counter --namespace default
```

### Restore from checkpoint

```bash
kubectl apply -f - <<'EOF'
apiVersion: lpm.my.domain/v1
kind: PodRestore
metadata:
  name: counter-restore
  namespace: default
spec:
  podCheckpointContentRef:
    name: counter-checkpoint
  targetNode: k8s-worker
EOF

# wait for restored pod to be Ready
kubectl wait --for=condition=Ready pod counter-restored --timeout=120s
kubectl logs counter-restored
```

### Cleanup
```bash
kubectl delete pod counter-restored
```