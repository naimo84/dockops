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
Phase 6b: SOPS-Sidecar — zweiter Resolver `sops.SidecarResolver` (HTTP-
Client mit 60s-Timeout/10MB-Limit) plus Server-Handler
`sops.NewSidecarHandler` (Pfad-Allow-List, `..`-Traversal-Block) und
Mini-Programm `cmd/sops-sidecar`; CLI-Flag `-sops-sidecar`
(`DOCKOPS_SOPS_SIDECAR_URL`, leer = lokaler `sops`-Exec); Vault-Template-
Syntax `env.RenderVaultTemplate` für `${VAULT:path#key}`/`${VAULT:path:key}`
mit neuem Stack-Feld `vault_env_templates` (gleiche Prioritätsstufe wie
vault_paths, Import-Merge concat+dedup, Hash Schema v3) und
`secrets.VaultReader`; Image `cmd/sops-sidecar/Dockerfile.sidecar`, sops-
Profil in der `compose.yaml`, README-Abschnitt „SOPS decryption“. Phase 6c:
Bugfixes aus dem devbox-Livetest — State-Persistenz jetzt pro Stack sofort
nach jeder Aufzeichnung (`deployStack`/`markFailed` sichern per `state.Save`
ab, nicht erst am Pass-Ende: ein abgebrochener Durchlauf kann deployte
Stacks nicht mehr „vergessen“, sonst Recreate bei identischem Hash und
vergessener Prune), Prune robuster in `pruneStack` (Compose-Files da →
`down` mit allen `-f`; File(s) weg → `DownProject` nur `-p`; Eintrag ohne
Compose-Spuren → State-Remove mit Warnung, ohne compose-Aufruf), Regressions-
Tests in `internal/reconciler/phase6c_test.go` (Prune-Fälle a/b/c,
Skip-über-Prune-Pass, unterbrochener Pass mit Restart) und E2E-Pass 5 löscht
die Stack-Ordner im Checkout komplett. Phase 7: WIP-Auto-Stash — `internal/git`
sichert lokale (Host-seitige) Änderungen vor dem deterministischen Reset:
`stashWIP` prüft im Update-Pfad von `EnsureRepo` vor fetch/reset/clean den
Worktree per `git status --porcelain` und stasht Dirty-State inkl. untracked
als `dockops-wip: <RFC3339>` (INFO-Log mit Restore-Hint; Stash-Fehler nur
WARN ohne Abbruch; bewusst kein Auto-Pop — Restore per `git stash list`/`pop`
ist Handarbeit, Konflikte dürfen Deploys nie blockieren). Phase 8: Monitoring-
Endpoint `GET /status` — Soll (state.json) vs. Ist (laufende Container):
`internal/compose/ps.go` mit `PsProject` (`docker compose -p X ps --all
--format json`, tolerant JSON-Array/Lines/Einzelobjekt, `Name`/`Names`-
Alias) und `PsAllProjects` (`docker ps --format {{json .}}`, Label-Parsing
der Formen String/array/object; ohne `com.docker.compose.project`-Label =
Rauschen, wird geskipped); `internal/webhook/status.go` mit Drift-Matrix
(`ok`/`stopped`/`partial`/`unknown` — State führt keine Soll-Services, also
Fallback-Vergleich running>0 und keine exited), unmanaged = laufende
Compose-Projekte ohne State-Eintrag minus Eigenprojekt (`DOCKOPS_SELF_PROJECT`,
Default `dockops`), per-Stack-ps-Fehler → `"error"`-Feld + drift `unknown`,
Endpoint bleibt 200; Konstruktor `webhook.New(webhook.Deps{...})` mit
injizierbarem `ComposeLister`, optionaler Bearer-Schutz via
`DOCKOPS_STATUS_TOKEN` (constant-time); `state.Store` jetzt RWMutex-gesichert
(HTTP-Goroutines lesen parallel zum Poll-Loop); CLI teilt compose-executor und
git-manager zwischen Reconciler und /status, `reconciler.HostRepoName`
exportiert. Phase 8b: One-Shot-Status ohne Server — die JSON-Erzeugung von
/status in die geteilte Funktion `webhook.BuildStatus(ctx, StatusDeps)`
gezogen (exportierte Response-Typen `StatusResponse`/`StackStatus`/
`UnmanagedProject`/`HostRepo`; nil State/Compose = einziger Fehler,
Docker-Fehler degradieren weiter ins Dokument); CLI-Flag `-print-status`
(env `DOCKOPS_PRINT_STATUS`) lädt den State und schreibt dasselbe JSON via
`writeStatus` mit 2-Spaces-Indent nach stdout — Logs/Fehler auf stderr
(stdout bleibt reines JSON), kein `-repo` nötig, gewinnt über
`-once`/Daemon-Modus (Hinweis-Log), exit 0 auch bei Docker-Problemen, nur
State-Ladefehler → stderr + exit 1. Phase 9: Benachrichtigungen bei
Reconcile-Fehlern — neues Paket `internal/notify` (stdlib-only Transport für
`webhook`/`ntfy`/`telegram`/`discord`/`slack`: POST je Destination mit eigenem
`context.WithTimeout`, HTTP-Status ≥ 400 = Fehler, WARN-Log nur mit Typ und
Host — nie Token oder URL-Query, Fehler erreichen den Aufrufer nicht),
In-Memory-Dedupe pro `(kind, stack)` mit `repeat_after` (Mutex + Map,
Neustart vergisst das Fenster — bewusst akzeptiert) und `Recovery`, das den
Key löscht; optionaler Root-Block `notifications` (Typen in
`internal/config/types.go`, Defaults/Validierung/`${VAR}`-Expansion aus der
Prozessumgebung in `load.go`; fehlende Variable = Ladefehler mit Feld und
Variablenname, fehlender Block = Feature aus, `enabled: false` lädt und
validiert weiter, sendet aber nichts; kein Stack-Hash-Bump, weil host-global)
und Reconciler-Verdrahtung: `deploy_failed` mit `stage`
(`secrets`/`pre_deploy`/`compose_up`/`post_deploy`), `prune_failed`,
`repo_failed` höchstens einmal pro Pass (`remote` = betroffener Remote bzw.
`host`, `stage` `repo`/`config`), Notifier lazy beim ersten Pass mit Block
und Neuaufbau nur bei geänderter Config (Vergleich über die serialisierte
Struktur), `Deps.Notifier` gewinnt immer (Tests), `Deps.NewNotifier` nil →
`notify.New`; INFO `notifications active destinations=… on=[…]` je Pass als
Verifikationssignal auf den Hosts. Tests: `internal/notify/notify_test.go`
(httptest je Sender inkl. Telegram-Basis-URL-Override, Timeout blockiert
Folgekanäle nicht, Dedupe mit injizierter Clock, Recovery, `on`-Filter,
Kürzung 2000/4000, keine Secrets im Log), `internal/config/phase9_test.go`
(Defaults, Expansion, fehlende Variable, unbekannter Typ/Pflichtfelder,
leere Destinations, fehlender Block) und
`internal/reconciler/phase9_test.go` (Fake-docker via PATH,
`compose_up`/`pre_deploy`/Prune/Repo-Events, Dedupe über zwei Pässe,
Notifier-Bau nur mit Block). Phase 10: Image-Update-Erkennung — neues Paket
`internal/registry` (`ParseRef` mit Docker-Hub-Kurzform/`library/`-Präfix/
Tag-Default/Digest-Pins, `HEAD /v2/<repo>/manifests/<tag>` mit `Accept` für
alle vier Manifest-Typen, Digest aus `Docker-Content-Digest`, Bearer-Challenge
mit Basic-Auth nur bei konfigurierten Credentials und Token-Cache je Host,
Fehlerklassen `ErrNotFound`/`ErrUnauthorized`/`ErrRateLimited` via
`errors.Is`, 10s pro Request, Klartext-HTTP nur für Loopback, Digest-Pin ohne
Netzaufruf); `compose.Images` (`docker compose -p X -f … config --format
json`, Build-only-Services ohne `image:` fallen heraus) und
`compose.ImageDigest` (`docker image inspect --format '{{json
.RepoDigests}}'`, Repository-Match mit Hub-Normalisierung, leer = lokal
gebaut); optionale Root-Config `registries` (`${VAR}`-Expansion über
`expandNotificationValue`) und `global.update_check_interval`
(`*config.Duration`, Default 1h, `0` = aus, negativ = Ladefehler); Stack-Feld
`update_policy` (`notify` Default | `pinned`, unbekannt = Validierungsfehler);
additive State-Felder `Images map[string]ImageRecord` mit `Store.SetImages`
(**fileVersion bleibt 1**, alte Dateien laden weiter); Reconciler-Schritt
`checkImageUpdates` NACH den Deploys (Fälligkeit über das jüngste `CheckedAt`
plus Intervall, Services aus `services`/`ignore_services` gefiltert, lokal
gebautes Image = Record ohne Alarm, Registry-Fehler nur im Record und nie
Pass-Fehler, Übergang false→true bzw. neuer Remote-Digest = genau ein
`update_available`-Event mit Stage `update_check`, Recovery bei wieder
deckungsgleichem Image); `/status` und `-print-status` bekommen das additive
Feld `updates{policy,checked_at,pending,services[]}` **ausschließlich aus dem
State** (kein Docker/Registry im HTTP-Pfad). Bewusste Entscheidungen: **kein
Hash-Bump** (`update_policy` ändert kein Deployment — Test im Hash-Paket),
State-Feld rein additiv (kein Migrationszwang), `/status` bleibt
offline-fähig, `update_available` ist **opt-in** (Default-On-Liste bleibt
`[deploy_failed, prune_failed, repo_failed, update_failed]`), diese Phase
deployt nichts (kein `pull`, kein Recreate). Phase 11: kontrolliertes Anwenden
(`update_policy: apply`) — `update_policy` akzeptiert `apply`, neues
Stack-Feld `health_timeout` (`Duration`, Default `DefaultHealthTimeout` = 90s,
`0` = Default, negativ = Ladefehler, bewusst **nicht** im Hash; Helper
`AppliesImageUpdates`/`EffectiveHealthTimeout`); `compose.Container` additiv um
`Service` (aus `compose ps --format json`, fehlt es bleibt es leer),
`compose.Pull` (`compose -p … pull <services…>`, leere Liste = Projekt) und
`compose.UpOverride` (zusätzliche `-f`-Dateien **nach** den Stack-Files,
`up -d --remove-orphans --no-deps`, positionale Services; gemeinsamer
`upSpec`-Aufbau mit `Up`, dessen Verhalten unverändert bleibt); State: `State`
merkt sich `update_policy` (für `/status` aus dem State) und `ImageRecord`
additiv `PreviousDigest`/`AppliedAt`/`RolledBackAt`/`ApplyBlockedDigest`/
`ApplyError` (**fileVersion bleibt 1**, Altdaten laden weiter); neue Datei
`internal/reconciler/apply.go`: `applyUpdates` läuft am Ende von
`checkImageUpdates`, nur für `apply`-Stacks und nur in fälligen Pässen —
Kandidaten aus `UpdateAvailable` (lokal gebaute Images, nicht deployte Stacks
und bereits blockierte Digests ausgenommen) → `Pull` → `UpOverride(nil,
services)` → Health-Gate-Polling (`healthPoll`, Default 3s, Fenster
`health_timeout`; `unhealthy`/`exited`/`dead`/`restarting`/fehlender Container/
Timeout = Fehlschlag) → bei Fehlschlag `rollbackImages`: temporäre
`dockops-rollback-<stack>.yaml` mit `image: <repository>@<PreviousDigest>` in
jedem Fall per `defer` gelöscht, `UpOverride` mit dieser Datei, erneutes Gate;
`ApplyBlockedDigest == RemoteDigest` blockiert weitere Versuche (degragiert zu
notify, bis der Tag wandert); genau ein `update_failed` je Stack und Versuch
(Stages `pull`/`compose_up`/`health_gate`/`rollback`, Rollback-Fehler ersetzt
den auslösenden Gate-Fehler) plus `update_applied` (Stage `apply`, opt-in,
Default-On-Liste bekommt nur `update_failed`); kein Fehler dieses Schritts
lässt den Pass scheitern (nur State-Persistenz wird zurückgegeben), kein
`panic`; `/status` erweitert `ServiceUpdate` additiv um `previous_digest`,
`applied_at`, `rolled_back_at`, `apply_error`, `blocked` und liest
`updates.policy` aus dem State (Phase-10-Annahme „Records ⇒ notify“ gilt nicht
mehr). Tests:
`internal/registry/registry_test.go` (Parsing-Tabelle, `httptest`-Registry mit
401→Token→200, falsches Token, fehlender Digest-Header, Klassen 404/403/429,
Timeout, `Accept` aller vier Typen, Pin ohne HTTP),
`internal/compose/images_test.go` (Fake-docker: Config-JSON-Parsing inkl.
Build-only-Service, RepoDigests-Match, Fehlerfälle),
`internal/state/phase10_test.go` (Round-Trip, Altdatei ohne `Images`, Version
1, `SetImages`-Semantik), `internal/config/phase10_test.go`
(Registries/Expansion/Fehler, Intervall Default/0/negativ, Policy
Default/`pinned`/ungültig, `update_available` opt-in),
`internal/reconciler/phase10_test.go` (pinned/Intervall 0 ohne Aufruf,
Fälligkeit, genau ein Event pro neuem Digest, Registry-Fehler, lokal gebautes
Image, `ignore_services`, degradierendes Compose-Config) und
`internal/webhook/phase10_test.go` (`updates` aus dem State, fehlend ohne
Records, unabhängig von ps-Fehlern).

