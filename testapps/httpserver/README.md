# Minimal Python HTTP Server

A simple Python HTTP server with health check and counter endpoints, designed for Kubernetes deployment.

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

### Run with Docker:
```bash
# Build the image
docker build -t httpserver:latest .

# Run the container
docker run -p 8080:8080 httpserver:latest
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
# Build Docker image
./build.sh

# Deploy to Kubernetes
kubectl apply -f k8s-deployment.yaml

# Check deployment status
kubectl get pods
kubectl get services
```

### Access the service:
```bash
# Port forward to access locally
kubectl port-forward service/httpserver-service 8080:80

# Then test with:
curl http://localhost:8080/health
curl http://localhost:8080/counter
```

## Features

- **Minimal**: Simple Flask application with just the required endpoints
- **Logging**: Basic structured logging for all requests
- **Containerized**: Docker image ready for deployment
- **Kubernetes Ready**: Includes deployment and service manifests
- **Health Checks**: Built-in health check endpoint for Kubernetes probes
- **Resource Limits**: Configured with appropriate resource requests/limits
