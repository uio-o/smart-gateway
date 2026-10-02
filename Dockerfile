# Builds both binaries in one stage so a single image can run either role.
# The Go version matches the toolchain the module requires.
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
ENV GOFLAGS=-mod=mod
# A proxy keeps builds working where the public module proxy is unreachable.
ARG GOPROXY_URL=https://proxy.golang.org,direct
RUN go env -w GOPROXY=${GOPROXY_URL} GOSUMDB=off && go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ENV CGO_ENABLED=0
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w" \
        -o /out/smart-gateway-agent ./cmd/smart-gateway-agent && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w" \
        -o /out/smart-gateway-panel ./cmd/smart-gateway-panel

FROM gcr.io/distroless/static-debian12:nonroot

# The distroless image has no shell, which keeps the attack surface small.
COPY --from=build /out/smart-gateway-agent /usr/local/bin/smart-gateway-agent
COPY --from=build /out/smart-gateway-panel /usr/local/bin/smart-gateway-panel

USER nonroot:nonroot
WORKDIR /data

EXPOSE 8080 8090

ENTRYPOINT ["/usr/local/bin/smart-gateway-panel"]