## Umgebung

- Go 1.25, Toolchain unter `~/.local/go/bin/go` — vor jedem go-Aufruf:
  `export PATH=$HOME/.local/go/bin:$PATH`
- Module-Pfad: `github.com/local/dockops`

## Struktur

- `cmd/dockops/` — CLI-Entry-Point: Flags mit Env-Fallbacks (`-repo`,
  `-branch`, `-webhook-secret`, `-sops-sidecar`, `-interval`), `-once`- und
  Daemon-Modus (Webhook-Server + Poll-Timer, debounced Trigger, Graceful
  Shutdown), slog-JSON auf stdout; teilt compose-executor und git-manager
  zwischen Reconciler und /status; `-print-status` (env
  `DOCKOPS_PRINT_STATUS`) gibt das /status-JSON als One-Shot nach stdout
  (Diagnose auf stderr, kein `-repo`, gewinnt über `-once`/Daemon) (Env ohne
  Flag: `DOCKOPS_STATUS_TOKEN`, `DOCKOPS_SELF_PROJECT`)
- `cmd/sops-sidecar/` — Mini-Programm des Decrypt-Sidecars: Startet den
  Handler aus `internal/sops` hinter `net/http` mit Graceful Shutdown;
  Wurzeln aus `SOPS_SIDECAR_ALLOWED_ROOTS` (doppelpunkt-separiert, absolut,
  Pflicht), Adresse aus `-addr`/`SOPS_SIDECAR_ADDR` (Default `:8081`)
