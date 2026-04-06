#!/bin/bash

if [ -z "$1" ]; then
  echo "Usage: $0 <number_of_pods>"
  exit 1
fi

LOOP_COUNT=$1

for i in $(seq 1 $LOOP_COUNT); do
kubectl apply -f - <<EOF
apiVersion: lpm.my.domain/v1
kind: PodMigration
metadata:
  name: memhog-migration-$i
  namespace: default
spec:
  podName: memhog-$i
  targetNode: k8s-worker
EOF
done