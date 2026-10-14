# Phase 3: Secret-Resolver, Hook-Engine & Docker-Compose-Executor für "dockops"

Projekt: `/home/hermes/projects/dockops`, Go 1.25.4 (`export PATH=$HOME/.local/go/bin:$PATH`). Lies zuerst `CLAUDE.md` und die Packages `internal/config`, `internal/git`, `internal/hash`, `internal/state`. Nichts an bestehenden APIs kaputt machen — nur erweitern falls nötig.

## Aufgabe 1: Vault-Client `internal/vault`
HashiCorp Vault KV v2, implementiert über net/http (KEINE Vault-Go-Library, damit die Testbarkeit mit httptest einfach bleibt und die Binary schlank).

- `type Client struct` mit:
  - `New(cfg config.Vault, logger *slog.Logger) (*Client, error)` — kein Login hier, nur Setup.
  - `Login(ctx context.Context) error`:
    - auth_method "token": Token aus Env `VAULT_TOKEN` (Fehler wenn leer und kein Login nötig). Der Token gilt direkt.
    - auth_method "approle": Rolle-ID aus `VAULT_ROLE_ID`, Secret-ID aus `VAULT_SECRET_ID`; POST an `/v1/auth/approle/login` mit `{"role_id","secret_id"}`,レスポONSE `auth.client_token` extrahieren.
  - `ReadKV(ctx, path string) (map[string]string, error)` — KV v2: GET `<address>/v1/<mount>/data/<path>` mit `X-Vault-Token` Header. Antwort: `data.data` als map[string]string (nur string-Werte; Nicht-String-Werte mit fmt.Sprintf("%v") flachen). 404 → Fehler `not found`. Andere Status → Fehler mit Statuscode und Body-Snippet (max 200 Zeichen).
  - Address-Handling: `cfg.Address` oder Env-Override `VAULT_ADDR`; Trailing-Slash normalisieren.

## Aufgabe 2: SOPS `internal/sops`
- `func DecryptFile(ctx context.Context, path string) (map[string]string, error)`:
  - `sops -d --input-type dotenv --output-type dotenv <path>` via os/exec mit kontext.
  - Output dotenv-Parsing (KEY=VALUE, Zeilen ohne `=` oder mit führendem `#` ignorieren, keine Shell-Expansion).
  - Fehler wenn Binary fehlt: klarer Hinweis "sops binary not found in PATH".
- `func DecryptFiles(ctx, files []config.SopsFile, baseDir string) (merged map[string]string, err error)` — bei ersten Fehler abbrechen.

## Aufgabe 3: Env-Merge `internal/env`
- `type Provider interface { Env(ctx context.Context, stack config.Stack) (map[string]string, error) }`-artiger Ansatz nicht nötig; stattdessen:
- `func MergeStackEnv(ctx, stack config.Stack, secrets secrets.Resolver, baseDir string) (map[string]string, error)`:
  - Priorität (niedrig → hoch): Basis-env_files < Import-env_files < sops_files < Basis-vault_paths < Import-vault_paths < Systemvars.
  - env_files: dotenv-Parsen der Dateien im Stack-Dir (gleiches Format wie SOPS-Output).
  - Systemvars: `STACK_NAME`, `STACK_DIR`, `GIT_COMMIT` (Werte werden als Parameter übergeben, nicht aus os.Getenv).
  - Basis/Import-Unterscheidung: Der Config-Loader (Phase 1) hat bereits gemergte Listen mit "Basis zuerst, dann Import"; nutze das: die Merge-Reihenfolge ist einfach die Reihenfolge in den gemergten Listen — spätere Einträge gewinnen. Dokumentiere das.
- `func WriteShm(env map[string]string) (path string, cleanup func(), err error)`:
  - Schreibt dotenv-Format nach `/dev/shm/dockops-<random>-<stackhash>.env` mit `0600`, Parent-Verzeichnis `/dev/shm` (falls nicht vorhanden: Fallback TMPDIR mit Warn-Log).
  - cleanup: Inhalt überschreiben (mit Zufallsbytes gleicher Länge), Datei entfernen. Muss idempotent sein.

## Aufgabe 4: Hooks `internal/hooks`
- `type Engine struct; New(logger) *Engine`
- `func (e *Engine) Run(ctx context.Context, commands []string, dir string, env []string) error`:
  - Sequenziell `sh -c <cmd>` mit `context.WithTimeout` (Timeout aus Parameter, Default aus globaler Config z.B. 5m; übernehme als Parameter).
  - stdout/stderr durchreicht an slog (Level Info, mit Kommandoname als Attribut).
  - Exit-Code != 0 → Fehler `hook failed: exit code N`; Timeout → `hook timed out after ...`.
- Tests: Exit!=0 bricht ab (nachfolgende Hooks laufen nicht mehr), Timeout-Verhalten, Env wird gesetzt (Hook schreibt `env` in Datei zum Nachweisen), cwd korrekt.

## Aufgabe 5: Compose-Executor `internal/compose`
- `type Executor struct; New(logger, binary string) *Executor` — binary leer = "docker".
- `func (e *Executor) Up(ctx, stack config.Stack, stackDir, envFile string) error`:
  - Baut: `docker compose -p <stack.Name> -f <stackDir>/<compose_file> [--profile p ...] --env-file <envFile> up -d --remove-orphans` + falls `len(stack.Services) > 0`: `--` dann die Service-Namen? NEIN — docker compose nimmt Services als Positionsargumente ohne `--`. Also: Services direkt anhängen.
  - `-p <name>` explizit (Projekt-Name = Stack-Name, damit deterministische Projektnamen).
- `func (e *Executor) Down(ctx, stack config.Stack, stackDir, envFile string) error` — `docker compose -p <name> -f ... down --remove-orphans` (für Prune).
- Alle Aufrufe mit CombinedOutput in slog ausgeben; Exit-Code != 0 → Fehler mit Ausgabe-Snippet.

## Tests
- Vault: httptest.Server für Login (approle+token), KV-Read, 404, Nicht-String-Werte, Env-Override VAULT_ADDR.
- SOPS: Fake-`sops`-Binary als Shell-Skript in TMPDIR (PATH manipulieren), dotenv-Parsing, fehlende Binary.
- env: Merge-Priorität, Systemvars, WriteShm (TMPDIR auf t.TempDir zeigen lassen, Cleanup idempotent, 0600-Prüfung via os.Stat).
- hooks: wie oben beschrieben.
- compose: Fake-`docker`-Binary, Flag-/Arg-Validierung (Args als Slice prüfen, env-file Pfad, -p, profiles, services, up/down).
- Alles stdlib testing, keine testify.

## Definition of Done
- `go build ./...` ✅ `go vet ./...` ✅ `go test ./... -count=1` ✅ — alles grün, keine Regressionen in config/git/hash/state.
