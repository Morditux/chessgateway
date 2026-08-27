# syntax=docker/dockerfile:1

# Builder: compile the gateway binary.
FROM golang:1.27-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/chessgateway ./cmd/chessgateway

# Runtime: minimal Debian with glibc for dynamically linked UCI engines (e.g. Stockfish).
FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --no-create-home --shell /usr/sbin/nologin chessgateway
COPY --from=builder /out/chessgateway /usr/local/bin/chessgateway
# Default container config (overridable via volume mount).
COPY config.docker.json /etc/chessgateway/config.json
RUN chown -R chessgateway:chessgateway /etc/chessgateway
USER chessgateway
EXPOSE 9000
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/chessgateway"]
CMD ["-config", "/etc/chessgateway/config.json"]
