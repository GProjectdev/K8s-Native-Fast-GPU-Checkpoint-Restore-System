# ---- Build the Go agent ----
FROM golang:1.22 AS gobuild
WORKDIR /src
COPY . .
# Don't depend on the external checksum DB (sum.golang.org) — it flakes with
# stream/INTERNAL_ERROR and breaks the build.
ENV GOFLAGS=-mod=mod GOSUMDB=off GOPROXY=https://proxy.golang.org,direct
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -o /out/node-agent ./cmd/node-agent

# ---- Runtime image ----
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=gobuild /out/node-agent /usr/local/bin/node-agent
ENTRYPOINT ["/usr/local/bin/node-agent"]
