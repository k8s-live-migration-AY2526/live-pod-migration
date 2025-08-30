#!/bin/bash

echo "Building Docker image..."
podman build -t httpserver:latest .

echo "Docker image built successfully!"
echo ""
echo "To run locally:"
echo "  docker run -p 8080:8080 httpserver:latest"
echo ""
echo "To deploy to Kubernetes:"
echo "  kubectl apply -f k8s-deployment.yaml"
echo ""
echo "Endpoints:"
echo "  GET  /health  - Health check"
echo "  GET  /counter - Counter endpoint"
echo "  POST /counter - Counter endpoint"
