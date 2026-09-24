# Sustained fp16 matmul for a fixed wall time; prints TFLOP/s. Long enough that
# concurrent jobs genuinely overlap (a short burst measures nothing).
import sys, time, torch
out, secs = sys.argv[1], float(sys.argv[2])
n = 8192
a = torch.randn(n, n, device="cuda", dtype=torch.float16)
b = torch.randn(n, n, device="cuda", dtype=torch.float16)
for _ in range(3): (a @ b)
torch.cuda.synchronize()
iters, t0 = 0, time.time()
while time.time() - t0 < secs:
    c = a @ b; iters += 1
    if iters % 20 == 0: torch.cuda.synchronize()
torch.cuda.synchronize()
dt = time.time() - t0
tflops = 2 * n**3 * iters / dt / 1e12
free, total = torch.cuda.mem_get_info()
open(out, "w").write(f"{tflops:.1f} {free/2**30:.1f}\n")
