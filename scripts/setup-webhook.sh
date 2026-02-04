#!/bin/bash
# Setup webhook with self-signed certificates
# Run this once after deploying the controller

set -e

NAMESPACE="live-pod-migration-controller-system"
CERT_DIR="/tmp/k8s-webhook-certs"

echo "🔐 Setting up webhook certificates..."

# Create cert directory
mkdir -p ${CERT_DIR}

# Generate self-signed certificate with proper SANs
echo "Generating self-signed certificate..."
cat > ${CERT_DIR}/csr.conf <<EOF
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name
[req_distinguished_name]
[v3_req]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names
[alt_names]
DNS.1 = lpm-webhook-service
DNS.2 = lpm-webhook-service.${NAMESPACE}
DNS.3 = lpm-webhook-service.${NAMESPACE}.svc
DNS.4 = lpm-webhook-service.${NAMESPACE}.svc.cluster.local
EOF

openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout ${CERT_DIR}/tls.key \
  -out ${CERT_DIR}/tls.crt \
  -subj "/CN=lpm-webhook-service.${NAMESPACE}.svc" \
  -config ${CERT_DIR}/csr.conf \
  -extensions v3_req \
  2>/dev/null

# Create or update the secret
echo "Creating webhook certificate secret..."
kubectl create secret tls webhook-server-cert \
  --cert=${CERT_DIR}/tls.crt \
  --key=${CERT_DIR}/tls.key \
  -n ${NAMESPACE} \
  --dry-run=client -o yaml | kubectl apply -f -

# Get CA bundle
CA_BUNDLE=$(cat ${CERT_DIR}/tls.crt | base64 | tr -d '\n')

# Patch webhook configuration with CA bundle
echo "Patching webhook configuration with CA bundle..."
kubectl patch mutatingwebhookconfiguration lpm-mutating-webhook-configuration \
  --type='json' \
  -p="[{\"op\": \"replace\", \"path\": \"/webhooks/0/clientConfig/caBundle\", \"value\":\"${CA_BUNDLE}\"}]"

# Restart controller to pick up new certificate
echo "Restarting controller to load new certificate..."
kubectl rollout restart deployment/lpm-controller-manager -n ${NAMESPACE}
kubectl rollout status deployment/lpm-controller-manager -n ${NAMESPACE} --timeout=60s

echo "✅ Webhook setup complete!"
echo ""
echo "To verify:"
echo "  kubectl get mutatingwebhookconfiguration lpm-mutating-webhook-configuration"