- `internal/config/` — Config-Loader: Root-Config, Stack-Discovery über
  Include-Globs, Compose-Auto-Detection, Import-Merge, Validierung;
  Root-Block `notifications` (Defaults, `${VAR}`-Expansion aus der
  Prozessumgebung, Pflichtfelder je Destination-Typ, `on`-Validierung);
  Stack-Felder: `services` (auch Basis-Stacks), `ignore_services`
  (Schnittmenge mit `services` = Validierungsfehler; Import-Merge:
  services ersetzt, ignore concat+dedup), `compose_files`
  (Multi-`-f`, exklusiv zu `compose_file`, alle Dateien müssen existieren),
  `vault_env_templates` (Import-Merge concat+dedup wie vault_paths, Pfade
  relativ zum Stack-Dir, Auflösung im Env-Merge), `update_policy`
  (`notify`/`pinned`, bewusst nicht im Hash); Root-Block `registries`
  (Credentials mit `${VAR}`-Expansion, Pflichtfelder, doppelte Hosts
  abgelehnt); `global.update_check_interval` als `*Duration` (nil = 1h, `0` =
  aus, `Global.EffectiveUpdateCheckInterval`);
  Helper `EffectiveComposeFiles`/`EffectiveServices`/`ExcludeServices`
- `internal/git/` — Git-Manager: Clone-or-Update der Remote-Checkouts über
  die lokal installierte git-Binary (`os/exec`, keine Git-Library); im
  Update-Pfad sichert `stashWIP` dirty Worktrees (inkl. untracked) vor
  `reset --hard`/`clean -fd` als `dockops-wip:`-Stash (Fehler nur WARN,
  kein Abbruch, kein Auto-Pop)
