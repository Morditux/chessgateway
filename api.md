# API — évolutions pour un client existant (`chessgateway/1`)

Ce document décrit les changements visibles par un client déjà implémenté
contre `chessgateway/1`, et comment s'y adapter. Le protocole reste
**JSON Lines**, la poignée de main (`hello`), l'authentification et tous les
types existants sont inchangés : un client non modifié continue de fonctionner,
mais il passe à côté d'un événement et de deux garde-fous documentés ci-dessous.

Référence complète : `README.md`. Comportements couverts par les tests
d'intégration dans `server_test.go`.

## 1. Nouvel événement : `engine_exited`

Quand le moteur attaché à la connexion meurt **de lui-même** (crash, OOM kill,
kill externe), le serveur détache le processus et envoie :

```json
{"type":"engine_exited","engine_id":"stockfish-17"}
```

- Comme `uci_output`, cet événement est asynchrone et **ne porte pas de
  `request_id`**.
- Après cet événement, toute commande `uci` est refusée avec
  `no_engine_selected` jusqu'au prochain `select_engine`.
- Les arrêts normaux (`stop_engine`, nouveau `select_engine`, commande UCI
  `quit`, fermeture de connexion) ne produisent **jamais** cet événement :
  ils ont déjà leur réponse synchrone.
- **Ordre d'arrivée** : si le moteur meurt immédiatement après `select_engine`,
  `engine_exited` peut arriver **avant** `engine_selected`. Ne supposez donc
  pas `engine_selected` en premier : corrèlez par `engine_id` et acceptez les
  deux ordres.

### Migration

```pseudo
on_frame(frame):
  match frame.type:
    case "uci_output": handle_uci_line(frame.engine_id, frame.line)
    case "engine_exited":                      # NOUVEAU
      mark_engine_dead(frame.engine_id)        # stopper l'attente de bestmove
      # proposer à l'utilisateur : select_engine à nouveau
    ...
```

Sans ce cas, une recherche en cours pend indéfiniment (aucun `bestmove` ne
viendra). `gatewayclient` relaie déjà l'événement au GUI sous forme de
`info string gateway engine <id> exited`, ignoré par la logique de coups.

## 2. Nouvelle erreur : `select_too_frequent`

Le serveur limite la vitesse de **changement** de moteur par connexion
(`min_select_interval_ms`, défaut 500 ms, `0` = désactivé) pour empêcher le
churn `fork/exec` en boucle. Un `select_engine` vers un moteur **différent**
envoyé trop tôt est refusé sans toucher au processus en cours :

```json
{"type":"error","request_id":"r2","code":"select_too_frequent","message":"wait before selecting another engine"}
```

### Migration

- Au reçu de `select_too_frequent`, attendre au moins `min_select_interval_ms`
  (ou 500 ms par défaut) puis **rejouer la même requête** — aucun état n'a changé.
- Backoff simple recommandé : `attendre 600 ms → retry une fois`, puis remonter
  l'erreur à l'utilisateur si elle persiste.
- Note : une tentative qui échoue au démarrage (`engine_start_failed`) arme
  aussi le cooldown (elle a forké) ; un `engine_id` inconnu (`unknown_engine`,
  sans fork) ne l'arme jamais.

## 3. Re-sélection du moteur courant : no-op

`select_engine` vers le moteur **déjà attaché** répond `engine_selected`
immédiatement, **sans redémarrer le processus** (une recherche en cours survit)
et sans consommer le cooldown du §2.

```json
{"type":"select_engine","request_id":"r2","engine_id":"stockfish-17"}
{"type":"engine_selected","request_id":"r2","engine_id":"stockfish-17","engine":{"id":"stockfish-17","name":"Stockfish","version":"17"}}
```

### Migration

- Aucun changement requis : c'est un assouplissement. Vous pouvez en profiter
  pour rendre vos re-sélections défensives idempotentes (ex. au reconnect
  logique sans recréer la connexion, renvoyer `select_engine` sans crainte de
  tuer une recherche).
- Ne vous en servez pas comme « ping » : il n'y a toujours pas de `ping` dans
  `chessgateway/1` ; préférez `isready` via `uci` pour tester la vivacité.

## 4. Commandes UCI vides désormais rejetées

`{"type":"uci","command":""}` (ou blanche, ex. `"   "`) est refusé, sans rien
envoyer au moteur :

```json
{"type":"error","request_id":"r3","code":"invalid_uci_command","message":"command must not be empty"}
```

### Migration

- Filtrez les lignes vides côté client avant envoi (ce que fait déjà
  `gatewayclient`). Si vous relayez du texte brut (stdin GUI), ignorez les
  lignes `trim(line) == ""` au lieu de les encapsuler.

## 5. Robustesse d'écriture (aucun changement de trame)

- Les réponses synchrones (`hello`, `engines`, `engine_selected`,
  `engine_stopped`, `authenticated`, `error`) sont écrites directement, comme
  avant : leur garantie de livraison est inchangée.
- Les événements asynchrones du moteur (`uci_output`, `engine_exited`)
  transitent par une file bornée (**256** trames) avec un écrivain dédié, sous
  le même verrou d'écriture : **les trames ne s'entrelacent jamais** sur le fil.
- La file absorbe les rafales du moteur ; un consommateur durablement lent
  remplit la file puis est déconnecté par la deadline d'écriture existante
  (30 s), ce qui libère son slot et son processus moteur.

### Migration

Aucune. Notez seulement qu'après `invalid_access_key`, la connexion est
fermée **après** l'envoi de l'erreur : lisez une trame d'erreur puis attendez
l'EOF, ne fermez pas dès l'envoi de `authenticate`.

## 6. Récapitulatif des codes

| Code | Quand | Conduite |
|---|---|---|
| `engine_exited` (événement, pas erreur) | moteur mort seul | `select_engine` à nouveau |
| `select_too_frequent` | changement trop rapide | attendre ~500 ms, retry |
| `invalid_uci_command` | commande vide/blanche (en plus des séparateurs) | ne pas envoyer de lignes vides |
| `no_engine_selected` | après `engine_exited`, avant re-sélection | `select_engine` d'abord |

## 7. Checklist de mise à jour (5 minutes)

1. Ajouter le cas `engine_exited` dans le dispatch (§1), avec gestion des deux
   ordres `engine_selected`/`engine_exited`.
2. Ajouter le retry sur `select_too_frequent` (§2).
3. Ignorer les commandes vides avant envoi (§4).
4. (Optionnel) Tirer parti du `select` idempotent (§3).
5. Tester contre le serveur à jour : `select` → `uci isready` → tuer le binaire
   moteur (`kill -9`) → attendre `engine_exited` → `select` → `isready` → `readyok`.
