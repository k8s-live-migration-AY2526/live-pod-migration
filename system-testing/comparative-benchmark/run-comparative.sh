#!/bin/bash
set -e

# --- Argument Handling ---
if [ "$#" -lt 2 ]; then
    echo "Usage: ./run-comparative.sh <path-to-evict-binary> <path-to-migrate-binary> [start-time-seconds]"
    exit 1
fi

EVICT_BIN=$1
MIGRATE_BIN=$2
START_TIME=${3:-30} # Default to 30 seconds
POLICY_FILE="policy.yaml"
POD_NAME="benchmark-cpu-0"

if [ ! -f "$EVICT_BIN" ]; then echo "ERROR: Evict binary not found: $EVICT_BIN"; exit 1; fi
if [ ! -f "$MIGRATE_BIN" ]; then echo "ERROR: Migrate binary not found: $MIGRATE_BIN"; exit 1; fi

# --- Functions ---

setup_environment() {
    echo "=== Setting up Benchmark Environment ==="
    
    kubectl label node k8s-master color=red --overwrite >/dev/null
    kubectl label node k8s-worker color=red --overwrite >/dev/null
    kubectl label node k8s-worker2 color=blue --overwrite >/dev/null
    
    echo "Cleaning up previous runs..."
    kubectl delete statefulset benchmark-cpu --wait=true 2>/dev/null || true
    kubectl delete job --all 2>/dev/null || true
    kubectl delete podmigration --all 2>/dev/null || true
    kubectl delete pod --all 2>/dev/null || true
    kubectl delete pvc benchmark-pvc --wait=true 2>/dev/null || true
    sleep 5
    
    echo "Deploying Workload..."
    kubectl apply -f benchmark-cpu.yaml
    
    echo "Waiting for pod ready..."
    kubectl wait --for=condition=Ready pod/$POD_NAME --timeout=90s
    
    NODE=$(kubectl get pod $POD_NAME -o jsonpath='{.spec.nodeName}')
    if [[ "$NODE" != "k8s-worker2" ]]; then
        echo "ERROR: Pod started on $NODE, expected k8s-worker2."
        exit 1
    fi
}

trigger_descheduling() {
    echo "Triggering Node Affinity change (k8s-worker2 -> k8s-worker)..."
    kubectl label node k8s-worker2 color=red --overwrite >/dev/null
    kubectl label node k8s-worker color=blue --overwrite >/dev/null
}

run_benchmark() {
    local BINARY_PATH=$1
    local STRATEGY=$2  # "evict" or "migrate"
    local TEST_NAME=$3
    
    echo "================================================================"
    echo "TEST: $TEST_NAME"
    echo "================================================================"
    
    setup_environment
    
    echo "Waiting for pod to start serving (Baseline)..."
    for i in {1..90}; do
        if kubectl exec $POD_NAME -- grep -q "Serving Request" /data/benchmark.log 2>/dev/null; then
            echo "Baseline established."
            break
        fi
        sleep 2
    done
    
    sleep 2
    trigger_descheduling
    
    echo "Running Descheduler ($STRATEGY)..."
    timeout 60s $BINARY_PATH --policy-config-file $POLICY_FILE --v=3 --kubeconfig "$HOME/.kube/config" --dry-run=false > ${STRATEGY}_descheduler.log 2>&1 &
    
    echo "Waiting for pod to move to k8s-worker..."
    for i in {1..60}; do
        NEW_NODE=$(kubectl get pod $POD_NAME -o jsonpath='{if not .metadata.deletionTimestamp}{.spec.nodeName}{end}' 2>/dev/null || echo "Pending")
        if [[ "$NEW_NODE" == "k8s-worker" ]]; then
            echo "Pod successfully moved to k8s-worker."
            break
        fi
        sleep 2
    done
    
    kubectl wait --for=condition=Ready pod/$POD_NAME --timeout=90s || true
    
    # --- Strategy-Specific Wait Logic ---
    if [[ "$STRATEGY" == "evict" ]]; then
        echo "Eviction detected. Waiting for restored pod to finish CPU init and start serving..."
        # AWK Logic: We wait until we see at least 2 "STARTED" blocks AND a "Serving Request" after the last one.
        for i in {1..90}; do
            if kubectl exec $POD_NAME -- sh -c "awk '/STARTED/{c++; f=0} /Serving Request/{f=1} END{if(c>=2 && f==1) print \"YES\"}' /data/benchmark.log" 2>/dev/null | grep -q "YES"; then
                echo "Restored pod has successfully resumed serving."
                break
            fi
            sleep 2
        done
    else
        echo "Live migration complete. Pod is Ready and continuing execution."
    fi
    
    echo "Waiting ${START_TIME}s to collect post-movement logs..."
    sleep $START_TIME

    echo "Fetching logs for analysis..."
    kubectl exec $POD_NAME -- cat /data/benchmark.log > ${STRATEGY}_benchmark.log 2>/dev/null
    
    echo "Analyzing downtime..."
    python3 -c "
import sys

last_ts = 0
max_gap = 0
try:
    with open('${STRATEGY}_benchmark.log', 'r') as f:
        for line in f:
            if 'Serving Request' not in line: continue
            try:
                timestamp = float(line.split()[0])
            except: continue
            
            if last_ts != 0:
                gap = timestamp - last_ts
                if gap > max_gap:
                    max_gap = gap
            last_ts = timestamp
            
    print(f'\\n>>> RESULT ($STRATEGY): Max Downtime = {max_gap:.4f}s <<<\\n')
except Exception as e:
    print(f'Error parsing logs: {e}')
"
}

# --- Main Execution ---

# 1. Run Standard Eviction Benchmark
run_benchmark "$EVICT_BIN" "evict" "Standard Pod Eviction"

# 2. Run Live Migration Benchmark
run_benchmark "$MIGRATE_BIN" "migrate" "CRIU Live Pod Migration"

echo "All Benchmarks Complete."