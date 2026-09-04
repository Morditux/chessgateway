# gatewayclient — UCI frontend for ChessGateway

`gatewayclient` behaves like a UCI engine for a chess GUI (Arena, CuteChess, Fritz, etc.) but connects to `chessgateway` instead of launching a local engine.

## Architecture

- `config.go` — loading and validation of `gatewayclient.conf` (JSON, `DisallowUnknownFields`, 1 MiB max).
- `client.go` — TCP/TLS connection, `hello` handshake, `access_keys` authentication, `select_engine`, then bidirectional bridge:
  - GUI `stdin` → `{"type":"uci","command":...}` → gateway
  - gateway `{"type":"uci_output","line":...}` → GUI `stdout`
- `cmd/gatewayclient/main.go` — `gatewayclient` binary (flags `-config`, logging, signals).

The `command` field is forwarded verbatim with a single `LF` (no restrictive UCI parser, proprietary extensions remain intact). No shell line is built from network data.

## Configuration

See `gatewayclient.conf.example` at the repository root and `client/gatewayclient.conf.example`:

```json
{
  "host": "127.0.0.1:9000",
  "engine_id": "stockfish-17",
  "access_key": "f81d4fae-7dec-11d0-a765-00a0c91e6bf6",
  "connect_timeout_ms": 5000,
  "max_line_bytes": 1048576,
  "log_file": "",
  "log_commands": false,
  "tls": {
    "enabled": false,
    "ca_file": "/etc/chessgateway/ca.crt",
    "cert_file": "",
    "key_file": "",
    "server_name": "",
    "insecure_skip_verify": false
  }
}
```

| Field | Description |
|-------|-------------|
| `host` | `host:port` of the gateway (required) |
| `engine_id` | server-side engine id (`config.json` → `engines[].id`) |
| `access_key` | UUID when `auth.enabled=true` on the server |
| `connect_timeout_ms` | connect timeout (1–60000, default 5000) |
| `max_line_bytes` | JSON/UCI line limit (1024–16MiB, default 1 MiB) |
| `log_file` | `""` → stderr, otherwise file (0600 recommended) |
| `log_commands` | `true` logs full UCI commands for debugging (default `false`: only sizes; enabling it may retain game data) |
| `tls.enabled` | enable TLS 1.3 |
| `tls.ca_file` | private CA (PEM) |
| `tls.cert_file`/`key_file` | mutual TLS client cert (both required) |
| `tls.server_name` | SNI (default: host without port) |
| `tls.insecure_skip_verify` | testing only |

```sh
cp gatewayclient.conf.example gatewayclient.conf
# edit host / engine_id / access_key
chmod 600 gatewayclient.conf
```

Config file lookup: explicit `-config` path, otherwise `./gatewayclient.conf`, otherwise `./client/gatewayclient.conf`.

## Build

```sh
go build -o gatewayclient ./cmd/gatewayclient
./gatewayclient -config gatewayclient.conf   # or client/gatewayclient.conf
```

## GUI usage

- **Arena**: `Engines → Install New Engine` → select `gatewayclient`
- **CuteChess GUI/cli**: `cutechess-cli -engine cmd=gatewayclient`
- **Fritz**: `Engine → Create UCI Engine`

Manual test:

```sh
./gatewayclient -config gatewayclient.conf
uci
isready
position startpos
go depth 10
quit
```

## Logging and security

- `access_key` is never logged.
- UCI command contents are logged only when `log_commands: true` (debug);
  `log_file` may then contain `position`/`go` commands → protect with `0600`.
- TLS is recommended outside a local network; `insecure_skip_verify` only for labs.
- On gateway errors (`no_engine_selected`, `engine_command_failed`…), the client logs and forwards `info string gateway error [...]` on stdout (without polluting the useful UCI stream).

## Troubleshooting

- `dial gateway: connection refused` → check `host` and that `chessgateway` is listening.
- `authentication failed [invalid_access_key]` → regenerate `access_key` (`uuidgen`) and check `clients.config` on the server.
- `select_engine failed [unknown_engine]` → `engine_id` must exist in `config.json`.
- `protocol line exceeds configured limit` → increase `max_line_bytes` on both client and server.
