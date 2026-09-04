# syntax=docker/dockerfile:1

# Builder: compile the gateway binary.
FROM golang:1.27-bookworm AS builder
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X github.com/Morditux/chessgateway.Version=${VERSION}" -o /out/chessgateway ./cmd/chessgateway

# Runtime: minimal Debian with glibc for dynamically linked UCI engines (e.g. Stockfish).
FROM ubuntu:24.04 AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates netcat-openbsd \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --no-create-home --shell /usr/sbin/nologin --uid 999 chessgateway
COPY --from=builder /out/chessgateway /usr/local/bin/chessgateway
# Default container config (overridable via volume mount).
COPY config.docker.json /etc/chessgateway/config.json
RUN chown -R chessgateway:chessgateway /etc/chessgateway
USER chessgateway
EXPOSE 9000
STOPSIGNAL SIGTERM
# The gateway sends hello on connect: reading it back proves the port serves
# chessgateway/1. Each probe holds a client slot only for milliseconds.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD sh -c 'exec nc -w 2 127.0.0.1 9000 | grep -q "\"type\":\"hello\""'
ENTRYPOINT ["/usr/local/bin/chessgateway"]
CMD ["-config", "/etc/chessgateway/config.json"]
