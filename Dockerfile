# syntax=docker/dockerfile:1.6
#
# Two-stage build: compile with the Go toolchain, then drop the binary into a
# distroless image. The final image has no shell, no package manager, and runs
# as a non-root user. It communicates over stdio only — no ports are exposed.
#
# Both FROM lines are pinned to content-addressable digests so Scorecard's
# PinnedDependencies check passes and an upstream tag rewrite cannot silently
# change our base image. Dependabot will track digest updates.

FROM golang:1.25-alpine@sha256:8e02eb337d9e0ea459e041f1ee5eece41cbb61f1d83e7d883a3e2fb4862063fa AS build
WORKDIR /src
# ca-certificates were installed here in an earlier iteration to let the
# build stage fetch modules over HTTPS, but the golang:alpine base image
# already ships them — the extra `apk add` was dead weight. The distroless
# runtime stage below brings its own trust store. Review finding on dead
# apk add.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.Version=${VERSION}" \
    -o /out/mcp-ynab .

FROM gcr.io/distroless/static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1
COPY --from=build /out/mcp-ynab /usr/local/bin/mcp-ynab
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/mcp-ynab"]
