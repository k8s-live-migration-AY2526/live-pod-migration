# Network: HostPort Pod (nginx)

---

## Scenario
- Single Pod `nginx-hostport`
- Container exposes `hostPort: 8080`

## Expected Outcome
- Migration should work and allow clients to connect to the new nginx server running on new node via hostPort

---

## Steps to Test

1. Deploy the pod
```bash
kubectl apply -f nginx.yaml
```

2. Verify pod running on worker
```bash
kubectl wait --for=condition=Ready pod/nginx-hostport --timeout=5m

# Running on worker
kubectl get pods -o wide
```

3. Test connectivity to nginx pod running on worker
```bash
# From outside VMs. Expected outcome: curl should succeed and return the result
curl http://192.168.56.11:8080/
```

4. Trigger migration
```bash
kubectl apply -f - <<EOF
apiVersion: lpm.my.domain/v1
kind: PodMigration
metadata:
  name: nginx-hostport-migration
  namespace: default
spec:
  podName: nginx-hostport
  targetNode: k8s-worker2
EOF
```

5. Verify pod running on master
```bash
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded podmigration/nginx-hostport-migration --timeout=5m

# Restored pod running on worker2
kubectl get pods-o wide
```

6. Test connectivity to nginx pod running on master
```bash
# From outside VMs. Expected outcome: curl should succeed and return the result
curl http://192.168.56.12:8080/
```

7. Cleanup
```bash
kubectl delete pod nginx-hostport nginx-hostport-restored --ignore-not-found
kubectl delete podmigration nginx-hostport-migration --ignore-not-found
```
