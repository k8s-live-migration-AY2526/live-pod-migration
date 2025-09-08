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
sudo podman build -t httpserver:latest .

# Run the container
sudo podman run -p 8080:8080 httpserver:latest
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
# (If not using Dockerhub, save and load the image locally)
# Build podman image
./build.sh

# On worker node:
sudo podman load -i /tmp_sync/httpserver.tar
sudo podman push localhost/httpserver:latest containers-storage:httpserver:latest

# Deploy to Kubernetes
kubectl apply -f k8s-deployment.yaml

-----

# (If using Dockerhub, push image to Dockerhub)
sudo podman build --no-cache -t $IMAGE_NAME:$TAG .
sudo podman push $IMAGE_NAME:$TAG

# Then deploy with:
kubectl apply -f k8s-deployment.yaml
```

### Access the service:
```bash
# Port forward to access locally
kubectl port-forward pod/httpserver 8080:8080

# Then test with:
curl http://localhost:8080/health
curl http://localhost:8080/counter
```
