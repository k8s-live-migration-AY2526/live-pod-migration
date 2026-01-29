# Network Close-Connection Test

---

## Pod Characteristics

- **Server Pod:** `redis-server`
  - **Image:** `redis:6.0-alpine`
  - **Function:** Standard Redis server listening on port 6379 or `redis-service` (ClusterIP `10.96.1.1`).
  - **Behavior:** Stores data in memory (and `emptyDir` volume).
- **Client Pod:** `redis-client`
  - **Image:** `python:3.9-slim`
  - **Behavior:**
    - Connects to `redis-service`.
    - Increments a counter (`my_counter`) every second.
    - Prints the "Peer IP" (actual pod IP it is connected to) and Redis Server Run ID.
    - Handles connection failures by retrying (validating reconnection logic).
- **Migration:** `redis-server-migration`
  - **Type:** Live migration with `closeTcp: true`.
  - **Purpose:** Verifies that active TCP connections are forcibly closed during migration, prompting clients to reconnect to the destination pod.

---

## Steps to Test

1. **Deploy the Workload**
```bash
kubectl apply -f server.yaml
kubectl apply -f client.yaml
```

2. **Verify Pods are running**
```bash
kubectl wait --for=condition=Ready pod/redis-server --timeout=5m
kubectl wait --for=condition=Ready pod/redis-client --timeout=5m

# Check running nodes (Server should be on k8s-worker initially)
kubectl get pods -o wide
```

3. **Observe Client Logs (Before Migration)**
Wait for the client to start printing successful connection logs.
```bash
kubectl logs -f redis-client
```
Expected output (example):
```
[12:00:01] Connected to IP: 10.244.1.4 | ServerID: 80a1b2c3 | Counter: 1
[12:00:02] Connected to IP: 10.244.1.4 | ServerID: 80a1b2c3 | Counter: 2
```

4. **Trigger Migration (Failure Case: Without `closeTcp`)**
First, attempt migration without the `closeTcp` flag. This should fail because the pod has established TCP connections.

```bash
kubectl apply -f migration.yaml
```

5. **Verify Migration Failed**
```bash
kubectl wait --for=jsonpath='{.status.phase}'=Failed podmigration/redis-server-migration --timeout=5m
```

6. **Trigger Migration (Success Case: With `closeTcp`)**
Now, apply the migration that forces TCP connections to close.
```bash
kubectl apply -f migration-closeTcp.yaml
```

7. **Verify Migration Succeeded**
```bash
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded podmigration/redis-server-migration-close-tcp --timeout=5m
```

8. **Verify Server Moved**
```bash
kubectl get pods -o wide
# redis-server should now be on k8s-worker2
```

9. **Observe Client Logs (After Migration)**
The client should experience a momentary disconnection but recover and continue incrementing the counter.

```bash
kubectl logs -f redis-client
```
Expected behavior:
- Brief error logs (ConnectionError/Timeout) during migration.
- Client successfully reconnects.
- **Counter continues incrementing** (proving memory state was migrated).
- **ServerID** remains consistent (proving it's the same Redis instance state).

10. **Cleanup**
```bash
kubectl delete -f migration.yaml --ignore-not-found=true
kubectl delete -f migration-closeTcp.yaml --ignore-not-found=true
kubectl delete -f client.yaml --ignore-not-found=true
kubectl delete -f server.yaml --ignore-not-found=true
```
