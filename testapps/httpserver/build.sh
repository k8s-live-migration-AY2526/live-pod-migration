#!/bin/bash

IMAGE_NAME="httpserver:latest"
TAR_FILE="/tmp_sync/httpserver.tar"

echo "Building Podman image..."
sudo podman build -t $IMAGE_NAME .
echo "Podman image built successfully!"

echo "Saving image to tarball..."
sudo podman save -o $TAR_FILE $IMAGE_NAME
echo "Image saved to $TAR_FILE"

echo "To use on worker node, run:"
echo "  sudo podman load -i $TAR_FILE"
echo "  sudo podman push localhost/httpserver:latest containers-storage:httpserver:latest"
