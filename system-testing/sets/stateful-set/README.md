# StatefulSet Migration Test

---

## StatefulSet Characteristics

- **Name:** `busybox`  
- **Replicas:** 2  
- **Image:** `busybox:1.35`  
- **Behavior:**  
  - Runs infinite loop (`while true; do sleep 30; done`)  
  - Simple sleep-based workload for migration testing  
- **Purpose:**  
  - Test pod migration behavior with **StatefulSet-managed pods**  

---

## Expected Output

- **Before Migration:**
  - Two StatefulSet pods (`busybox-0`, `busybox-1`) running with sleep loop  

- **After Migration:**  
  - Migrated pod is restored on the target node with `-restored` suffix  
  - Restored pod continues its sleep loop independently  
  - StatefulSet maintains desired replica count (2) for managed pods only  
  - Total pods: **3** → 2 StatefulSet pods + 1 restored pod  
  - StatefulSet automatically recreates a replacement for the terminated pod to maintain consistency  

---

## Steps to Test

1. **Deploy the StatefulSet and Headless Service**
```bash
kubectl apply -f busybox.yaml
```

2. **Verify StatefulSet and Pods are running**
```bash
kubectl get statefulset
kubectl get pods -o wide
```
Expected:
```
NAME      READY   AGE
busybox   2/2     25s
NAME        READY   STATUS    RESTARTS   AGE   IP            NODE          NOMINATED NODE   READINESS GATES
busybox-0   1/1     Running   0          25s   10.244.1.14   k8s-worker    <none>           <none>
busybox-1   1/1     Running   0          24s   10.244.2.10   k8s-worker2   <none>           <none>
```

3. **Trigger migration (Migrate one StatefulSet pod)**
```bash
kubectl apply -f - <<EOF
apiVersion: lpm.my.domain/v1
kind: PodMigration
metadata:
  name: busybox-migration
  namespace: default
spec:
  podName: busybox-0 # Update this accordingly to the busybox on k8s-worker
  targetNode: k8s-worker2
EOF
```

4. **Verify after migration**
```bash
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded podmigration/busybox-migration --timeout=5m

kubectl get statefulset
kubectl get pods -o wide
```
Expected:
- StatefulSet maintains **2 managed replicas** (`busybox-0`, `busybox-1`)  
- Restored pod runs independently with `-restored` suffix  
- Total of **3 pods**: 2 StatefulSet pods + 1 restored pod  
- The source pod was terminated, but StatefulSet recreated it on its assigned identity  

5. **Cleanup:**
```bash
kubectl delete statefulset busybox --ignore-not-found=true
kubectl delete service busybox --ignore-not-found=true
kubectl delete pods -l app=busybox --ignore-not-found=true
kubectl delete podmigration busybox-migration --ignore-not-found=true
```

---

## Notes  
- Restored pods (`*-restored`) are **not** managed by the StatefulSet and must be cleaned up manually if no longer needed.  
- The key reason behind this is that stateful sets use `ownerReferences` to define pods owned and managed by it, the pods that are restored by our controller has `PodMigration` (i.e. the CRD used to trigger migration) as its owner
- You can verify this via the command `kubectl get pod <pod_name> -o yaml`, and find the `ownerReferences` section under `metadata`.