# Network: Long-Lived TCP (SSH Server) - IN PROGRESS

---

## Scenario
- Single Pod `ssh-server`
- Container exposes `hostPort: 2222` for SSH (`containerPort: 22`)

## Expected Outcome
- Migration should work and allow SSH clients to connect to the SSH server on the new node via the same `hostPort`.

---

## Steps to Test

1. Deploy the pod
```bash
kubectl apply -f ssh-server.yaml
```

2. Verify pod running and on which node
```bash
kubectl get pods -o wide
```

3. Test connectivity to SSH server on the current node
```bash
# Get the username used for ssh
kubectl logs ssh-server | grep "User name is set to"

# From outside the VMs. Expected outcome: SSH should prompt for and accept the password
ssh -p 2222 linuxserver.io@192.168.56.11
# When prompted, enter: password

# Keep the connection opened...
```

4. Trigger migration
```bash
kubectl apply -f - <<EOF
apiVersion: lpm.my.domain/v1
kind: PodMigration
metadata:
  name: ssh-server-migration
  namespace: default
spec:
  podName: ssh-server
  targetNode: k8s-master
EOF
```

5. Verify pod running on the target node
```bash
kubectl get pods -o wide
```

6. Test connectivity to SSH server on the new node
```bash
# From outside the VMs. Expected outcome: SSH should succeed on the new node's IP via port 2222
ssh -p 2222 linuxserver.io@192.168.56.10
# When prompted, enter: password
```

7. Cleanup
```bash
kubectl delete pod ssh-server ssh-server-restored --ignore-not-found
kubectl delete podmigration ssh-server-migration --ignore-not-found
```


