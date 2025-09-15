# Network: NodePort Service (nginx)

---

## Scenario
- Single Pod (via Deployment) `nginx-nodeport`
- Exposed through a NodePort Service at `30080`
- Migration should work and allow clients to connect to the new nginx pod on the new node via the NodePort

---

## Steps to Test

1. Deploy nginx Deployment + NodePort Service
```bash
kubectl apply -f nginx.yaml
```

2. Verify pod running on worker
```bash
kubectl wait --for=condition=Ready pod/nginx-nodeport --timeout=5m

# Running on worker
kubectl get pods -o wide

# Verify service endpoint correctly configured
kubectl get svc
kubectl get endpoints nginx-nodeport-svc
```
Expected:
```
NAME                 TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)        AGE
kubernetes           ClusterIP   10.96.0.1       <none>        443/TCP        3d14h
nginx-nodeport-svc   NodePort    10.102.133.58   <none>        80:30080/TCP   101s
NAME                 ENDPOINTS        AGE
nginx-nodeport-svc   10.244.1.11:80   101s
```

3. Test connectivity to nginx pod through Service (NodePort) on worker
```bash
# From outside VM, curl to worker --> success, since there is pod running on worker
curl http://192.168.56.11:30080/

# From outside VM, curl to worker2 --> fail, since there is no pod running and we do not have any routing rules to route this to worker (restriction of testing on vagrant)
curl http://192.168.56.12:30080/
```
4. Trigger migration
```bash
kubectl apply -f - <<EOF
apiVersion: lpm.my.domain/v1
kind: PodMigration
metadata:
  name: nginx-nodeport-migration
  namespace: default
spec:
  podName: nginx-nodeport
  targetNode: k8s-worker2
EOF
```

5. Verify pod running on worker2
```bash
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded podmigration/nginx-nodeport-migration --timeout=5m

kubectl get pods -o wide 
```

6. Test connectivity to nginx pod through Service (NodePort) on worker2
```bash
# From outside VM, curl to worker --> fail (original pod terminated)
curl http://192.168.56.11:30080/

# From outside VM, curl to worker2 --> success
curl http://192.168.56.12:30080/
```

7. Cleanup
```bash
kubectl delete pod nginx-nodeport nginx-nodeport-restored --ignore-not-found
kubectl delete service nginx-nodeport-svc --ignore-not-found
kubectl delete podmigration nginx-nodeport-migration --ignore-not-found
```