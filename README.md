# dockops

dockops ist ein deklarativer Docker-Compose-Stack-Manager für einen einzelnen
Docker-Host: ein Host-Git-Repo (`stacks.yaml` + lokale `stack.yaml`s)
beschreibt, welche Stacks laufen sollen — inklusive Importen von geteilten
Stack-Definitionen aus anderen Git-Remotes. Ein Agent-Reconcile-Durchlauf
bringt die Realität idempotent in Einklang: Repos aktualisieren, Secrets aus
SOPS-Dateien und HashiCorp Vault auflösen, Hooks ausführen und per
`docker compose up -d --remove-orphans` deployen. Ausgelöst wird er per
Poll-Timer, Webhook (GitHub/GitLab) oder einmalig mit `-once`.

## Architektur

```
   ┌───────────────────────────┐      ┌───────────────────────────┐
   │         Host-Repo         │      │       Shared-Remotes      │
   │ stacks.yaml (Root-Config) │      │ docker-stacks.git, …      │
   │ web/stack.yaml            │      │ monitoring/…  immich/…    │
   │ media/immich/stack.yaml   │      │                           │
   └─────────────┬─────────────┘      └─────────────┬─────────────┘
                 │  git clone / fetch / reset     │
                 └───────────────┬────────────────┘
                                 ▼
                 ┌───────────────────────────────┐
                 │         dockops-Agent         │
                 │  Reconcile-Pass (idempotent): │
                 │  git → config → hash → state  │
                 │  → secrets → hooks → compose  │
                 └───────────────┬───────────────┘
                                 │ docker compose (CLI)
                                 │ über /var/run/docker.sock
                                 ▼
                       ┌──────────────────┐
                       │   Docker-Daemon  │
                       └──────────────────┘
```

- **Host-Repo**: die Wurzel. Enthält die Root-`stacks.yaml` und lokale
  Stacks, die über Include-Globs entdeckt werden. Auschecken nach
  `<work_dir>/host`.
- **Shared-Remotes**: beliebige weitere Git-Repos (z. B. eine Bibliothek
  wiederverwendbarer Stack-Definitionen). Auschecken nach
  `<work_dir>/<remote-name>`.
- **Imports**: Referenzieren einen Stack aus einem Shared-Remote und mergen
  Overrides darüber (eigener Name, Profile, Secrets, Hooks — oder nur eine
  Teilmenge der Container).
- Der Agent selbst ist ein einzelnes statisches Go-Binary, das `git` und
  `docker compose` als CLI-Aufrufe nutzt, SOPS-Dateien lokal per `sops`-CLI
  oder über einen Sidecar-HTTP-Dienst entschlüsselt (siehe „SOPS
  decryption“) und Vault per HTTP (KV v2) liest.

## Root-`stacks.yaml`

```yaml
global:
  # Wo Host- und Remote-Checkouts liegen
  # (Default: /var/lib/dockops/repos; relative Angaben: relativ zum Host-Repo-Root)
  work_dir: /var/lib/dockops/repos
  # Poll-Intervall des Daemon-Modus (Default: 5m)
  poll_interval: 5m
  # Stacks, die aus der Config verschwinden, mit `compose down` entfernen
  # (Default: false — nie ohne explizites Opt-in löschen)
  prune_removed_stacks: true
  # Welche Dateien als lokale Stack-Definitionen entdeckt werden
  # (Defaults)
  include:
    - "**/stack.yaml"
    - "**/*.stack.yaml"
  # Auto-Detection der Compose-Datei im Stack-Verzeichnis (Defaults)
  compose_file_patterns:
    - compose.yaml
    - compose.yml
    - compose.docker-compose.yaml
    - docker-compose.yml
  vault:
    # Leer gelassen → Fallback auf die Env-Variable VAULT_ADDR
    address: https://vault.example.com
    mount_path: secret        # KV-v2-Mount (Default: secret)
    auth_method: token        # token | approle (Default: token)
    token_from_env: VAULT_TOKEN          # Default
    role_id_from_env: VAULT_ROLE_ID      # Default (approle)
    secret_id_from_env: VAULT_SECRET_ID  # Default (approle)

remotes:
  shared:
    url: git@github.com:example/docker-stacks.git
    branch: main              # Default

imports:
  # Variante 1: kompletter Stack-Import — alle Services, eigener Name,
  # zusätzliche env_files und Hooks werden an die Basis-Definition
  # angehängt (base zuerst, dann Import).
  - remote: shared
    path: monitoring/prometheus
    name: prometheus
    env_files:
      - /srv/monitoring/prometheus.env   # absolute Pfade gehen durch, relative: im Remote-Checkout
    hooks:
      post_deploy:
        - echo prometheus deployed

  # Variante 2: Container-Subset-Import — `services` ersetzt die
  # Service-Liste der Basis-Definition; hier läuft immich ohne ML-Worker.
  - remote: shared
    path: immich
    name: immich
    services:
      - immich-server
      - immich-postgres
      - immich-redis
```

