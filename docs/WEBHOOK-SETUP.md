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
- Mutating webhook service
- Certificate secret
- Local certificate files

## Verification

Check webhook is working:

```bash
# Check webhook configuration exists
kubectl get mutatingwebhookconfigurations lpm-mutating-webhook-configuration

# Check webhook service
kubectl get svc -n live-pod-migration-controller-system lpm-webhook-service
```

## Troubleshooting

**TLS handshake errors?** 
- Re-run `./scripts/setup-webhook.sh` to regenerate certificates and patch the webhook

**Webhook not being called?**
- Verify `failurePolicy: Ignore` is set (webhook won't block operations if it fails)
- Check the controller manager logs for webhook server startup messages
