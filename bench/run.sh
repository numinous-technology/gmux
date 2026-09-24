#!/usr/bin/env bash
# The benchmark behind the README table: a sustained fp16 matmul (bench/matmul.py)
# run as one dedicated job, as one 1/4 share alone, and as four 1/4 shares at
# once, all through gmux. Needs a real NVIDIA GPU, torch with CUDA, and a
# running `gmux serve`.
set -euo pipefail
D=${D:-20}
HERE=$(cd "$(dirname "$0")" && pwd)
OUT=$(mktemp -d)
wait_for() { for _ in $(seq 1 120); do [ "$(ls "$OUT"/"$1"*.txt 2>/dev/null | wc -l)" -ge "$2" ] && return; sleep 1; done; }

echo "one job, whole card (no gmux):"
python3 "$HERE/matmul.py" "$OUT/dedicated.txt" "$D"; cat "$OUT/dedicated.txt"

echo "one 1/4 share, alone:"
gmux run --share 0.25 --name alone -- python3 "$HERE/matmul.py" "$OUT/alone.txt" "$D" >/dev/null
wait_for alone 1; cat "$OUT/alone.txt"

echo "four 1/4 shares at once:"
for i in 1 2 3 4; do
  gmux run --share 0.25 --name "q$i" -- python3 "$HERE/matmul.py" "$OUT/q$i.txt" "$D" >/dev/null
done
wait_for q 4; for i in 1 2 3 4; do echo "q$i: $(cat "$OUT/q$i.txt")"; done
echo "(columns: TFLOP/s, GiB visible to the job)"
