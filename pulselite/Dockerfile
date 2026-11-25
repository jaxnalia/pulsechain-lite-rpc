# syntax=docker/dockerfile:1.7
# Build context: this directory. All Go dependencies (including the go-pulse
# packages used) are vendored in ./vendor, so the build downloads no Go
# modules; GOPROXY=off makes any attempt fail instead of fetching.
FROM golang:1.24-bookworm AS build
ENV GOFLAGS=-mod=vendor GOPROXY=off
WORKDIR /src/pulselite
COPY . .
# Windows checkouts with core.autocrlf=true rewrite vendored .c/.h to CRLF;
# that breaks the C compiler ("invalid digit in octal constant"). Normalize.
RUN find vendor -type f \( -name '*.c' -o -name '*.h' -o -name '*.go' \) \
      -exec sed -i 's/\r$//' {} +
RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags "-s -w" -o /out/ ./cmd/pulselite ./cmd/litecheck

FROM debian:bookworm-slim
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/pulselite /out/litecheck /usr/local/bin/
EXPOSE 8545 8551 6060 30303 30303/udp
# Liveness only; readiness to serve wallets is GET /ready (200 when ready).
HEALTHCHECK --interval=10s --timeout=5s --start-period=60s CMD ["litecheck", "health"]
ENTRYPOINT ["pulselite"]
