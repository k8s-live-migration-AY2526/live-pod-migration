# Network: Long-Lived TCP (SSH Server) - IN PROGRESS

---

## Scenario
- Single Pod `ssh-server`
- Container exposes `hostPort: 2222` for SSH (`containerPort: 22`)

## Expected Outcome
- Migration should not work if there is an open tcp connection to the ssh-server
    - This is a limitation of the CRIU checkpoint api, where checkpointing will fail if there are open connections
    - To checkpoint pods with open connections, need to use `--tcp-established` flag 

---

## Steps to Test

1. Deploy the pod
```bash
kubectl apply -f ssh-server.yaml
```

2. Verify pod running and on which node
```bash
kubectl wait --for=condition=Ready pod/ssh-server --timeout=5m

# Running on worker
kubectl get pods -o wide
```

3. Test connectivity to SSH server on the current node
```bash
# Get the username used for ssh and update the next command if required
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
  targetNode: k8s-worker2
EOF
```

5. Verify pod migration failed
```bash
# Wait for the status to transition to failed, this will take a while...
kubectl wait --for=jsonpath='{.status.phase}'=Failed podmigration/ssh-server-migration --timeout=5m

kubectl get podmigration
```

6. Cleanup
```bash
kubectl delete pod ssh-server ssh-server-restored --ignore-not-found
kubectl delete podmigration ssh-server-migration --ignore-not-found

# Outside VM, remove ssh keys
ssh-keygen -R "[192.168.56.11]:2222" 
```
