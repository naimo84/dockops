# dockops

Deklarativer Docker-Compose-Stack-Manager: `stacks.yaml` im Host-Repo-Root
beschreibt lokale Stacks (rekursiv via Include-Globs) und Import-Stacks aus
Remote-Checkouts unter `global.work_dir`. Phase 1: Go-Bootstrap und
YAML-Config-Loader (`internal/config`). Phase 2: Git-Manager
(`internal/git`), deterministisches Stack-Hashing (`internal/hash`) und
State-Store (`internal/state`). Phase 3: Secret-Auflösung und Deployment-
Bausteine — Vault-Client (`internal/vault`), SOPS (`internal/sops`),
Env-Merge (`internal/env`), Hook-Engine (`internal/hooks`) und Compose-
Executor (`internal/compose`). Phase 4: Reconciliation-Loop
(`internal/reconciler`), CLI-Agent (`cmd/dockops`: `-once`, Poll- und
Webhook-Modus mit Graceful Shutdown) und Webhook-Server
(`internal/webhook`: GitHub-HMAC/GitLab-Token, `/healthz`). Phase 5:
E2E-Integrationstest (`internal/reconciler/e2e_test.go`: echte Git-Repos,
Fake-docker/sops via PATH, Fake-Vault via httptest), Dockerfile
(Multi-Stage, alpine + docker-cli/git/ssh), `compose.yaml` für den
dockops-Container und `README.md`. Phase 6a: Env-File-Realität — generierte
`<stackDir>/.env` neben der shm `--env-file` (`env.WriteStackEnv`, Self-
Reference-Skip beim Env-Merge, bewusst NICHT im Stack-Hash: Secret-Rotation
triggert keinen Deploy), Service-Filter für lokale Stacks (`services` jetzt
wirksam auch für Basis-Stacks, neu `ignore_services` mit Komodo-Semantik,
beide im Hash als effective Services) und Compose-Override-Files (neu
`compose_files`, Multi-`-f` in Listenreihenfolge, Hash über alle Inhalte).

## Umgebung

- Go 1.25, Toolchain unter `~/.local/go/bin/go` — vor jedem go-Aufruf:
  `export PATH=$HOME/.local/go/bin:$PATH`
- Module-Pfad: `github.com/local/dockops`

## Struktur

- `cmd/dockops/` — CLI-Entry-Point: Flags mit Env-Fallbacks, `-once`- und
  Daemon-Modus (Webhook-Server + Poll-Timer, debounced Trigger, Graceful
  Shutdown), slog-JSON auf stdout
- `internal/config/` — Config-Loader: Root-Config, Stack-Discovery über
  Include-Globs, Compose-Auto-Detection, Import-Merge, Validierung;
  Stack-Felder: `services` (auch Basis-Stacks), `ignore_services`
  (Schnittmenge mit `services` = Validierungsfehler; Import-Merge:
  services ersetzt, ignore concat+dedup), `compose_files`
  (Multi-`-f`, exklusiv zu `compose_file`, alle Dateien müssen existieren);
  Helper `EffectiveComposeFiles`/`EffectiveServices`/`ExcludeServices`
- `internal/git/` — Git-Manager: Clone-or-Update der Remote-Checkouts über
  die lokal installierte git-Binary (`os/exec`, keine Git-Library)
- `internal/hash/` — Deterministisches Stack-Hashing (Package `stackhash`,
  Schema-Version 2): kanonische Serialisierung von Stack (inkl. Compose-
  File-Liste, effective Services und ignore_services) + normalisierten
  Compose-Inhalten (einer pro Datei, in `-f`-Reihenfolge)
- `internal/state/` — State-Store: Deployment-Zustand pro Stack (JSON-Datei,
  atomar geschrieben; `compose_files` für Multi-File-Prune aufgezeichnet)
- `internal/dotenv/` — Minimaler dotenv-Dialekt (Parse/Format), geteilt von
  SOPS-Output, env_files und der erzeugten `--env-file`/Stack-`.env`
- `internal/secrets/` — `Resolver`-Interface: abstrahiert SOPS-Dateien und
  Vault-Pfade für den Env-Merge
- `internal/vault/` — Vault-Client für KV v2 über net/http (keine
  Vault-Library): Token/AppRole-Login, ReadKV
