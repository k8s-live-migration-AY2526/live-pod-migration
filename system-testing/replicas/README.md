# ReplicaSet Migration Test

---

## ReplicaSet Characteristics

- **Name:** `busybox`  
- **Replicas:** 2
- **Image:** `busybox:1.35`  
- **Behavior:**  
  - Runs infinite loop (`while true; do sleep 30; done`)
  - Simple sleep-based workload for migration testing
- **Purpose:** Test pod migration behavior with ReplicaSet (non-stateful)

---

## Expected Output

- **Before Migration:**
  - Two pods running with simple sleep loop

- **After Migration:**  
  - Migrated pod continues running on target node with `-restored` suffix
  - Pod continues its sleep loop 
  - ReplicaSet maintains desired replica count (2) but restored pod is NOT managed by ReplicaSet
  - Total pods will be 3: 2 original ReplicaSet pods + 1 restored pod  

---

## Steps to Test

1. **Deploy the ReplicaSet**
```bash
kubectl apply -f busybox.yaml
```

2. **Verify ReplicaSet and Pods are running**
```bash
kubectl get replicaset
kubectl get pods -o wide
```
Expected:
```
NAME      DESIRED   CURRENT   READY   AGE
busybox   2         2         2       30s

NAME            READY   STATUS    RESTARTS   AGE   IP           NODE          NOMINATED NODE   READINESS GATES
busybox-bmdmb   1/1     Running   0          13s   10.244.2.5   k8s-worker2   <none>           <none>
busybox-hlvb9   1/1     Running   0          13s   10.244.1.7   k8s-worker    <none>           <none>
```

3. **Trigger migration (Migrate the busybox on worker -> worker2)**
```bash
kubectl apply -f - <<EOF
apiVersion: lpm.my.domain/v1
kind: PodMigration
metadata:
    name: busybox-migration
    namespace: default
spec:
    podName: busybox-hlvb9 # Update this accordingly
    targetNode: k8s-worker2
EOF
```

4. **Verify after migration**
```bash
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded podmigration/busybox-migration --timeout=60s

kubectl get replicaset
kubectl get pods -o wide
```
Expected:
- ReplicaSet maintains 2 replicas (original pods only)
- Restored pod runs independently with `-restored` suffix (not managed by ReplicaSet)
- Total of 3 pods: 2 ReplicaSet pods + 1 restored pod
- The source pod was terminated, and a new pod on k8s-worker is spun up by replica set

5. **Cleanup:**
```bash
kubectl delete replicaset busybox --ignore-not-found=true
kubectl delete pods -l app=busybox --ignore-not-found=true
kubectl delete podmigration busybox-migration --ignore-not-found=true
```