# syntax=docker/dockerfile:1
# Multi-stage build -> distroless runtime (docs/14-deployment.md).
# The frontend build stage joins here in sprint 11.

FROM golang:1.26-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG VERSION=dev
ARG COMMIT=none
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      -o /out/proxui ./cmd/proxui

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/proxui /proxui
EXPOSE 8080 9090
USER nonroot:nonroot
ENTRYPOINT ["/proxui"]
CMD ["--role=all"]
