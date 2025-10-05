# Simple StatefulSet Migration Test

A simple StatefulSet with persistent storage for testing pod migration with PVCs.

**Key difference from Deployment**: This test verifies that persistent volumes are correctly reattached to migrated StatefulSet pods, which is critical for stateful applications.

Expected behaviour: When the StatefulSet pod is migrated, the busybox container should continue writing to the persistent volume. The counter should continue incrementing without reset, demonstrating that the PVC is correctly reattached to the migrated pod on the new node.

## Deploy

```bash
kubectl apply -f statefulset.yaml
```

## Wait for pod to be ready

```bash
kubectl get pods -l app=statefulset-test -w
```

## Migrate the pod

The pod name is predictable: `statefulset-test-0`

```bash
kubectl apply -f migration.yaml
```

## Verify migration

Check the logs to ensure the counter continues without interruption:

```bash
kubectl logs statefulset-test-0 -c busybox -f
```

Check the persistent data survived migration:

```bash
kubectl exec statefulset-test-0 -- cat /data/log.txt
```

## Cleanup

```bash
kubectl delete -f migration.yaml
kubectl delete -f statefulset.yaml
```