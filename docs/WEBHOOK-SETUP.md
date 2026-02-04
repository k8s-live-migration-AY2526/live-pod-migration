# Webhook Setup Guide

The mutating webhook intercepts pod creation requests to detect StatefulSet pods that are being recreated as part of a migration.

## Quick Setup

### Prerequisites
- Namespace: `live-pod-migration-controller-system`

### Setup Sequence

The controller deployment requires the `webhook-server-cert` Secret to exist (it's mounted as a volume). Follow this sequence:

1. **Deploy the controller:**
   ```bash
   make deploy IMG=localhost/controller:latest AGENT_IMG=localhost/checkpoint-agent:latest
   ```
   Note: The controller manager pod will remain in `ContainerCreating` or `Pending` state until the certificate secret is created.

2. **Run the webhook setup script immediately** (before waiting for pod readiness):
   ```bash
   ./scripts/setup-webhook.sh
   ```
   
   This script will:
   - Generate self-signed certificates with proper SANs
   - Create the `webhook-server-cert` Secret (allowing the controller pod to start)
   - Patch the webhook configuration with the CA bundle
   - Restart the controller to load the certificates

3. **Verify the controller is running:**
   ```bash
   kubectl get pods -n live-pod-migration-controller-system
   ```

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
