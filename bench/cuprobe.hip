// cuprobe: prints which physical compute units (chiplet, shader engine) a
// process runs on, so an HSA_CU_MASK can be checked against the hardware.
// Build with hipcc --offload-arch=<gfx target>; run under a gmux share.
// Records the physical compute unit each workgroup ran on, so a CU mask can be
// checked against the hardware instead of inferred from throughput.
#include <hip/hip_runtime.h>
#include <cstdio>
#include <set>
#include <tuple>
#include <vector>
__global__ void probe(unsigned long long *out) {
  if (__builtin_amdgcn_workitem_id_x() == 0) {
    unsigned hw = __builtin_amdgcn_s_getreg((31 << 11) | (0 << 6) | 4);   // HW_REG_HW_ID
    unsigned xcc = __builtin_amdgcn_s_getreg((31 << 11) | (0 << 6) | 20); // HW_REG_XCC_ID
    out[__builtin_amdgcn_workgroup_id_x()] = ((unsigned long long)(xcc & 0xf) << 32) | hw;
    for (volatile int i = 0; i < 20000; i++) {}  // keep the CU busy so work spreads
  }
}
int main() {
  const int n = 200000;
  unsigned long long *d; hipMalloc(&d, n * 8);
  hipMemset(d, 0xff, n * 8);
  hipLaunchKernelGGL(probe, dim3(n), dim3(64), 0, 0, d);
  hipError_t e1 = hipGetLastError(), e2 = hipDeviceSynchronize();
  printf("launch: %s / sync: %s\n", hipGetErrorString(e1), hipGetErrorString(e2));
  std::vector<unsigned long long> h(n); hipMemcpy(h.data(), d, n * 8, hipMemcpyDeviceToHost);
  std::set<std::tuple<int,int,int,int>> cus; std::set<int> xccs;
  for (auto v : h) {
    unsigned hw = v & 0xffffffff; int xcc = v >> 32;
    int cu = (hw >> 8) & 0xf, sh = (hw >> 12) & 1, se = (hw >> 13) & 0x7;
    cus.insert({xcc, se, sh, cu}); xccs.insert(xcc);
  }
  printf("distinct CUs used: %zu  across %zu XCDs\n", cus.size(), xccs.size());
  int per[16] = {0}; for (auto &c : cus) per[std::get<0>(c)]++;
  printf("per XCD:"); for (int x = 0; x < 8; x++) printf(" %d", per[x]); printf("\n");
  int m[8][8] = {{0}}; for (auto &c : cus) m[std::get<0>(c)][std::get<1>(c)]++;
  int mn = 99; printf("XCDxSE:");
  for (int x = 0; x < 8; x++) { printf(" ["); for (int s2 = 0; s2 < 4; s2++) { printf("%d%s", m[x][s2], s2<3?",":""); if (m[x][s2] < mn) mn = m[x][s2]; } printf("]"); }
  printf("  min per SE: %d\n", mn);
}
