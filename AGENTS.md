# AGENTS.md

## Project

ChessGateway is a Go TCP/TLS gateway between chess clients and UCI engine
processes. The root package is named `gateway`; the CLI entry point is
`cmd/chessgateway`.

## Invariants to preserve

- The transport is JSON Lines, versioned by `chessgateway/1`.
- A client connection owns at most one engine and an engine is never shared
  between connections.
- The `uci.command` field is relayed as UCI text, with only the transport line
  separator added. Do not introduce a restrictive UCI parser: proprietary
  extensions must keep working.
- Executables and arguments come exclusively from `Config.Engines`. Never
  build a shell line from network input and never use `sh -c`.
- The engine's stdout is the client UCI stream; stderr must be drained to
  avoid blocking but stays in the logs.
- Any write to a client must go through the connection's write lock. Any
  modification of a session's state must be protected by its mutex.
- An engine change stops the old process first. A disconnect and a server
  shutdown must release the engine processes.

## Protocol

The user specification and normative examples are in `README.md`. Any frame
change must update the README and the integration tests. Protocol errors must
stay structured (`type=error`, `code`, `message`) and must not leak executable
paths.

## Security

- Validate all network data and bound lines before decoding them.
- Use `exec.Command` with separate arguments; never shell interpolation.
- Keep the safe defaults: local listen address, client limit, line limit,
  shutdown timeout and initial connection timeout.
- TLS protects the transport but is not client authentication. Do not present
  this gateway as safe for the Internet without identity verification
  upstream.
- Do not log commands or game data if that could expose client information;
  engine stderr is already treated as untrusted data.

## Verification

Before shipping a change:

```sh
gofmt -w <modified-go-files>
GOCACHE=/tmp/chessgateway-gocache go test ./...
GOCACHE=/tmp/chessgateway-gocache go test -race ./...
go vet ./...
```

Add a test when a new frame, session transition, limit or concurrent path is
introduced. Preserve the repository's existing changes and do not use
destructive commands without an explicit request.
