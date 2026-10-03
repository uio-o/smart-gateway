# syntax=docker/dockerfile:1
#
# Three build targets, so a deployment only ships what it runs:
#
#   --target agent   entry/relay node image
#   --target panel   control plane image
#   --target all     both binaries, panel as the entrypoint (default)
#
# The version is stamped into the binary so `-version` reports it.

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
ARG VERSION=dev
ENV CGO_ENABLED=0

RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/smart-gateway-agent ./cmd/smart-gateway-agent && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/smart-gateway-panel ./cmd/smart-gateway-panel


# --- agent node ------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS agent

COPY --from=build /out/smart-gateway-agent /usr/local/bin/smart-gateway-agent

USER nonroot:nonroot
WORKDIR /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/smart-gateway-agent"]
CMD ["-help"]


# --- control plane ---------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS panel

COPY --from=build /out/smart-gateway-panel /usr/local/bin/smart-gateway-panel

USER nonroot:nonroot
WORKDIR /data
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/smart-gateway-panel"]
CMD ["-addr", ":8090", "-data", "/data"]


# --- both binaries ---------------------------------------------------------
# Kept for operators who prefer a single image that can run either role.
FROM gcr.io/distroless/static-debian12:nonroot AS all

COPY --from=build /out/smart-gateway-agent /usr/local/bin/smart-gateway-agent
COPY --from=build /out/smart-gateway-panel /usr/local/bin/smart-gateway-panel

USER nonroot:nonroot
WORKDIR /data
EXPOSE 8080 8090
ENTRYPOINT ["/usr/local/bin/smart-gateway-panel"]
