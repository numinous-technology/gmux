// A stand-in libamdhip64: allocations come from malloc. Loaded RTLD_LOCAL by
// the test program, the way PyTorch loads its bundled HIP runtime, so the shim
// must find it without the global symbol scope.
#include <stdlib.h>
int hipMalloc(void **p, size_t n) { *p = malloc(n ? n : 1); return *p ? 0 : 2; }
int hipFree(void *p) { free(p); return 0; }
