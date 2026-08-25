# ChessGateway

ChessGateway is a TCP gateway in Go between a chess GUI and one or more UCI
compatible engines. Each client connection owns its own engine process: several
players can therefore search in parallel without sharing UCI state (`position`,
options, search, etc.).

The server never chooses an executable provided by the client. The allowed
engines, their name, version, command and arguments are defined in the server
configuration.

## Getting started

Prerequisites: Go 1.27 or later and a UCI engine installed on the server
machine, for example Stockfish.

```sh
cp config.example.json config.json
# Adjust engines[0].command to your local installation.
go build -o chessgateway ./cmd/chessgateway
./chessgateway -config config.json
```

By default, it listens on `127.0.0.1:9000`. For remote deployment, explicitly
configure the desired address and use TLS or an authenticated tunnel. The
protocol does not include application-level authentication.

### Configuration

```json
{
  "listen": "127.0.0.1:9000",
  "max_clients": 64,
  "max_line_bytes": 1048576,
  "shutdown_timeout_ms": 2000,
  "tls": {
    "cert_file": "/etc/chessgateway/server.crt",
    "key_file": "/etc/chessgateway/server.key"
  },
  "engines": [
    {
      "id": "stockfish-17",
      "name": "Stockfish",
      "version": "17",
      "command": "/usr/games/stockfish",
      "args": []
    }
  ]
}
```

`tls` is optional, but `cert_file` and `key_file` must be provided together.
`command` and `args` are passed directly to `os/exec`; they are not executed via
`sh -c`. The ids are the stable keys used by clients.

## `chessgateway/1` protocol

The transport is a persistent TCP (or TLS) connection using **JSON Lines**: one
JSON object per UTF-8 line, terminated by `LF` or `CRLF`. Responses and engine
events use the same format. The maximum line size is configurable
(`max_line_bytes`, 1 MiB by default).

On connection, the server sends:

```json
{"type":"hello","protocol":"chessgateway/1","features":["engine_list","engine_selection","engine_stop","uci_stream"]}
```

`request_id` is optional and opaque. When provided, the server copies it into
the associated synchronous response or into an error. `uci_output` events do not
carry a `request_id`, because a search can produce lines after several
successive commands.

### Listing engines

Request:

```json
{"type":"list_engines","request_id":"r1"}
```

Response:

```json
{"type":"engines","request_id":"r1","engines":[{"id":"stockfish-17","name":"Stockfish","version":"17"}]}
```

Only `id`, `name` and `version` are exposed; the executable path and its
arguments stay on the server side.

### Selecting an engine

```json
{"type":"select_engine","request_id":"r2","engine_id":"stockfish-17"}
```

The server stops the engine currently attached to this connection, starts the
requested engine and replies:

```json
{"type":"engine_selected","request_id":"r2","engine_id":"stockfish-17","engine":{"id":"stockfish-17","name":"Stockfish","version":"17"}}
```

Selection does not automatically run `uci`: the client keeps control of the UCI
dialogue and must send the initialization itself.

### Forwarding a UCI command

```json
{"type":"uci","request_id":"r3","command":"uci"}
{"type":"uci","command":"setoption name Threads value 8"}
{"type":"uci","command":"isready"}
{"type":"uci","command":"position startpos moves e2e4 e7e5"}
{"type":"uci","command":"go wtime 300000 btime 300000 winc 2000 binc 2000"}
```

The `command` value is forwarded to the engine as is with a single transport
`LF` appended. The server does not restrict UCI to a known list of commands:
standard commands and engine-specific extensions are therefore available,
notably:

- initialization: `uci`, `debug`, `isready`, `setoption`, `register`,
  `ucinewgame`;
- position: `position startpos ...` and `position fen ...`;
- search: `go` with `searchmoves`, `ponder`, `wtime`, `btime`, `winc`, `binc`,
  `movestogo`, `depth`, `nodes`, `mate`, `movetime`, `infinite`;
- control: `stop`, `ponderhit`, `quit`;
- any extension command accepted by the engine.

Each engine stdout line becomes an event:

```json
{"type":"uci_output","engine_id":"stockfish-17","line":"id name Stockfish 17"}
{"type":"uci_output","engine_id":"stockfish-17","line":"uciok"}
```

Lines arrive asynchronously and in engine stdout order. Stderr is drained and
sent to the server logs, never to the client. A UCI command does not produce an
additional acknowledgement: the absence of an error means it was written to the
engine's stdin; UCI responses (`uciok`, `readyok`, `bestmove`, etc.) are the
only business results.

### Stopping an engine

```json
{"type":"stop_engine","request_id":"r4"}
```

This operation is a session control: the server sends `stop` then `quit`,
waits at most `shutdown_timeout_ms`, then terminates the process if it does not
exit. It replies:

```json
{"type":"engine_stopped","request_id":"r4","engine_id":"stockfish-17"}
```

To stop a search while keeping the selected engine, use the UCI command `stop`.
The UCI command `quit` is also relayed and releases the engine from the
connection.

On connection close, the server automatically stops the attached process. A new
selection always replaces the previous process.

### Errors

Common format:

```json
{"type":"error","request_id":"r5","code":"no_engine_selected","message":"select an engine before sending UCI commands"}
```

Main codes: `invalid_json`, `invalid_request`, `line_too_long`,
`unknown_request_type`, `unknown_engine`, `engine_start_failed`,
`no_engine_selected`, `invalid_uci_command`, `engine_command_failed` and
`server_busy`. Runtime error messages are deliberately generic so as not to
leak paths or internal server details.

## Security and operations

- The engine whitelist prevents the client from launching an arbitrary command;
  no shell is used.
- The number of connections and the frame sizes are bounded.
- The server listens locally by default.
- TLS can protect the transport, but the server certificate alone does not
  authenticate clients. For Internet access, add upstream authentication, an
  SSH/VPN tunnel or TLS termination with identity verification.
- Each client consumes one engine process: size `max_clients` according to the
  available CPU/RAM and apply the appropriate system limits.

## Development

```sh
GOCACHE=/tmp/chessgateway-gocache go test ./...
GOCACHE=/tmp/chessgateway-gocache go test -race ./...
go vet ./...
```

The root package contains the server and testable primitives; the binary is in
`cmd/chessgateway`.

The project is distributed under the MIT license, see [LICENSE](LICENSE).
