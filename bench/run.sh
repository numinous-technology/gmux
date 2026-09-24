#!/usr/bin/env bash
# The benchmark behind the README table. It runs the same workload three ways
# on one real GPU and reports wall time and card utilisation:
#
#   1. one at a time     the jobs run back to back, no sharing
#   2. plain MPS         all at once under MPS, no admission or caps
#   3. gmux              all at once under gmux
#
# It needs a real NVIDIA GPU, nvidia-smi, and a workload in ./workload.sh that
# uses a slice of a card (a small fine-tune or an eval). Fill WORKLOAD and N.
set -euo pipefail
N=${N:-4}
WORKLOAD=${WORKLOAD:-./workload.sh}

util() { nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits | head -1; }
run_group() { local label="$1"; shift; local start=$(date +%s); "$@"; echo "$label: $(( $(date +%s) - start ))s"; }

echo "gmux benchmark, $N jobs"
run_group "one at a time" bash -c "for i in \$(seq $N); do $WORKLOAD; done"

# plain MPS: start the daemon, launch N at once, wait
run_group "plain MPS" bash -c '
  nvidia-cuda-mps-control -d
  for i in $(seq '"$N"'); do '"$WORKLOAD"' & done; wait
  echo quit | nvidia-cuda-mps-control'

# gmux: each job takes 1/N of the card
run_group "gmux" bash -c '
  gmux serve & sleep 2
  for i in $(seq '"$N"'); do gmux run --share '"$(python3 -c "print(1/$N)")"' -- '"$WORKLOAD"'; done
  while gmux ps | grep -q running; do sleep 1; done
  gmux usage --since 1h'
