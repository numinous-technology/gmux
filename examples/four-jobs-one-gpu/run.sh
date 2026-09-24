#!/usr/bin/env bash
set -euo pipefail
for i in 1 2 3 4; do
  gmux run --share 0.25 --name "job-$i" -- sleep 20
done
echo
gmux top
echo
sleep 3
gmux usage --since 1h --by job