- `internal/git/phase7_test.go` — Phase-7-Regressionstests: WIP-Auto-Stash
  (dirty tracked File im Stash + Reset + INFO-Log, untracked File per
  `stash pop` verifiziert, clean Worktree ohne Stash-Eintrag, Stash-Failure
  via git-PATH-Wrapper ohne EnsureRepo-Fehler)
- `internal/hash/` — Deterministisches Stack-Hashing (Package `stackhash`,
  Schema-Version 3): kanonische Serialisierung von Stack (inkl. Compose-
  File-Liste, effective Services, ignore_services und
  vault_env_templates) + normalisierten Compose-Inhalten (einer pro Datei,
  in `-f`-Reihenfolge)
- `internal/state/` — State-Store: Deployment-Zustand pro Stack (JSON-Datei,
  atomar geschrieben; `compose_files` für Multi-File-Prune aufgezeichnet;
  additives `images`-Feld mit `ImageRecord` je Image-Ref inkl. der
  Phase-11-Apply-Felder, additives `update_policy`, `SetImages`
  verlangt einen bestehenden Stack-Eintrag, `fileVersion` bleibt 1);
  RWMutex-gesichert, /status liest aus HTTP-Goroutines parallel zum Poll-Loop
- `internal/dotenv/` — Minimaler dotenv-Dialekt (Parse/Format), geteilt von
  SOPS-Output, env_files, Vault-Templates und der erzeugten
  `--env-file`/Stack-`.env`
