# Minimal Python HTTP Server

A simple Python HTTP server with health check and counter endpoints.

## Endpoints

- `GET /health` - Health check endpoint
- `GET/POST /counter` - Counter endpoint (increments on each call)
- `GET /` - Root endpoint with API information

## Local Development

### Run with Python directly:
```bash
pip install -r requirements.txt
python app.py
```

### Run with podman:
```bash
# Install podman
sudo apt-get update
sudo apt-get -y install podman

# Build the image
podman build -t httpserver:latest .

# Run the container
podman run -p 8080:8080 httpserver:latest
```

### Test the endpoints:
```bash
# Health check
curl http://localhost:8080/health

# Counter endpoint
curl http://localhost:8080/counter
curl -X POST http://localhost:8080/counter
```

## Kubernetes Deployment

### Build and deploy:
```bash
# Build podman image
./build.sh

# Copy to tmp_sync folder for access in k8s-worker
podman save httpserver:latest -o httpserver.tar
mv httpserver.tar /tmp_sync/

# Load the image in k8s-worker
podman load -i /tmp_sync/httpserver.tar

# Push to crio
sudo podman push localhost/httpserver:latest containers-storage:httpserver:latest

# Deploy to Kubernetes
kubectl apply -f k8s-deployment.yaml

# Check deployment status
kubectl get pods
```

### Access the service:
```bash
# Port forward to access locally
kubectl port-forward service/httpserver-service 8080:8080

# Then test with:
curl http://localhost:8080/health
curl http://localhost:8080/counter
```
