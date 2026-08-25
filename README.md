# ChessGateway

ChessGateway est une passerelle TCP en Go entre une IHM d’échecs et un ou
plusieurs moteurs compatibles UCI. Chaque connexion cliente possède son propre
processus moteur : plusieurs joueurs peuvent donc calculer en parallèle sans
partager l’état UCI (`position`, options, recherche, etc.).

Le serveur ne choisit jamais un exécutable fourni par le client. Les moteurs
autorisés, leur nom, leur version, leur commande et leurs arguments sont définis
dans la configuration du serveur.

## Démarrage

Prérequis : Go 1.27 ou ultérieur et un moteur UCI installé sur la machine
serveur, par exemple Stockfish.

```sh
cp config.example.json config.json
# Adapter engines[0].command à l’installation locale.
go build -o chessgateway ./cmd/chessgateway
./chessgateway -config config.json
```

Par défaut, l’écoute se fait sur `127.0.0.1:9000`. Pour un déploiement distant,
configurer explicitement l’adresse souhaitée et utiliser TLS ou un tunnel
authentifié. Le protocole n’intègre pas d’authentification applicative.

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

`tls` est optionnel, mais `cert_file` et `key_file` doivent être fournis
ensemble. `command` et `args` sont transmis directement à `os/exec`; ils ne
sont pas exécutés via `sh -c`. Les identifiants sont les clés stables utilisées
par les clients.

## Protocole `chessgateway/1`

Le transport est une connexion TCP (ou TLS) persistante en **JSON Lines** : un
objet JSON par ligne UTF-8, terminé par `LF` ou `CRLF`. Les réponses et les
événements du moteur utilisent le même format. La taille maximale par ligne est
configurable (`max_line_bytes`, 1 MiB par défaut).

À la connexion, le serveur envoie :

```json
{"type":"hello","protocol":"chessgateway/1","features":["engine_list","engine_selection","engine_stop","uci_stream"]}
```

`request_id` est facultatif et opaque. Lorsqu’il est fourni, le serveur le
recopie dans la réponse synchrone associée ou dans une erreur. Les événements
`uci_output` ne portent pas de `request_id`, car une recherche peut produire
des lignes après plusieurs commandes successives.

### Lister les moteurs

Requête :

```json
{"type":"list_engines","request_id":"r1"}
```

Réponse :

```json
{"type":"engines","request_id":"r1","engines":[{"id":"stockfish-17","name":"Stockfish","version":"17"}]}
```

Seuls `id`, `name` et `version` sont exposés ; le chemin de l’exécutable et ses
arguments restent côté serveur.

### Sélectionner un moteur

```json
{"type":"select_engine","request_id":"r2","engine_id":"stockfish-17"}
```

Le serveur arrête le moteur actuellement attaché à cette connexion, démarre le
moteur demandé et répond :

```json
{"type":"engine_selected","request_id":"r2","engine_id":"stockfish-17","engine":{"id":"stockfish-17","name":"Stockfish","version":"17"}}
```

La sélection ne lance pas automatiquement `uci` : le client garde le contrôle
du dialogue UCI et doit envoyer lui-même l’initialisation.

### Transmettre une commande UCI

```json
{"type":"uci","request_id":"r3","command":"uci"}
{"type":"uci","command":"setoption name Threads value 8"}
{"type":"uci","command":"isready"}
{"type":"uci","command":"position startpos moves e2e4 e7e5"}
{"type":"uci","command":"go wtime 300000 btime 300000 winc 2000 binc 2000"}
```

La valeur de `command` est transmise telle quelle au moteur avec un seul
`LF` de transport ajouté. Le serveur ne réduit pas l’UCI à une liste de
commandes connue : les commandes standard et les extensions propres au moteur
sont donc disponibles, notamment :

- initialisation : `uci`, `debug`, `isready`, `setoption`, `register`,
  `ucinewgame` ;
- position : `position startpos ...` et `position fen ...` ;
- recherche : `go` avec `searchmoves`, `ponder`, `wtime`, `btime`, `winc`,
  `binc`, `movestogo`, `depth`, `nodes`, `mate`, `movetime`, `infinite` ;
- contrôle : `stop`, `ponderhit`, `quit` ;
- toute commande d’extension acceptée par le moteur.

Chaque ligne stdout du moteur devient un événement :

```json
{"type":"uci_output","engine_id":"stockfish-17","line":"id name Stockfish 17"}
{"type":"uci_output","engine_id":"stockfish-17","line":"uciok"}
```

Les lignes arrivent de façon asynchrone et dans l’ordre de stdout du moteur.
Stderr est drainé et envoyé dans les logs du serveur, jamais au client. Une
commande UCI ne produit pas d’accusé de réception supplémentaire : l’absence
d’erreur signifie qu’elle a été écrite dans stdin du moteur ; les réponses UCI
(`uciok`, `readyok`, `bestmove`, etc.) sont les seuls résultats métier.

### Arrêter un moteur

```json
{"type":"stop_engine","request_id":"r4"}
```

Cette opération est un contrôle de session : le serveur envoie `stop` puis
`quit`, attend au plus `shutdown_timeout_ms`, puis termine le processus s’il ne
sort pas. Elle répond :

```json
{"type":"engine_stopped","request_id":"r4","engine_id":"stockfish-17"}
```

Pour arrêter une recherche tout en conservant le moteur sélectionné, utiliser
la commande UCI `stop`. La commande UCI `quit` est également relayée et libère
le moteur de la connexion.

À la fermeture de la connexion, le serveur arrête automatiquement le processus
qui lui est attaché. Une nouvelle sélection remplace toujours l’ancien
processus.

### Erreurs

Format commun :

```json
{"type":"error","request_id":"r5","code":"no_engine_selected","message":"select an engine before sending UCI commands"}
```

Codes principaux : `invalid_json`, `invalid_request`, `line_too_long`,
`unknown_request_type`, `unknown_engine`, `engine_start_failed`,
`no_engine_selected`, `invalid_uci_command`, `engine_command_failed` et
`server_busy`. Les messages d’erreur d’exécution sont volontairement génériques
pour ne pas divulguer les chemins ou détails internes du serveur.

## Sécurité et exploitation

- La liste blanche des moteurs empêche le client de lancer une commande
  arbitraire ; aucun shell n’est utilisé.
- Le nombre de connexions et la taille des trames sont bornés.
- Le serveur écoute localement par défaut.
- TLS peut protéger le transport, mais le certificat serveur seul n’authentifie
  pas les clients. Pour un accès Internet, ajouter une authentification en
  amont, un tunnel SSH/VPN ou une terminaison TLS avec contrôle d’identité.
- Chaque client consomme un processus moteur : dimensionner `max_clients` selon
  les CPU/RAM disponibles et appliquer les limites système adaptées.

## Développement

```sh
GOCACHE=/tmp/chessgateway-gocache go test ./...
GOCACHE=/tmp/chessgateway-gocache go test -race ./...
go vet ./...
```

Le paquet racine contient le serveur et les primitives testables ; le binaire
est dans `cmd/chessgateway`.

Le projet est distribué sous licence MIT, voir [LICENSE](LICENSE).
