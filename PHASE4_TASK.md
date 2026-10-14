# Phase 4: Reconciler-Loop, CLI & Webhook-Server für "dockops"

Projekt: `/home/hermes/projects/dockops`, Go 1.25.4 (`export PATH=$HOME/.local/go/bin:$PATH`). Lies zuerst `CLAUDE.md` und alle `internal/*`-Packages. Bestehende Packages nur erweitern, nicht brechen.

## Kontext (Stände aus Phase 1–3)
- `internal/config`: Root-`stacks.yaml`, Stack-Discovery, Import-Merge, Validierung.
- `internal/git`: `Manager.EnsureRepo(name, url, branch)` — Clone-or-Update nach `<workDir>/<name>`.
- `internal/hash`: `Compute(stack, composeContent)`.
- `internal/state`: JSON-State-Store (`/var/lib/dockops/state.json` Pfad konfigurierbar).
- `internal/vault`, `internal/sops`, `internal/env`, `internal/hooks`, `internal/compose`: Secrets, Merge, SHM-Env-File, Hooks, Compose-Executor.

## Aufgabe 1: Reconciler `internal/reconciler`
Das Herzstück — verdrahte alle Packages zu einem Reconciliation-Loop.

```go
type Reconciler struct { ... }
func New(deps ...) *Reconciler
func (r *Reconciler) Reconcile(ctx context.Context) error   // ein kompletter Durchlauf
```

Ablauf von `Reconcile`:
1. Primary Host-Repo: `EnsureRepo("host", repoURL, branch)` — der Aufrufer (CLI) liefert URL/Branch; das Host-Repo liegt danach unter `<workDir>/host`. Root-`stacks.yaml` aus dem Host-Repo laden (Phase-1-Loader mit Root-Pfad `<workDir>/host`).
2. Alle `remotes` aus der Config: `EnsureRepo(name, url, branch)` parallel oder sequenziell (sequenziell reicht).
3. Config-Laden + Import-Merge (Phase 1).
4. Für jeden Stack (enabled=true):
   a. Compose-Datei lesen, `hash.Compute` → newHash.
   b. State-Vergleich: `state.Get(stackName)`; wenn Hash identisch → skip (log debug).
   c. Secrets auflösen: vault (nur wenn `vault_paths` non-empty → Vault-Client + Login einmal pro Reconcile-Durchlauf), sops.
   d. `env.MergeStackEnv` → `env.WriteShm` → defer cleanup.
   e. Hooks `pre_deploy` (Fehler → Stack überspringen, Reconcile läuft weiter, Fehler sammeln).
   f. `compose.Up`.
   g. Hooks `post_deploy` (Fehler → log error, Stack gilt trotzdem als deployed).
   h. State setzen: newHash + deployedAt.
5. Prune: Wenn `global.prune_removed_stacks == true`: alle State-Einträge, deren Stack-Name nicht mehr in der Config existiert → `compose.Down` + State-Remove. (Down braucht compose_file/stackDir — wenn das Verzeichnis weg ist, `docker compose -p <name> down --remove-orphans` ohne -f Attempt, mit Fehlerlog falls fehlschlägt.)
6. Rückgabe: `errors.Join` aller aufgetretenen Fehler (nil wenn alle gut).

Reconcile muss idempotent sein und via Context abbrechbar.

## Aufgabe 2: CLI `cmd/dockops/main.go` (komplett neu schreiben)
- Flags: `-repo` (Env `DOCKOPS_REPO_URL`), `-branch` (Env `DOCKOPS_BRANCH`, default "main"), `-state` (default `/var/lib/dockops/state.json`), `-webhook-secret` (Env `DOCKOPS_WEBHOOK_SECRET`), `-addr` (default ":8080"), `-once` (single reconcile + exit), `-interval` (override poll_interval, z.B. "5m").
- Modes:
  - `-once`: ein Reconcile, exit code 0/1.
  - Poll-Mode (default wenn kein -addr… nein): Immer: Webhook-Server starten (außer `-once`), UND Poll-Timer aus Config `global.poll_interval` (oder -interval Override). Webhook → triggert sofortigen Reconcile (debounced: laufender Reconcile wird nicht unterbrochen; ein neuer Request während eines Laufs wird zu genau einem nachfolgenden Lauf gemerged).
- Graceful Shutdown: SIGINT/SIGTERM → Server down, laufender Reconcile bekommt Context-Cancel, Cleanup (z.B. SHM-Files) via defer.
- Loggen mit slog JSON auf stdout.

## Aufgabe 3: Webhook-Server `internal/webhook`
- `POST /webhook`: HMAC-SHA256-Signaturprüfung. Header: `X-Hub-Signature-256` (GitHub: `sha256=<hex>`) UND `X-Gitlab-Token` (GitLab: plain token Vergleich mit subtle.ConstantTimeCompare) — wenn `webhookSecret` konfiguriert, muss mindestens eine Methode matchen, sonst 401.
  - GitHub: `hex(HMAC-SHA256(secret, body))` mit konstantem Vergleich gegen Header-Wert nach "sha256="-Prefix-Strip.
  - GitLab: Header-Wert == Secret.
- `GET /healthz` → 200 "ok".
- Handler löst `trigger()`-Callback aus, das der Reconciler-Loop kennt.

## Tests
- reconciler: mit Fake-Strukturen — für git/vault/sops/compose Interfaces oder Funktions-Felder injizieren, damit Tests ohne echte Binaries laufen. Abdeckung: skip-bei-gleichem-Hash, pre_deploy-Fehler überspringt Stack aber Reconcile läuft weiter, post_deploy-Fehler → trotzdem deployed, prune entfernt weggefallene Stacks, Hook-Env enthält STACK_NAME/STACK_DIR/GIT_COMMIT + Secrets.
- webhook: httptest mit gültiger/ungültiger GitHub-Signatur (sha256), GitLab-Token, 401-Fälle, healthz.
- CLI: minimal — Flag-Parsing als eigene Funktion mit Test.

## Definition of Done
- `go build ./...` ✅ `go vet ./...` ✅ `go test ./... -count=1` ✅ — alles grün, keine Regressionen.
