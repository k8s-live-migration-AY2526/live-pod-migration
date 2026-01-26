#!/bin/bash
# Remove webhook configuration and certificates

set -e

NAMESPACE="live-pod-migration-controller-system"
CERT_DIR="/tmp/k8s-webhook-certs"

echo "🗑️  Cleaning up webhook..."

# Delete webhook configuration
echo "Deleting webhook configuration..."
kubectl delete mutatingwebhookconfiguration lpm-mutating-webhook-configuration --ignore-not-found=true

# Delete certificate secret
echo "Deleting webhook certificate secret..."
kubectl delete secret webhook-server-cert -n ${NAMESPACE} --ignore-not-found=true

# Clean up local certificate files
if [ -d ${CERT_DIR} ]; then
  echo "Removing local certificate files..."
  rm -rf ${CERT_DIR}
fi

echo "✅ Webhook cleanup complete!"
echo ""
echo "Note: The webhook code is still in the controller."
echo "To fully disable, comment out the webhook registration in cmd/main.go"
