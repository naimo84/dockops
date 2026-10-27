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
  `<work_dir>/host` — oder in-place am konfigurierten
  `global.host_checkout` (siehe „Konfigurierbare Checkout-Pfade“).
- **Shared-Remotes**: beliebige weitere Git-Repos (z. B. eine Bibliothek
  wiederverwendbarer Stack-Definitionen). Auschecken nach
  `<work_dir>/<remote-name>` — oder in-place am konfigurierten
  `remotes.<name>.checkout`.
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
  # Absoluter Pfad des Host-Repo-Checkouts (Default: <work_dir>/host;
  # Env-Fallback DOCKOPS_HOST_CHECKOUT, ein YAML-Wert gewinnt) —
  # siehe „Konfigurierbare Checkout-Pfade“
  # host_checkout: /home/docker/apps
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
    # Absoluter Pfad dieses Remote-Checkouts (Default: <work_dir>/<name>)
    # checkout: /home/docker/infrastructure

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

## Konfigurierbare Checkout-Pfade

Auf Fleet-Hosts existieren die „echten“ Clones oft schon an festen Pfaden
(`/home/docker/apps` = Host-Repo, `/home/docker/infrastructure` =
Shared-Remote). Statt sie per Bind-Mount-Gymnastik unter den Default zu
zwingen, konfiguriert dockops die Pfade direkt in der Root-`stacks.yaml`
und arbeitet **in place** auf den bestehenden Clones (Adopt-or-Clone):

```yaml
global:
  work_dir: /var/lib/dockops/repos
  # Absoluter Pfad des Host-Repo-Checkouts (Default: <work_dir>/host).
  # Env-Fallback: DOCKOPS_HOST_CHECKOUT — ein YAML-Wert gewinnt.
  host_checkout: /home/docker/apps

remotes:
  infrastructure:
    url: git@github.com:example/infrastructure.git
    branch: main
    # Absoluter Pfad dieses Remote-Checkouts (Default: <work_dir>/<name>)
    checkout: /home/docker/infrastructure
```

Regeln:

- Beide Felder müssen, wenn gesetzt, absolut sein und dürfen nur die
  Zeichen `A-Z a-z 0-9 . _ - /` enthalten (kein Whitespace, keine Quotes,
  keine Shell-Metazeichen); Validierungsfehler nennen Datei und Feld.