- `internal/secrets/` — `Resolver`-Interface (abstrahiert SOPS-Dateien und
  Vault-Pfade für den Env-Merge) und `VaultReader` (ReadKV-Teilmenge für
  `env.RenderVaultTemplate`)
- `internal/vault/` — Vault-Client für KV v2 über net/http (keine
  Vault-Library): Token/AppRole-Login, ReadKV
- `internal/sops/` — Zwei Resolver: lokales Entschlüsseln via `sops`-Binary
  (os/exec, age-Keyfiles über SOPS_AGE_KEY_FILE im Child-Prozess) und
  `SidecarResolver` via HTTP (`NewSidecar`; 60s-Request-Timeout,
  10MB-Response-Limit, age_keyfile = Fehler); `NewSidecarHandler` dient den
  Endpunkte `POST /decrypt` (JSON `{"path"}` → `{"content"}` bzw.
  `{"error"}`) und `GET /healthz` mit Pfad-Allow-List
  (`filepath.Clean` + Prefix-Check blockt `..`-Traversal)
- `internal/env/` — Env-Merge (env_files < sops < vault_paths ≈
  vault_env_templates < Systemvars; `<stackDir>/.env` wird als Quelle
  übersprungen, weil dockops sie selbst generiert), RenderVaultTemplate
  (`${VAULT:path#key}`/`${VAULT:path:key}`, zeilenweises Ersetzen, Fehler
  mit Zeilennummer + Referenz, per Template gecachte KV-Reads), WriteShm
  (0600-Env-Datei in /dev/shm mit idempotentem Cleanup) und WriteStackEnv
  (atomare 0600-`<stackDir>/.env` mit Rollback: restore-or-remove;
  bewusst nicht im Stack-Hash)
- `internal/hooks/` — Hook-Engine: sequenzielle `sh -c`-Ausführung mit
  Per-Kommando-Timeout (Prozessgruppen-Kill) und slog-Output
- `internal/compose/` — Compose-Executor: `docker compose up/down` mit
  deterministischem Projektnamen (`-p <stack-name>`), einem `-f` pro
  Compose-Datei in Listenreihenfolge, Service-Filter als positionale
  Argumente (services minus ignore_services; ohne services: Service-Namen
  der Compose-Dateien minus ignore, "alles ignoriert" = Fehler), plus
  `DownProject` (Prune ohne Compose-Datei), `images.go` mit `Images`
  (Service→Image über `config --format json`, Build-only ausgelassen) und
  `ImageDigest` (lokaler RepoDigest für die Update-Prüfung) sowie `ps.go` mit
  `PsProject`/`PsAllProjects` (Container-/Projekt-Listen für `GET /status`,
  nie deploy-relevant)
