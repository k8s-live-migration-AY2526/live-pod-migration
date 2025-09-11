# Local Storage Test (Redis with Local PV)

---

## Pod Characteristics

- **Name:** `redis-local`  
- **Image:** `redis:7`  
- **Behavior:**  
  - Runs a Redis server storing data at `/data`  
- **Volume:** PersistentVolumeClaim (`redis-local-pvc`) backed by a **Local PersistentVolume** (node-bound)  
- **Access Mode:** `ReadWriteOnce`  
- **Purpose:** Observe live pod migration behavior when using node-local storage  

---

## Expected Outcome

- **Before Migration:**  
  - Pod is running on the node where the Local PV resides  
  - Redis stores data under `/data` on the local disk  
  - Logs show Redis startup and readiness  

- **After Migration Attempt:**  
  - Because the volume is node-bound, migration to a different node will likely be blocked by the controller or will fail during restore.  
  - If the system allows migration to another node, the restored pod will not have the original local data available, and the previously written key may be missing.  
  - This test highlights that for reliable data persistence across nodes, network-backed storage (e.g., NFS/RWX) is recommended.

---

## Steps to Test

1. **Prepare local path on the target node**
```bash
# on k8s-worker
sudo mkdir -p /mnt/disks/ssd1
sudo chown 1001:1001 /mnt/disks/ssd1 
sudo chmod 0777 /mnt/disks/ssd1   
```

2. **Deploy PV, PVC, and Pod**
```bash
# On k8s-master
kubectl apply -f redis-local.yaml
```

3. **Verify Pod is running**
```bash
kubectl wait --for=condition=Ready pod/redis-local --timeout=5m

# Running on worker
kubectl get pods -o wide
```
Expected:
```
NAME          READY   STATUS    RESTARTS   AGE   IP           NODE         NOMINATED NODE   READINESS GATES
redis-local   1/1     Running   0          22s   10.244.1.5   k8s-worker   <none>           <none>
```

4. **Check Redis server logs before migration**
```bash
kubectl logs -f redis-local
```

5. **Trigger migration**
   - Note: Migrating to a different node is expected to fail due to the Local PV being node-bound. 
```bash
kubectl apply -f - <<EOF
apiVersion: lpm.my.domain/v1
kind: PodMigration
metadata:
  name: redis-local-migration
  namespace: default
spec:
  podName: redis-local
  targetNode: k8s-worker2
EOF
```

6. **Observe results**
   - Check Pod statuses and events:
```bash
kubectl wait --for=jsonpath='{.status.phase}'=Restoring podmigration/redis-local-migration --timeout=5m

kubectl get pods -o wide
kubectl describe pod redis-local-restored
```
   - Verify that restored pod fails to start
      - The pod `redis-local-restored` is stuck in `ContainerCreating` STATUS
      - Describing the pod will show the following event that is the reason the restoration is stuck
      ```
      Events:
      Type     Reason       Age               From     Message
      ----     ------       ----              ----     -------
      Warning  FailedMount  5s (x5 over 12s)  kubelet  MountVolume.NodeAffinity check failed for volume "redis-local-pv" : no matching NodeSelectorTerms
      ```

7. **Cleanup**
```bash
kubectl delete pod redis-local redis-local-restored --ignore-not-found=true
kubectl delete pvc redis-local-pvc --ignore-not-found=true
kubectl delete pv redis-local-pv --ignore-not-found=true
kubectl delete podmigration redis-local-migration --ignore-not-found=true
```

---

## Notes
- Local PVs are not portable across nodes. For successful cross-node migration with data persistence, prefer network-backed volumes (e.g., NFS/CSI with `ReadWriteMany`).
- This scenario is intended to illustrate the limitation and to validate controller behavior when node-bound storage is used.


