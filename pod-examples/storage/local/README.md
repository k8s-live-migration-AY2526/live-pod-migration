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

1. **Review and edit the manifest**
   - In `redis-local.yaml`, set the Local PV `spec.local.path` to a real path on the target node.
   - Ensure `nodeAffinity` hostname matches your worker node (e.g., `k8s-worker`).

2. **Prepare local path on the target node**
   - Ensure the directory exists and is writable on the node backing the Local PV (e.g., `k8s-worker`).
```bash
# on k8s-worker
sudo mkdir -p /mnt/disks/ssd1
sudo chown 1001:1001 /mnt/disks/ssd1   # or set appropriate perms for Redis container
sudo chmod 0777 /mnt/disks/ssd1        # quick test-friendly option
```

3. **Deploy PV, PVC, and Pod**
```bash
kubectl apply -f redis-local.yaml
```

4. **Verify Pod is running**
```bash
kubectl get pods -o wide
```
Expected:
```
NAME          READY   STATUS    RESTARTS   AGE   NODE
redis-local   1/1     Running   0          30s   k8s-worker
```

5. **Check Redis server logs before migration**
```bash
kubectl logs -f redis-local
```

6. **Store some test data in Redis**
```bash
kubectl exec redis-local -- redis-cli SET mykey "local pv test"
kubectl exec redis-local -- redis-cli GET mykey
```
Expected output:
```
local pv test
```

7. **Trigger migration**
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
  targetNode: k8s-master
EOF
```

8. **Observe results**
   - Check Pod statuses and events:
```bash
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

9. **Cleanup**
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


