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
#include <link.h>
#include <pthread.h>

static gmux_limit g_limit;
static pthread_mutex_t g_mu = PTHREAD_MUTEX_INITIALIZER;
static int g_ready;

static void gmux_init(void) {
    if (g_ready) return;
    g_limit.limit = gmux_limit_from_env(getenv("GMUX_MEM_LIMIT_MIB"));
    g_ready = 1;
}

// Resolving the real allocator. Frameworks usually load the GPU runtime with
// dlopen(RTLD_LOCAL) (PyTorch bundles its own libamdhip64.so), so its symbols
// are not in the global scope and dlsym(RTLD_NEXT) finds nothing. We first try
// RTLD_NEXT, then walk the loaded objects for the runtime library by name and
// resolve the symbol from that exact handle.
struct find { const char *needle; char path[4096]; };

static int find_cb(struct dl_phdr_info *info, size_t size, void *data) {
    struct find *f = data;
    if (info->dlpi_name && strstr(info->dlpi_name, f->needle)) {
        strncpy(f->path, info->dlpi_name, sizeof f->path - 1);
        return 1;
    }
    return 0;
}

static void *real_sym(const char *lib, const char *name) {
    void *p = dlsym(RTLD_NEXT, name);
    if (p) return p;
    struct find f = { .needle = lib };
    f.path[0] = 0;
    dl_iterate_phdr(find_cb, &f);
    if (!f.path[0]) return NULL;
    void *h = dlopen(f.path, RTLD_NOLOAD | RTLD_LAZY);
    if (!h) return NULL;
    p = dlsym(h, name);
    dlclose(h); // drops the reference NOLOAD took; the library stays loaded
    return p;
}

// Out of memory is 2 in both the CUDA driver (CUDA_ERROR_OUT_OF_MEMORY) and
// HIP (hipErrorOutOfMemory), so frameworks treat a refused allocation exactly
// like a full card. If the real function cannot be found we report that
// instead of crashing: 3 is CUDA_ERROR_NOT_INITIALIZED / hipErrorNotInitialized.
#define OOM 2
#define NOT_FOUND 3

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

// Pointer -> size, so a free releases what its allocation reserved. An open
// addressing table with tombstones; caching allocators hold few, large blocks.
#define MAP (1 << 16)
#define TOMB ((void *)1)
static struct { void *p; size_t n; } g_map[MAP];

static size_t slot(void *p) { return ((uintptr_t)p >> 8) & (MAP - 1); }

static void remember(void *p, size_t n) {
    pthread_mutex_lock(&g_mu);
    for (size_t i = slot(p), k = 0; k < MAP; i = (i + 1) & (MAP - 1), k++)
        if (!g_map[i].p || g_map[i].p == TOMB) { g_map[i].p = p; g_map[i].n = n; break; }
    pthread_mutex_unlock(&g_mu);
}

static size_t forget(void *p) {
    size_t n = 0;
    pthread_mutex_lock(&g_mu);
    for (size_t i = slot(p), k = 0; k < MAP && g_map[i].p; i = (i + 1) & (MAP - 1), k++)
        if (g_map[i].p == p) { n = g_map[i].n; g_map[i].p = TOMB; g_map[i].n = 0; break; }
    pthread_mutex_unlock(&g_mu);
    return n;
}

static int hooked_alloc(alloc_fn real, void **out, size_t size) {
    if (!real) return NOT_FOUND;
    if (!cap(size)) return OOM;
    int rc = real((void *)out, size);
    if (rc != 0) uncap(size); else remember(*out, size);
    return rc;
}

static int hooked_free(free_fn real, void *p) {
    if (!real) return NOT_FOUND;
    uncap(forget(p));
    return real(p);
}

int hipMalloc(void **ptr, size_t size) {
    static alloc_fn real; if (!real) real = (alloc_fn)real_sym("libamdhip64", "hipMalloc");
    return hooked_alloc(real, ptr, size);
}

int hipFree(void *ptr) {
    static free_fn real; if (!real) real = (free_fn)real_sym("libamdhip64", "hipFree");
    return hooked_free(real, ptr);
}

int cuMemAlloc_v2(void *dptr, size_t size) {
    static alloc_fn real; if (!real) real = (alloc_fn)real_sym("libcuda.so", "cuMemAlloc_v2");
    return hooked_alloc(real, (void **)dptr, size);
}

int cuMemFree_v2(void *dptr) {
    static free_fn real; if (!real) real = (free_fn)real_sym("libcuda.so", "cuMemFree_v2");
    return hooked_free(real, dptr);
}
#endif