Merge-Regeln für Imports: `profiles`, `env_files`, `sops_files`,
`vault_paths` und `hooks.*` werden angehängt (base zuerst, Dubletten
entfallen); `services` ersetzt die Basis-Liste, wenn gesetzt (leer =
alle); `name` überschreibt den Basis-Namen.

## Lokale `stack.yaml` (Beispiel immich)

Liegt im Stack-Verzeichnis neben der Compose-Datei; `name` fällt auf den
Verzeichnisnamen zurück, `enabled` ist per Default `true`, `compose_file`
wird über `compose_file_patterns` auto-erkannt.

```yaml
# media/immich/stack.yaml
name: immich
enabled: true

# Nur diese Services starten (positionsargumente hinter `up -d`)
services:
  - immich-server
  - immich-postgres
  - immich-redis
  - immich-machine-learning

# Zusätzliche Compose-Profile (docker compose --profile)
profiles:
  - full

# Klartext-Env-Dateien (relative Pfade: Stack-Verzeichnis)
env_files:
  - .env

# SOPS-verschlüsselte dotenv-Dateien; age_keyfile optional und relativ
# zum Stack-Verzeichnis (wird als SOPS_AGE_KEY_FILE an den sops-Prozess
# gereicht, nie an die dockops-Prozessumgebung)
sops_files:
  - path: secrets.env.sops
    age_keyfile: keys/age.key

# Vault KV-v2-Pfade unterhalb von global.vault.mount_path
vault_paths:
  - homelab/immich

# Vault-Templates: dotenv-Dateien mit ${VAULT:<path>#<key>}-Referenzen
# (Kompatibilität zu bestehenden vault.env-Files); gleiche Prioritätsstufe
# wie vault_paths, Pfad relativ zum Stack-Verzeichnis
vault_env_templates:
  - vault.env

hooks:
  pre_deploy:
    - mkdir -p /srv/immich/library /srv/immich/upload
  post_deploy:
    - echo "immich deployed from $GIT_COMMIT"
```

Dazu passend `media/immich/compose.yaml` (Auszug):

```yaml
services:
  immich-server:
    image: ghcr.io/immich-app/immich-server:${IMMICH_VERSION}
    volumes:
      - /srv/immich/library:/usr/src/app/upload
    environment:
      DB_PASSWORD: ${DB_PASSWORD}       # aus sops/vault interpoliert
      IMMICH_MACHINE_LEARNING_ENABLED: "true"
  immich-postgres:
    image: tensorchord/pgvecto-rs:pg16-v0.2.1
    environment:
      POSTGRES_PASSWORD: ${DB_PASSWORD}
  # …
```

Dazu passend `media/immich/vault.env` (das Template):

```dotenv
DB_PASSWORD=${VAULT:hosts/host-01/traefik#db_password}
IMMICH_API_KEY=${VAULT:hosts/host-01/traefik:api_key}
```

