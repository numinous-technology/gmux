# gmux in a container. Build the Go binary and the memory-cap shim, then run
# on a CUDA base so nvidia-smi and MPS are present. AMD and Intel users can
# swap the base image; gmux detects whatever vendor tools exist.
FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gmux ./cmd/gmux

FROM nvidia/cuda:12.4.1-runtime-ubuntu22.04
RUN apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev && rm -rf /var/lib/apt/lists/*
COPY shim /src/shim
RUN cc -O2 -fPIC -DGMUX_SHIM_HOOKS -shared -o /usr/local/lib/libgmux.so /src/shim/gmux_shim.c -ldl -lpthread
COPY --from=build /out/gmux /usr/local/bin/gmux
ENV GMUX_SHIM=/usr/local/lib/libgmux.so
ENV GMUX_STATE=/var/lib/gmux
# The daemon and jobs share this container. Start the daemon, then exec into it
# to run jobs, or drive it from the SDK.
ENTRYPOINT ["gmux"]
CMD ["serve"]
