# Network: ClusterIP Service (nginx)

---

## Scenario
- Single Pod `nginx-clusterip`
- Exposed through a ClusterIP Service
- Migration should work and allow internal clients (or port-forward) to connect to the new pod after migration

---

## Steps to Test

1. Deploy nginx Pod + ClusterIP Service
```bash
kubectl apply -f nginx.yaml
```

2. Verify pod running on worker
```bash
kubectl wait --for=condition=Ready pod/nginx-clusterip --timeout=5m

# Verify pod is scheduled on worker
kubectl get pods -o wide

# Verify service endpoint correctly configured
kubectl get svc
kubectl get endpoints nginx-clusterip-svc
```
Expected:
```
NAME                  TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)   AGE
kubernetes            ClusterIP   10.96.0.1       <none>        443/TCP   3d14h
nginx-clusterip-svc   ClusterIP   10.111.52.159   <none>        80/TCP    24s
NAME                  ENDPOINTS        AGE
nginx-clusterip-svc   10.244.1.12:80   24s
```

3. Test connectivity to nginx pod via port-forward (since ClusterIP is internal)
```bash
# Forward local port 8080 to Service port 80, terminate port-forwarding after testing the next curl command
kubectl port-forward svc/nginx-clusterip-svc 8080:80

# Create a new terminal and ssh into master, and execute
curl http://localhost:8080/ # Should curl successfully
```

---

4. Trigger migration
```bash
kubectl apply -f - <<EOF
apiVersion: lpm.my.domain/v1
kind: PodMigration
metadata:
  name: nginx-clusterip-migration
  namespace: default
spec:
  podName: nginx-clusterip
  targetNode: k8s-worker2
EOF
```

5. Verify pod running on worker2
```bash
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded podmigration/nginx-clusterip-migration --timeout=5m

kubectl get pods -o wide
kubectl get endpoints nginx-clusterip-svc
```

---

6. Test connectivity via port-forward again
```bash
# Forward local port 8080 to Service port 80, terminate port-forwarding after testing the next curl command
kubectl port-forward svc/nginx-clusterip-svc 8080:80

# Create a new terminal and ssh into master, and execute
curl http://localhost:8080/ # Should curl successfully
```
Expected: Response from nginx on the new node

---

7. Cleanup
```bash
kubectl delete pod nginx-clusterip nginx-clusterip-restored --ignore-not-found
kubectl delete service nginx-clusterip-svc --ignore-not-found
kubectl delete podmigration nginx-clusterip-migration --ignore-not-found
```
