# ---- Build the Go agent ----
FROM golang:1.22 AS gobuild
WORKDIR /src
COPY . .
ENV GOFLAGS=-mod=mod GOSUMDB=off GOPROXY=https://proxy.golang.org,direct
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -o /out/node-agent ./cmd/node-agent

# ---- Build the LD_PRELOAD interceptor shim (GCR selective data engine) ----
FROM gcc:13 AS ccbuild
WORKDIR /icpt
COPY interceptor/ ./
RUN make

# ---- Runtime image ----
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates nfs-common \
 && rm -rf /var/lib/apt/lists/*
COPY --from=gobuild /out/node-agent /usr/local/bin/node-agent
# Interceptor artifact shipped to the node by the agent at startup.
COPY --from=ccbuild /icpt/libgcr-interceptor.so /opt/gpu-cr-dist/libgcr-interceptor.so
ENTRYPOINT ["/usr/local/bin/node-agent"]
