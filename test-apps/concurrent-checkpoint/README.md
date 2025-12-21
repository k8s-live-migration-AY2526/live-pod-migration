# Concurrent Checkpoint Test

Tests concurrent checkpoint and migration of multiple pods.

## Prerequisites

Three-node cluster: `k8s-master`, `k8s-worker`, `k8s-worker2`

## Scenario 1: One Node to Multiple Nodes

**Setup:** 2 pods on `k8s-master`  
**Result:** 1 pod on `k8s-worker`, 1 pod on `k8s-worker2`

### Deploy Pods
```yaml
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: counter-1
spec:
  nodeName: k8s-master
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

kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: counter-2
spec:
  nodeName: k8s-master
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
```

### Trigger Migration

```yaml
kubectl apply -f - <<EOF
apiVersion: lpm.my.domain/v1
kind: PodMigration
metadata:
  name: counter-migration-1
  namespace: default
spec:
  podName: counter-1
  targetNode: k8s-worker
  retainOriginalPod: false
EOF

kubectl apply -f - <<EOF
apiVersion: lpm.my.domain/v1
kind: PodMigration
metadata:
  name: counter-migration-2
  namespace: default
spec:
  podName: counter-2
  targetNode: k8s-worker2
  retainOriginalPod: false
EOF
```

Expected: Both pods are migrated concurrently to their target nodes.

### Cleanup

```bash
kubectl delete pod counter-1 counter-2
kubectl delete podmigration counter-migration-1 counter-migration-2
```

## Scenario 2: Multiple Nodes to One Node

**Setup:** 1 pod on `k8s-worker`, 1 pod on `k8s-worker2`  
**Result:** Both pods on `k8s-master`

### Deploy Pods
```yaml
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: counter-1
spec:
  nodeName: k8s-worker
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

kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: counter-2
spec:
  nodeName: k8s-worker2
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
```

### Trigger Migration

```yaml
kubectl apply -f - <<EOF
apiVersion: lpm.my.domain/v1
kind: PodMigration
metadata:
  name: counter-migration-1
  namespace: default
spec:
  podName: counter-1
  targetNode: k8s-master
  retainOriginalPod: false
EOF

kubectl apply -f - <<EOF
apiVersion: lpm.my.domain/v1
kind: PodMigration
metadata:
  name: counter-migration-2
  namespace: default
spec:
  podName: counter-2
  targetNode: k8s-master
  retainOriginalPod: false
EOF
```

Expected: Both pods are migrated concurrently to their target nodes.

### Cleanup

```bash
kubectl delete pod counter-1 counter-2
kubectl delete podmigration counter-migration-1 counter-migration-2
```
