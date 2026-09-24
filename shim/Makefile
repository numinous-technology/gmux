# libgmux.so is the LD_PRELOAD memory-cap shim. Build it where CUDA or HIP
# headers are present (they are not needed: symbols resolve at runtime).
CC ?= cc
CFLAGS ?= -O2 -fPIC -Wall

libgmux.so: gmux_shim.c
	$(CC) $(CFLAGS) -DGMUX_SHIM_HOOKS -shared -o $@ $< -ldl -lpthread

test: shim_test
	./shim_test
	sh test/run.sh

shim_test: gmux_shim.c shim_test.c
	$(CC) $(CFLAGS) -o $@ gmux_shim.c shim_test.c

clean:
	rm -f libgmux.so shim_test
