#!/bin/bash
# on the GPU droplet: torch, gmux, the shim, and `gmux serve --addr :7070`
cd /tmp
# boot-time unattended upgrades hold the dpkg lock for a while: wait for it
A="apt-get -o DPkg::Lock::Timeout=900 -q"
$A update >/dev/null 2>&1; $A install -y python3-venv python3-pip >/dev/null 2>&1 || echo "apt install failed"
python3 -m venv /opt/tv
for i in rocm7.1 rocm7.0 rocm6.4; do /opt/tv/bin/pip install -q torch --index-url https://download.pytorch.org/whl/$i >/dev/null 2>&1 && /opt/tv/bin/python -c "import torch;torch.randn(8,device='cuda').sum().item()" 2>/dev/null && break; done
/opt/tv/bin/python -c "import torch;print('torch',torch.__version__,torch.cuda.get_device_name(0))" || { echo "SETUP FAILED: no torch"; exit 1; }
install -m 0755 gmux-linux /usr/local/bin/gmux
tar xzf shim.tgz && cc -O2 -fPIC -DGMUX_SHIM_HOOKS -shared -o /usr/local/lib/libgmux.so shim/gmux_shim.c -ldl -lpthread
command -v ufw >/dev/null && ufw allow 7070/tcp >/dev/null 2>&1
mkdir -p /var/lib/gmux
GMUX_STATE=/var/lib/gmux GMUX_SOCKET=/var/lib/gmux/s.sock GMUX_SHIM=/usr/local/lib/libgmux.so setsid nohup gmux serve --addr :7070 > /var/log/gmux.log 2>&1 < /dev/null &
sleep 5; cat /var/log/gmux.log
