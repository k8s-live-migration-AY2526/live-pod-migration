# Webhook Setup Guide

The mutating webhook intercepts pod creation requests to detect StatefulSet pods that are being recreated as part of a migration.

## Quick Setup

### Prerequisites
- Controller must be deployed first: `make deploy IMG=localhost/controller:latest AGENT_IMG=localhost/checkpoint-agent:latest`
- Namespace: `live-pod-migration-controller-system`

### Setup Webhook

Run the setup script:

```bash
./scripts/setup-webhook.sh
```

This script will:
1. Generate self-signed certificates
2. Create a Kubernetes secret with the certificates
3. Patch the webhook configuration with the CA bundle

### Cleanup Webhook

To remove the webhook:

```bash
./scripts/cleanup-webhook.sh
```

This removes:
- MutatingWebhookConfiguration
- Certificate secret
- Local certificate files

## Manual Setup (if needed)

<details>
<summary>Click to expand manual steps</summary>

### Step 1: Generate Certificate

```bash
mkdir -p /tmp/k8s-webhook-certs
openssl req -x509 -newkey rsa:2048 \
  -keyout /tmp/k8s-webhook-certs/tls.key \
  -out /tmp/k8s-webhook-certs/tls.crt \
  -days 365 -nodes \
  -subj "/CN=lpm-webhook-service.live-pod-migration-controller-system.svc"
```

### Step 2: Create Kubernetes Secret

```bash
kubectl create secret tls webhook-server-cert \
  --cert=/tmp/k8s-webhook-certs/tls.crt \
  --key=/tmp/k8s-webhook-certs/tls.key \
  -n live-pod-migration-controller-system
```

### Step 3: Patch Webhook Configuration

```bash
CA_BUNDLE=$(cat /tmp/k8s-webhook-certs/tls.crt | base64 | tr -d '\n')
kubectl patch mutatingwebhookconfiguration lpm-mutating-webhook-configuration \
  --type='json' \
  -p="[{'op': 'add', 'path': '/webhooks/0/clientConfig/caBundle', 'value':'${CA_BUNDLE}'}]"
```

</details>

## Verification

Check webhook is working:

```bash
# Check webhook configuration exists
kubectl get mutatingwebhookconfigurations lpm-mutating-webhook-configuration

# Check webhook service
kubectl get svc -n live-pod-migration-controller-system lpm-webhook-service

# View webhook logs (create a StatefulSet pod to see detection)
kubectl logs -n live-pod-migration-controller-system deployment/lpm-controller-manager -f | grep "pod-mutator"
```

## Troubleshooting

**TLS handshake errors?** 
- Re-run `./scripts/setup-webhook.sh` to regenerate certificates and patch the webhook

**Webhook not being called?**
- Verify `failurePolicy: Ignore` is set (webhook won't block operations if it fails)
- Check the controller manager logs for webhook server startup messages
