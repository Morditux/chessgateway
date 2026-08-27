# gatewayclient — client UCI frontend for ChessGateway

`gatewayclient` se comporte comme un moteur UCI pour une GUI d'échecs (Arena, CuteChess, Fritz, etc.) mais se connecte à `chessgateway` au lieu de lancer un moteur local.

## Architecture

- `config.go` — chargement et validation de `gatewayclient.conf` (JSON, `DisallowUnknownFields`, 1 MiB max).
- `client.go` — connexion TCP/TLS, handshake `hello`, authentification `access_keys`, `select_engine`, puis pont bidirectionnel :
  - GUI `stdin` → `{"type":"uci","command":...}` → gateway
  - gateway `{"type":"uci_output","line":...}` → GUI `stdout`
- `cmd/gatewayclient/main.go` — binaire `gatewayclient` (flags `-config`, logs, signaux).

Le champ `command` est relayé tel quel avec un seul `LF` (pas de parser UCI restrictif, extensions propriétaires intactes). Aucune ligne shell n'est construite depuis le réseau.

## Configuration

Voir `gatewayclient.conf.example` à la racine et `client/gatewayclient.conf.example` :

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

| Champ | Description |
|-------|-------------|
| `host` | `host:port` du gateway (requis) |
| `engine_id` | id moteur côté serveur (`config.json` → `engines[].id`) |
| `access_key` | UUID si `auth.enabled=true` côté serveur |
| `connect_timeout_ms` | timeout de connexion (1–60000, défaut 5000) |
| `max_line_bytes` | limite ligne JSON/UCI (1024–16MiB, défaut 1 MiB) |
| `log_file` | `""` → stderr, sinon fichier (0600 recommandé) |
| `tls.enabled` | active TLS 1.3 |
| `tls.ca_file` | CA privé (PEM) |
| `tls.cert_file`/`key_file` | client mTLS (ensemble) |
| `tls.server_name` | SNI (défaut : host sans port) |
| `tls.insecure_skip_verify` | test uniquement |

```sh
cp gatewayclient.conf.example gatewayclient.conf
# éditer host / engine_id / access_key
chmod 600 gatewayclient.conf
```

Recherche du fichier : `-config` explicite, sinon `./gatewayclient.conf`, sinon `./client/gatewayclient.conf`.

## Construction

```sh
go build -o gatewayclient ./cmd/gatewayclient
./gatewayclient -config gatewayclient.conf   # ou client/gatewayclient.conf
```

## Utilisation GUI

- **Arena** : `Engines → Install New Engine` → choisir `gatewayclient`
- **CuteChess GUI/cli** : `cutechess-cli -engine cmd=gatewayclient`
- **Fritz** : `Engine → Create UCI Engine`

Test manuel :

```sh
./gatewayclient -config gatewayclient.conf
uci
isready
position startpos
go depth 10
quit
```

## Logs et sécurité

- `access_key` n'est jamais loguée.
- `log_file` peut contenir des commandes `position`/`go` → protéger en `0600`.
- TLS est recommandé hors réseau local ; `insecure_skip_verify` seulement en labo.
- En cas d'erreur gateway (`no_engine_selected`, `engine_command_failed`…), le client loggue et relaie `info string gateway error [...]` sur stdout (sans polluer le flux UCI utile).

## Dépannage

- `dial gateway: connection refused` → vérifier `host` et que `chessgateway` écoute.
- `authentication failed [invalid_access_key]` → régénérer `access_key` (`uuidgen`) et vérifier `clients.config` côté serveur.
- `select_engine failed [unknown_engine]` → `engine_id` doit exister dans `config.json`.
- `protocol line exceeds configured limit` → augmenter `max_line_bytes` côté client et serveur.
