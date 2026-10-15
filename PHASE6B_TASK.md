# Phase 6b: SOPS-Sidecar, Vault-Template-Unterstützung, Image-Update

Projekt: `/home/hermes/projects/dockops`, Go 1.25.4 (`export PATH=$HOME/.local/go/bin:$PATH`). Lies zuerst `CLAUDE.md` und alle `internal/*`-Packages. Bestehende Packages nur erweitern, nicht brechen. Voraussetzung: Phase 6a ist gemerged (Stack-Dir-Env, services/ignore_services, compose_files). Update CLAUDE.md am Ende.

## Hintergrund
- Fleet-Realität: 18 Stacks entschlüsseln via `sops -d` mit **Vault-Transit** (`.sops.yaml` mit `hc_vault_transit_uri`), 18 Stacks nutzen `vault-env --template <tpl> --env <out>` mit `${VAULT:path#key}`-Syntax.
- Nutzer-Entscheidung: **SOPS-Binary + Credentials kommen NICHT ins dockops-Image** — stattdessen ein Sidecar-Pattern.

## Aufgabe 1 (Prio 3): SOPS-Sidecar `internal/sops`
Umstellung von lokalem `sops -d`-Exec auf einen decrypt-Dienst:

- `internal/sops` bekommt NEU einen zweiten Resolver: `type SidecarResolver struct; NewSidecar(baseURL string, httpClient *http.Client) *SidecarResolver`.
- Sidecar-HTTP-Vertrag (wir definieren ihn, Implementierung als separates Mini-Programm `cmd/sops-sidecar/main.go` im selben Repo):
  - `POST /decrypt` mit JSON-Body `{"path": "/abs/path/file.env"}` → Response `{"content": "<dotenv-plaintext>"}` oder HTTP 4xx/5xx `{"error": "..."}`.
  - Der Sidecar läuft als eigener Container, teilt sich per Volume die Stack-Verzeichnisse (read-only reicht für Decrypt) und hat `sops`-Binary + `.sops.yaml` + Vault-Transit-Zugriff.
  - Sicherheit: Sidecar lauscht NUR auf localhost des Docker-Netzes (kein Published Port nötig — dockops und Sidecar teilen ein compose-Netz), Bindung auf `127.0.0.1`-Erreichbarkeit im internen Netz ist genug. Kein Auth nötig (Netz-Isolation), aber: Response-Größenlimit (10 MB) und Request-Timeout (60s) im Client.
- `cmd/sops-sidecar/main.go`: schlanker HTTP-Server (stdlib `net/http`), führt `sops -d --input-type dotenv --output-type dotenv <path>` aus (Pfad-Validierung: nur Dateien unter erlaubten Wurzeln — env `SOPS_SIDECAR_ALLOWED_ROOTS`, Doppelpunkt-Separated; blockt `..`-Traversal via `filepath.Clean`+Prefix-Check), loggt mit slog, graceful shutdown.
- Reconciler-Verdrahtung: NEU CLI-Flag `-sops-sidecar` (Env `DOCKOPS_SOPS_SIDECAR_URL`, default leer). Wenn gesetzt → SidecarResolver für alle sops_files; wenn leer → bestehender lokaler `sops -d`-Exec (Fallback, für Tests/Einzel-Host-Betrieb).
- `internal/sops`-Tests: httptest-Server als Fake-Sidecar (erfolgreicher Decrypt, 4xx mit Error-Body, Timeout-Verhalten, Größenlimit, Traversal-Block im Sidecar-Handler selbst via `httptest` + echtem Handler).

## Aufgabe 2 (Prio 5): Vault-Template-Syntax `internal/vault` + `internal/env`
Unterstützung für `${VAULT:path#key}`-Templates (Kompatibilität mit bestehenden vault.env-Files):

- NEU in `internal/env`: `func RenderVaultTemplate(ctx, templateContent string, vault secrets.VaultReader) (map[string]string, error)`:
  - Findet alle `${VAULT:<path>#<key>}`-Vorkommen (Regex `\$\\{VAULT:[^}]+\\}`), resolved jeden via KV-Read (Batche nicht — KV-Reads sind billig und gecached auf Client-Seite optional).
  - `path` ist der KV-Pfad unterm Mount (wie `vault_paths`), `key` ist der Feldname in der `data.data`-Map.
  - Alternativ-Syntax `${VAULT:path:key}` auch akzeptieren (beide Trenner).
  - Erzeugt pro Template-Datei eine Map `KEY=resolved_value` — ABER: die KEY-Namen links vom `=` stehen IM Template (Zeilenformat `MY_VAR=${VAULT:hosts/host-01/traefik#db_password}`). Render-Ergebnis ist also: Zeile für Zeile, `${VAULT:...}` durch den Wert ersetzt, alles andere literal.
  - Fehlender Key/Path → klarer Fehler mit Template-Dateiname + Referenz.
- NEU in `internal/config` Stack-Def: `vault_env_templates: []string` (relativ zum Stack-Dir; bei Imports concat+dedupe wie vault_paths).
- Merge-Priorität: vault_env_templates liegen auf der gleichen Stufe wie vault_paths (höher als sops, niedriger als Systemvars); bei mehreren Templates in Listenreihenfolge, später gewinnt.
- Tests: Template-Rendering mit Fake-Vault (httptest), beide Trenner-Syntaxen, mehrfache Referenzen pro Zeile, fehlender Key → Fehler mit Kontext, Merge-Priorität.

## Aufgabe 3: Image + compose.yaml Update
- `Dockerfile` bleibt schlank (KEIN sops, KEIN vault-client) — nur dockops-Binary + docker-cli + compose + openssh + ca-certificates + git (bereits so).
- NEU `cmd/sops-sidecar/Dockerfile.sidecar`: kleiner Sidecar-Container (alpine + sops + ca-certificates + das Sidecar-Binary; build-stage kompiliert das Binary statisch).
- `compose.yaml` (dockops' eigene) erweitern: optionales `sops-sidecar`-Profil (`profiles: ["sops"]`), dockops-Service bekommt `DOCKOPS_SOPS_SIDECAR_URL=http://sops-sidecar:8081` nur wenn Profil aktiv, gemeinsames Volume für Stack-Dirs read-only im Sidecar.
- README: Abschnitt "SOPS decryption" mit beiden Modi (lokal vs. sidecar) und dem HTTP-Vertrag.

## Tests
- Siehe oben je Aufgabe. Alle stdlib testing, keine Regressionen.

## Definition of Done
- `go build ./...` ✅ `go vet ./...` ✅ `go test ./... -count=1` ✅
- `go build ./cmd/sops-sidecar` ✅ (separates Binary kompiliert)
