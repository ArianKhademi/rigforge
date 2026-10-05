# syntax=docker/dockerfile:1
#
# Builds two images from one context (pick with --target):
#   api      the Rigforge HTTP api
#   issuer   the development token issuer
#
# Stage 1 compiles static binaries; the final images contain nothing else: no
# shell, no package manager, no libc. That keeps them small and leaves very
# little to patch or to exploit.

FROM golang:1.26-alpine AS build
WORKDIR /src
# Dependencies first: this layer is reused until go.mod or go.sum changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# CGO off = fully static binaries that run on distroless/static.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/issuer ./cmd/issuer

FROM gcr.io/distroless/static-debian12:nonroot AS issuer
COPY --from=build /out/issuer /issuer
EXPOSE 8090
ENTRYPOINT ["/issuer"]

# Last stage = the default target.
FROM gcr.io/distroless/static-debian12:nonroot AS api
COPY --from=build /out/api /api
EXPOSE 8080
ENTRYPOINT ["/api"]
