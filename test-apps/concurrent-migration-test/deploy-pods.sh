#!/bin/bash

if [ -z "$1" ]; then
  echo "Usage: $0 <number_of_pods>"
  exit 1
fi

LOOP_COUNT=$1

for i in $(seq 1 $LOOP_COUNT); do
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: memhog-$i
spec:
  nodeName: k8s-master
  restartPolicy: Always
  containers:
  - name: memhog
    image: python:3.12-alpine
    resources:
      requests:
        memory: "280Mi"
      limits:
        memory: "280Mi"
    command: ["python", "-c"]
    args:
    - |
      import os, time
      m = 256 * 1024 * 1024
      b = bytearray(m)
      for i in range(0, len(b), 4096):
          b[i] = 1
      print(f"Allocated {m} bytes, pid={os.getpid()}", flush=True)
      while True:
          time.sleep(60)
EOF
done