- **Adopt-or-Clone**: Existiert am Pfad bereits ein Checkout mit zum
  konfigurierten `url` passendem `origin`, wird er übernommen und wie jeder
  dockops-Checkout deterministisch aktualisiert (WIP-Stash → `fetch` →
  `reset --hard` → `clean -fd`, siehe [WIP-Auto-Stash](#wip-auto-stash)).
  **Der `origin` muss zur konfigurierten URL passen** — ein abweichender
  Origin ist ein Fehler statt eines stillen Fetch von der alten Quelle.
  Ein fremdes, nicht-leeres Verzeichnis ohne `.git` wird ebenso verweigert
  (nie Daten überschreiben, die dockops nicht selbst angelegt hat); fehlt
  der Pfad, wird ganz normal geklont.
- Henne-Ei beim Host-Repo: die Root-`stacks.yaml` liegt im Host-Repo und
  muss erst unter dem Bootstrap-Pfad `<work_dir>/host` lesbar sein, bevor
  irgendein konfigurierter Pfad greifen kann. `host_checkout` wird deshalb
  erst **nach** dem Config-Load angewendet: der Bootstrap-Checkout bleibt
  bestehen und ist dann nur noch Träger der Root-Config; die volle Config
  (lokale Stacks inklusive) liest dockops aus dem konfigurierten Checkout.
- Imports lösen entsprechend im konfigurierten Remote-Checkout auf
  (`<checkout>/<path>` statt `<work_dir>/<name>/<path>`) — exakt dort, wo
  der Reconciler das Remote klont bzw. adoptiert.
- Das Hash-Schema bleibt unberührt (v3): Ein Checkout-Umzug ändert zwar die
  absoluten Compose-Dateipfade im Stack-Hash und löst damit **einmalig**
  Re-Deploys der betroffenen Stacks aus — danach ist der Zustand wieder
  stabil.

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
   maschinell verwalteter Zustand). Dies ist der Bootstrap; ist
   `global.host_checkout` gesetzt, übernimmt dockops danach den konfigurierten
   Checkout in-place (Adopt-or-Clone, siehe
   [Konfigurierbare Checkout-Pfade](#konfigurierbare-checkout-pfade)) und
   liest die weitere Config von dort.
2. **Root-Config lesen** (`stacks.yaml` im Host-Repo) und alle
   **Shared-Remotes** nach `<work_dir>/<remote>` bzw. dem konfigurierten
   `remotes.<remote>.checkout` clone-odern/updaten.
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
6. **Image-Update-Prüfung** (Phase 10, optional): für fällige Stacks
   (`update_check_interval`, `update_policy != pinned`) lokale gegen
   Remote-Digests vergleichen und im State ablegen — deployt **nichts**,
   siehe [Image-Update-Erkennung](#image-update-erkennung).
7. **Image-Update anwenden** (Phase 11, nur `update_policy: apply`): im
   selben fälligen Pass Pull → `up --no-deps` der betroffenen Services →
   Health-Gate → bei Fehlschlag Rollback per Digest-Override, siehe
   [Image-Updates anwenden](#image-updates-anwenden-phase-11-update_policy-apply).
8. **State atomar persistieren**; alle aufgetretenen Fehler werden
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

## Monitoring (`/status` Endpoint)

`GET /status` (Daemon-Modus) stellt den Ist-Zustand der verwalteten Stacks
dem Soll-Zustand aus `state.json` gegenüber — Grundlage für externes
Monitoring (Gatus, curl, Telegram-Bot), ohne dass der Abfrager selbst
Docker-Zugriff bräuchte:

    curl http://localhost:8080/status

```json
{
  "generated_at": "2026-10-15T09:32:11Z",
  "host_repo": {"url": "git@github.com:example/homelab-stacks.git", "branch": "main", "commit": "abc123def456"},
  "stacks": [
    {
      "name": "immich",
      "deployed_hash": "abc123…",
      "last_deployed": "2026-10-14T12:00:00Z",
      "containers": [
        {"name": "immich-server", "state": "running", "health": "healthy"}
      ],
      "drift": "ok"
    }
  ],
  "unmanaged": [
    {"project": "legacy-thing", "containers": ["legacy-1"]}
  ]
}
```

**Drift pro Stack** (Container via `docker compose -p <stack> ps --all`):

| drift | Bedeutung |
|---|---|
| `ok` | min. 1 Container `running`, keiner `exited` |
| `stopped` | Stack im State, aber 0 Container laufen (alles exited/missing) |
| `partial` | Container laufen, aber mind. 1 ist `exited` |
| `unknown` | `compose ps` schlug fehl → `"error": "<msg>"` im Stack-Objekt |

Schlägt das `ps` eines Stacks fehl, bleibt der Endpoint trotzdem **200** und
listet den Stack mit `drift: "unknown"` — Monitoring soll an einem kaputten
Stack nicht komplett scheitern. (Da der State bisher keine Soll-Service-Liste
führt, läuft der partial-Vergleich in der Fallback-Form: running > 0 und
kein exited-Container ⇒ `ok`.) `stacks` und `unmanaged` sind immer JSON-
Arrays (auch leer), beide sortiert.

**Unmanaged**: laufende Compose-Projekte (`docker ps`, Label
`com.docker.compose.project`) ohne State-Eintrag. Container ohne
Compose-Label zählen nicht (Buildkit & Co.). Das dockops-eigene Projekt wird
nie als unmanaged gemeldet — default `dockops` (der Projektname beim
Deployment aus der `compose.yaml` dieses Repos), über `DOCKOPS_SELF_PROJECT`
anpassbar, falls der Agent unter einem anderen Projektnamen läuft.

**Optionaler Schutz**: Ist `DOCKOPS_STATUS_TOKEN` gesetzt, verlangt der
Endpoint `Authorization: Bearer <token>` (konstanter Zeitvergleich), sonst
**401**. Ohne die Variable ist er offen wie `/healthz` — dann sollte der Port
nicht öffentlich erreichbar sein.

### Ohne Server: `-print-status`

Dasselbe Dokument (gemeinsame `BuildStatus`-Funktion, keine zweite Logik)
erzeugt der Agent auch als One-Shot auf stdout — für Cron-Monitoring und
Umgebungen ohne freigegebenen Port:

    dockops -print-status -state /var/lib/dockops/state.json

Der Aufruf braucht **kein `-repo`** und startet weder Reconcile-Pass noch
Server (`-print-status` gewinnt über `-once`/Daemon-Modus). Die Ausgabe ist
mit 2 Spaces eingerückt, Diagnose-Logs gehen auf stderr — stdout lässt sich
also direkt weiterpipen:

    dockops -print-status | jq '.stacks[] | select(.drift != "ok")'

Auszug der Ausgabe:

```json
{
  "generated_at": "2026-10-15T09:32:11Z",
  "host_repo": {
    "url": "git@github.com:example/homelab-stacks.git",
    "branch": "main",
    "commit": "abc123def456"
  },
  "stacks": [
    {
      "name": "immich",
      "deployed_hash": "abc123…",
      "last_deployed": "2026-10-14T12:00:00Z",
      "containers": [
        {
          "name": "immich-server",
          "state": "running",
          "health": "healthy"
        }
      ],
      "drift": "ok"
    }
  ],
  "unmanaged": []
}
```

Fehler-Toleranz wie beim Endpoint: Docker-Probleme landen im Dokument
(drift `unknown` plus `"error"` pro Stack, top-level `"error"` für die
Projekt-Listung) und der Aufruf endet mit **exit 0**. Nur eine nicht
lesbare State-Datei schlägt fehl — Fehler auf stderr, **exit 1**. Eine
fehlende State-Datei gilt wie beim Endpoint als leerer Snapshot.

## Log-API (`/logs*`)

Drei Endpunkte, die Container-Logs für **Maschinen** lesbar machen (Agent,
`curl`, `fleet-ssh`) — bewusst ohne UI: wer Logs ansieht, hat ohnehin
Shell-Zugriff. Sie ersetzen einen zentralen Log-Viewer wie Dozzle, weil jeder
Agent die Logs seines eigenen Hosts liefert und der Aufrufer über Hosts hinweg
merged.

| Endpoint | Zweck |
|---|---|
| `GET /logs/containers` | Inventar dieses Hosts: `container`, `project`, `service`, `image`, `state`, `health`, `created` |
| `GET /logs?container=…` bzw. `?project=…` | Snapshot, begrenzt |
| `GET /logs/stream?container=…` | Live-Tail als Server-Sent Events |

**Zugriff ist fail-closed**: `DOCKOPS_LOGS_TOKEN` ist Pflicht. Ohne die
Variable antworten alle `/logs*`-Pfade mit **503**, mit falschem oder
fehlendem `Authorization: Bearer <token>` mit **401** (konstanter
Zeitvergleich). Log-Zeilen enthalten regelmäßig Secrets — ein optionaler
Schutz wie bei `/status` wäre hier die falsche Voreinstellung. Der Token wird
nie geloggt, ebenso wenig der Query-String (ein `grep`-Muster kann ein Secret
enthalten).

```bash
TOKEN=$(cat /etc/dockops/logs-token)
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/logs/containers"

# letzte 200 Zeilen der letzten Stunde, nur Fehler
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/logs?container=n8n-1&since=1h&tail=200&grep=ERROR"

# dasselbe als NDJSON (Zeitstempel und Container als Felder)
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/logs?project=n8n&since=30m&format=json"

# live mittailen
curl -N -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/logs/stream?container=n8n-1&grep=error&regex=1"
```

**Query-Parameter** (`container` und `project` schließen sich aus, einer ist
Pflicht; Namen müssen `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$` erfüllen — ein
führendes `-` würde `docker logs` sonst als Flag lesen):

| Parameter | Default | Grenze |
|---|---|---|
| `since` | `1h` | max. `168h`; RFC3339 oder Dauer (`90m`, `2h30m`, `7d`) |
| `tail` | `200` (Stream: `0`) | max. `5000`; `0` nur im Stream (= nur neue Zeilen) |
| `grep` | — | max. 256 Zeichen, literal; mit `regex=1` RE2 |
| `regex` | `0` | `0`/`1`, verlangt `grep` |
| `format` | `text` | `text` (Rohzeilen) oder `json` (NDJSON `{"t":…,"c":…,"m":…}`) |

Im `project`-Modus läuft `docker compose -p <projekt> logs` — Compose merged
und präfixt selbst, deshalb bleibt `c` dort leer. Antwort-Header eines
Snapshots: `X-Dockops-Lines` (gelieferte Zeilen) und `X-Dockops-Truncated:
true`, wenn die Byte-Grenze gegriffen hat.

**Harte Limits** (Konstanten in `internal/logs`, nicht konfigurierbar — es
gibt genau eine Art Aufrufer):

- Snapshot: 2 MiB, max. 5000 Zeilen
- Stream: 10 MiB, max. 15 min, max. 8 parallel pro Host (sonst **429**)
- pro Zeile: 64 KiB; eine längere Zeile wird gekürzt und der Rest verworfen
  (ein crashender Container kann megabyte-große Zeilen ohne Newline schreiben)

**SSE-Format**: Events `log` (eine Zeile), `ping` (alle 15 s Keepalive) und
`end` mit `{"reason":"docker_exit"}` bzw. `{"reason":"limit"}`. Ein
Client-Disconnect bricht den `docker logs --follow`-Prozess ab (kein Zombie
überlebt den Request).

**Fehler-Mapping**: fehlender Container/Projekt **404**, nicht lesbarer
Log-Treiber **422**, Docker-Fehler **502**, Client-Abbruch **504**, ungültige
Parameter **400**.

**Einschränkung**: Der `journald`-Log-Treiber ist über `docker logs` nicht
lesbar (→ 422). Praktisch irrelevant: alle Fleet-Hosts fahren das
Daemon-Default `json-file` (im Playbook `core/docker` gepinnt, am 25.10.2026
auf hvaler, netcup2, bifrost und omv verifiziert), und keine Compose-Datei
setzt den Treiber.

## Benachrichtigungen (`notifications`)

Ein fehlgeschlagener Deploy steht heute in `GET /status` und im Log — gesehen
wird er erst, wenn jemand nachsieht. Der optionale Root-Block
`notifications` schickt Reconcile-Fehler an einen oder mehrere Kanäle:

```yaml
notifications:
  enabled: true            # default true; false = still (Wartungsfenster)
  on: [deploy_failed, prune_failed, repo_failed, update_failed]  # default
  repeat_after: 1h         # Dedupe-Fenster je (kind, stack); default 1h
  timeout: 10s             # pro Destination; default 10s
  destinations:
    - type: webhook
      url: https://example.invalid/hook
      headers: {X-Source: dockops}       # optional
    - type: ntfy
      url: https://ntfy.example.invalid/dockops
      priority: high                      # optional -> Header "Priority"
      tags: [warning, docker]             # optional -> Header "Tags"
    - type: telegram
      token: ${TELEGRAM_BOT_TOKEN}
      chat_id: "123456789"
    - type: discord
      webhook_url: https://discord.com/api/webhooks/...
    - type: slack
      webhook_url: https://hooks.slack.com/services/...
```

Ohne den Block ist das Feature aus (kein Fehler, kein Log-Spam). Ein
vorhandener Block wird **immer** geladen und validiert — auch mit
`enabled: false`, damit ein kaputter Kanal beim Konfigurieren auffällt und
nicht erst am Ende des Wartungsfensters. Fehlende Pflichtfelder, ein
unbekannter `type` oder ein unbekannter `on`-Eintrag sind Ladefehler.

### Ereignisse

| `kind` | Wann | `stack` | `stage` |
| --- | --- | --- | --- |
| `deploy_failed` | Deploy eines Stacks scheitert | Stackname | `secrets`, `pre_deploy`, `compose_up`, `post_deploy` |
| `prune_failed` | Teardown eines entfernten Stacks scheitert | Stackname | leer |
| `repo_failed` | Host-Repo, ein Remote-Checkout oder das Laden der Config scheitert (höchstens einmal pro Pass) | leer (siehe `remote`) | `repo`, `config` |

`secrets` umfasst die ganze Secret-Kette: Vault-Login, Env-Merge,
`<stackDir>/.env` und die `/dev/shm`-Env-Datei.

### Dedupe

Ein Fehler alarmiert **einmal pro `repeat_after`** und `(kind, stack)`; ein
weiterlaufender kaputter Stack spammt den Kanal also nicht zu. Nach einem
erfolgreichen Deploy (Recovery) ist der Schlüssel gelöscht — der nächste
Fehler meldet sich sofort wieder. Das Fenster liegt **in-memory**: ein
Neustart des Agents vergisst es und wiederholt höchstens eine Meldung
(bewusst, statt eine zweite State-Datei zu pflegen).

### `${VAR}`-Expansion

Jedes String-Feld einer Destination darf `${VAR}`-Referenzen enthalten; sie
werden beim Laden aus der **Prozessumgebung** expandiert (`os.Expand`-
Semantik). Das ist der Secret-Pfad des Fleets: `vault-env` schreibt die
`.env` des Containers, dockops liest sie als Umgebung. Fehlt eine Variable
oder ist sie leer, schlägt das Laden fehl und nennt Feld und Variablenname:

    stacks.yaml: notifications.destinations[0].token: environment variable TELEGRAM_BOT_TOKEN is not set

Ehrlich scheitern statt still nicht alarmieren — der Pass läuft dann gar
nicht erst los.

### Payloads

- `webhook` — `POST` mit `Content-Type: application/json` und den
  konfigurierten `headers`:

  ```json
  {"kind":"deploy_failed","stack":"immich","remote":"","stage":"compose_up",
   "error":"stack immich: up: ...","hash":"abc123…",
   "at":"2026-10-21T12:00:00Z","host":"docker-01"}
  ```

  Das ist die generische Variante für eigene Automationen (n8n, eigener
  Receiver): `host` unterscheidet mehrere Hosts an einem Kanal.
- `ntfy` — `POST` an die Topic-URL, Body `[<kind>] <stack>: <error>` (ohne
  Stack bei `repo_failed`), Header `Title`, `Priority` und `Tags`.
- `telegram` — `POST https://api.telegram.org/bot<token>/sendMessage` mit
  `chat_id` und `text` (auf 4000 Zeichen gekürzt, `parse_mode` bewusst nicht
  gesetzt — kein Markdown-Escaping).
- `discord` / `slack` — `POST` `{"content": ...}` bzw. `{"text": ...}` mit
  demselben Klartext.

Jede Destination bekommt ihr eigenes `timeout` (Default 10s) und wird
**nacheinander** versucht. Ein toter Kanal wird als WARN mit Typ und Host
geloggt — niemals mit Token oder URL-Query — und stoppt die folgenden
Kanäle nicht. Notifier-Fehler ändern das Pass-Ergebnis nie.

Beim Pass-Start loggt der Agent einmal `notifications active` mit Anzahl und
Ereignisliste (bzw. `notifications inactive (enabled: false)`) — das ist das
Verifikationssignal auf den Hosts:

    docker logs dockops 2>&1 | grep 'notifications active'

## Image-Update-Erkennung

dockops deployt nur, wenn sich der **Stack-Hash** ändert (Compose-Files,
Config, Service-Listen). Ein Image-Tag wie `ghcr.io/x:latest` bewegt sich
davon unabhängig: der Drift ist unsichtbar, weil niemand `pull` aufruft.
Die Update-Erkennung macht ihn sichtbar — **sie deployt nichts**, sie
vergleicht nur und schreibt das Ergebnis in den State (das Anwenden ist eine
spätere Phase).

```yaml
global:
  update_check_interval: 1h      # default 1h; 0 = aus (globale Notbremse)

registries:                      # optional, nur für private Registries
  - host: gitea.naimo84.dev:3000
    username: loki
    password: ${GITEA_REGISTRY_TOKEN}
  # ghcr.io, quay.io und Docker Hub funktionieren anonym — nichts nötig
```

```yaml
# stack.yaml
update_policy: notify           # notify (default) | pinned | apply
```

| Feld | Bedeutung |
|---|---|
| `global.update_check_interval` | Wie oft die Registries gefragt werden. `0` schaltet die Prüfung komplett ab (Rate-Limit-Notbremse); der Default ist `1h`. Ein Stack wird höchstens einmal pro Intervall geprüft, nicht bei jedem 5-Minuten-Poll. |
| `registries[]` | Optionale Zugangsdaten je Host (`host`, `username`, `password`). `password`/`username` dürfen `${VAR}` enthalten — derselbe Secret-Pfad wie bei `notifications` (Prozessumgebung, `vault-env`). Unbekannte Variablen sind Ladefehler mit Feld und Variablenname. |
| `update_policy` | `notify` (Default) prüft und meldet; `pinned` lässt den Stack komplett aus (keine Registry-Abfragen, keine Records); `apply` wendet ein erkanntes Update an (Pull, gezieltes Recreate, Health-Gate, Rollback — siehe unten). |

**Ablauf je Stack** (nach den Deploys, am Pass-Ende):

1. `docker compose -p <stack> -f … config --format json` liefert die
   aufgelöste Service→Image-Map; Build-only-Services (ohne `image:`) fallen
   heraus.
2. Services aus `ignore_services` (bzw. außerhalb von `services`) werden nicht
   geprüft — was nicht deployt wird, wird auch nicht beobachtet.
3. Lokaler Digest je Image: `docker image inspect --format '{{json
   .RepoDigests}}'`. Ohne RepoDigest ist das Image **lokal gebaut** → Record
   mit `error: "locally built image"`, kein Update-Alarm.
4. Remote-Digest über die Registry-API v2 (`HEAD …/manifests/<tag>`,
   Bearer-Challenge inkl. Token-Cache pro Host, 10s pro Request).
5. Vergleich → `update_available` im State. Registry-Fehler (404, 401,
   429, Netz) landen **nur** im Record und machen den Pass nicht kaputt.

**Rate-Limits**: Docker Hub limitiert anonyme Abrufe (derzeit 100
Manifest-Abfragen pro 6h und IP). Bei vielen `latest`-Images auf einem Host
ist das erreichbar — deshalb `update_check_interval` großzügig wählen
(Default 1h, pro Stack) oder für betroffene Stacks `update_policy: pinned`
setzen. Ein `429` wird als Fehler im Record vermerkt, nie als Update.

**`/status`** meldet die Lage rein aus dem State (kein Docker-, kein
Registry-Zugriff im HTTP-Pfad):

```json
{
  "name": "immich",
  "deployed_hash": "abc123…",
  "containers": [{"name": "immich-server", "state": "running", "health": "healthy"}],
  "drift": "ok",
  "updates": {
    "policy": "notify",
    "checked_at": "2026-10-21T20:00:00Z",
    "pending": 1,
    "services": [
      {
        "service": "app",
        "image": "ghcr.io/x:latest",
        "current_digest": "sha256:7f3a…",
        "remote_digest": "sha256:9c14…",
        "update_available": true,
        "error": ""
      }
    ]
  }
}
```

Ohne Records fehlt `updates` ganz (kein Check gelaufen oder `pinned`);
`services` ist immer ein Array, sortiert nach Service. Ein kaputter
Image-Record ändert die Drift-Aussage nicht und umgekehrt.

**Benachrichtigung**: `update_available` ist ein Ereignis des
Phase-9-Notifiers, aber **opt-in** — der Default
(`[deploy_failed, prune_failed, repo_failed, update_failed]`, Phase 11 nimmt
den neuen Fehler-Kind gleich mit auf) bleibt unverändert:

```yaml
notifications:
  on: [deploy_failed, prune_failed, repo_failed, update_available]
```

Gemeldet wird genau einmal pro **neuem** Remote-Digest und Stack
(`<service>: <image> sha256:… -> sha256:…`, Stage `update_check`); bleibt der
Digest gleich, bleibt es still — auch über Neustarts hinweg, weil der
Vergleich gegen den gespeicherten Record läuft. Geht der Stack wieder in
Deckung (Image gepullt, Tag zurückgezogen), wird der Dedupe-Schlüssel
gelöscht.

## Image-Updates anwenden (Phase 11, `update_policy: apply`)

`notify` macht Drift sichtbar, `apply` räumt ihn auf — kontrolliert, also mit
Health-Gate und automatischem Rollback:

```yaml
# stack.yaml
update_policy: apply
health_timeout: 90s      # Default 90s; 0 = Default; negativ = Ladefehler
```

`health_timeout` gehört wie `update_policy` **nicht** in den Stack-Hash: es
stimmt den Apply-Schritt ab, nicht das Deployment.

**Ablauf**, am Ende von `checkImageUpdates` — nur für `apply`-Stacks und nur
in einem Pass, in dem die Prüfung für den Stack fällig war:

1. **Kandidaten**: Records mit `update_available` (lokaler Digest != remote).
   Übersprungen werden lokal gebaute Images (kein `CurrentDigest` → keine
   Registry-Identität, kein Rollback-Ziel), Stacks, deren Deployment in genau
   diesem Pass fehlgeschlagen ist, und Records, deren `apply_blocked_digest`
   schon dem aktuellen Remote-Digest entspricht — dieser Digest ist bereits
   gescheitert, `apply` degradiert für ihn zu `notify`, bis der Tag auf einen
   anderen Digest wandert.
2. **Pull**: `docker compose -p <stack> -f … pull <services…>`, nur die
   betroffenen Services. Ein Pull-Fehler blockiert den Digest und meldet
   `update_failed` mit Stage `pull` — es wird nichts recreated.
3. **Recreate**: `up -d --no-deps <services…>` (derselbe Pfad wie `Up`,
   inklusive `--remove-orphans`), damit Nachbarn nicht mitgerissen werden.
   Der bisherige Digest wird vorher als `previous_digest` festgehalten.
4. **Health-Gate**: Polling alle 3s bis `health_timeout`; gesund ist ein
   Service, dessen Container `running` ist und `healthy` meldet (ohne
   Healthcheck genügt `running`). Sofortiges Scheitern bei `unhealthy`,
   `exited`, `dead`, `restarting`, fehlendem Container oder Timeout.
5. **Rollback** bei jedem Fehlschlag: eine temporäre Override-Datei
   (`dockops-rollback-<stack>.yaml`) pinnt die betroffenen Services auf
   `image: <repository>@<previous_digest>` und wird **nach** den
   Stack-Compose-Files übergeben (spätere Datei gewinnt), danach erneut
   `up --no-deps` + Health-Gate. Die Datei wird in jedem Fall wieder gelöscht.
   Ein fehlgeschlagener Rollback ist der Fall für einen Menschen: der Stack
   läuft dann weder auf dem alten noch verlässlich auf dem neuen Digest
   (`update_failed`, Stage `rollback`).

Jeder Fehlschlag landet als `apply_error` (+ `apply_blocked_digest`,
`rolled_back_at`) im Record und beendet den Apply-Versuch für diesen Digest.
Ein erfolgreicher Apply setzt `current_digest = remote_digest`,
`update_available = false`, `applied_at`.

**Isolation**: Pull/Up passieren ausschließlich hier. Deploy-, Prune- und
Hash-Semantik bleiben unverändert, das State-Schema bleibt `fileVersion: 1`
(alle Felder additiv). Fehler dieses Schritts lassen den Reconcile-Pass nie
scheitern — nur ein Persistenz-Fehler des States wird zurückgegeben.

**`/status`** bekommt pro Service die additiven Felder `previous_digest`,
`applied_at`, `rolled_back_at`, `apply_error` und `blocked` (immer vorhanden:
„wartet dieser Stack auf einen Menschen?“). `updates.policy` kommt jetzt aus
dem State statt aus der Annahme „Records ⇒ notify“.

**Benachrichtigungen**:

```yaml
notifications:
  on: [deploy_failed, prune_failed, repo_failed, update_failed, update_applied, update_available]
```

| Kind | Default | Stages |
|---|---|---|
| `update_failed` | **an** (Fehlerklasse wie `deploy_failed`) | `pull`, `compose_up`, `health_gate`, `rollback` |
| `update_applied` | opt-in (normaler Betrieb, steht in `/status`) | `apply` |
| `update_available` | opt-in (Phase 10) | `update_check` |

Pro Stack und Versuch geht genau ein `update_failed` raus; ein
Rollback-Fehler ersetzt dabei den Gate-Fehler, der ihn ausgelöst hat.

## Self-Update (Phase 12)

Ein Stack, den dockops selbst betreibt (`DOCKOPS_SELF_PROJECT`, Default
`dockops`), lässt sich nicht im eigenen Prozess aktualisieren: `compose up`
ersetzt den Container, in dem der Compose-Aufruf, das Health-Gate und der
Rollback laufen. Für genau diesen Stack übernimmt ein **detached
Helper-Container** den Recreate.

**Ablauf** (nur `update_policy: apply`, und erst nach erfolgreichem Pull):

1. Der Agent schreibt `selfupdate-plan.json` ins State-Dir (Projekt,
   Compose-Dateien, `--env-file`, Services, Health-Ziele, Ziel- und
   Vorher-Digest) und markiert die Records mit `apply_pending_digest` +
   `pending_at`; `previous_digest` ist das Rollback-Ziel.
2. Er startet `docker run -d --rm --name dockops-selfupdate-<projekt>-<unix>`
   mit `/var/run/docker.sock`, dem Stack-Verzeichnis und dem State-Dir — beide
   am **identischen Host-Pfad** — sowie dem gerade gepullten Image.
3. Der Helper wartet `grace_seconds` (10s), fährt
   `compose -p … -f … --env-file … up -d --no-deps <services>`, pollt das
   Health-Gate (`docker inspect`, 180s Timeout, 3s Poll) und schreibt
   `selfupdate-result.json`: `ok`, `rolled_back` oder `rollback_failed`. Bei
   einem Fehlschlag schreibt er vorher die Override-Datei
   `dockops-rollback-<projekt>.yaml` (`<repository>@<previous>`), fährt erneut
   up und gated noch einmal.
4. Beim nächsten Start finalisiert der Agent: `ok` → `current_digest`,
   `applied_at`, `update_applied`; `rolled_back` → `apply_blocked_digest`,
   `apply_error`, `rolled_back_at`, `update_failed`; `rollback_failed` wie
   `rolled_back`, zusätzlich mit `rollback FAILED` im Text. Fehlt das
   Ergebnis, entscheidet der laufende Digest: gleich Ziel ⇒ als Erfolg
   verbuchen (WARN), gleich Vorher-Digest ⇒ als Fehlschlag blockieren.

Solange `apply_pending_digest` gesetzt ist, startet kein Pass einen zweiten
Helper, und `/status` meldet den Service mit `pending: true`
(`updates.services[].pending`; `updates.pending` bleibt der Zähler der
verschobenen Tags). Alles Weitere des Helpers landet in
`<state-dir>/selfupdate-helper.log` — bei `--rm` ist das die einzige
Diagnosequelle.

**Grenzen**:

- Ein Self-Stack pro Host.
- Der Helper läuft im **laufenden** Image; ein kaputtes neues Image kann sich
  nicht selbst retten — dafür ist der Rollback auf `previous` da.
- Der Helper hat Socket-Rechte (wie der Agent), aber keinen Repo-/Secret-
  Zugriff: er liest den Plan und schreibt Ergebnis und Log.
- Der Helper erbt die Mounts des Agenten auf **zwei** Wegen:
  `--volumes-from <agent>` für Named Volumes **und** explizite
  `-v <host>:<container>[:ro]` für jeden Bind-Mount, den der Agent beim Start
  per `docker inspect <eigener Container> --format '{{json .Mounts}}'` meldet
  (`DOCKOPS_SELF_MOUNTS` als Override: kommagetrennt `host:container[:ro]`).
  `--volumes-from` allein genügt nicht — es vererbt nur Volumes, **keine**
  Bind-Mounts. Ein Host, der State-Dir, Checkouts und Socket als Binds fährt
  (der Normalfall), bekäme sonst einen Helper ohne Plan, ohne Compose-Dateien
  und ohne Socket: er stirbt ohne Ergebnis (`self deploy: helper left no
  result`), und der Agent hat keine Diagnosequelle. Mit den Mounts sieht der
  Helper Stack-Dir, State-Dir und Socket unter **denselben absoluten Pfaden**
  wie der Agent. Voraussetzung ist nur, dass der Agent seinen eigenen Container
  kennt; im Container ist das `os.Hostname()` (Kurz-ID) und damit automatisch
  richtig. Ist er nicht ermittelbar, bricht die Übergabe ab, bevor ein Helper
  startet (kein Update verbrannt).
- `DOCKOPS_SELF_UPDATE=off` schaltet den Pfad ab (nur zum Debuggen; der
  Self-Stack fällt dann auf den In-Process-Apply zurück und stirbt beim
  Recreate).

## Deploy-Handover (Phase 13)

Der Image-Pfad oben deckt nur einen Fall ab: ein neues Image für einen
laufenden Self-Stack. Jeder andere Recreate des Self-Stacks — geänderte
Compose-Datei, geänderte `.env`, neues Profil, neuer Service und vor allem die
**Erst-Adoption** (Hash ändert sich von „nichts" auf „irgendwas") — läuft im
Agent durch denselben Abgrund: `compose up` entfernt den alten Container,
*bevor* es den neuen erstellt; stirbt der Prozess in diesem Fenster, ist der
Stack unten, und `restart:` hat nichts mehr, was es neustarten könnte. Deshalb
geht auch dieser Recreate an denselben detached Helper.

**Ablauf**:

1. Der Agent schreibt `selfupdate-plan.json` mit `kind: deploy`: Projekt,
   Compose-Dateien, `--env-file`, `profiles`, die gefilterten Services (leer =
   alle) und `deploy_hash` — dazu Grace- und Health-Deadlines. Im State setzt
   er `deploy_pending_hash` + `deploy_pending_at`; `hash` bleibt der alte
   Stand (ehrlicher Zwischenstand: der Deploy ist beauftragt, nicht passiert).
2. Er startet denselben Helper-Container wie in Phase 12
   (`docker run -d --rm --name … --volumes-from <agent> -v <host>:<pfad>…
   <self-image> -selfupdate-helper -plan <pfad>`; die `-v`-Paare sind die
   Bind-Mounts des Agenten, siehe „Grenzen" oben). Das Helper-Image ist der
   eigene Build:
   `DOCKOPS_SELF_IMAGE`, sonst `docker inspect <eigener Container>
   --format '{{.Config.Image}}'` beim Start.
3. Der Helper wartet `grace_seconds` und fährt `compose -p … -f …
   --profile … --env-file … up -d --remove-orphans [services]`; danach
   schreibt er `selfupdate-result.json` mit `kind: deploy` und `status: ok`
   oder `failed`. Kein Health-Gate und kein Rollback: ein Deploy hat kein
   Vorher, auf das er zurückfallen könnte — der Fehlertext steht im Result und
   im `selfupdate-helper.log`.
4. Beim nächsten Start finalisiert der Agent: `ok` → `hash =
   deploy_pending_hash`, Status `deployed`, Recovery eines offenen
   `deploy_failed`; `failed` → `hash` wie der Versuch, Status `failed`,
   `deploy_failed` (Stage `compose_up`). Fehlt das Result, gilt der Deploy erst
   nach `grace + health_timeout + 120s` als verschollen; bis dahin bleibt
   `deploy_pending_hash` stehen.

Solange `deploy_pending_hash` gesetzt ist, lässt `deployStack` den Stack
unangetastet — kein zweiter `compose up`, kein zweiter Helper, auch wenn der
Hash inzwischen weiter auseinandergelaufen ist: „pending" heißt „in flight",
nicht „unchanged".

**Grenzen**: Helper, Rechte und Mounts sind die aus Phase 12 (ein Self-Stack pro
Host, Socket-Rechte, kein Repo-/Secret-Zugriff). Ist kein Helper-Image bekannt
(weder `DOCKOPS_SELF_IMAGE` noch ein `docker inspect`-Ergebnis), startet
**kein** Helper — mit geratenem Image würde der Stack mit einem anderen Build
laufen als der Agent. Der Deploy fällt dann auf den In-Process-Recreate zurück
(Warnung im Log, inklusive Hinweis auf die Variable), weil der Image-Pfad
desselben Passes die Records braucht, die dieser Deploy schreibt: das ist
genau das Fenster, das die Phase schließt, und deshalb ein
Konfigurationsfehler, kein Betriebsmodus. `DOCKOPS_SELF_UPDATE=off` schaltet
auch diesen Pfad ab.

Nicht abgedeckt: der **Prune** des Self-Stacks (`compose down`) läuft weiter
in-process. Der Agent stirbt dabei zwar, aber der Stack verschwindet sowieso;
das State-Entry bleibt, und der nächste Pass wiederholt den Down.

## CLI

    dockops [-repo URL] [-branch NAME] [-state PFAD] [-addr HOST:PORT]
            [-webhook-secret GEHEIMNIS] [-sops-sidecar URL]
            [-interval DAUER] [-once] [-print-status]

| Flag | Env-Variable | Default | Bedeutung |
|---|---|---|---|
| `-repo` | `DOCKOPS_REPO_URL` | — (Pflicht, außer bei `-print-status`) | URL des Host-Repos (HTTPS/SSH/lokal) |
| `-branch` | `DOCKOPS_BRANCH` | `main` | Branch des Host-Repos |
| `-state` | — | `/var/lib/dockops/state.json` | Pfad der JSON-State-Datei |
| `-webhook-secret` | `DOCKOPS_WEBHOOK_SECRET` | leer | Shared Secret für `POST /webhook`; leer ⇒ Webhooks ohne Auth (Warnung im Log) |
| `-addr` | — | `:8080` | Listen-Adresse des Webhook-Servers |
| `-sops-sidecar` | `DOCKOPS_SOPS_SIDECAR_URL` | leer | URL des SOPS-Sidecar (z. B. `http://sops-sidecar:8081`); leer ⇒ lokales `sops -d` |
| `-interval` | — | `global.poll_interval` (Default 5m) | Poll-Intervall-Override, z. B. `90s` |
| `-once` | — | `false` | Ein einzelner Reconcile-Durchlauf, dann Exit |
| `-print-status` | `DOCKOPS_PRINT_STATUS` | `false` | `/status`-JSON auf stdout ausgeben und exiten (kein `-repo` nötig; gewinnt über `-once` und Daemon) |

Dazu die Vault-Variablen (Namen in `global.vault` konfigurierbar):
`VAULT_ADDR` (Fallback für `global.vault.address`), `VAULT_TOKEN`
(`auth_method: token`) bzw. `VAULT_ROLE_ID` + `VAULT_SECRET_ID`
(`auth_method: approle`). Ohne eigenes Flag: `DOCKOPS_STATUS_TOKEN`
(Bearer-Schutz für `GET /status`, leer = offen), `DOCKOPS_SELF_PROJECT`
(eigenes Compose-Projekt für die Unmanaged-Erkennung **und** die
Self-Update-Erkennung, default `dockops`) und `DOCKOPS_SELF_UPDATE`
(Default an; nur die exakte Angabe `off` deaktiviert beide Helper-Pfade, siehe
„Self-Update“ und „Deploy-Handover“), `DOCKOPS_SELF_IMAGE` (Image, aus dem der
Phase-13-Helper startet; leer ⇒ `docker inspect` auf den eigenen Container).
`DOCKOPS_HOST_CHECKOUT` füllt ein leeres `global.host_checkout` (YAML
gewinnt; `-print-status` und `/status` folgen der Variable auch ohne
Config, siehe „Konfigurierbare Checkout-Pfade“).

`-selfupdate-helper` und `-selfupdate-plan` sind ausschließlich für den
internen Helper-Container gedacht (Phase 12 und 13, siehe „Self-Update“ und
„Deploy-Handover“): sie werden vom Agent selbst gesetzt, brauchen kein `-repo`
und schalten Reconciler und HTTP-Server ab. Der Helper führt je nach
`plan.kind` entweder den Image-Move (Health-Gate, Rollback) oder den
Deploy (`compose up`, kein Gate) aus.

**Betriebsmodi**: Ohne `-once` läuft dockops als Daemon — sofortiger erster
Durchlauf, danach Poll-Timer (`-interval` überschreibt
`global.poll_interval`) plus Webhook-Trigger, die entprellt und gemerged
werden; ein laufender Durchlauf wird nie unterbrochen. SIGINT/SIGTERM
beendet ihn kontrolliert. `-print-status` schaltet auf den One-Shot-
Statusmodus (siehe „Ohne Server: `-print-status`“) und gewinnt über beide
anderen Modi.

**HTTP-Endpunkte**:

- `POST /webhook` — authentifiziert via GitHub
  (`X-Hub-Signature-256: sha256=<hex>` HMAC-SHA256 über den Body) **oder**
  GitLab (`X-Gitlab-Token`), beides in konstanter Zeit verglichen:

      curl -X POST http://localhost:8080/webhook \
        -H "X-Gitlab-Token: $DOCKOPS_WEBHOOK_SECRET"

- `GET /healthz` — Liveness.

- `GET /status` — Soll-vs-Ist-Snapshot für Monitoring (Drift-Matrix,
  Container, unmanaged Projekte, optional Bearer-Token); Details siehe
  „Monitoring (`/status` Endpoint)“:

      curl -H "Authorization: Bearer $DOCKOPS_STATUS_TOKEN" \
        http://localhost:8080/status

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
  bzw. einer `.env`-Datei neben der `compose.yaml` substituiert; optional
  `DOCKOPS_STATUS_TOKEN` (Bearer-Schutz für `GET /status`),
  `DOCKOPS_LOGS_TOKEN` (Pflicht-Token für die `/logs*`-API; ohne die Variable
  ist die Log-API mit 503 abgeschaltet) und `DOCKOPS_SELF_PROJECT`
  (Unmanaged-Erkennung, default `dockops`).
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

## Lizenz

MIT - siehe [LICENSE](LICENSE).