- `internal/webhook/` — Webhook-HTTP-Handler: `POST /webhook` mit
  GitHub-HMAC-SHA256 (`X-Hub-Signature-256`) oder GitLab-Token
  (`X-Gitlab-Token`), konstante Vergleiche, `GET /healthz`; Konstruktor
  `New(Deps{...})` (Secret/Trigger/Logger + /status-Kollaborateure)
- `internal/reconciler/` — Reconciliation-Loop: verdrahtet git, config,
  hash, state, secrets (Vault-Login einmal pro Durchlauf, sobald ein Stack
  vault_paths ODER vault_env_templates hat), env, hooks und compose zu
  einem idempotenten Durchlauf mit Prune und `errors.Join`; schreibt vor
  jedem compose-Aufruf die generierte `<stackDir>/.env` (Rollback nur bei
  Up-Fehler); State-Aufzeichnungen (deployed/failed) werden pro Stack
  SOFORT persistiert (`state.Save` direkt nach jedem `Set`, Final-Save am
  Pass-Ende deckt die Prunes ab — Phase 6c); `pruneStack` unterscheidet
  drei Fälle: Compose-Files vorhanden → `down` mit allen `-f`, Files weg →
  `DownProject` nur mit `-p`, Eintrag ohne Compose-Spuren → State-Remove
  mit Warnung ohne compose-Aufruf; Git/Compose/Hooks/Sops/Vault als
  injizierbare Interfaces für Tests, `Deps.SopsSidecarURL` schaltet die
  sops_files-Dekryptierung auf den Sidecar um (injiziertes Sops gewinnt);
  Benachrichtigungen: `deploy_failed` mit Stage, `prune_failed`,
  `repo_failed` (max. einmal pro Pass) und `update_available`, Notifier lazy +
  Neuaufbau nur bei geänderter Config, `Deps.Notifier` gewinnt,
  `Deps.NewNotifier` nil → `notify.New`, INFO `notifications active` je Pass;
  `updatecheck.go` prüft nach den Deploys die Images (`Deps.Registry`
  injizierbar, sonst `registry.New` aus `registries` mit Key-Vergleich) und
  ruft am Ende `applyUpdates` aus `apply.go` für `update_policy: apply`
  (Pull, gezieltes `up --no-deps`, Health-Gate über das unexportierte Feld
  `healthPoll`, Rollback per Digest-Override-Datei)
- `internal/webhook/status.go` — `GET /status` (Phase 8): Drift-Matrix pro
  State-Stack (`ok`/`stopped`/`partial`, ps-Fehler → `unknown` +
  `"error"`-Feld, Endpoint bleibt 200), unmanaged = laufende Compose-
  Projekte ohne State-Eintrag minus Eigenprojekt, `host_repo` mit HEAD-Commit
  via injizierter Callback-Funktion, Bearer-Schutz (`DOCKOPS_STATUS_TOKEN`,
  constant-time, leer = offen wie `/healthz`), Stacks/Unmanaged immer
  sortierte JSON-Arrays, additives `updates`-Feld (`StackUpdates`/
  `ServiceUpdate`) rein aus den State-Records (nil = Key fehlt);
  Dokument-Bau seit Phase 8b in exportierter `BuildStatus(ctx, StatusDeps)`
  (Handler und `-print-status` teilen sie)
- `internal/notify/` — Benachrichtigungs-Transport (Phase 9): Sender für
  `webhook` (JSON), `ntfy` (Klartext + Title/Priority/Tags-Header),
  `telegram` (Bot-API, Text auf 4000 Zeichen, kein `parse_mode`),
  `discord`/`slack` (`content`/`text`); per-Destination-Timeout, Kürzung des
  Fehlertexts auf 2000 Zeichen, In-Memory-Dedupe pro `(kind, stack)` mit
  `repeat_after`, `Recovery` löscht den Key, Fehler nur als WARN (Typ + Host)
