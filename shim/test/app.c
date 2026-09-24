// dlopen()s the fake runtime RTLD_LOCAL and allocates through the symbol the
// dynamic linker binds for it. With libgmux.so preloaded, that is the shim.
#include <dlfcn.h>
#include <stdio.h>
typedef int (*alloc_fn)(void **, size_t);
typedef int (*free_fn)(void *);
int main(int argc, char **argv) {
    void *h = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
    if (!h) { printf("dlopen failed\n"); return 1; }
    // resolve through the global scope first, as a dependent library would
    alloc_fn m = (alloc_fn)dlsym(RTLD_DEFAULT, "hipMalloc");
    free_fn f = (free_fn)dlsym(RTLD_DEFAULT, "hipFree");
    if (!m) { m = (alloc_fn)dlsym(h, "hipMalloc"); f = (free_fn)dlsym(h, "hipFree"); }
    void *a, *b, *c;
    int r1 = m(&a, 60 << 20);          // 60 MiB: under a 100 MiB cap
    int r2 = m(&b, 60 << 20);          // another 60: would cross it
    f(a);                              // release the first
    int r3 = m(&c, 60 << 20);          // fits again
    printf("%d %d %d\n", r1, r2, r3);
    return 0;
}
