# Multi-arch: docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 .
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH TARGETVARIANT VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN GOARM=${TARGETVARIANT#v} CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/wa-transcriber ./cmd/wa-transcriber \
 && mkdir -p /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
# /data must exist and be writable by the unprivileged user: for a fresh
# named volume Docker copies this ownership, for a bind mount chown it.
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build /out/wa-transcriber /wa-transcriber
# Numeric uid/gid: the scratch image has no /etc/passwd.
USER 65532:65532
# Non-root cannot bind :80, and the process needs no more than the UI port.
ENV WAT_DATA=/data \
    WAT_LISTEN=:8080
VOLUME /data
EXPOSE 8080
# Also answers while the setup AP and the captive portal are active.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/wa-transcriber", "-healthcheck"]
ENTRYPOINT ["/wa-transcriber"]