- `internal/notify/notify_test.go` — Phase-9-Sendertests: `httptest` je Kanal
  (Methode/Pfad/Header/Body), Timeout blockiert die Folgekanäle nicht, Dedupe
  mit injizierter Clock, Recovery, `on`-Filter, Kürzung, keine Secrets im Log
- `internal/registry/` — Registry-Client (Phase 10): `ParseRef`,
  `Resolver`/`Client`/`Credentials`/`New`; `RemoteDigest` per `HEAD` auf
  `/v2/<repo>/manifests/<tag>`, Bearer-Challenge, Token-Cache je Host,
  Fehlerklassen und 10s-Timeout
- `internal/registry/registry_test.go` — Phase-10-Registrytests:
  Parsing-Tabelle, `httptest`-Challenge (401→Token→200, Basic-Auth nur mit
  Credentials), falsches Token, fehlender Digest-Header, Klassen 404/403/429,
  Timeout, `Accept` aller vier Manifest-Typen, Pin ohne HTTP
- `internal/reconciler/phase10_test.go` — Phase-10-Regressionstests: `pinned`
  und `update_check_interval: 0` ohne Registry-/Docker-Aufruf, Fälligkeit über
  das Intervall, genau ein `update_available` je neuem Digest (zweiter Pass
  still), Registry-Fehler nur im Record, lokal gebautes Image ohne Alarm,
  `ignore_services`, degradierendes Compose-Config
- `internal/webhook/phase10_test.go` — Phase-10-`/status`-Tests: `updates`
  aus dem State (policy/pending/checked_at/sortierte Services), Key fehlt
  ohne Records, Überleben eines ps-Fehlers
- `internal/compose/images_test.go` — Phase-10-Compose-Tests:
  `config --format json`-Parsing (Fake-docker via PATH, Build-only
  ausgelassen), `-f`-Reihenfolge, Fehlerfälle, RepoDigests-Match inkl.
  Hub-Normalisierung
- `internal/state/phase10_test.go` — Phase-10-State-Tests: Round-Trip,
  Altdatei ohne `images`, Version 1, `SetImages`-Semantik (Kopie, UTC,
  Ersetzen, unbekannter Stack)
- `internal/config/phase10_test.go` — Phase-10-Config-Tests: `registries`
  inkl. `${VAR}`/Fehlern, `update_check_interval` (Default/0/negativ),
  `update_policy` (Default/`pinned`/ungültig), `update_available` opt-in
- `internal/hash/phase10_test.go` — Phase-10-Hash-Test: `update_policy`
  ändert den Stack-Hash nicht
- `internal/reconciler/apply.go`/`internal/reconciler/phase11_test.go` —
  Phase-11-Apply: Fake-docker via PATH protokolliert die argv (pull/up/ps
  sequenziert beantwortet), `httptest`-Registry; Apply im fälligen Pass,
  Pull-Reihenfolge vor `up --no-deps`, Health-Gate-Erfolg (`applied_at`,
  `current_digest = remote_digest`), Rollback bei unhealthy/exited/Timeout
  (Override-Datei nach den Stack-Files, `previous_digest` gepinnt, Datei
  wieder entfernt), blockierter Digest wird nicht erneut versucht, lokal
  gebautes Image und nicht deployter Stack werden übersprungen, genau ein
  `update_failed` je Versuch, `healthPoll` = 1ms (keine echten sleeps)
- `internal/compose/phase11_test.go` — `Pull`/`UpOverride`-argv (Override-Datei
  hinter den Stack-Files, `--no-deps`, positionale Services) und `Service` aus
  `compose ps --format json`
- `internal/config/phase11_test.go` — `update_policy: apply`, `health_timeout`
  (Default/0/negativ), `update_failed` im Default-Set, `update_applied` opt-in
- `internal/state/phase11_test.go` — Apply-Felder Round-Trip, Altdaten ohne
  sie, `fileVersion` bleibt 1
- `internal/webhook/phase11_test.go` — `ServiceUpdate`-Apply-Felder und
  `policy` aus dem State
- `internal/hash/phase11_test.go` — `health_timeout`/`update_policy: apply`
  ändern den Stack-Hash nicht
