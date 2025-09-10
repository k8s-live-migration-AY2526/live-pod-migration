# Vagrant Kubernetes Cluster for Live Pod Migration Testing

This directory contains a simplified Vagrant setup suitable for testing the live pod migration controller with a 3-node Kubernetes cluster.

## Prerequisites

- [Vagrant](https://www.vagrantup.com/downloads) installed
- [VirtualBox](https://www.virtualbox.org/wiki/Downloads) or [Parallels](https://www.parallels.com/) installed
- At least 8GB RAM available for VMs

## Cluster Architecture

- **Master Node** (`k8s-master`): 4GB RAM, 2 CPUs, IP: 192.168.56.10
- **Worker Node** (`k8s-worker`): 2GB RAM, 2 CPUs, IP: 192.168.56.11
- **Worker2 Node** (`k8s-worker2`): 2GB RAM, 2 CPUs, IP: 192.168.56.12

## Quick Start

### 1. Start the Cluster

```bash
cd vagrant/
vagrant up
```

### 2. Join the workers
```bash
# SSH into master node
vagrant ssh master

# On master, generate the join command, should generate something like kubeadm join 192.168.56.10:6443 --token ... 
kubeadm token create --print-join-command

# Copy the above command and run it on both workers with sudo access

# SSH into worker node
vagrant ssh worker
sudo kubeadm join 192.168.56.10:6443 --token ...

# SSH into worker2 node
vagrant ssh worker2
sudo kubeadm join 192.168.56.10:6443 --token ...

# On master, wait for all nodes to be ready
kubectl wait --for=condition=Ready nodes --all --timeout=10m
```

### 3. Verify Cluster

```bash
# SSH into master node
vagrant ssh master

# Check cluster status, all pods should be running
kubectl get nodes
kubectl get pods --all-namespaces
```

### 4. Deploy Shared Storage (NFS)
```bash
# SSH into master node
vagrant ssh master
cd /home/vagrant/live-pod-migration-controller/vagrant
./deploy-shared-storage.sh
```

## What's Included

### Software Stack
- **Ubuntu 22.04** base image
- **CRIO** container runtime
- **Kubernetes 1.28** (kubeadm, kubelet, kubectl)
- **Flannel CNI** for pod networking
- **Buildah** package manager
- **Go 1.21.5** for building controllers

### Network Configuration
- Pod CIDR: `10.244.0.0/16`
- Service CIDR: `10.96.0.0/12` (default)
- Node network: `192.168.56.0/24`

## Useful Commands

```bash
# Cluster management
vagrant status                    # Check VM status
vagrant ssh master               # SSH to master
vagrant ssh worker               # SSH to worker
vagrant halt                     # Stop all VMs
vagrant destroy                  # Delete all VMs
```

## Troubleshooting

### VMs Won't Start
- Ensure you have enough RAM (6GB minimum)
- Check VirtualBox/Parallels is properly installed
- Try `vagrant reload` if VMs are stuck

### Kubernetes Issues
- Check kubelet logs: `sudo journalctl -u kubelet -f`
- Verify Docker is running: `sudo systemctl status docker`
- Check CNI pods: `kubectl get pods -n kube-flannel`

## Development Workflow

1. **Make changes** to the controller code on your host machine
2. **Sync changes** to VMs: `vagrant rsync`
3. **Build + Deploy + Test** inside master VM, run your build + deploy commands

## Cleanup

```bash
# Stop VMs
vagrant halt

# Remove VMs completely
vagrant destroy -f
```

This setup provides a clean, minimal Kubernetes environment perfect for testing the live pod migration system without the complexity of custom builds.
