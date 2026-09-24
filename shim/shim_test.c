// Unit test for the shim's accounting core. Builds with plain gcc, no CUDA.
#include <assert.h>
#include <stdint.h>
#include <stdio.h>

typedef struct { uint64_t limit; uint64_t used; } gmux_limit;
int gmux_reserve(gmux_limit *l, uint64_t size);
void gmux_release(gmux_limit *l, uint64_t size);
uint64_t gmux_limit_from_env(const char *v);

int main(void) {
    gmux_limit l = { .limit = 1000, .used = 0 };
    assert(gmux_reserve(&l, 600) == 1 && l.used == 600);
    assert(gmux_reserve(&l, 600) == 0 && l.used == 600); // would cross the cap
    assert(gmux_reserve(&l, 400) == 1 && l.used == 1000); // exactly fills it
    gmux_release(&l, 600);
    assert(l.used == 400);
    assert(gmux_reserve(&l, 600) == 1 && l.used == 1000); // room again

    gmux_limit none = { .limit = 0, .used = 0 };
    assert(gmux_reserve(&none, 1ull << 40) == 1); // no limit, anything fits

    assert(gmux_limit_from_env("20480") == 20480ull * 1024 * 1024);
    assert(gmux_limit_from_env("") == 0);
    assert(gmux_limit_from_env(0) == 0);
    printf("shim accounting core: ok\n");
    return 0;
}
