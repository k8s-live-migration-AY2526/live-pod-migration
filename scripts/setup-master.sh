#!/bin/bash
set -euo pipefail

echo "=== Building controller manager ==="
sudo buildah bud -t localhost/controller:latest .
echo

echo "=== Building checkpoint agent ==="
sudo buildah bud -t localhost/checkpoint-agent:latest -f Dockerfile.agent .
echo

echo "=== Pushing images into CRI-O local store ==="
sudo buildah push localhost/controller:latest oci:/var/lib/containers/storage:localhost/controller:latest
sudo buildah push localhost/checkpoint-agent:latest oci:/var/lib/containers/storage:localhost/checkpoint-agent:latest
echo

echo "=== Deploying system (CRDs, RBAC, controller, agent DaemonSet) ==="
make deploy IMG=localhost/controller:latest AGENT_IMG=localhost/checkpoint-agent:latest
echo

echo "=== Deploying shared storage for cross-node checkpoint access ==="
./deploy-shared-storage.sh
echo

echo "=== Setup complete! ==="