`${VAULT:<path>#<key>}` (alternativ `:` statt `#`) löst `key` aus dem KV-v2-
Secret `path` unter `global.vault.mount_path` auf — `path` ist derselbe
Pfad wie in `vault_paths`. Alles andere in der Zeile bleibt literal, auch
mehrere Referenzen pro Zeile; die KEY-Namen links vom `=` stehen im
Template. Fehlt der Pfad oder Key in Vault, scheitert der Deploy mit
Datei- und Referenz-Kontext. Bei mehreren Templates gewinnt das spätere in
Listenreihenfolge.

Hooks laufen als `sh -c` im Stack-Verzeichnis, sequenziell mit
Per-Kommando-Timeout (Default 5 Minuten), und erhalten die komplette
gemergte Umgebung des Stacks (inkl. Secrets) sowie `STACK_NAME`,
`STACK_DIR` und `GIT_COMMIT`.

## Ablauf eines Reconcile-Durchlaufs

1. **Host-Repo aktualisieren**: clone-or-update von `-repo`/`-branch` nach
   `<work_dir>/host` — bei vorhandenem Checkout werden lokale Änderungen
   zuerst als WIP-Stash gesichert (siehe [WIP-Auto-Stash](#wip-auto-stash)),
   dann `git fetch` + `reset --hard` + `clean -fd` (der Checkout ist
   maschinell verwalteter Zustand).
2. **Root-Config lesen** (`stacks.yaml` im Host-Repo) und alle
   **Shared-Remotes** nach `<work_dir>/<remote>` clone-odern/updaten.
3. **Komplette Config laden**: lokale Stacks via Include-Globs entdecken,
   Imports laden und mergen, Validierung (u. a. doppelne Stack-Namen).
4. **Pro aktivem Stack deployen**, wenn sich sein deterministischer Hash
   (Stack-Definition + normalisierte Compose-Datei) gegen den State geändert
   hat oder der letzte Versuch fehlschlug:
   1. Hash berechnen und gegen State vergleichen — unverändert und
      deployed ⇒ überspringen.
   2. Vault-Client bauen und einmal pro Durchlauf einloggen (nur wenn
      überhaupt `vault_paths` existieren).
   3. Umgebung mergen und als 0600-Env-Datei nach `/dev/shm` schreiben.
   4. `pre_deploy`-Hooks ausführen (Fehler ⇒ Stack wird nicht deployed,
      Fehler landet im Durchlauf-Ergebnis).
   5. `docker compose -p <name> -f <compose> [--profile p …]
      --env-file <envfile> up -d --remove-orphans [services …]` ausführen.
   6. `post_deploy`-Hooks ausführen (Fehler ⇒ weiterhin als deployed
      gezählt, Fehler landet trotzdem im Durchlauf-Ergebnis).
   7. State (Hash, Status) setzen; Env-Datei wird überschrieben und
      gelöscht.
5. **Prune** (`prune_removed_stacks: true`): Stacks, die nicht mehr in der
   Config existieren, abräumen — per `down --remove-orphans` mit `-f
   <compose>`, solange die Compose-Datei noch existiert, sonst als Fallback
   ohne `-f` allein über den Projektnamen
   (`docker compose -p <name> down --remove-orphans`).
6. **State atomar persistieren**; alle aufgetretenen Fehler werden
   gesammelt und als ein Fehler zurückgegeben (`errors.Join`) — ein
   kaputter Stack verhindert die Reconciliation der anderen nie.

## WIP-Auto-Stash

Nutzer editieren gelegentlich direkt auf dem Host im Checkout des Host-Repos
(z.B. `/var/lib/dockops/repos/host`). Der deterministische Update-Pfad
(`fetch` + `reset --hard` + `clean -fd`) würde diese Änderungen
kommentarlos vernichten — deshalb legt dockops sie vorher auf das
Stash-Regal. Das gilt für jeden von dockops verwalteten Checkout (Host-Repo
und Shared-Remotes):

- Vor dem Update prüft `EnsureRepo` den Worktree per `git status --porcelain`.
- Ist er dirty (modifiziert ODER untracked), läuft vorher
  `git stash push --include-untracked -m "dockops-wip: <RFC3339-Timestamp>"`
  plus ein INFO-Log mit Hinweis auf die Wiederherstellung.
- Der Stash wird bewusst **nie automatisch angewendet** — `git stash pop` ist
  Handarbeit: Merge-Konflikte dürfen einen Deploy niemals blockieren.
- Schlägt das Stash fehl, gibt es nur ein WARN-Log und es wird wie bisher
  zurückgesetzt (gleiches Verhalten wie vor der Absicherung).

Wiederherstellen der Änderungen:

    cd <work_dir>/<repo>
    git stash list   # Einträge "dockops-wip: <Timestamp>" suchen
    git stash pop    # oder drop, wenn die Änderungen verworfen werden sollen

Hinweis: Die von dockops generierten `<stackDir>/.env`-Dateien sind untracked
und landen daher ebenfalls im Stash, bevor sie der Reset entfernt. Das ist
harmlos — die `.env` wird vor jedem Deploy neu geschrieben —, bedeutet aber,
dass pro dirty Durchlauf ein Stash-Eintrag entsteht. Nicht benötigte Einträge
lassen sich per `git stash drop` / `git stash clear` aufräumen.

## Secret-Auflösung & Priorität

Die Umgebung jedes Stacks entsteht durch Merge mit fester Priorität
(niedrig → hoch; innerhalb einer Quelle gewinnen spätere Einträge, bei
Imports gilt Basis vor Import):

| Priorität | Quelle |
|---|---|
| 1 | `env_files` (Klartext-dotenv) |
| 2 | `sops_files` (via `sops -d --input-type dotenv`, lokal oder Sidecar) |
| 3 | `vault_paths` (KV v2, `GET /v1/<mount>/data/<path>`) und
    `vault_env_templates` (gleich danach angewendet; bei Kollision gewinnt
    das Template) |
| 4 | Systemvariablen: `STACK_NAME`, `STACK_DIR`, `GIT_COMMIT` — immer
    zuletzt, von keinem Secret überschreibbar |

Das Ergebnis geht als `--env-file` an `docker compose` (Interpolation in
der Compose-Datei) und als Umgebung an Hooks. Die Datei liegt in
`/dev/shm` (Tmpfs) mit `0600`, wird nach dem Stack-Durchlauf mit Zufallsbytes
überschrieben und gelöscht; Secrets erscheinen nie in Logs oder
Fehlermeldungen.

## SOPS decryption

dockops kennt zwei Modi, `sops_files` zu entschlüsseln:

### Modus 1: lokales `sops`-CLI (Default)

dockops führt `sops -d --input-type dotenv --output-type dotenv <path>` selbst
aus. Voraussetzung: `sops`-Binary, `.sops.yaml`-Regeln und die Credentials
(age-Keyfiles via `SOPS_AGE_KEY_FILE` an den Child-Prozess, Vault-Transit via
`VAULT_ADDR`/`VAULT_TOKEN`) liegen im dockops-Container. Passend für
Einzel-Host-Betrieb und Tests (`-once` mit Fake-Binaries auf dem PATH).

### Modus 2: SOPS-Sidecar (Fleet-Realität)

SOPS-Binary und Credentials kommen NICHT ins dockops-Image. Stattdessen
läuft `cmd/sops-sidecar` als eigener Container im selben Compose-Netz,
teilt sich die Stack-Checkouts per Read-only-Volume und hält `sops` +
`.sops.yaml` + Vault-Transit-Zugang. dockops bekommt die URL über
`-sops-sidecar`/`DOCKOPS_SOPS_SIDECAR_URL` (z. B.
`http://sops-sidecar:8081`) und löst damit ALLE `sops_files` auf.

HTTP-Vertrag (`POST /decrypt`):

    Anfrage:   {"path": "/abs/path/file.env"}
    Antwort:   200 {"content": "<dotenv-klartext>"}
           oder 4xx/5xx {"error": "..."} — 400 malformed Body/Pfad,
             403 Pfad außerhalb der erlaubten Wurzeln, 500 Decrypt-Fehler

    GET /healthz → 200 {"status":"ok"}

Sicherheit: keine Auth (Netz-Isolation — der Sidecar veröffentlicht keinen
Port, nur dockops im gemeinsamen Netz erreicht ihn), dafür Client-seitig
ein 60-s-Request-Timeout und ein 10-MB-Response-Limit sowie Server-seitig
die Pfad-Allow-List `SOPS_SIDECAR_ALLOWED_ROOTS` (doppelpunkt-separiert;
`filepath.Clean` + Prefix-Check blockt `..`-Traversal, bevor `sops` je
läuft). `age_keyfile` ist im Sidecar-Modus nicht transportierbar und wird
als Fehler gemeldet — age-Keys gehören in den Sidecar-Container, per
`SOPS_AGE_KEY_FILE` dort.

Deployment im eigenen `compose.yaml` des Repos (siehe auch
„Docker-Deployment“): der Sidecar-Service trägt das Profil `sops` und muss
explizit aktiviert werden, inkl. der URL-Variable:

    DOCKOPS_SOPS_SIDECAR_URL=http://sops-sidecar:8081 \
      docker compose --profile sops up -d

(Build des Images: `docker compose --profile sops build`.)

## CLI

    dockops [-repo URL] [-branch NAME] [-state PFAD] [-addr HOST:PORT]
            [-webhook-secret GEHEIMNIS] [-sops-sidecar URL]
            [-interval DAUER] [-once]

| Flag | Env-Variable | Default | Bedeutung |
|---|---|---|---|
| `-repo` | `DOCKOPS_REPO_URL` | — (Pflicht) | URL des Host-Repos (HTTPS/SSH/lokal) |
| `-branch` | `DOCKOPS_BRANCH` | `main` | Branch des Host-Repos |
| `-state` | — | `/var/lib/dockops/state.json` | Pfad der JSON-State-Datei |
| `-webhook-secret` | `DOCKOPS_WEBHOOK_SECRET` | leer | Shared Secret für `POST /webhook`; leer ⇒ Webhooks ohne Auth (Warnung im Log) |
| `-addr` | — | `:8080` | Listen-Adresse des Webhook-Servers |
| `-sops-sidecar` | `DOCKOPS_SOPS_SIDECAR_URL` | leer | URL des SOPS-Sidecar (z. B. `http://sops-sidecar:8081`); leer ⇒ lokales `sops -d` |
| `-interval` | — | `global.poll_interval` (Default 5m) | Poll-Intervall-Override, z. B. `90s` |
| `-once` | — | `false` | Ein einzelner Reconcile-Durchlauf, dann Exit |

Dazu die Vault-Variablen (Namen in `global.vault` konfigurierbar):
`VAULT_ADDR` (Fallback für `global.vault.address`), `VAULT_TOKEN`
(`auth_method: token`) bzw. `VAULT_ROLE_ID` + `VAULT_SECRET_ID`
(`auth_method: approle`).

**Betriebsmodi**: Ohne `-once` läuft dockops als Daemon — sofortiger erster
Durchlauf, danach Poll-Timer (`-interval` überschreibt
`global.poll_interval`) plus Webhook-Trigger, die entprellt und gemerged
werden; ein laufender Durchlauf wird nie unterbrochen. SIGINT/SIGTERM
beendet ihn kontrolliert.

**HTTP-Endpunkte**:

- `POST /webhook` — authentifiziert via GitHub
  (`X-Hub-Signature-256: sha256=<hex>` HMAC-SHA256 über den Body) **oder**
  GitLab (`X-Gitlab-Token`), beides in konstanter Zeit verglichen:

      curl -X POST http://localhost:8080/webhook \
        -H "X-Gitlab-Token: $DOCKOPS_WEBHOOK_SECRET"

- `GET /healthz` — Liveness.

Logs gehen als `slog`-JSON auf stdout.

## Docker-Deployment

`compose.yaml` im Repo-Root bringt den Agent als Container auf dem Host
zum Laufen, den er verwaltet (`docker compose build && docker compose up -d`):

- **Socket-Mount** `/var/run/docker.sock:/var/run/docker.sock` — dockops
  ruft `docker compose` als CLI auf; im Container läuft die CLI, der Daemon
  ist der des Hosts. Deployte Stacks erscheinen auf dem Host als normale
  Compose-Projekte (`-p <stack-name>`).
- **SSH-Key** `~/.ssh/id_ed25519:/root/.ssh/id_ed25519:ro` (plus
  `known_hosts`, read-only) — Authentifizierung für Git-Remotes per SSH;
  `GIT_SSH_COMMAND` im Beispiel pinnt Key und Host-Key-Prüfung. HTTPS-Tokens
  können stattdessen direkt in der Remote-URL stehen.
- **Env** `DOCKOPS_REPO_URL`, `DOCKOPS_BRANCH`, `DOCKOPS_WEBHOOK_SECRET`
  sowie `VAULT_ADDR`/`VAULT_TOKEN` (bzw. `VAULT_ROLE_ID`/`VAULT_SECRET_ID`).
  `DOCKOPS_WEBHOOK_SECRET` und `VAULT_TOKEN` werden beim `up` aus der Shell
  bzw. einer `.env`-Datei neben der `compose.yaml` substituiert.
- **Volume** `dockops-data:/var/lib/dockops` — überlebt Container-Neubau:
  `state.json` (flag `-state`) und die Checkouts unter
  `/var/lib/dockops/repos` (Default `global.work_dir`).
- **Port** `8080:8080` für Webhooks und `/healthz`.
- **SOPS-Sidecar** (optional, Profil `sops`): `sops-sidecar` baut aus
  `cmd/sops-sidecar/Dockerfile.sidecar` (alpine + sops + CA-Certs, mehr
  nicht) und mountet `dockops-data` read-only — die Checkouts sind lesbar,
  Decrypt schreibt nie. Er veröffentlicht keinen Port; dockops erreicht ihn
  über das gemeinsame Compose-Netz. Aktivieren mitsamt der URL-Variable:

      DOCKOPS_SOPS_SIDECAR_URL=http://sops-sidecar:8081 \
        docker compose --profile sops up -d

  Ohne Profil und Variable bleibt der lokale `sops`-Modus aktiv — age-
  Keyfiles für `sops_files[].age_keyfile` müssen dann im dockops-Container
  erreichbar sein (analog zum SSH-Key mounten und in der Stack-Definition
  auf den Container-Pfad zeigen lassen). Details: „SOPS decryption“.

Einmal-Alternativen: `command: ["-once"]` in der `compose.yaml` oder
`docker compose run --rm dockops -once`.

## Development

    export PATH=$HOME/.local/go/bin:$PATH   # Go 1.25
    go build ./... && go vet ./... && go test ./... -count=1

Der E2E-Test (`internal/reconciler/e2e_test.go`) fährt vollständige
Reconcile-Durchläufe gegen echte Git-Repos auf `t.TempDir()`;
`docker`/`sops` sind Fake-Binaries auf dem PATH, Vault ein
`httptest.Server`. Benötigt werden nur `git` und `sh` auf der Maschine.
Der SOPS-Sidecar ist ein eigenes Binary (`go build ./cmd/sops-sidecar`,
Image via `cmd/sops-sidecar/Dockerfile.sidecar`).
Externe Abhängigkeit ist allein `gopkg.in/yaml.v3`.
