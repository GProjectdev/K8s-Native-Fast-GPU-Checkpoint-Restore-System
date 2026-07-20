# Build the WorkloadCheckpoint orchestrator (central controller). Build from the
# REPO ROOT so it can import the shared api/v1alpha1 package:
#   docker build -f orchestrator/Dockerfile -t <registry>/gpu-cr-orchestrator:latest .
FROM golang:1.22 AS build
WORKDIR /src
COPY . .
ENV GOFLAGS=-mod=mod GOSUMDB=off GOPROXY=https://proxy.golang.org,direct
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -o /out/orchestrator ./orchestrator/cmd

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/orchestrator /usr/local/bin/orchestrator
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/orchestrator"]
