# Ephemeral-Storage Test

---

## Pod Characteristics

- **Name:** `counter-migration-test`  
- **Image:** `busybox:1.35`  
- **Behavior:**  
  - Increments a counter every 3 seconds  
  - Writes timestamped output to `/data/counter.log`  
- **Volume:** `emptyDir` mounted at `/data` (ephemeral storage)  
- **Purpose:** Test stateful live pod migration (CRIU) with ephemeral storage  

---

## Expected Output

- **Before Migration:**
  - Counter increments continuously every 3 seconds, starting from 1
  - `/data/counter.log` exists
  - Example (head of log):
    ```
    Mon Sep  8 12:00:00 UTC 2025: Count=1
    ```
  - Example (tail of log):  
    ```
    Mon Sep  8 12:01:54 UTC 2025: Count=18
    Mon Sep  8 12:01:57 UTC 2025: Count=19
    Mon Sep  8 12:02:00 UTC 2025: Count=20
    ```

- **After Migration:**  
  - Pod continues running on target node  
  - Counter continues incrementing from the point of checkpoint (not from 1)
  - Log truncated if using `emptyDir` (ephemeral), expected behavior  

---

## Steps to Test

1. **Deploy the Pod**
```bash
kubectl apply -f counter.yaml
```

2. **Verify Pod is running**
```bash
kubectl wait --for=condition=Ready pod/counter-migration-test --timeout=5m

# Running on worker
kubectl get pods -o wide
```
Expected:
```
NAME                     READY   STATUS    RESTARTS   AGE   IP           NODE         NOMINATED NODE   READINESS GATES
counter-migration-test   1/1     Running   0          5s    10.244.1.9   k8s-worker   <none>           <none>
```

3. **Observe counter before migration**
```bash
kubectl logs -f counter-migration-test
```

4. **Trigger migration**
```bash
kubectl apply -f - <<EOF
apiVersion: lpm.my.domain/v1
kind: PodMigration
metadata:
    name: counter-migration
    namespace: default
spec:
    podName: counter-migration-test
    targetNode: k8s-worker2
EOF
```

5. **Verify after migration**
```bash
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded podmigration/counter-migration --timeout=5m

# Restored pod running on worker2
kubectl get pods -o wide

# First line should start from the point we triggered migration and pod was checkpointed
kubectl exec -it counter-migration-test-restored -- head -n 1 /data/counter.log

# Pod should continue running successfully
kubectl logs -f counter-migration-test-restored
```
Expected:
- Pod continues running
- Counter increments without restarting  
- First line of log is not preserved (should not start from 1), and last lines reflect continuous count  

6. **Cleanup:**
```bash
kubectl delete pod counter-migration-test counter-migration-test-restored --ignore-not-found=true
kubectl delete podmigration counter-migration --ignore-not-found=true
```