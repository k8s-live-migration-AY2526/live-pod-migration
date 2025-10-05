# Simple Deployment Migration Test

A simple Deployment with nginx and busybox containers for testing pod migration.

Expected behaviour: When a pod is migrated, the busybox container should continue printing the date every 30 seconds without interruption.

## Deploy

```bash
kubectl apply -f deployment.yaml
```

## Get pod name and migrate

```bash
kubectl get pods -l app=simple-test
# Copy one of the pod names and update migration.yaml
kubectl apply -f migration.yaml
```

## Cleanup

```bash
kubectl delete -f deployment.yaml
kubectl delete -f migration.yaml
```