# Ephemeral-Storage Test

---

## Pod Characteristics

- **Name:** `writer-pod`  
- **Image:** `busybox:1.35`  
- **Behavior:**  
  - Increments a counter every second  
  - Writes timestamped output to `/data/counter.log`  
- **Volume:** PersistentVolumeClaim (`redis-nfs-pvc`) backed by **NFS** (network storage)
- **Purpose:** Test stateful live pod migration (CRIU) with opened-file handlers

---


## Steps to Test

1. **Deploy the Pod**
```bash
kubectl apply -f writer.yaml
```

2. **Verify Pod is running**
```bash
kubectl wait --for=condition=Ready pod/writer-pod --timeout=5m

# Running on worker
kubectl get pods -o wide
```
Expected:
```
NAME         READY   STATUS    RESTARTS   AGE   IP           NODE         NOMINATED NODE   READINESS GATES
writer-pod   1/1     Running   0          37s   10.244.1.3   k8s-worker   <none>           <none>
```

3. **Observe logs before migration**
```bash
kubectl exec -it writer-pod -- tail -f /data/migration.log
kubectl exec -it writer-pod -- cat /data/migration.log
```

4. **Trigger migration with deferTermination**
```bash
kubectl apply -f - <<EOF
apiVersion: lpm.my.domain/v1
kind: PodMigration
metadata:
    name: writer-pod-migration-defer-termination
    namespace: default
spec:
    podName: writer-pod
    targetNode: k8s-worker2
    deferTermination: true
EOF
```

5. **Verify migration failed (due to opened-file modified by sourcePod that continues to run)**
```bash
kubectl wait --for=jsonpath='{.status.phase}'=Failed podmigration/writer-pod-migration-defer-termination --timeout=5m
```

6. **Trigger migration without deferTermination**
```bash
kubectl delete pod writer-pod-restored

kubectl apply -f - <<EOF
apiVersion: lpm.my.domain/v1
kind: PodMigration
metadata:
    name: writer-pod-migration
    namespace: default
spec:
    podName: writer-pod
    targetNode: k8s-worker2
EOF
```

7. **Verify migration succeeded**
```bash
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded podmigration/writer-pod-migration --timeout=5m
```

# Restored pod running on worker2
kubectl get pods -o wide

# Pod should continue running successfully
```bash
kubectl exec -it writer-pod-restored -- tail -f /data/migration.log
kubectl exec -it writer-pod-restored -- cat /data/migration.log
```
Expected:
- Pod continues running
- Counter increments without restarting 

6. **Cleanup:**
```bash
kubectl delete pod writer-pod writer-pod-restored --ignore-not-found=true
kubectl delete podmigration writer-pod-migration writer-pod-migration-defer-termination  --ignore-not-found=true
kubectl delete pvc nfs-pvc-migration-test --ignore-not-found=true
```