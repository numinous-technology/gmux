// libgmux: cap a job's GPU memory from user space, no root.
//
// A job's memory share (GMUX_MEM_LIMIT_MIB) is enforced by intercepting the
// driver's allocation calls. NVIDIA MPS already caps memory at the driver, so
// this shim is mainly for backends without that (AMD HIP): it tracks how much
// the job has allocated and fails an allocation that would cross the cap, the
// same error the driver returns when a card is full, so frameworks handle it.
//
// It resolves the real functions with dlsym(RTLD_NEXT), so it needs no CUDA or
// HIP headers to build. The accounting core (gmux_reserve / gmux_release) is
// plain C and is unit tested.
#ifdef GMUX_SHIM_HOOKS
#define _GNU_SOURCE // must precede every include so RTLD_NEXT is declared
#endif
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <stdio.h>

// ---- accounting core (tested by shim_test) -------------------------------

typedef struct {
    uint64_t limit;   // bytes; 0 means no limit
    uint64_t used;    // bytes currently allocated
} gmux_limit;

// gmux_reserve records size bytes if they fit under the limit. Returns 1 when
// the allocation is allowed, 0 when it would cross the cap.
int gmux_reserve(gmux_limit *l, uint64_t size) {
    if (l->limit == 0) { l->used += size; return 1; }
    if (l->used + size > l->limit) return 0;
    l->used += size;
    return 1;
}

// gmux_release records that size bytes were freed.
void gmux_release(gmux_limit *l, uint64_t size) {
    if (size > l->used) l->used = 0;
    else l->used -= size;
}

// gmux_limit_from_env reads GMUX_MEM_LIMIT_MIB into bytes.
uint64_t gmux_limit_from_env(const char *v) {
    if (!v || !*v) return 0;
    char *end = 0;
    unsigned long long mib = strtoull(v, &end, 10);
    if (end == v) return 0;
    return (uint64_t)mib * 1024ull * 1024ull;
}

#ifdef GMUX_SHIM_HOOKS
// ---- the hooks (compiled where CUDA/HIP is present) ----------------------
//
// Built into libgmux.so and loaded with LD_PRELOAD. Without GMUX_SHIM_HOOKS
// (the default for the unit test) only the accounting core above is compiled.
#include <dlfcn.h>
#include <pthread.h>

static gmux_limit g_limit;
static pthread_mutex_t g_mu = PTHREAD_MUTEX_INITIALIZER;
static int g_ready;

static void gmux_init(void) {
    if (g_ready) return;
    g_limit.limit = gmux_limit_from_env(getenv("GMUX_MEM_LIMIT_MIB"));
    g_ready = 1;
}

// CUDA driver: CUresult cuMemAlloc_v2(CUdeviceptr*, size_t)
// HIP: hipError_t hipMalloc(void**, size_t)
// The out-of-memory code is 2 for both the CUDA driver (CUDA_ERROR_OUT_OF_MEMORY)
// and HIP (hipErrorOutOfMemory).
#define OOM 2

typedef int (*alloc_fn)(void *, size_t);
typedef int (*free_fn)(void *);

static int cap(size_t size) {
    gmux_init();
    pthread_mutex_lock(&g_mu);
    int ok = gmux_reserve(&g_limit, size);
    pthread_mutex_unlock(&g_mu);
    return ok;
}

static void uncap(size_t size) {
    pthread_mutex_lock(&g_mu);
    gmux_release(&g_limit, size);
    pthread_mutex_unlock(&g_mu);
}

// We cannot know the freed size from the pointer alone without a table, so we
// track a per-pointer size map. Kept small and simple.
#define MAP 4096
static struct { void *p; size_t n; } g_map[MAP];

static void remember(void *p, size_t n) {
    pthread_mutex_lock(&g_mu);
    for (int i = 0; i < MAP; i++) if (!g_map[i].p) { g_map[i].p = p; g_map[i].n = n; break; }
    pthread_mutex_unlock(&g_mu);
}
static size_t forget(void *p) {
    size_t n = 0;
    pthread_mutex_lock(&g_mu);
    for (int i = 0; i < MAP; i++) if (g_map[i].p == p) { n = g_map[i].n; g_map[i].p = 0; break; }
    pthread_mutex_unlock(&g_mu);
    return n;
}

int cuMemAlloc_v2(void *dptr, size_t size) {
    static alloc_fn real; if (!real) real = (alloc_fn)dlsym(RTLD_NEXT, "cuMemAlloc_v2");
    if (!cap(size)) return OOM;
    int rc = real(dptr, size);
    if (rc != 0) uncap(size); else remember(*(void **)dptr, size);
    return rc;
}

int hipMalloc(void **ptr, size_t size) {
    static alloc_fn real; if (!real) real = (alloc_fn)dlsym(RTLD_NEXT, "hipMalloc");
    if (!cap(size)) return OOM;
    int rc = real((void *)ptr, size);
    if (rc != 0) uncap(size); else remember(*ptr, size);
    return rc;
}

int cuMemFree_v2(void *dptr) {
    static free_fn real; if (!real) real = (free_fn)dlsym(RTLD_NEXT, "cuMemFree_v2");
    uncap(forget(dptr));
    return real(dptr);
}

int hipFree(void *ptr) {
    static free_fn real; if (!real) real = (free_fn)dlsym(RTLD_NEXT, "hipFree");
    uncap(forget(ptr));
    return real(ptr);
}
#endif
