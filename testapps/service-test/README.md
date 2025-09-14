# Service Migration Test

Simple service migration test - nginx server behind a service with a client that continuously tests connectivity.

## Usage

1) Deploy server and client:
```sh
kubectl apply -f service-test.yaml
```

2) Watch client logs:
```sh
kubectl logs -f client-pod
```

3) Access from outside the cluster:
```sh
# Get your node IP
kubectl get nodes -o wide

# Access nginx via NodePort (port 30080), use worker node's IP
curl http://<node-ip>:30080
```

4) Trigger migration:
```sh
kubectl apply -f migration.yaml
```

You should see mostly OK messages. Brief FAIL messages during migration are normal.
The external access via NodePort should continue working during migration.

## Cleanup

```sh
kubectl delete -f migration.yaml --ignore-not-found
kubectl delete -f service-test.yaml
```