- `internal/webhook/phase8b_test.go` — Phase-8b-Regressionstest:
  `BuildStatus` mit identischen Kollaborateuren wie der Endpoint liefert
  dasselbe Dokument (inkl. beider Fehler-Modi), fehlende Konfiguration ist
  der einzige Fehler
- `cmd/dockops/phase8b_test.go` — Phase-8b-CLI-Tests: `-print-status`
  (Default/Env/Flag/ungültiges Env, `-repo`-Befreiung) und `writeStatus`
  (2-Spaces-Indent, Newline, Docker-Fehler im Dokument statt Fehler)
- `internal/reconciler/phase9_test.go` — Phase-9-Regressionstests:
  `compose_up`-Fehler (auch über Fake-`docker` via PATH) erzeugt genau ein
  `deploy_failed` mit Stage, `pre_deploy`-Hook → `stage=pre_deploy`,
  erfolgreicher Pass → kein Event + Recovery, zwei Pässe mit identischem
  Fehler → ein POST, Prune-Fehler → `prune_failed`, Repo-Fehler →
  `repo_failed`, kein Notifier-Bau ohne Block, `notifications active` einmal
  pro Pass
- `internal/reconciler/phase6c_test.go` — Phase-6c-Regressionstests: die
  drei Prune-Fälle (a/b/c), Skip-über-Prune-Pass (identischer Hash deployt
  nicht neu) und der unterbrochene Pass (State-Dir wird mid-Pass read-only,
  Restart aus der Datei — ohne per-Stack-Persistenz Recreate + vergessener
  Prune, danach grün)
- `internal/webhook/` — Webhook-HTTP-Handler: `POST /webhook` mit
  GitHub-HMAC-SHA256 (`X-Hub-Signature-256`) oder GitLab-Token
  (`X-Gitlab-Token`), konstante Vergleiche, `GET /healthz`
- `internal/reconciler/e2e_test.go` — E2E-Integrationstest: komplette
  Reconcile-Durchläufe gegen Bare-Repos (Host + Shared-Remote), Fake-
  `docker`/`sops`-Skripte via PATH-Override, Fake-Vault via
  `httptest.Server`; Pass 5 löscht die Stack-Ordner im Checkout komplett
  (Prune muss per `DownProject` funktionieren)
- `Dockerfile` — Multi-Stage-Build (statisches Binary, alpine-Runtime mit
  docker-cli + compose-Plugin, git, openssh-client, CA-Certs; bewusst KEIN
  sops — Decrypt-Last trägt der Sidecar)
- `cmd/sops-sidecar/Dockerfile.sidecar` — Sidecar-Image: Build-Stage
  kompiliert das Binary statisch, Runtime alpine + sops + CA-Certs (mehr
  nicht — Credentials/Vault-Transit leben hier, nicht im dockops-Image)
- `compose.yaml` — dockops selbst als Container: docker.sock-Mount,
  SSH-Key-Mount (ro), `DOCKOPS_*`-Env (inkl. `DOCKOPS_SOPS_SIDECAR_URL`,
  per Shell/.env substituiert), Vault-Env, Port 8080; optionaler
  `sops-sidecar`-Service hinter Profil `sops` (gemeinsames Volume read-
  only, kein published Port)
- `README.md` — Dokumentation: Architektur, Config-Beispiele (Root +
  immich-Stack, beide Import-Varianten, vault.env-Template), Reconcile-
  Ablauf, Secret-Priorität, SOPS decryption (lokal vs. Sidecar inkl.
  HTTP-Vertrag), CLI-Referenz, Docker-Deployment, Development
- `PHASE1_TASK.md` — Aufgabenstellung (nicht ändern)
- `PHASE2_TASK.md` — Aufgabenstellung (nicht ändern)
- `PHASE3_TASK.md` — Aufgabenstellung (nicht ändern)
- `PHASE4_TASK.md` — Aufgabenstellung (nicht ändern)
- `PHASE6A_TASK.md` — Aufgabenstellung (nicht ändern)
- `PHASE6B_TASK.md` — Aufgabenstellung (nicht ändern)
- `PHASE6C_TASK.md` — Aufgabenstellung (nicht ändern)

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
