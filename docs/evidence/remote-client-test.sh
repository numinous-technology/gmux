#!/bin/bash
# Runs on this machine, which has no GPU, against a gmux GPU host over the
# internet. Args: TARGET (TOKEN@IP:7070#FP), host IP.
set -u -o pipefail
TARGET=$1; IP=$2
W=$(mktemp -d); export GMUX_CONFIG=$W/remotes.json GMUX_STATE=$W/state GMUX_SOCKET=$W/state/none.sock
G=/home/ubuntu/.gpurun/gmux-client; P=/opt/tv/bin/python
PASS=0; FAIL=0
check(){ if eval "$2"; then echo "PASS  $1"; PASS=$((PASS+1)); else echo "FAIL  $1   [$2]"; FAIL=$((FAIL+1)); fi; }
ms(){ local s=$(date +%s%N); "$@"; T=$(( ($(date +%s%N)-s)/1000000 )); }
echo "##### this machine: $(hostname), GPUs: $(ls /dev/nvidia* /dev/kfd 2>/dev/null | wc -l) device nodes; host $IP"
echo "round trip: $(ping -c 5 -q $IP 2>/dev/null | tail -1 | cut -d/ -f5) ms average"
echo "##### setup"
$G remote add do "$TARGET"
$G remote ls
$G cards > $W/cards.txt 2>&1; cat $W/cards.txt
check "no local gmux: commands go to the GPU host by default" "grep -q 'on do' $W/cards.txt"
echo "##### time added per command (empty directory, 1/8 share, run true)"
mkdir -p $W/empty && cd $W/empty; TS=""
for i in 1 2 3 4 5; do ms $G run --share 0.125 -- true 2>/dev/null; TS="$TS $T"; done
MED=$(echo $TS | tr ' ' '\n' | sort -n | sed -n 3p); echo "  ms:$TS  median $MED"
check "a remote command adds under 500 ms" "[ $MED -lt 500 ]"
echo "##### sync"
mkdir -p $W/data && cd $W/data && head -c 268435456 /dev/urandom > big.bin
ms $G run --share 0.125 -- ls -la big.bin; FIRST=$T
ms $G run --share 0.125 -- true; AGAIN=$T
echo "  first run with a new 256 MiB file: ${FIRST} ms; again, unchanged: ${AGAIN} ms"
check "an unchanged directory syncs nothing" "[ $AGAIN -lt $((FIRST/4)) ]"
echo "##### benchmark on the remote GPU"
mkdir -p $W/b0 && cp /home/ubuntu/.gpurun/bench.py /home/ubuntu/.gpurun/memcap.py $W/b0/ && cd $W/b0
$G run --share 0.25 --name alone --pull r.txt -- $P bench.py r.txt 20 2>/dev/null
echo "  1/4 share alone: $(cat r.txt 2>/dev/null)"
for i in 1 2 3 4; do mkdir -p $W/q$i && cp /home/ubuntu/.gpurun/bench.py $W/q$i/; (cd $W/q$i && $G run --share 0.25 --name q$i --pull r.txt -- $P bench.py r.txt 20 >/dev/null 2>&1) & done; wait
echo "  four clients, 1/4 share each: $(for i in 1 2 3 4; do cut -d' ' -f1 $W/q$i/r.txt 2>/dev/null; done | tr '\n' ' ')"
check "four remote clients got results" "[ \$(cat $W/q*/r.txt 2>/dev/null | wc -l) -eq 4 ]"
check "the four remote shares split evenly (within 5%)" "python3 -c \"import sys;v=[float(open('$W/q%d/r.txt'%i).read().split()[0]) for i in (1,2,3,4)];sys.exit(0 if max(v)/min(v)<1.05 else 1)\""
echo "##### memory cap on a remote share"
cd $W/b0; $G run --share 0.25 --pull m.txt -- $P memcap.py m.txt 63 78 2>/dev/null; cat m.txt
check "a remote 1/4 share is capped (63 GiB ok, 78 GiB refused)" "grep -q '63GiB:ok 78GiB:refused' m.txt"
echo "##### network fence on remote jobs"
cd $W/empty
$G run --share 0.125 --deny-net -- python3 -c "import socket
try: socket.create_connection(('1.1.1.1',443),timeout=6); print('CONNECTED')
except Exception as e: print('BLOCKED', type(e).__name__)" > $W/dn.txt 2>&1; cat $W/dn.txt
check "--deny-net blocks a remote job" "grep -q BLOCKED $W/dn.txt"
$G run --share 0.125 --allow example.com -- python3 -c "import socket, ssl
for h, sni in [('example.com','example.com'), ('1.1.1.1','one.one.one.one')]:
    try:
        s=ssl.create_default_context().wrap_socket(socket.create_connection((h,443),timeout=8), server_hostname=sni)
        s.sendall(b'HEAD / HTTP/1.1\r\nHost: '+sni.encode()+b'\r\nConnection: close\r\n\r\n'); print(h, 'ALLOWED', s.recv(20).split(b'\r\n')[0].decode())
    except Exception as e: print(h, 'refused', type(e).__name__)" > $W/al.txt 2>&1; cat $W/al.txt
check "--allow lets the allowlisted host through and refuses the rest" "grep -q 'example.com ALLOWED HTTP/1.1 200' $W/al.txt && grep -q '1.1.1.1 refused' $W/al.txt"
echo "##### exit codes and Ctrl-C"
$G run --share 0.125 -- sh -c 'exit 7' 2>/dev/null; CODE=$?
check "a remote exit code comes back (7)" "[ $CODE -eq 7 ]"
$G run --share 0.25 --name ctrlc -- sleep 300 >/dev/null 2>&1 & CP=$!
sleep 6; kill -INT $CP; wait $CP 2>/dev/null
sleep 2; $G ps | grep ctrlc
check "Ctrl-C on the client stops the job on the host" "$G ps | grep ctrlc | grep -q exited"
echo "##### placement across hosts"
BAD="${TARGET%@*}@10.255.255.1:7070#${TARGET##*#}"
python3 -c "import json;p='$GMUX_CONFIG';c=json.load(open(p));c['remotes']['ghost']='$BAD';c['default']='ghost';json.dump(c,open(p,'w'))"
$G remote ls
$G run --share 0.125 -- true 2> $W/place.txt; cat $W/place.txt
check "an unreachable default host is skipped for one that answers" "grep -q '\[gmux\] do:' $W/place.txt"
echo "##### security"
$G remote add wrongfp "${TARGET%#*}#$(printf '0%.0s' $(seq 1 64))" > $W/fp.txt 2>&1; cat $W/fp.txt
check "a host with a different certificate is refused" "grep -q fingerprint $W/fp.txt"
$G remote add wrongtok "nottheright@${TARGET#*@}" > $W/tok.txt 2>&1; cat $W/tok.txt
check "a wrong token is refused" "grep -qi 'token' $W/tok.txt"
echo "##### the host's view"
$G ps --remote do | head -8; $G usage --remote do --since 1h --by job | head -6
echo "##### result: $PASS passed, $FAIL failed"
rm -rf $W
