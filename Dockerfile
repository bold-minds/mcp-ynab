# syntax=docker/dockerfile:1.6
#
# Two-stage build: compile with the Go toolchain, then drop the binary into a
# distroless image. The final image has no shell, no package manager, and runs
# as a non-root user. It communicates over stdio only — no ports are exposed.

FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.Version=${VERSION}" \
    -o /out/mcp-ynab .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mcp-ynab /usr/local/bin/mcp-ynab
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/mcp-ynab"]
