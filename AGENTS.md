# AGENTS.md

## Projet

ChessGateway est une passerelle TCP/TLS en Go entre des clients d’échecs et
des processus moteurs UCI. Le paquet racine s’appelle `gateway`; le point
d’entrée CLI est `cmd/chessgateway`.

## Invariants à préserver

- Le transport est JSON Lines, versionné par `chessgateway/1`.
- Une connexion cliente possède au plus un moteur et un moteur n’est jamais
  partagé entre connexions.
- Le champ `uci.command` est relayé comme texte UCI, avec uniquement le
  séparateur de ligne de transport ajouté. Ne pas introduire de parseur UCI
  restrictif : les extensions propriétaires doivent fonctionner.
- Les exécutables et arguments viennent exclusivement de `Config.Engines`.
  Ne jamais construire une ligne shell à partir d’une entrée réseau et ne pas
  utiliser `sh -c`.
- stdout du moteur est le flux UCI client ; stderr doit être drainé pour éviter
  un blocage mais reste dans les logs.
- Toute écriture vers un client doit passer par le verrou d’écriture de la
  connexion. Toute modification de l’état d’une session doit être protégée par
  son mutex.
- Un changement de moteur arrête d’abord l’ancien processus. Une déconnexion et
  un arrêt de serveur doivent libérer les processus moteurs.

## Protocole

La spécification utilisateur et les exemples normatifs sont dans `README.md`.
Tout changement de trame doit mettre à jour README et les tests d’intégration.
Les erreurs de protocole doivent rester structurées (`type=error`, `code`,
`message`) et ne doivent pas divulguer les chemins d’exécutables.

## Sécurité

- Valider toutes les données réseau et borner les lignes avant de les décoder.
- Utiliser `exec.Command` avec des arguments séparés ; jamais d’interpolation
  shell.
- Conserver les valeurs par défaut sûres : écoute locale, limite de clients,
  limite de ligne, délai d’arrêt et délai de connexion initiale.
- TLS protège le transport mais n’est pas une authentification client. Ne pas
  présenter cette passerelle comme sécurisée pour Internet sans contrôle
  d’identité en amont.
- Ne pas logger les commandes ou données de jeu si cela peut exposer des
  informations client ; stderr moteur est déjà traité comme donnée non fiable.

## Vérification

Avant de livrer une modification :

```sh
gofmt -w <fichiers-go-modifiés>
GOCACHE=/tmp/chessgateway-gocache go test ./...
GOCACHE=/tmp/chessgateway-gocache go test -race ./...
go vet ./...
```

Ajouter un test quand une nouvelle trame, une transition de session, une limite
ou un chemin concurrent est introduit. Préserver les changements existants du
dépôt et ne pas utiliser de commande destructive sans demande explicite.
