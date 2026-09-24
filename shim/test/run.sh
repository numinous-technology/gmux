#!/bin/sh
# Hooks test: the preloaded shim must cap a runtime that was dlopen()ed
# RTLD_LOCAL (the PyTorch case) and never crash. Expected: "0 2 0".
set -e
d=$(mktemp -d)
here=$(dirname "$0")
cc -O2 -fPIC -shared -o "$d/libamdhip64.so" "$here/fakehip.c"
cc -O2 -o "$d/app" "$here/app.c" -ldl
cc -O2 -fPIC -DGMUX_SHIM_HOOKS -shared -o "$d/libgmux.so" "$here/../gmux_shim.c" -ldl -lpthread
out=$(GMUX_MEM_LIMIT_MIB=100 LD_PRELOAD="$d/libgmux.so" "$d/app" "$d/libamdhip64.so")
rm -rf "$d"
if [ "$out" = "0 2 0" ]; then echo "shim hooks: ok ($out)"; else echo "shim hooks: FAIL (got '$out', want '0 2 0')"; exit 1; fi
