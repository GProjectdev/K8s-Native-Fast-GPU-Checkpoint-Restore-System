# ---- Build the Go agent ----
FROM golang:1.22 AS gobuild
WORKDIR /src
# Copy the whole module and resolve dependencies during the build.
# (go.sum may be absent in the repo; `go mod tidy` generates it here. This avoids
# the Docker-only `COPY go.su[m]` glob trick, which Buildah does not support.)
COPY . .
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -o /out/node-agent ./cmd/node-agent

# ---- Build the LD_PRELOAD interceptor shim ----
FROM gcc:13 AS ccbuild
WORKDIR /icpt
COPY interceptor/ ./
RUN make

# ---- Runtime image ----
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl tar \
 && rm -rf /var/lib/apt/lists/*

# crictl for container PID resolution.
ARG CRICTL_VERSION=v1.30.0
RUN curl -sSL "https://github.com/kubernetes-sigs/cri-tools/releases/download/${CRICTL_VERSION}/crictl-${CRICTL_VERSION}-linux-amd64.tar.gz" \
      | tar -xz -C /usr/local/bin || true

COPY --from=gobuild /out/node-agent /usr/local/bin/node-agent
# Interceptor artifacts shipped to the node by the agent at startup.
COP