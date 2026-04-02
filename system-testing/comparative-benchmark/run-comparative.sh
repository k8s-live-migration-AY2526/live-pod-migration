#!/bin/bash
set -e

# --- Argument Handling ---
if [ -z "$1" ]; then
    echo "Usage: ./run-comparative.sh <path-to-descheduler-binary> [start-time-seconds]"
    exit 1
fi

BINARY_PATH=$1
# Default to 30 seconds if $2 is not provided
START_TIME=${2:-30} 
POLICY_FILE="policy.yaml"
POD_NAME="benchmark-cpu-0"

if [ ! -f "$BINARY_PATH" ]; then
    echo "ERROR: Binary not found at $BINARY_PATH"
    exit 1
fi

# --- Functions ---

setup_environment() {
    echo "=== Setting up Benchmark Environment ==="
    
    # 1. Label Nodes
    # Initial State: Only worker2 is valid (blue), others invalid (red)
    echo "Setting initial node labels (worker2=blue, others=red)..."
    kubectl label node k8s-master color=red --overwrite >/dev/null
    kubectl label node k8s-worker color=red --overwrite >/dev/null
    kubectl label node k8s-worker2 color=blue --overwrite >/dev/null
    
    # 2. Cleanup Previous Runs
    echo "Cleaning up previous runs..."
    kubectl delete statefulset benchmark-cpu --wait=true 2>/dev/null || true
    kubectl delete job --all 2>/dev/null || true
    kubectl delete podmigration --all 2>/dev/null || true
    kubectl delete pod --all 2>/dev/null || true
    kubectl delete pvc bench benchmark-pvc --wait=true 2>/dev/null || true
    # Wait for everything to be cleanly removed
    sleep 5
    
    echo "Deploying Workload..."
    kubectl apply -f benchmark-cpu.yaml
    
    echo "Waiting for pod ready..."
    kubectl wait --for=condition=Ready pod/$POD_NAME --timeout=90s
    
    # Verify it is on worker2
    NODE=$(kubectl get pod $POD_NAME -o jsonpath='{.spec.nodeName}')
    if [[ "$NODE" != "k8s-worker2" ]]; then
        echo "ERROR: Pod started on $NODE, expected k8s-worker2. Check labels/taints or YAML affinity."
        exit 1
    fi
    echo "Pod verified on $NODE."
}

trigger_descheduling() {
    echo "Triggering Migration Path: k8s-worker2 -> k8s-worker"
    # Make worker2 invalid (Red)
    kubectl label node k8s-worker2 color=red --overwrite >/dev/null
    # Make worker valid (Blue)
    kubectl label node k8s-worker color=blue --overwrite >/dev/null
}

measure_downtime() {
    DESCHEDULER_CMD=$1
    TEST_NAME=$2
    
    echo "------------------------------------------------"
    echo "TEST: $TEST_NAME"
    echo "------------------------------------------------"
    
    # Call setup before each test to ensure consistent starting state
    setup_environment
    
    # Reset logs before test
    kubectl exec $POD_NAME -- sh -c 'echo "RESET" > /data/benchmark.log'
    
    # Wait for the pod to finish initialization and start serving
    echo "Waiting for pod to start serving (Baseline)..."
    for i in {1..90}; do
        if kubectl exec $POD_NAME -- grep -q "Serving Request" /data/benchmark.log 2>/dev/null; then
            echo "Pod is serving (Baseline established)."
            break
        fi
        sleep 2
    done
    
    sleep 2
    
    trigger_descheduling
    
    echo "Running Descheduler..."
    timeout 60s $DESCHEDULER_CMD --policy-config-file $POLICY_FILE --v=3 --kubeconfig "$HOME/.kube/config" --dry-run=false 2>&1 | tee descheduler.log &
    
    echo "Descheduler running. Waiting for pod eviction and recreation..."
    
    # Wait for pod to settle on k8s-worker
    for i in {1..60}; do
        NEW_NODE=$(kubectl get pod $POD_NAME -o jsonpath='{if not .metadata.deletionTimestamp}{.spec.nodeName}{end}' 2>/dev/null || echo "Pending")
        
        if [[ "$NEW_NODE" == "k8s-worker" ]]; then
            echo "Pod successfully moved to k8s-worker."
            break
        fi
        sleep 2
    done
    
    # Wait for the new pod to be completely ready
    kubectl wait --for=condition=Ready pod/$POD_NAME --timeout=90s || true
    
    # --- Wait for NEW Serving Requests ---
    echo "Waiting ${START_TIME}s to collect post-migration logs..."
    sleep $START_TIME

    echo "Fetching logs..."
    kubectl exec $POD_NAME -- cat /data/benchmark.log > benchmark.log 2>/dev/null
    
    echo "Analyzing downtime..."
    python3 -c '
import sys

last_ts = 0
max_gap = 0
try:
    with open("benchmark.log", "r") as f:
        for line in f:
            if "Serving Request" not in line: continue
            try:
                timestamp = float(line.split()[0])
            except: continue
            
            if last_ts != 0:
                gap = timestamp - last_ts
                if gap > max_gap:
                    max_gap = gap
            last_ts = timestamp
            
    print(f"Max Downtime: {max_gap:.4f}s")
except Exception as e:
    print(f"Error parsing logs: {e}")
'
}

# --- Main Execution ---
measure_downtime "$BINARY_PATH" "Descheduler Benchmark"
echo "Benchmark Complete."