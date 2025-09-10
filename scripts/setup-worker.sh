#!/bin/bash
echo "=== Building checkpoint agent ==="
sudo buildah bud -t localhost/checkpoint-agent:latest -f Dockerfile.agent .
echo

echo "=== Pushing checkpoint agent into CRI-O local store ==="
sudo buildah push localhost/checkpoint-agent:latest oci:/var/lib/containers/storage:localhost/checkpoint-agent:latest
echo

echo "=== Setup complete! ==="
