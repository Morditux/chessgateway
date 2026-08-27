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
configure the desired address and use TLS or an authenticated tunnel.
Optional application-level client authentication with UUID access keys is
described in [Client authentication](#client-authentication).

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

## Client authentication

Authentication is disabled unless an `auth` section enables it:

```json
{
  "auth": {
    "enabled": true,
    "clients_file": "/etc/chessgateway/clients.config"
  }
}
```

`clients_file` lists the authorized clients, one per line: a name used for
logging and a unique UUID access key. Empty lines and lines starting with `#`
are ignored:

```text
# One client per line: <name> <access-key>
# Generate a key with: uuidgen
desktop f81d4fae-7dec-11d0-a765-00a0c91e6bf6
laptop  06ec1ea5-3b6d-4312-9f2c-8f514a9c37f1
```

Names accept 1 to 64 ASCII letters, digits, `.`, `_` and `-`; keys must be
canonical UUIDs (case-insensitive). Each name and each key may appear only
once and at least one client is required: the server refuses to start when the
file is missing or invalid. See [`clients.config.example`](clients.config.example).

## `chessgateway/1` protocol

The transport is a persistent TCP (or TLS) connection using **JSON Lines**: one
JSON object per UTF-8 line, terminated by `LF` or `CRLF`. Responses and engine
events use the same format. The maximum line size is configurable
(`max_line_bytes`, 1 MiB by default).

On connection, the server sends:

```json
{"type":"hello","protocol":"chessgateway/1","features":["engine_list","engine_selection","engine_stop","uci_stream"]}
```

`access_keys` is appended to `features` when client authentication is enabled.

`request_id` is optional and opaque. When provided, the server copies it into
the associated synchronous response or into an error. `uci_output` events do not
carry a `request_id`, because a search can produce lines after several
successive commands.

### Authenticating

Required when the hello features contain `access_keys`: the first request must
then be

```json
{"type":"authenticate","request_id":"auth","access_key":"f81d4fae-7dec-11d0-a765-00a0c91e6bf6"}
```

and the server replies

```json
{"type":"authenticated","request_id":"auth"}
```

Until authentication succeeds, any other request is refused with the
`authentication_required` error. An invalid key produces a single
`invalid_access_key` error and the server closes the connection. Sending
`authenticate` on an already authenticated connection is refused with
`already_authenticated`. Keys are compared in constant time and never logged.

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
`no_engine_selected`, `invalid_uci_command`, `engine_command_failed`,
`authentication_required`, `invalid_access_key`, `already_authenticated`,
`server_busy`. Runtime error messages are deliberately generic so as not to
leak paths or internal server details.

## Security and operations

- The engine whitelist prevents the client from launching an arbitrary command;
  no shell is used.
- The number of connections and the frame sizes are bounded.
- The server listens locally by default.
- Optional access keys authenticate clients at the application level. Treat
  `clients.config` as secret material and restrict its file permissions; keys
  are bearer secrets, so combine them with TLS to keep them off the wire in
  clear.
- TLS can protect the transport, but the server certificate alone does not
  authenticate clients. For Internet access, add upstream authentication, an
  SSH/VPN tunnel or TLS termination with identity verification.
- Each client consumes one engine process: size `max_clients` according to the
  available CPU/RAM and apply the appropriate system limits.

## Deployment with systemd

`deploy/` contains the packaging and deployment files:

- `deploy/build-deb.sh` builds `chessgateway_<version>_<arch>.deb` (override
  `VERSION`, `ARCH` and `MAINTAINER`). Installing the package creates the
  `chessgateway` system user, installs the unit, the `sysctl` tuning and
  **enables and starts the service automatically**:
  ```sh
  ./deploy/build-deb.sh
  sudo apt-get install ./chessgateway_*.deb
  ```
- `deploy/install.sh` does the same from a source checkout
  (`sudo ./deploy/install.sh --start`); `--uninstall` removes the service.
- `deploy/chessgateway.service` runs the binary as a dedicated user, with
  `LimitNOFILE=65536` and `Restart=on-failure`. SIGTERM triggers the server's
  graceful shutdown, so `systemctl stop` releases the engine processes without
  an `ExecStop`.
- `deploy/sysctl.d/99-chessgateway.conf` tunes the kernel for many concurrent
  connections (listen backlog, SYN queue, TIME_WAIT); it is applied on install
  and can be re-applied with `sysctl -p /etc/sysctl.d/99-chessgateway.conf`.

Once installed, manage the service with `systemctl`:

```sh
sudo systemctl enable --now chessgateway   # enable at boot and start
sudo systemctl start chessgateway          # start
sudo systemctl stop chessgateway           # stop (graceful, engines released)
sudo systemctl restart chessgateway        # restart
sudo systemctl status chessgateway         # status and recent log
journalctl -u chessgateway -f              # follow the logs
```

The runtime configuration lives in `/etc/chessgateway/config.json` (installed
from `config.example.json`, never overwritten on reinstall): adjust the
`listen` address, `max_clients` and the `engines` entries to the installed
engine binaries. Logs and engine stderr go to the journal.

## Deployment with Docker

`Dockerfile` (multi-stage `golang:1.27-bookworm` → `debian:bookworm-slim`) and
`docker-compose.yml` are provided. Engine binaries are **not baked into the
image**: the host folder that contains them is mounted via the
`ENGINES_HOST_DIR` environment variable. The clients access-key file also
**stays on the host** and is never copied into the image — it is mounted
read-only via `CLIENTS_FILE_HOST`.

```sh
cp .env.example .env
# Edit .env:
#   ENGINES_HOST_DIR=/home/mordicus/chess/engines  # host folder with stockfish, etc.
#   CLIENTS_FILE_HOST=/home/mordicus/chessgateway/clients.config  # only if auth.enabled=true
#   CHESSGATEWAY_PORT=9000

# Edit config.docker.json for the container paths:
#   "listen": "0.0.0.0:9000"
#   "command": "/engines/stockfish"   # container path, not host path
#   "auth": { "enabled": true, "clients_file": "/etc/chessgateway/clients.config" }

# Prepare a host folder with executable engines:
mkdir -p ./engines
cp /usr/games/stockfish ./engines/
chmod +x ./engines/stockfish

# If authentication is enabled, prepare the keys file on the host:
# chmod 644 clients.config  # one line per client: "<name> <uuid>"
# # The container runs as user 999 (chessgateway), so the file must be
# # readable inside the container. Use 644, or more restrictively:
# #   chgrp 999 clients.config && chmod 640 clients.config

docker compose up --build -d
docker compose logs -f
# Test:
echo '{"type":"list_engines","request_id":"r1"}' | nc localhost 9000
docker compose down
```

`config.docker.json` is the container configuration template (listening on
`0.0.0.0:9000`, engines at `/engines/...`, `auth.clients_file` at
`/etc/chessgateway/clients.config`). It is copied into the image as
`/etc/chessgateway/config.json` and can be overridden by the volume
`./config.docker.json:/etc/chessgateway/config.json:ro`. TLS certificates, if
used, should be mounted similarly (e.g. `./certs:/certs:ro` and
`tls.cert_file`/`key_file` pointing inside the container).

Security notes:
- `.dockerignore` excludes `clients.config`, `*.key`, `*.crt` and `engines/` so
  secrets and host binaries never enter the image layers.
- Both engine and clients mounts are `:ro`.
- The container runs as unprivileged user `chessgateway` and respects
  `ulimits.nofile=65536` and `stop_grace_period: 30s` (mirroring
  `deploy/chessgateway.service`).

## Gateway client (`gatewayclient`) — UCI frontend

`gatewayclient` se comporte comme un moteur UCI pour une interface graphique (Arena,
CuteChess, Fritz, etc.) mais relaye les commandes UCI vers `chessgateway` via le
protocole `chessgateway/1`. Il est utile pour déporter le calcul sur une machine
distante tout en gardant une GUI locale.

### Construction

```sh
go build -o gatewayclient ./cmd/gatewayclient
# ou depuis le dossier client :
go build -o gatewayclient ./cmd/gatewayclient
```

### Configuration (`gatewayclient.conf`)

Le binaire lit `gatewayclient.conf` (JSON, même format que le serveur) — voir
[`gatewayclient.conf.example`](gatewayclient.conf.example) et
[`client/gatewayclient.conf.example`](client/gatewayclient.conf.example) :

```json
{
  "host": "127.0.0.1:9000",
  "engine_id": "stockfish-17",
  "access_key": "f81d4fae-7dec-11d0-a765-00a0c91e6bf6",
  "connect_timeout_ms": 5000,
  "max_line_bytes": 1048576,
  "log_file": "",
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

- `host` (`host:port`) — adresse du gateway (requis).
- `engine_id` — id d'un moteur autorisé côté serveur (requis, voir `config.json`).
- `access_key` — UUID d'authentification si `auth.enabled=true` côté serveur.
- `connect_timeout_ms` / `max_line_bytes` — bornes réseau (défauts 5 s / 1 MiB).
- `log_file` — fichier de log (`""` → `stderr`). Contient les événements de
  connexion ; les commandes UCI y sont journalisées — protégez ce fichier s'il
  contient des données de parties.
- `tls.enabled` — active TLS (TLS 1.3). `ca_file` pour un CA privé, `cert_file`/
  `key_file` pour un client mTLS, `server_name` pour SNI, `insecure_skip_verify`
  uniquement en test.

Copiez l'exemple et éditez-le :

```sh
cp gatewayclient.conf.example gatewayclient.conf
# ou
cp client/gatewayclient.conf.example client/gatewayclient.conf
# éditez host, engine_id et access_key
chmod 600 gatewayclient.conf
```

La clé `access_key` est une bearer secret : combinez-la avec TLS et des
permissions `0600`.

### Utilisation comme moteur UCI

1. Déclarez `gatewayclient` comme moteur dans votre GUI :
   - **Arena** : `Engines → Install New Engine → gatewayclient` (pointez vers le binaire)
   - **CuteChess** : `cutechess-cli -engine cmd=gatewayclient -engine cmd=stockfish` ou via `Settings → Engines`
   - **Fritz / ChessBase** : `Engine → Create UCI Engine → gatewayclient.exe`

   Le binaire se comporte exactement comme un moteur UCI : la GUI écrit sur son
   `stdin` (`uci`, `isready`, `position`, `go`, `stop`, `quit`…) et lit les
   réponses sur `stdout`.

2. Spécifiez le fichier de configuration :
   - Par défaut `gatewayclient` cherche `./gatewayclient.conf` puis
     `./client/gatewayclient.conf`.
   - Chemin explicite : `gatewayclient -config /path/to/gatewayclient.conf`

3. Exemple de session manuelle :

   ```sh
   ./gatewayclient -config gatewayclient.conf
   uci
   # → id name Stockfish 17
   # → uciok
   isready
   # → readyok
   position startpos moves e2e4 e7e5
   go wtime 300000 btime 300000
   # → info ... / bestmove ...
   quit
   ```

Au démarrage, `gatewayclient` se connecte, attend `hello`, s'authentifie si
`access_keys` est annoncé, sélectionne `engine_id`, puis ponte `stdin` →
`{"type":"uci","command":...}` et `{"type":"uci_output","line":...}` → `stdout`.
Aucune ligne shell n'est construite depuis le réseau ; le champ `command` est
relayé tel quel avec un seul `LF`.

Fermer la GUI ou envoyer `quit` coupe la connexion ; le serveur libère alors le
processus moteur associé.

### Sécurité

- Ne loguez pas les parties si `log_file` est exposé ; traitez-le comme une
  donnée sensible.
- Préférez `tls.enabled=true` avec un CA privé dès que le réseau n'est pas
  local.
- Le client ne logue jamais `access_key`.

## Development

```sh
GOCACHE=/tmp/chessgateway-gocache go test ./...
GOCACHE=/tmp/chessgateway-gocache go test -race ./...
go vet ./...
```

The root package contains the server and testable primitives; the binaries are in
`cmd/chessgateway` and `cmd/gatewayclient` (`client/` holds the library and
config example).

The project is distributed under the MIT license, see [LICENSE](LICENSE).