- `internal/sops/` — Entschlüsseln via `sops`-Binary (os/exec), Auflösung
  von age-Keyfiles über SOPS_AGE_KEY_FILE im Child-Prozess
- `internal/env/` — Env-Merge (env_files < sops < vault < Systemvars;
  `<stackDir>/.env` wird als Quelle übersprungen, weil dockops sie selbst
  generiert), WriteShm (0600-Env-Datei in /dev/shm mit idempotentem
  Cleanup) und WriteStackEnv (atomare 0600-`<stackDir>/.env` mit Rollback:
  restore-or-remove; bewusst nicht im Stack-Hash)
- `internal/hooks/` — Hook-Engine: sequenzielle `sh -c`-Ausführung mit
  Per-Kommando-Timeout (Prozessgruppen-Kill) und slog-Output
- `internal/compose/` — Compose-Executor: `docker compose up/down` mit
  deterministischem Projektnamen (`-p <stack-name>`), einem `-f` pro
  Compose-Datei in Listenreihenfolge, Service-Filter als positionale
  Argumente (services minus ignore_services; ohne services: Service-Namen
  der Compose-Dateien minus ignore, "alles ignoriert" = Fehler), plus
  `DownProject` (Prune ohne Compose-Datei)
- `internal/reconciler/` — Reconciliation-Loop: verdrahtet git, config,
  hash, state, secrets (Vault-Login einmal pro Durchlauf), env, hooks und
  compose zu einem idempotenten Durchlauf mit Prune und `errors.Join`;
  schreibt vor jedem compose-Aufruf die generierte `<stackDir>/.env`
  (Rollback nur bei Up-Fehler); Git/Compose/Hooks/Sops/Vault als
  injizierbare Interfaces für Tests
- `internal/webhook/` — Webhook-HTTP-Handler: `POST /webhook` mit
  GitHub-HMAC-SHA256 (`X-Hub-Signature-256`) oder GitLab-Token
  (`X-Gitlab-Token`), konstante Vergleiche, `GET /healthz`
- `internal/reconciler/e2e_test.go` — E2E-Integrationstest: komplette
  Reconcile-Durchläufe gegen Bare-Repos (Host + Shared-Remote), Fake-
  `docker`/`sops`-Skripte via PATH-Override, Fake-Vault via
  `httptest.Server`
- `Dockerfile` — Multi-Stage-Build (statisches Binary, alpine-Runtime mit
  docker-cli + compose-Plugin, git, openssh-client, CA-Certs)
- `compose.yaml` — dockops selbst als Container: docker.sock-Mount,
  SSH-Key-Mount (ro), `DOCKOPS_*`-Env, Vault-Env, Port 8080
- `README.md` — Dokumentation: Architektur, Config-Beispiele (Root +
  immich-Stack, beide Import-Varianten), Reconcile-Ablauf, Secret-Priorität,
  CLI-Referenz, Docker-Deployment, Development
- `PHASE1_TASK.md` — Aufgabenstellung (nicht ändern)
- `PHASE2_TASK.md` — Aufgabenstellung (nicht ändern)
- `PHASE3_TASK.md` — Aufgabenstellung (nicht ändern)
- `PHASE4_TASK.md` — Aufgabenstellung (nicht ändern)
- `PHASE6A_TASK.md` — Aufgabenstellung (nicht ändern)

## Konventionen

- Logging ausschließlich über stdlib `log/slog` (kein anderes Log-Paket).
- Tests mit stdlib `testing`, kein testify. Fixtures über `t.TempDir()`
  (Host-Repo + Fake-Remote-Checkouts als Temp-Verzeichnisse aufbauen).
- YAML: `gopkg.in/yaml.v3` (einzige externe Abhängigkeit), Strict-Decoding
  mit `KnownFields(true)`, damit Tippfehler als Load-Fehler auffallen.
- Kein `panic` in Library-Code: alle Fehler als `error` zurückgeben.
- Validierungsfehler enthalten immer Kontext (Datei, Pfad, Feld, Index).
- Alle exportierten Symbole sind mit Doc-Kommentaren dokumentiert.
- Offene Punkte im Briefing werden konservativ entschieden (stdlib, klare
  Fehler) und die Entscheidung als Kommentar im Code festgehalten.

## Checks vor jedem Commit

    export PATH=$HOME/.local/go/bin:$PATH
    go build ./... && go vet ./... && go test ./...
