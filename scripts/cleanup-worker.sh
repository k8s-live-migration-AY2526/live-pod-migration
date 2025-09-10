#!/bin/bash
echo "=== Cleaning up checkpoint agent image ==="

# Remove the image from Buildah's local storage
if sudo buildah images | grep -q "localhost/checkpoint-agent"; then
    sudo buildah rmi localhost/checkpoint-agent:latest
    echo "Removed Buildah local image"
else
    echo "No Buildah image found to remove"
fi
echo

# Remove the image from CRI-O local store
OCI_PATH="/var/lib/containers/storage"
if [ -d "$OCI_PATH" ]; then
    sudo rm -rf "$OCI_PATH/localhost/checkpoint-agent:latest"
    echo "Removed image from CRI-O local store"
else
    echo "CRI-O storage path not found"
fi
echo

echo "=== Cleanup complete! ==="
