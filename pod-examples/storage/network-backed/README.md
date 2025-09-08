# Network-Backed Storage Test

---

## Pre-requisites
- Requires a storage class deployed in cluster
- Example uses `nfs-csi` storage class which is the same storage class used to sync checkpoint files, in a real world setting they would not be the same

## Pod Characteristics

- **Name:** `redis-nfs`  
- **Image:** `redis:7`  
- **Behavior:**  
  - Runs a Redis server storing data at `/data`  
- **Volume:** PersistentVolumeClaim (`redis-nfs-pvc`) backed by **NFS** (network storage)  
- **Access Mode:** `ReadWriteMany`  
- **Purpose:** Test live pod migration (CRIU) with network-backed storage  

---

## Expected Output

- **Before Migration:**  
  - Pod is running  
  - Redis stores data under `/data` on the NFS volume  
  - Logs show Redis startup and connections:  
    ```
    1:C 08 Sep 2025 12:00:00.000 # Server started, Redis version 7.x
    1:M 08 Sep 2025 12:00:03.000 * DB loaded from disk: 0.000 seconds
    1:M 08 Sep 2025 12:00:03.001 * Ready to accept connections
    ```

- **After Migration:**  
  - Pod continues running on target node  
  - Redis data persists because it is stored on network volume  
  - No data is lost across migration  
  - Redis logs continue normally  

---

## Steps to Test

1. **Deploy PVC and Pod**
```bash
kubectl apply -f redis-nfs.yaml
```

2. **Verify Pod is running**
```bash
kubectl get pods
```
Expected:
```
NAME       READY   STATUS    RESTARTS   AGE
redis-nfs  1/1     Running   0          30s
```

3. **Check Redis server logs before migration**
```bash
kubectl logs -f redis-nfs
```

4. **Store some test data in Redis**
```bash
kubectl exec redis-nfs -- redis-cli SET mykey "migration test"
kubectl exec redis-nfs -- redis-cli GET mykey
```
Expected output:
```
"migration test"
```

4. **Trigger migration**
```bash
kubectl apply -f - <<EOF
apiVersion: lpm.my.domain/v1
kind: PodMigration
metadata:
    name: redis-nfs-migration
    namespace: default
spec:
    podName: redis-nfs
    targetNode: k8s-master
EOF
```
6. **Verify after migration**
    - Check that the restored pod is running:
```bash
kubectl get pods
```
    - Verify stored data still exists:
```bash
kubectl exec redis-nfs-restored -- redis-cli GET mykey
```
      Expected output:
      ```
      migration test
      ```

7. **Cleanup**
```bash
kubectl delete pod redis-nfs redis-nfs-restored --ignore-not-found=true
kubectl delete pvc redis-nfs-pvc --ignore-not-found=true
kubectl delete podmigration redis-nfs-migration --ignore-not-found=true
```