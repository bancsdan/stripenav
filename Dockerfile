# syntax=docker/dockerfile:1
#
# Multi-stage build for the stripenav binary.
#
# This module imports github.com/bancsdan/go-stripenav as a regular Go
# dependency. The build pulls it from the module proxy — no local
# checkout of the library is required inside the image.

FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache deps first so source edits don't bust the layer.
COPY go.mod go.sum ./
RUN go mod download

# Now the source.
COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN CGO_ENABLED=0 GOOS=linux \
    go build \
      -trimpath \
      -ldflags='-s -w -extldflags "-static"' \
      -o /out/stripenav ./cmd/stripenav

# Final image: distroless static, ~2 MB beyond the binary.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/stripenav /usr/local/bin/stripenav

EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/stripenav", "--healthcheck"]
ENTRYPOINT ["/usr/local/bin/stripenav"]
